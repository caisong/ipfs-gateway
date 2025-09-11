package gateway

import (
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	boxogateway "github.com/ipfs/boxo/gateway"
	"github.com/ipfs/boxo/ipld/merkledag"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"ipfs-gateway/pubsub"
)

// Transfer protocol constants
const (
	// TransferProtocol defines the protocol ID for file transfer
	TransferProtocol protocol.ID = "/ipfs-gateway/transfer/1.0.0"
	// DownloadRequestProtocol defines the protocol ID for download requests
	DownloadRequestProtocol protocol.ID = "/ipfs-gateway/download/1.0.0"
)

// Transfer message types
type TransferRequest struct {
	RequestID string `gob:"request_id"`
	FilePath  string `gob:"file_path"`
	Timestamp int64  `gob:"timestamp"`
}

type TransferResponse struct {
	RequestID string `gob:"request_id"`
	Success   bool   `gob:"success"`
	Error     string `gob:"error"`
	Timestamp int64  `gob:"timestamp"`
}

// FileChunk represents a chunk of file data during transfer
type FileChunk struct {
	RequestID   string `gob:"request_id"`
	ChunkIndex  int    `gob:"chunk_index"`
	TotalChunks int    `gob:"total_chunks"`
	Data        []byte `gob:"data"`
	IsLast      bool   `gob:"is_last"`
	Filename    string `gob:"filename"`
	FileSize    int64  `gob:"file_size"`
}

type DownloadRequest struct {
	RequestID string `gob:"request_id"`
	FilePath  string `gob:"file_path"`
	Timestamp int64  `gob:"timestamp"`
}

type DownloadResponse struct {
	RequestID string `gob:"request_id"`
	Success   bool   `gob:"success"`
	Error     string `gob:"error"`
	Timestamp int64  `gob:"timestamp"`
}

// DownloadTask represents a download task
type DownloadTask struct {
	TaskID      string     `json:"task_id"`
	TargetNode  string     `json:"target_node"`
	FilePath    string     `json:"file_path"`
	Status      string     `json:"status"` // "pending", "in_progress", "completed", "failed"
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	LocalPath   string     `json:"local_path,omitempty"`
}

// Service represents the gateway service
type Service struct {
	ctx       context.Context
	cancel    context.CancelFunc
	echo      *echo.Echo
	gwHandler http.Handler
	host      host.Host
	dht       *dht.IpfsDHT

	// Shared IPFS components
	datastore    datastore.Datastore
	blockstore   blockstore.Blockstore
	bitswap      *bitswap.Bitswap
	blockService blockservice.BlockService
	dag          format.DAGService

	// Download configuration
	downloadDir string
	serveDir    string

	// Task management
	downloadTasks map[string]*DownloadTask
	taskMutex     sync.RWMutex
}

// NewService creates a new gateway service
func NewService(ctx context.Context, host host.Host, kadDHT *dht.IpfsDHT, port int) (*Service, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Create shared IPFS components
	ds := dssync.MutexWrap(datastore.NewMapDatastore())
	bstore := blockstore.NewBlockstore(ds)
	bsNet := bsnet.NewFromIpfsHost(host)
	bs := bitswap.New(ctx, bsNet, kadDHT, bstore)
	blockService := blockservice.New(bstore, bs)
	dag := merkledag.NewDAGService(blockService)

	// Initialize official remote block storage backend
	backend, err := boxogateway.NewBlocksBackend(blockService)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create remote backend: %w", err)
	}

	// Configure gateway
	gatewayCfg := boxogateway.Config{
		PublicGateways: map[string]*boxogateway.PublicGateway{
			"localhost": {
				Paths:         []string{"/ipfs", "/ipns"},
				UseSubdomains: false,
			},
		},
	}

	// Create standard gateway handler
	gwHandler := boxogateway.NewHandler(gatewayCfg, backend)

	// Initialize Echo framework
	e := echo.New()
	e.HideBanner = true

	// Set middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// Set timeout
	e.Use(middleware.TimeoutWithConfig(middleware.TimeoutConfig{
		Timeout: 30 * time.Second,
	}))

	service := &Service{
		ctx:           ctx,
		cancel:        cancel,
		echo:          e,
		gwHandler:     gwHandler,
		host:          host,
		dht:           kadDHT,
		datastore:     ds,
		blockstore:    bstore,
		bitswap:       bs,
		blockService:  blockService,
		dag:           dag,
		downloadDir:   "./downloads",
		serveDir:      "./serve",
		downloadTasks: make(map[string]*DownloadTask),
	}

	// Set up transfer stream handlers
	service.setupTransferHandlers()

	// Create download directory
	if err := os.MkdirAll(service.downloadDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create download directory: %w", err)
	}

	// Create serve directory
	if err := os.MkdirAll(service.serveDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create serve directory: %w", err)
	}

	service.setupRoutes()

	return service, nil
}

// NewServiceWithComponents creates a new gateway service using shared IPFS components
func NewServiceWithComponents(ctx context.Context, components *pubsub.IPFSComponents, port int) (*Service, error) {
	ctx, cancel := context.WithCancel(ctx)

	// Initialize official remote block storage backend using shared components
	backend, err := boxogateway.NewBlocksBackend(components.BlockService)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create remote backend: %w", err)
	}

	// Configure gateway
	gatewayCfg := boxogateway.Config{
		PublicGateways: map[string]*boxogateway.PublicGateway{
			"localhost": {
				Paths:         []string{"/ipfs", "/ipns"},
				UseSubdomains: false,
			},
		},
	}

	// Create standard gateway handler
	gwHandler := boxogateway.NewHandler(gatewayCfg, backend)

	// Initialize Echo framework
	e := echo.New()
	e.HideBanner = true

	// Set middleware
	e.Use(middleware.Logger())
	e.Use(middleware.Recover())
	e.Use(middleware.CORS())

	// Set timeout
	e.Use(middleware.TimeoutWithConfig(middleware.TimeoutConfig{
		Timeout: 30 * time.Second,
	}))

	service := &Service{
		ctx:           ctx,
		cancel:        cancel,
		echo:          e,
		gwHandler:     gwHandler,
		host:          components.Host,
		dht:           components.DHT,
		datastore:     components.Datastore,
		blockstore:    components.Blockstore,
		bitswap:       components.Bitswap,
		blockService:  components.BlockService,
		dag:           components.DAG,
		downloadDir:   "./downloads",
		serveDir:      "./serve",
		downloadTasks: make(map[string]*DownloadTask),
	}

	// Set up transfer stream handlers
	service.setupTransferHandlers()

	// Create download directory
	if err := os.MkdirAll(service.downloadDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create download directory: %w", err)
	}

	// Create serve directory
	if err := os.MkdirAll(service.serveDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create serve directory: %w", err)
	}

	service.setupRoutes()

	return service, nil
}

// setupRoutes sets up routes
func (s *Service) setupRoutes() {
	// IPFS content routing
	s.echo.Any("/ipfs/*", s.handleIPFSRequest)
	s.echo.Any("/ipns/*", s.handleIPNSRequest)

	// API routing
	api := s.echo.Group("/api/v1")

	// File operations
	api.POST("/files/download", s.handleDownloadFile) // CID download using bsnet
	api.POST("/files/upload", s.handleUploadFile)     // Upload file to gateway
	api.POST("/files/publish", s.handlePublishFile)   // Publish file to target node

	// Task-based target path downloads
	api.POST("/files/download-task", s.handleCreateDownloadTask)   // Create download task, return task ID
	api.GET("/files/task/:task_id", s.handleGetTaskStatus)         // Get task status
	api.GET("/files/task/:task_id/download", s.handleDownloadTask) // Download file by task ID

	// File pushing
	api.POST("/files/push", s.handlePushFile) // Push file to target node
}

// handleIPFSRequest handles IPFS requests
func (s *Service) handleIPFSRequest(c echo.Context) error {
	path := c.Request().URL.Path
	log.Printf("Processing IPFS request: %s", path)
	return s.proxyToRemoteGateway(c)
}

// handleIPNSRequest handles IPNS requests
func (s *Service) handleIPNSRequest(c echo.Context) error {
	path := c.Request().URL.Path
	log.Printf("Processing IPNS request: %s", path)

	// IPNS requests go directly to remote gateway
	return s.proxyToRemoteGateway(c)
}

// extractCIDFromPath extracts CID from path
func (s *Service) extractCIDFromPath(path string) (string, error) {
	if !strings.HasPrefix(path, "/ipfs/") {
		return "", fmt.Errorf("not an IPFS path")
	}

	cidPart := strings.TrimPrefix(path, "/ipfs/")
	// Remove possible subpaths
	if idx := strings.Index(cidPart, "/"); idx != -1 {
		cidPart = cidPart[:idx]
	}

	if cidPart == "" {
		return "", fmt.Errorf("empty CID")
	}

	return cidPart, nil
}

// proxyToRemoteGateway proxies to remote gateway
func (s *Service) proxyToRemoteGateway(c echo.Context) error {
	s.gwHandler.ServeHTTP(c.Response().Writer, c.Request())
	return nil
}

// Start starts the gateway service
func (s *Service) Start() error {
	log.Println("Starting gateway service on port 8080")
	return s.echo.Start(":8080")
}

// Stop stops the gateway service
func (s *Service) Stop() error {
	log.Println("Stopping gateway service...")
	s.cancel()
	return s.echo.Shutdown(s.ctx)
}

// handleDownloadFile handles file download requests
func (s *Service) handleDownloadFile(c echo.Context) error {
	type DownloadRequest struct {
		CID      string `json:"cid"`
		Filename string `json:"filename"`
	}

	var req DownloadRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	if req.Filename == "" {
		req.Filename = req.CID + ".dat"
	}

	err := s.DownloadFile(req.CID, req.Filename)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Download completed", "filename": req.Filename})
}

// handleUploadFile handles file upload requests
func (s *Service) handleUploadFile(c echo.Context) error {
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "No file uploaded"})
	}

	filename := file.Filename
	filePath := fmt.Sprintf("./downloads/%s", filename)

	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to open uploaded file"})
	}
	defer src.Close()

	dst, err := os.Create(filePath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to create file"})
	}
	defer dst.Close()

	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "Failed to save file"})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message":  "File uploaded successfully",
		"filename": filename,
	})
}

// handlePublishFile handles file publishing via pubsub
func (s *Service) handlePublishFile(c echo.Context) error {
	type PublishRequest struct {
		FilePath   string `json:"file_path"`
		Topic      string `json:"topic,omitempty"`
		TargetNode string `json:"target_node"` // Target peer ID
	}

	var req PublishRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	if req.Topic == "" {
		req.Topic = "ipfs-gateway-files" // Default topic
	}

	if req.TargetNode == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Target node ID is required"})
	}

	// Publish file via pubsub to specific target node
	err := pubsub.PublishFile(s.ctx, s.getIPFSComponents(), req.Topic, req.FilePath, req.TargetNode)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message":     "File CID published successfully",
		"target_node": req.TargetNode,
	})
}

// DownloadFile downloads a file via CID using shared components
func (s *Service) DownloadFile(cidStr string, filename string) error {
	// Parse CID
	targetCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	// Create transfer context
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	// Get block via bitswap
	block, err := s.bitswap.GetBlock(ctx, targetCID)
	if err != nil {
		return fmt.Errorf("failed to get block: %w", err)
	}

	// Create target file
	filePath := filepath.Join(s.downloadDir, filename)
	outFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	// Write data
	bytesWritten, err := outFile.Write(block.RawData())
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	log.Printf("File download completed: %s (%d bytes) -> %s", cidStr, bytesWritten, filePath)
	return nil
}

// DownloadFileContent downloads a file via CID and returns content directly
func (s *Service) DownloadFileContent(cidStr string) ([]byte, error) {
	// Parse CID
	targetCID, err := cid.Decode(cidStr)
	if err != nil {
		return nil, fmt.Errorf("invalid CID: %w", err)
	}

	// Create transfer context
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	// Get block via bitswap
	block, err := s.bitswap.GetBlock(ctx, targetCID)
	if err != nil {
		return nil, fmt.Errorf("failed to get block: %w", err)
	}

	log.Printf("File content downloaded: %s (%d bytes)", cidStr, len(block.RawData()))
	return block.RawData(), nil
}

// handleDownloadByPath handles file download requests by target node and path using task-based approach
func (s *Service) handleDownloadByPath(c echo.Context) error {
	type DownloadByPathRequest struct {
		TargetNode string `json:"target_node"`        // Target node peer ID
		FilePath   string `json:"file_path"`          // File path on target node
		Filename   string `json:"filename,omitempty"` // Optional local filename
	}

	var req DownloadByPathRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	if req.TargetNode == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Target node ID is required"})
	}

	if req.FilePath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "File path is required"})
	}

	// Generate task ID
	taskID := fmt.Sprintf("%s-%d", s.host.ID().String(), time.Now().UnixNano())

	// Create download task
	task := &DownloadTask{
		TaskID:     taskID,
		TargetNode: req.TargetNode,
		FilePath:   req.FilePath,
		Status:     "pending",
		CreatedAt:  time.Now(),
	}

	// Store task
	s.taskMutex.Lock()
	s.downloadTasks[taskID] = task
	s.taskMutex.Unlock()

	// Start download in background
	go s.processDownloadTask(taskID)

	// Return task ID immediately (non-blocking)
	return c.JSON(http.StatusOK, map[string]interface{}{
		"task_id":     taskID,
		"target_node": req.TargetNode,
		"file_path":   req.FilePath,
		"status":      "pending",
		"message":     "Download task created. Use task_id to check status and download file.",
	})
}

// getIPFSComponents returns the IPFS components for pubsub operations
func (s *Service) getIPFSComponents() *pubsub.IPFSComponents {
	return &pubsub.IPFSComponents{
		Host:         s.host,
		DHT:          s.dht,
		Datastore:    s.datastore,
		Blockstore:   s.blockstore,
		Bitswap:      s.bitswap,
		BlockService: s.blockService,
		DAG:          s.dag,
	}
}

// handleCreateDownloadTask creates a download task and returns task ID
func (s *Service) handleCreateDownloadTask(c echo.Context) error {
	type CreateDownloadTaskRequest struct {
		TargetNode string `json:"target_node"`
		FilePath   string `json:"file_path"`
	}

	var req CreateDownloadTaskRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	if req.TargetNode == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Target node ID is required"})
	}

	if req.FilePath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "File path is required"})
	}

	// Generate task ID
	taskID := fmt.Sprintf("%s-%d", s.host.ID().String(), time.Now().UnixNano())

	// Create download task
	task := &DownloadTask{
		TaskID:     taskID,
		TargetNode: req.TargetNode,
		FilePath:   req.FilePath,
		Status:     "pending",
		CreatedAt:  time.Now(),
	}

	// Store task
	s.taskMutex.Lock()
	s.downloadTasks[taskID] = task
	s.taskMutex.Unlock()

	// Start download in background
	go s.processDownloadTask(taskID)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"task_id":     taskID,
		"target_node": req.TargetNode,
		"file_path":   req.FilePath,
		"status":      "pending",
	})
}

// processDownloadTask processes a download task using direct transfer
func (s *Service) processDownloadTask(taskID string) {
	s.taskMutex.Lock()
	task, exists := s.downloadTasks[taskID]
	if !exists {
		s.taskMutex.Unlock()
		return
	}
	task.Status = "in_progress"
	s.taskMutex.Unlock()

	// Parse target peer ID
	targetPeer, err := peer.Decode(task.TargetNode)
	if err != nil {
		s.updateTaskStatus(taskID, "failed", fmt.Sprintf("Invalid target peer ID: %v", err), "")
		return
	}

	// Use direct transfer to download file from target peer
	ctx, cancel := context.WithTimeout(s.ctx, 60*time.Second)
	defer cancel()

	// Use the integrated download functionality
	err = s.DownloadFileFromPeer(ctx, targetPeer, task.FilePath)
	if err != nil {
		s.updateTaskStatus(taskID, "failed", fmt.Sprintf("Transfer failed: %v", err), "")
		fmt.Printf("Download task %s failed: %v\n", taskID, err)
		return
	}

	// File downloaded successfully - determine local path
	filename := filepath.Base(task.FilePath)
	localPath := filepath.Join(s.downloadDir, filename)

	// Check if file exists locally
	if _, err := os.Stat(localPath); err != nil {
		s.updateTaskStatus(taskID, "failed", fmt.Sprintf("File not found after download: %v", err), "")
		fmt.Printf("File not found after download for task %s: %s\n", taskID, localPath)
		return
	}

	s.updateTaskStatus(taskID, "completed", "", localPath)
	fmt.Printf("Download task %s completed successfully: %s\n", taskID, localPath)
}

// updateTaskStatus updates the status of a download task
func (s *Service) updateTaskStatus(taskID, status, errorMsg, localPath string) {
	s.taskMutex.Lock()
	defer s.taskMutex.Unlock()

	task, exists := s.downloadTasks[taskID]
	if !exists {
		return
	}

	task.Status = status
	task.Error = errorMsg
	task.LocalPath = localPath
	if status == "completed" || status == "failed" {
		now := time.Now()
		task.CompletedAt = &now
	}
}

// handleGetTaskStatus returns the status of a download task
func (s *Service) handleGetTaskStatus(c echo.Context) error {
	taskID := c.Param("task_id")

	s.taskMutex.RLock()
	task, exists := s.downloadTasks[taskID]
	s.taskMutex.RUnlock()

	if !exists {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Task not found"})
	}

	return c.JSON(http.StatusOK, task)
}

// handleDownloadTask downloads the file from a completed task
func (s *Service) handleDownloadTask(c echo.Context) error {
	taskID := c.Param("task_id")

	s.taskMutex.RLock()
	task, exists := s.downloadTasks[taskID]
	s.taskMutex.RUnlock()

	if !exists {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "Task not found"})
	}

	if task.Status != "completed" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error":  "Task not completed",
			"status": task.Status,
		})
	}

	// Read file and return
	fileData, err := os.ReadFile(task.LocalPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "Failed to read downloaded file",
		})
	}

	// Set appropriate headers
	filename := filepath.Base(task.FilePath)
	c.Response().Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filename))
	return c.Blob(http.StatusOK, "application/octet-stream", fileData)
}

// handlePushFile handles file pushing to target nodes
func (s *Service) handlePushFile(c echo.Context) error {
	type PushFileRequest struct {
		TargetNode string `json:"target_node"`
		FilePath   string `json:"file_path,omitempty"` // For file name pushes
		CID        string `json:"cid,omitempty"`       // For CID pushes
		PushType   string `json:"push_type"`           // "file" or "cid"
	}

	var req PushFileRequest
	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Invalid request"})
	}

	if req.TargetNode == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Target node ID is required"})
	}

	if req.PushType != "file" && req.PushType != "cid" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "Push type must be 'file' or 'cid'"})
	}

	if req.PushType == "file" && req.FilePath == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "File path is required for file push"})
	}

	if req.PushType == "cid" && req.CID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "CID is required for CID push"})
	}

	// Use original IPFS push logic
	var err error
	if req.PushType == "file" {
		// For file pushes, use the original PublishFile function
		filePath := filepath.Join(s.serveDir, req.FilePath)
		err = pubsub.PublishFile(s.ctx, s.getIPFSComponents(), pubsub.TopicFileTransfer, filePath, req.TargetNode)
	} else if req.PushType == "cid" {
		// For CID pushes, create a temporary file message
		err = pubsub.PublishCID(s.ctx, s.getIPFSComponents(), pubsub.TopicFileTransfer, req.CID, req.TargetNode)
	}

	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]interface{}{
		"message":     "Push completed successfully",
		"target_node": req.TargetNode,
		"push_type":   req.PushType,
	})
}

// Transfer functionality integrated into gateway

// setupTransferHandlers sets up stream handlers for transfer protocols
func (s *Service) setupTransferHandlers() {
	// Set stream handler for incoming transfer requests (file receives)
	s.host.SetStreamHandler(TransferProtocol, s.handleTransferRequest)
	// Set stream handler for incoming download requests (file sends)
	s.host.SetStreamHandler(DownloadRequestProtocol, s.handleDownloadRequest)
	log.Printf("Transfer stream handlers setup complete")
}

// UploadFile uploads a file to a target peer (client function)
func (s *Service) UploadFile(ctx context.Context, targetPeer peer.ID, filePath string) error {
	// Create stream to target peer
	stream, err := s.host.NewStream(ctx, targetPeer, TransferProtocol)
	if err != nil {
		return fmt.Errorf("failed to create stream: %w", err)
	}
	defer stream.Close()

	// Get full file path
	fullPath := filepath.Join(s.serveDir, filePath)

	// Check if file exists
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("file not found: %w", err)
	}

	// Create request
	request := TransferRequest{
		RequestID: fmt.Sprintf("%s-%d", s.host.ID().String(), time.Now().UnixNano()),
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	// Set timeouts
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Send request
	encoder := gob.NewEncoder(stream)
	if err := encoder.Encode(request); err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	// Read response
	decoder := gob.NewDecoder(stream)
	var response TransferResponse
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("failed to read response: %w", err)
	}

	if !response.Success {
		return fmt.Errorf("transfer request rejected: %s", response.Error)
	}

	log.Printf("Transfer request %s accepted by peer %s", request.RequestID, targetPeer)

	// Send file chunks
	if err := s.sendFileChunks(encoder, request.RequestID, fullPath, fileInfo); err != nil {
		return fmt.Errorf("failed to send file chunks: %w", err)
	}

	log.Printf("File upload completed: %s to peer %s", filePath, targetPeer)
	return nil
}

// DownloadFileFromPeer downloads a file from a target peer (client function)
func (s *Service) DownloadFileFromPeer(ctx context.Context, targetPeer peer.ID, filePath string) error {
	// Create stream to target peer for download request
	stream, err := s.host.NewStream(ctx, targetPeer, DownloadRequestProtocol)
	if err != nil {
		return fmt.Errorf("failed to create download request stream: %w", err)
	}
	defer stream.Close()

	// Create download request
	request := DownloadRequest{
		RequestID: fmt.Sprintf("%s-%d", s.host.ID().String(), time.Now().UnixNano()),
		FilePath:  filePath,
		Timestamp: time.Now().Unix(),
	}

	// Set timeouts
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))

	// Send download request
	encoder := gob.NewEncoder(stream)
	if err := encoder.Encode(request); err != nil {
		return fmt.Errorf("failed to send download request: %w", err)
	}

	log.Printf("Sent download request %s for file %s to peer %s", request.RequestID, filePath, targetPeer)

	// Read response
	decoder := gob.NewDecoder(stream)
	var response DownloadResponse
	if err := decoder.Decode(&response); err != nil {
		return fmt.Errorf("failed to read download response: %w", err)
	}

	if !response.Success {
		return fmt.Errorf("download request rejected: %s", response.Error)
	}

	log.Printf("Download request %s accepted by peer %s", request.RequestID, targetPeer)

	// Receive file chunks - no notification needed for client downloads
	_, err = s.receiveFile(decoder, request.RequestID, targetPeer)
	if err != nil {
		return fmt.Errorf("failed to receive file: %w", err)
	}

	log.Printf("File download completed: %s from peer %s", filePath, targetPeer)
	return nil
}

// handleTransferRequest handles incoming transfer requests (receiving files)
func (s *Service) handleTransferRequest(stream network.Stream) {
	defer stream.Close()

	peerID := stream.Conn().RemotePeer()
	log.Printf("Handling transfer request from peer %s", peerID)

	// Set timeouts
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))

	decoder := gob.NewDecoder(stream)
	encoder := gob.NewEncoder(stream)

	// Read request
	var request TransferRequest
	if err := decoder.Decode(&request); err != nil {
		log.Printf("Failed to decode request from %s: %v", peerID, err)
		return
	}

	log.Printf("Processing transfer request %s for file %s from %s", request.RequestID, request.FilePath, peerID)

	// Send response
	response := TransferResponse{
		RequestID: request.RequestID,
		Success:   true,
		Timestamp: time.Now().Unix(),
	}

	if err := encoder.Encode(response); err != nil {
		log.Printf("Failed to send response to %s: %v", peerID, err)
		return
	}

	// Receive file chunks - no notification needed for downloads (push handling)
	_, err := s.receiveFile(decoder, request.RequestID, peerID)
	if err != nil {
		log.Printf("Failed to receive file from %s: %v", peerID, err)
		return
	}

	// File received successfully - no notification needed for download operations
}

// handleDownloadRequest handles incoming download requests from other nodes
func (s *Service) handleDownloadRequest(stream network.Stream) {
	defer stream.Close()

	peerID := stream.Conn().RemotePeer()
	log.Printf("Handling download request from peer %s", peerID)

	// Set timeouts
	stream.SetReadDeadline(time.Now().Add(30 * time.Second))
	stream.SetWriteDeadline(time.Now().Add(30 * time.Second))

	decoder := gob.NewDecoder(stream)
	encoder := gob.NewEncoder(stream)

	// Read download request
	var request DownloadRequest
	if err := decoder.Decode(&request); err != nil {
		log.Printf("Failed to decode download request from %s: %v", peerID, err)
		return
	}

	log.Printf("Processing download request %s for file %s from %s", request.RequestID, request.FilePath, peerID)

	// Check if file exists
	fullPath := filepath.Join(s.serveDir, request.FilePath)
	fileInfo, err := os.Stat(fullPath)
	if err != nil {
		response := DownloadResponse{
			RequestID: request.RequestID,
			Success:   false,
			Error:     fmt.Sprintf("File not found: %v", err),
			Timestamp: time.Now().Unix(),
		}
		encoder.Encode(response)
		log.Printf("File not found for download request %s: %s", request.RequestID, request.FilePath)
		return
	}

	// Send success response
	response := DownloadResponse{
		RequestID: request.RequestID,
		Success:   true,
		Timestamp: time.Now().Unix(),
	}

	if err := encoder.Encode(response); err != nil {
		log.Printf("Failed to send download response to %s: %v", peerID, err)
		return
	}

	// Send file using chunked transfer
	if err := s.sendFileChunks(encoder, request.RequestID, fullPath, fileInfo); err != nil {
		log.Printf("Failed to send file chunks to %s: %v", peerID, err)
		return
	}

	log.Printf("Download request %s completed for file %s to peer %s", request.RequestID, request.FilePath, peerID)
}

// receiveFile receives file chunks and reconstructs the file
func (s *Service) receiveFile(decoder *gob.Decoder, requestID string, peerID peer.ID) (string, error) {
	var chunks []FileChunk
	var filename string
	var fileSize int64
	totalChunks := 0

	// Receive all chunks
	for {
		var chunk FileChunk
		if err := decoder.Decode(&chunk); err != nil {
			if err == io.EOF {
				break
			}
			return "", fmt.Errorf("failed to decode chunk: %w", err)
		}

		if chunk.RequestID != requestID {
			return "", fmt.Errorf("chunk request ID mismatch: expected %s, got %s", requestID, chunk.RequestID)
		}

		chunks = append(chunks, chunk)
		filename = chunk.Filename
		fileSize = chunk.FileSize
		totalChunks = chunk.TotalChunks

		log.Printf("Received chunk %d/%d (%d bytes) from peer %s", chunk.ChunkIndex+1, chunk.TotalChunks, len(chunk.Data), peerID)

		if chunk.IsLast {
			break
		}
	}

	if len(chunks) != totalChunks {
		return "", fmt.Errorf("incomplete file transfer: expected %d chunks, got %d", totalChunks, len(chunks))
	}

	// Reconstruct file
	outputPath := filepath.Join(s.downloadDir, filename)
	file, err := os.Create(outputPath)
	if err != nil {
		return "", fmt.Errorf("failed to create output file: %w", err)
	}
	defer file.Close()

	// Sort chunks by index and write to file
	for i := 0; i < totalChunks; i++ {
		found := false
		for _, chunk := range chunks {
			if chunk.ChunkIndex == i {
				if _, err := file.Write(chunk.Data); err != nil {
					return "", fmt.Errorf("failed to write chunk %d: %w", i, err)
				}
				found = true
				break
			}
		}
		if !found {
			return "", fmt.Errorf("missing chunk %d", i)
		}
	}

	// Verify file size
	actualSize, _ := file.Seek(0, io.SeekCurrent)
	if actualSize != fileSize {
		return "", fmt.Errorf("file size mismatch: expected %d, got %d", fileSize, actualSize)
	}

	log.Printf("File transfer completed: %s (%d bytes) from peer %s", filename, fileSize, peerID)
	return filename, nil
}

// sendFileChunks sends file in chunks
func (s *Service) sendFileChunks(encoder *gob.Encoder, requestID, filePath string, fileInfo os.FileInfo) error {
	// Open file
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Send file in chunks
	const chunkSize = 64 * 1024 // 64KB chunks
	buffer := make([]byte, chunkSize)
	fileSize := fileInfo.Size()
	totalChunks := int((fileSize + int64(chunkSize) - 1) / int64(chunkSize))
	chunkIndex := 0

	for {
		n, err := file.Read(buffer)
		if err != nil && err != io.EOF {
			return fmt.Errorf("failed to read file: %w", err)
		}

		if n == 0 {
			break
		}

		chunk := FileChunk{
			RequestID:   requestID,
			ChunkIndex:  chunkIndex,
			TotalChunks: totalChunks,
			Data:        buffer[:n],
			IsLast:      err == io.EOF,
			Filename:    filepath.Base(filePath),
			FileSize:    fileSize,
		}

		if err := encoder.Encode(chunk); err != nil {
			return fmt.Errorf("failed to send chunk %d: %w", chunkIndex, err)
		}

		log.Printf("Sent chunk %d/%d (%d bytes)", chunkIndex+1, totalChunks, n)
		chunkIndex++

		if err == io.EOF {
			break
		}
	}

	log.Printf("File chunks sent successfully: %s (%d chunks)", filePath, totalChunks)
	return nil
}
