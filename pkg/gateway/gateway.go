package gateway

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	boxogateway "github.com/ipfs/boxo/gateway"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"ipfs-gateway/pkg/discovery"
	"ipfs-gateway/pkg/transfer"
)

// Config 网关配置
type Config struct {
	RemoteGateway     string
	Port              int
	EnableHealthCheck bool
	RequestTimeout    time.Duration
	MaxFileSize       int64
	EnableCORS        bool
	LogRequests       bool
}

// RequestStats 请求统计
type RequestStats struct {
	TotalRequests      int64
	SuccessfulRequests int64
	FailedRequests     int64
	IPFSRequests       int64
	IPNSRequests       int64
	BytesServed        int64
}

// Service 网关服务
type Service struct {
	config    Config
	echo      *echo.Echo
	gwHandler http.Handler
	transfer  *transfer.Service
	discovery *discovery.Service
	ctx       context.Context
	cancel    context.CancelFunc

	// 统计信息
	stats RequestStats
}

// NewService 创建新的网关服务
func NewService(cfg Config, transferService *transfer.Service, discoveryService *discovery.Service) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// 设置默认值
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = 30 * time.Second
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 100 * 1024 * 1024 // 100MB
	}

	// 初始化官方远程块存储后端
	backend, err := boxogateway.NewRemoteBlocksBackend([]string{cfg.RemoteGateway}, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create remote backend: %w", err)
	}

	// 配置网关
	gatewayCfg := boxogateway.Config{
		PublicGateways: map[string]*boxogateway.PublicGateway{
			"localhost": {
				Paths:         []string{"/ipfs", "/ipns"},
				UseSubdomains: false,
			},
		},
	}

	// 创建标准网关处理器
	gwHandler := boxogateway.NewHandler(gatewayCfg, backend)

	// 初始化 Echo 框架
	e := echo.New()
	e.HideBanner = true

	// 设置中间件
	if cfg.LogRequests {
		e.Use(middleware.Logger())
	}
	e.Use(middleware.Recover())

	if cfg.EnableCORS {
		e.Use(middleware.CORS())
	}

	// 设置超时
	e.Use(middleware.TimeoutWithConfig(middleware.TimeoutConfig{
		Timeout: cfg.RequestTimeout,
	}))

	service := &Service{
		config:    cfg,
		echo:      e,
		gwHandler: gwHandler,
		transfer:  transferService,
		discovery: discoveryService,
		ctx:       ctx,
		cancel:    cancel,
	}

	service.setupRoutes()

	// 注册服务到发现模块
	if discoveryService != nil {
		if err := discoveryService.RegisterService(discovery.ServiceGateway); err != nil {
			log.Printf("注册网关服务失败: %v", err)
		}
	}

	// 启动定期统计报告
	go service.startStatsReporting()

	return service, nil
}

// setupRoutes 设置路由
func (s *Service) setupRoutes() {
	// IPFS 内容路由
	s.echo.Any("/ipfs/*", s.handleIPFSRequest)
	s.echo.Any("/ipns/*", s.handleIPNSRequest)

	// API 路由
	api := s.echo.Group("/api/v1")
	api.GET("/stats", s.handleStats)
	api.GET("/files", s.handleListFiles)
	api.POST("/files/download", s.handleDownloadFile)
	api.POST("/files/upload", s.handleUploadFile)
	api.GET("/peers", s.handleListPeers)

	// 新增：直接文件传输命令路由
	api.POST("/transfer/download/:cid", s.handleTransferDownload)
	api.POST("/transfer/upload", s.handleTransferUpload)
	api.GET("/transfer/status/:transferId", s.handleTransferStatus)

	// 健康检查端点
	if s.config.EnableHealthCheck {
		s.echo.GET("/health", s.handleHealth)
		s.echo.GET("/readiness", s.handleReadiness)
	}

	// 静态文件服务（可选）
	s.echo.Static("/static", "static")
}

// handleIPFSRequest 处理IPFS请求
func (s *Service) handleIPFSRequest(c echo.Context) error {
	s.stats.TotalRequests++
	s.stats.IPFSRequests++

	start := time.Now()
	defer func() {
		log.Printf("IPFS请求处理耗时: %v", time.Since(start))
	}()

	// 获取请求路径
	path := c.Request().URL.Path
	log.Printf("处理IPFS请求: %s", path)

	// 尝试优先从本地传输服务获取
	if s.transfer != nil {
		if err := s.tryLocalTransfer(c, path); err == nil {
			s.stats.SuccessfulRequests++
			return nil
		} else {
			log.Printf("本地传输失败，回退到远程网关: %v", err)
		}
	}

	// 回退到远程网关
	return s.proxyToRemoteGateway(c)
}

// handleIPNSRequest 处理IPNS请求
func (s *Service) handleIPNSRequest(c echo.Context) error {
	s.stats.TotalRequests++
	s.stats.IPNSRequests++

	path := c.Request().URL.Path
	log.Printf("处理IPNS请求: %s", path)

	// IPNS请求直接转发到远程网关
	return s.proxyToRemoteGateway(c)
}

// tryLocalTransfer 尝试从本地传输服务获取文件
func (s *Service) tryLocalTransfer(c echo.Context, path string) error {
	// 解析CID
	cidStr, err := s.extractCIDFromPath(path)
	if err != nil {
		return fmt.Errorf("invalid path: %w", err)
	}

	// 检查本地是否有该文件
	fileInfo, exists := s.transfer.GetFileInfo(cidStr)
	if exists && fileInfo.Type == "downloaded" {
		// 直接从本地文件系统服务
		return c.File(fileInfo.Path)
	}

	// 尝试从网络下载
	filename := fmt.Sprintf("%s.tmp", cidStr)

	// 下载文件
	if err := s.transfer.DownloadFile(cidStr, filename); err != nil {
		return fmt.Errorf("download failed: %w", err)
	}

	// 获取下载后的文件信息
	fileInfo, exists = s.transfer.GetFileInfo(cidStr)
	if !exists {
		return fmt.Errorf("file not found after download")
	}

	// 服务文件
	return c.File(fileInfo.Path)
}

// extractCIDFromPath 从路径中提取CID
func (s *Service) extractCIDFromPath(path string) (string, error) {
	if !strings.HasPrefix(path, "/ipfs/") {
		return "", fmt.Errorf("not an IPFS path")
	}

	cidPart := strings.TrimPrefix(path, "/ipfs/")
	// 移除可能的子路径
	if idx := strings.Index(cidPart, "/"); idx != -1 {
		cidPart = cidPart[:idx]
	}

	if cidPart == "" {
		return "", fmt.Errorf("empty CID")
	}

	return cidPart, nil
}

// proxyToRemoteGateway 代理到远程网关
func (s *Service) proxyToRemoteGateway(c echo.Context) error {
	// 创建新的ResponseWriter包装器
	w := &responseWriterWrapper{
		ResponseWriter: c.Response().Writer,
		header:         c.Response().Header(),
		service:        s,
	}

	// 调用原始网关处理器
	s.gwHandler.ServeHTTP(w, c.Request())

	if w.statusCode >= 200 && w.statusCode < 300 {
		s.stats.SuccessfulRequests++
		s.stats.BytesServed += w.bytesWritten
	} else {
		s.stats.FailedRequests++
	}

	return nil
}

// responseWriterWrapper 包装ResponseWriter以记录统计信息
type responseWriterWrapper struct {
	http.ResponseWriter
	header       http.Header
	statusCode   int
	bytesWritten int64
	service      *Service
}

func (w *responseWriterWrapper) Header() http.Header {
	return w.header
}

func (w *responseWriterWrapper) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *responseWriterWrapper) Write(b []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = 200
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytesWritten += int64(n)
	return n, err
}

// API处理器

// handleStats 处理统计信息请求
func (s *Service) handleStats(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]interface{}{
		"gateway":  s.stats,
		"transfer": s.transfer.GetStats(),
	})
}

// handleListFiles 处理文件列表请求
func (s *Service) handleListFiles(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	files := s.transfer.ListLocalFiles()
	return c.JSON(http.StatusOK, files)
}

// handleDownloadFile 处理文件下载请求
func (s *Service) handleDownloadFile(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	var req struct {
		CID      string `json:"cid" validate:"required"`
		Filename string `json:"filename"`
	}

	if err := c.Bind(&req); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body",
		})
	}

	if req.Filename == "" {
		req.Filename = req.CID + ".download"
	}

	if err := s.transfer.DownloadFile(req.CID, req.Filename); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message":  "download started",
		"cid":      req.CID,
		"filename": req.Filename,
	})
}

// handleUploadFile 处理文件上传请求
func (s *Service) handleUploadFile(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	// 获取上传的文件
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "no file uploaded",
		})
	}

	// 保存临时文件
	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to open uploaded file",
		})
	}
	defer src.Close()

	tempPath := "/tmp/" + file.Filename
	dst, err := os.Create(tempPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to create temporary file",
		})
	}
	defer dst.Close()
	defer os.Remove(tempPath)

	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to save file",
		})
	}

	// 添加到IPFS
	cidStr, err := s.transfer.AddFile(tempPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	return c.JSON(http.StatusOK, map[string]string{
		"message":  "file uploaded successfully",
		"cid":      cidStr,
		"filename": file.Filename,
	})
}

// handleListPeers 处理节点列表请求
func (s *Service) handleListPeers(c echo.Context) error {
	if s.discovery == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "discovery service not available",
		})
	}

	gatewayPeers := s.discovery.GetCachedPeers(discovery.ServiceGateway)
	streamPeers := s.discovery.GetCachedPeers(discovery.ServiceStream)
	transferPeers := s.discovery.GetCachedPeers(discovery.ServiceTransfer)

	return c.JSON(http.StatusOK, map[string]interface{}{
		"gateway_peers":  gatewayPeers,
		"stream_peers":   streamPeers,
		"transfer_peers": transferPeers,
	})
}

// handleHealth 处理健康检查
func (s *Service) handleHealth(c echo.Context) error {
	return c.JSON(http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"service":   "gateway",
		"timestamp": time.Now().Unix(),
		"uptime":    time.Since(time.Now()).String(), // 简化处理
	})
}

// handleReadiness 处理就绪检查
func (s *Service) handleReadiness(c echo.Context) error {
	ready := true
	checks := map[string]bool{
		"transfer_service":  s.transfer != nil,
		"discovery_service": s.discovery != nil,
	}

	for _, status := range checks {
		if !status {
			ready = false
			break
		}
	}

	status := http.StatusOK
	if !ready {
		status = http.StatusServiceUnavailable
	}

	return c.JSON(status, map[string]interface{}{
		"ready":  ready,
		"checks": checks,
	})
}

// startStatsReporting 启动定期统计报告
func (s *Service) startStatsReporting() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.Printf("网关统计 - 总请求: %d, 成功: %d, 失败: %d, IPFS: %d, IPNS: %d, 数据传输: %d bytes",
				s.stats.TotalRequests, s.stats.SuccessfulRequests, s.stats.FailedRequests,
				s.stats.IPFSRequests, s.stats.IPNSRequests, s.stats.BytesServed)
		case <-s.ctx.Done():
			return
		}
	}
}

// 新增的传输API处理器

// handleTransferDownload 处理直接传输下载请求
func (s *Service) handleTransferDownload(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	cidStr := c.Param("cid")
	if cidStr == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "CID parameter is required",
		})
	}

	// 可选参数
	filename := c.QueryParam("filename")
	if filename == "" {
		filename = cidStr + ".download"
	}

	log.Printf("收到文件传输下载命令: CID=%s, filename=%s", cidStr, filename)

	// 直接调用transfer服务进行下载
	if err := s.transfer.DownloadFile(cidStr, filename); err != nil {
		log.Printf("文件传输下载失败: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	log.Printf("文件传输下载成功: %s", cidStr)
	return c.JSON(http.StatusOK, map[string]string{
		"message":  "download completed successfully",
		"cid":      cidStr,
		"filename": filename,
		"status":   "completed",
	})
}

// handleTransferUpload 处理直接传输上传请求
func (s *Service) handleTransferUpload(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	// 获取上传的文件
	file, err := c.FormFile("file")
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "no file uploaded",
		})
	}

	log.Printf("收到文件传输上传命令: filename=%s", file.Filename)

	// 保存临时文件
	src, err := file.Open()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to open uploaded file",
		})
	}
	defer src.Close()

	tempPath := "/tmp/" + file.Filename
	dst, err := os.Create(tempPath)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to create temporary file",
		})
	}
	defer dst.Close()
	defer os.Remove(tempPath)

	if _, err = io.Copy(dst, src); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": "failed to save file",
		})
	}

	// 调用transfer服务上传
	cidStr, err := s.transfer.AddFile(tempPath)
	if err != nil {
		log.Printf("文件传输上传失败: %v", err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	log.Printf("文件传输上传成功: %s -> %s", file.Filename, cidStr)
	return c.JSON(http.StatusOK, map[string]string{
		"message":  "upload completed successfully",
		"cid":      cidStr,
		"filename": file.Filename,
		"status":   "completed",
	})
}

// handleTransferStatus 处理传输状态查询
func (s *Service) handleTransferStatus(c echo.Context) error {
	if s.transfer == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "transfer service not available",
		})
	}

	transferID := c.Param("transferId")
	if transferID == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "transferId parameter is required",
		})
	}

	// 检查是否为 CID，如果是则查询文件信息
	fileInfo, exists := s.transfer.GetFileInfo(transferID)
	if exists {
		return c.JSON(http.StatusOK, map[string]interface{}{
			"transferId": transferID,
			"status":     "completed",
			"fileInfo":   fileInfo,
		})
	}

	// 否则返回未找到
	return c.JSON(http.StatusNotFound, map[string]string{
		"error": "transfer not found",
	})
}

// Start 启动网关服务
func (s *Service) Start() error {
	log.Printf("网关运行于 http://localhost:%d", s.config.Port)
	return s.echo.Start(":" + strconv.Itoa(s.config.Port))
}

// Stop 停止网关服务
func (s *Service) Stop() error {
	log.Println("正在停止网关服务...")
	s.cancel()
	return s.echo.Shutdown(s.ctx)
}
