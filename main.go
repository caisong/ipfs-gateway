package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"ipfs-gateway/config"
	"ipfs-gateway/pkg/discovery"
	"ipfs-gateway/pkg/gateway"
	"ipfs-gateway/pkg/stream"
	"ipfs-gateway/pkg/transfer"
)

func main() {
	// 加载配置
	cfg := config.LoadConfig()
	cfg.PrintConfig()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	var services []Service

	// 首先启动服务发现模块（所有其他服务都依赖它）
	log.Println("启动服务发现模块...")
	discoveryService, err := startDiscoveryService(ctx, cfg)
	if err != nil {
		log.Fatalf("启动服务发现失败: %v", err)
	}
	services = append(services, &DiscoveryServiceWrapper{service: discoveryService})

	// 启动文件传输服务（网关依赖它）
	var transferService *transfer.Service
	if cfg.IsTransferEnabled() || cfg.IsGatewayEnabled() {
		log.Println("启动文件传输服务...")
		transferService, err = startTransferService(ctx, cfg, discoveryService)
		if err != nil {
			log.Fatalf("启动文件传输服务失败: %v", err)
		}
		services = append(services, &TransferServiceWrapper{service: transferService})
	}

	// 启动流通信服务
	if cfg.IsStreamEnabled() {
		log.Println("启动流通信服务...")
		streamService, err := startStreamService(cfg, discoveryService)
		if err != nil {
			log.Fatalf("启动流通信服务失败: %v", err)
		}
		services = append(services, &StreamServiceWrapper{service: streamService})
	}

	// 启动网关服务（依赖传输服务）
	if cfg.IsGatewayEnabled() {
		log.Println("启动网关服务...")
		gatewayService, err := startGatewayService(cfg, transferService, discoveryService)
		if err != nil {
			log.Fatalf("启动网关服务失败: %v", err)
		}
		services = append(services, gatewayService)
	}

	if len(services) <= 1 { // 只有discovery服务
		log.Fatal("没有启动任何业务服务")
	}

	// 启动周期性服务发现
	go startPeriodicDiscovery(discoveryService, cfg)

	// 启动所有服务
	for _, service := range services {
		wg.Add(1)
		go func(s Service) {
			defer wg.Done()
			if err := s.Start(); err != nil {
				log.Printf("服务启动错误: %v", err)
			}
		}(service)
	}

	log.Println("所有服务已启动，按 Ctrl+C 退出")

	// 等待中断信号
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("正在关闭服务...")
	cancel()

	// 停止所有服务（反向顺序）
	for i := len(services) - 1; i >= 0; i-- {
		if err := services[i].Stop(); err != nil {
			log.Printf("停止服务错误: %v", err)
		}
	}

	wg.Wait()
	log.Println("所有服务已关闭")
}

// Service 服务接口
type Service interface {
	Start() error
	Stop() error
}

// startDiscoveryService 启动服务发现
func startDiscoveryService(ctx context.Context, cfg *config.Config) (*discovery.Service, error) {
	discoveryConfig := discovery.Config{
		ListenAddress:     cfg.ListenAddress,
		BootstrapPeers:    cfg.BootstrapPeers,
		DiscoveryTag:      "ipfs-gateway",
		AdvertiseInterval: 10 * time.Minute,
	}

	return discovery.NewService(ctx, discoveryConfig)
}

// startGatewayService 启动网关服务
func startGatewayService(cfg *config.Config, transferService *transfer.Service, discoveryService *discovery.Service) (Service, error) {
	gatewayConfig := gateway.Config{
		RemoteGateway:     cfg.RemoteGateway,
		Port:              cfg.Port,
		EnableHealthCheck: cfg.EnableHealthCheck,
		RequestTimeout:    30 * time.Second,
		MaxFileSize:       100 * 1024 * 1024, // 100MB
		EnableCORS:        true,
		LogRequests:       true,
	}

	return gateway.NewService(gatewayConfig, transferService, discoveryService)
}

// startStreamService 启动流通信服务
func startStreamService(cfg *config.Config, discoveryService *discovery.Service) (*stream.Service, error) {
	streamConfig := stream.Config{
		ListenAddress:    cfg.ListenAddress,
		PingInterval:     time.Duration(cfg.PingInterval) * time.Second,
		ResponseTimeout:  5 * time.Second,
		MaxConnections:   100,
		HeartbeatEnabled: true,
	}

	return stream.NewService(streamConfig, discoveryService)
}

// startTransferService 启动文件传输服务
func startTransferService(ctx context.Context, cfg *config.Config, discoveryService *discovery.Service) (*transfer.Service, error) {
	transferConfig := transfer.Config{
		ListenAddress:  cfg.ListenAddress,
		DatastorePath:  "", // 使用内存存储
		BitswapWorkers: 8,
		SessionTimeout: 30 * time.Second,
		MaxFileSize:    100 * 1024 * 1024, // 100MB
		DownloadDir:    "./downloads",
	}

	return transfer.NewService(ctx, transferConfig, discoveryService)
}

// startPeriodicDiscovery 启动周期性服务发现
func startPeriodicDiscovery(discoveryService *discovery.Service, cfg *config.Config) {
	// 定期发现不同类型的节点
	if cfg.IsGatewayEnabled() {
		discoveryService.StartPeriodicDiscovery(discovery.ServiceGateway, 2*time.Minute, 10)
	}
	if cfg.IsStreamEnabled() {
		discoveryService.StartPeriodicDiscovery(discovery.ServiceStream, 2*time.Minute, 10)
	}
	if cfg.IsTransferEnabled() {
		discoveryService.StartPeriodicDiscovery(discovery.ServiceTransfer, 2*time.Minute, 10)
	}
}

// 服务包装器

// DiscoveryServiceWrapper 服务发现包装器
type DiscoveryServiceWrapper struct {
	service *discovery.Service
}

func (w *DiscoveryServiceWrapper) Start() error {
	// 服务发现在创建时已经启动，这里只需要保持运行
	log.Printf("服务发现已启动，节点ID: %s", w.service.GetPeerID())
	log.Printf("监听地址: %v", w.service.GetAddresses())

	// 保持服务运行（使用一个永远阻塞的频道）
	forever := make(chan struct{})
	<-forever
	return nil
}

func (w *DiscoveryServiceWrapper) Stop() error {
	return w.service.Stop()
}

// StreamServiceWrapper 流通信包装器
type StreamServiceWrapper struct {
	service *stream.Service
}

func (w *StreamServiceWrapper) Start() error {
	// 流通信服务在创建时已经启动，这里只需要保持运行
	log.Printf("流通信服务已启动")

	// 保持服务运行
	forever := make(chan struct{})
	<-forever
	return nil
}

func (w *StreamServiceWrapper) Stop() error {
	return w.service.Stop()
}

// TransferServiceWrapper 文件传输包装器
type TransferServiceWrapper struct {
	service *transfer.Service
}

func (w *TransferServiceWrapper) Start() error {
	// 文件传输服务在创建时已经启动，这里只需要保持运行
	log.Printf("文件传输服务已启动，节点ID: %s", w.service.GetPeerID())
	log.Printf("下载目录: ./downloads")

	// 保持服务运行
	forever := make(chan struct{})
	<-forever
	return nil
}

func (w *TransferServiceWrapper) Stop() error {
	return w.service.Stop()
}
