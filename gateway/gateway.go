package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
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

	"ipfs-gateway/pubsub"
)

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
		ctx:          ctx,
		cancel:       cancel,
		echo:         e,
		gwHandler:    gwHandler,
		host:         host,
		dht:          kadDHT,
		datastore:    ds,
		blockstore:   bstore,
		bitswap:      bs,
		blockService: blockService,
		dag:          dag,
		downloadDir:  "./downloads",
	}

	// Create download directory
	if err := os.MkdirAll(service.downloadDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create download directory: %w", err)
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
	api.POST("/files/download", s.handleDownloadFile)
	api.POST("/files/upload", s.handleUploadFile)
	api.POST("/files/publish", s.handlePublishFile)
	api.POST("/transfer/download/:cid", s.handleTransferDownload)
	api.POST("/transfer/upload", s.handleTransferUpload)
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
	err := pubsub.PublishFile(s.ctx, s.host, req.Topic, req.FilePath, req.TargetNode)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message":     "File published successfully",
		"target_node": req.TargetNode,
	})
}

// handleTransferDownload handles direct transfer download
func (s *Service) handleTransferDownload(c echo.Context) error {
	cid := c.Param("cid")
	filename := c.QueryParam("filename")

	if filename == "" {
		filename = cid + ".dat"
	}

	err := s.DownloadFile(cid, filename)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}

	return c.JSON(http.StatusOK, map[string]string{"message": "Transfer completed", "filename": filename})
}

// handleTransferUpload handles direct transfer upload
func (s *Service) handleTransferUpload(c echo.Context) error {
	return s.handleUploadFile(c) // Reuse upload logic
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
