package transfer

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/bitswap/network/bsnet"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	format "github.com/ipfs/go-ipld-format"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
	
	"ipfs-gateway/pkg/discovery"
)

// Config 文件传输配置
type Config struct {
	ListenAddress  string
	DatastorePath  string
	BitswapWorkers int
	SessionTimeout time.Duration
	MaxFileSize    int64
	DownloadDir    string
}

// TransferStats 传输统计信息
type TransferStats struct {
	TotalDownloads  int64
	TotalUploads    int64
	BytesDownloaded int64
	BytesUploaded   int64
	ActiveTransfers int
	FailedTransfers int64
}

// FileInfo 文件信息
type FileInfo struct {
	CID      string
	Size     int64
	Type     string
	Path     string
	Modified time.Time
}

// Service 文件传输服务
type Service struct {
	config    Config
	discovery *discovery.Service
	host      host.Host
	ctx       context.Context
	cancel    context.CancelFunc

	// Bitswap 组件
	blockstore   blockstore.Blockstore
	bitswap      *bitswap.Bitswap
	blockService blockservice.BlockService
	dag          format.DAGService

	// 传输管理
	activeTransfers map[string]context.CancelFunc
	transferMutex   sync.RWMutex

	// 统计信息
	stats      TransferStats
	statsMutex sync.RWMutex

	// 文件索引
	localFiles map[string]*FileInfo
	filesMutex sync.RWMutex
}

// NewService 创建新的文件传输服务
func NewService(ctx context.Context, cfg Config, discoveryService *discovery.Service) (*Service, error) {
	ctx, cancel := context.WithCancel(ctx)

	if discoveryService == nil {
		cancel()
		return nil, fmt.Errorf("discovery service is required")
	}

	// 设置默认值
	if cfg.BitswapWorkers == 0 {
		cfg.BitswapWorkers = 8
	}
	if cfg.SessionTimeout == 0 {
		cfg.SessionTimeout = 30 * time.Second
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 100 * 1024 * 1024 // 100MB
	}
	if cfg.DownloadDir == "" {
		cfg.DownloadDir = "./downloads"
	}

	// 创建下载目录
	if err := os.MkdirAll(cfg.DownloadDir, 0755); err != nil {
		cancel()
		return nil, fmt.Errorf("create download directory failed: %w", err)
	}

	// 创建数据存储
	ds := dssync.MutexWrap(datastore.NewMapDatastore())

	// 创建块存储 - 使用简单的内存块存储
	bstore := blockstore.NewBlockstore(ds)

	// 获取libp2p主机
	host := discoveryService.GetHost()

	// 创建 Bitswap 网络层
	bsNet := bsnet.NewFromIpfsHost(host)

	// 创建 Bitswap 实例 - 简化参数
	bs := bitswap.New(ctx, bsNet, nil, bstore)

	// 创建块服务
	blockService := blockservice.New(bstore, bs)

	// 创建 DAG 服务
	dag := merkledag.NewDAGService(blockService)

	service := &Service{
		config:          cfg,
		discovery:       discoveryService,
		host:            host,
		ctx:             ctx,
		cancel:          cancel,
		blockstore:      bstore,
		bitswap:         bs,
		blockService:    blockService,
		dag:             dag,
		activeTransfers: make(map[string]context.CancelFunc),
		localFiles:      make(map[string]*FileInfo),
	}

	// 注册服务到发现模块
	if err := service.discovery.RegisterService(discovery.ServiceTransfer); err != nil {
		log.Printf("注册文件传输服务失败: %v", err)
	}

	// 启动定期统计报告
	go service.startStatsReporting()

	log.Printf("文件传输服务已启动，下载目录: %s", cfg.DownloadDir)
	return service, nil
}

// DownloadFile 下载文件通过CID (简化版本)
func (s *Service) DownloadFile(cidStr string, filename string) error {
	// 解析CID
	targetCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	// 创建传输上下文
	ctx, cancel := context.WithTimeout(s.ctx, s.config.SessionTimeout)
	defer cancel()

	// 添加到活跃传输
	transferID := fmt.Sprintf("download-%s", cidStr)
	s.transferMutex.Lock()
	s.activeTransfers[transferID] = cancel
	s.transferMutex.Unlock()

	defer func() {
		s.transferMutex.Lock()
		delete(s.activeTransfers, transferID)
		s.transferMutex.Unlock()
	}()

	// 更新统计
	s.statsMutex.Lock()
	s.stats.ActiveTransfers++
	s.statsMutex.Unlock()

	defer func() {
		s.statsMutex.Lock()
		s.stats.ActiveTransfers--
		s.statsMutex.Unlock()
	}()

	log.Printf("开始下载文件: %s", cidStr)

	// 通过bitswap获取块
	block, err := s.bitswap.GetBlock(ctx, targetCID)
	if err != nil {
		s.statsMutex.Lock()
		s.stats.FailedTransfers++
		s.statsMutex.Unlock()
		return fmt.Errorf("failed to get block: %w", err)
	}

	// 创建目标文件
	filePath := filepath.Join(s.config.DownloadDir, filename)
	outFile, err := os.Create(filePath)
	if err != nil {
		s.statsMutex.Lock()
		s.stats.FailedTransfers++
		s.statsMutex.Unlock()
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	// 写入数据
	bytesWritten, err := outFile.Write(block.RawData())
	if err != nil {
		s.statsMutex.Lock()
		s.stats.FailedTransfers++
		s.statsMutex.Unlock()
		return fmt.Errorf("failed to write file: %w", err)
	}

	// 更新统计
	s.statsMutex.Lock()
	s.stats.TotalDownloads++
	s.stats.BytesDownloaded += int64(bytesWritten)
	s.statsMutex.Unlock()

	// 更新文件索引
	fileInfo := &FileInfo{
		CID:      cidStr,
		Size:     int64(bytesWritten),
		Type:     "downloaded",
		Path:     filePath,
		Modified: time.Now(),
	}

	s.filesMutex.Lock()
	s.localFiles[cidStr] = fileInfo
	s.filesMutex.Unlock()

	log.Printf("文件下载完成: %s (%d bytes) -> %s", cidStr, bytesWritten, filePath)
	return nil
}

// AddFile 添加文件到IPFS网络 (简化版本)
func (s *Service) AddFile(filePath string) (string, error) {
	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// 获取文件信息
	fileInfo, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("failed to get file info: %w", err)
	}

	// 检查文件大小
	if fileInfo.Size() > s.config.MaxFileSize {
		return "", fmt.Errorf("file too large: %d bytes (max: %d)", fileInfo.Size(), s.config.MaxFileSize)
	}

	log.Printf("添加文件到IPFS: %s (%d bytes)", filePath, fileInfo.Size())

	// 读取文件内容
	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	// 创建原始块
	block := blocks.NewBlock(data)
	
	// 存储块
	err = s.blockstore.Put(s.ctx, block)
	if err != nil {
		s.statsMutex.Lock()
		s.stats.FailedTransfers++
		s.statsMutex.Unlock()
		return "", fmt.Errorf("failed to add block: %w", err)
	}

	// 更新统计
	s.statsMutex.Lock()
	s.stats.TotalUploads++
	s.stats.BytesUploaded += fileInfo.Size()
	s.statsMutex.Unlock()

	// 更新文件索引
	fileInfoData := &FileInfo{
		CID:      block.Cid().String(),
		Size:     fileInfo.Size(),
		Type:     "uploaded",
		Path:     filePath,
		Modified: fileInfo.ModTime(),
	}

	s.filesMutex.Lock()
	s.localFiles[block.Cid().String()] = fileInfoData
	s.filesMutex.Unlock()

	log.Printf("文件添加成功: %s -> %s", filePath, block.Cid().String())
	return block.Cid().String(), nil
}

// ConnectToPeer 连接到指定节点
func (s *Service) ConnectToPeer(peerInfo peer.AddrInfo) error {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	if err := s.host.Connect(ctx, peerInfo); err != nil {
		return fmt.Errorf("connect to peer failed: %w", err)
	}

	log.Printf("连接节点成功: %s", peerInfo.ID)
	return nil
}

// DiscoverTransferPeers 发现文件传输节点
func (s *Service) DiscoverTransferPeers(limit int) ([]peer.AddrInfo, error) {
	return s.discovery.DiscoverPeers(discovery.ServiceTransfer, limit)
}

// GetFileInfo 获取文件信息
func (s *Service) GetFileInfo(cidStr string) (*FileInfo, bool) {
	s.filesMutex.RLock()
	defer s.filesMutex.RUnlock()

	info, exists := s.localFiles[cidStr]
	if !exists {
		return nil, false
	}

	// 返回副本
	infoCopy := *info
	return &infoCopy, true
}

// ListLocalFiles 列出本地文件
func (s *Service) ListLocalFiles() map[string]*FileInfo {
	s.filesMutex.RLock()
	defer s.filesMutex.RUnlock()

	result := make(map[string]*FileInfo)
	for cid, info := range s.localFiles {
		infoCopy := *info
		result[cid] = &infoCopy
	}
	return result
}

// CancelTransfer 取消传输
func (s *Service) CancelTransfer(transferID string) bool {
	s.transferMutex.Lock()
	defer s.transferMutex.Unlock()

	cancel, exists := s.activeTransfers[transferID]
	if exists {
		cancel()
		delete(s.activeTransfers, transferID)
		log.Printf("取消传输: %s", transferID)
		return true
	}
	return false
}

// GetStats 获取传输统计
func (s *Service) GetStats() TransferStats {
	s.statsMutex.RLock()
	defer s.statsMutex.RUnlock()

	return s.stats
}

// startStatsReporting 启动定期统计报告
func (s *Service) startStatsReporting() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			stats := s.GetStats()
			log.Printf("文件传输统计 - 下载: %d (%d bytes), 上传: %d (%d bytes), 活跃: %d, 失败: %d",
				stats.TotalDownloads, stats.BytesDownloaded,
				stats.TotalUploads, stats.BytesUploaded,
				stats.ActiveTransfers, stats.FailedTransfers)
		case <-s.ctx.Done():
			return
		}
	}
}

// GetPeerID 返回节点ID
func (s *Service) GetPeerID() string {
	return s.host.ID().String()
}

// GetAddresses 返回监听地址
func (s *Service) GetAddresses() []multiaddr.Multiaddr {
	return s.host.Addrs()
}

// Stop 停止服务
func (s *Service) Stop() error {
	log.Println("正在停止文件传输服务...")

	// 取消所有活跃传输
	s.transferMutex.Lock()
	for transferID, cancel := range s.activeTransfers {
		cancel()
		log.Printf("取消传输: %s", transferID)
	}
	s.activeTransfers = make(map[string]context.CancelFunc)
	s.transferMutex.Unlock()

	s.cancel()

	// 关闭 Bitswap
	if s.bitswap != nil {
		if err := s.bitswap.Close(); err != nil {
			log.Printf("关闭 Bitswap 失败: %v", err)
		}
	}

	log.Println("文件传输服务已停止")
	return nil
}
