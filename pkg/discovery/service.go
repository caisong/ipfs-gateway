package discovery

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/multiformats/go-multiaddr"
)

// ServiceType 定义服务类型
type ServiceType string

const (
	ServiceGateway  ServiceType = "ipfs-gateway"
	ServiceStream   ServiceType = "ipfs-stream"
	ServiceTransfer ServiceType = "ipfs-transfer"
)

// Config 服务发现配置
type Config struct {
	ListenAddress     string
	BootstrapPeers    []string
	DiscoveryTag      string
	AdvertiseInterval time.Duration
}

// Service 服务发现服务
type Service struct {
	config    Config
	host      host.Host
	dht       *dht.IpfsDHT
	discovery *routing.RoutingDiscovery
	ctx       context.Context
	cancel    context.CancelFunc

	// 服务注册
	registeredServices map[ServiceType]bool
	servicesMutex      sync.RWMutex

	// 发现的节点
	discoveredPeers map[ServiceType][]peer.AddrInfo
	peersMutex      sync.RWMutex
}

// NewService 创建新的服务发现服务
func NewService(ctx context.Context, cfg Config) (*Service, error) {
	ctx, cancel := context.WithCancel(ctx)

	// 设置默认值
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = "/ip4/0.0.0.0/tcp/0"
	}
	if cfg.AdvertiseInterval == 0 {
		cfg.AdvertiseInterval = 10 * time.Minute
	}

	// 解析监听地址
	listenAddr, err := multiaddr.NewMultiaddr(cfg.ListenAddress)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}

	// 创建libp2p主机
	host, err := libp2p.New(
		libp2p.ListenAddrs(listenAddr),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
	)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	// 创建DHT
	kadDHT, err := dht.New(ctx, host, dht.Mode(dht.ModeAuto))
	if err != nil {
		host.Close()
		cancel()
		return nil, fmt.Errorf("failed to create DHT: %w", err)
	}

	// 启动DHT
	if err := kadDHT.Bootstrap(ctx); err != nil {
		host.Close()
		cancel()
		return nil, fmt.Errorf("failed to bootstrap DHT: %w", err)
	}

	// 创建路由发现
	routingDiscovery := routing.NewRoutingDiscovery(kadDHT)

	service := &Service{
		config:             cfg,
		host:               host,
		dht:                kadDHT,
		discovery:          routingDiscovery,
		ctx:                ctx,
		cancel:             cancel,
		registeredServices: make(map[ServiceType]bool),
		discoveredPeers:    make(map[ServiceType][]peer.AddrInfo),
	}

	// 连接到引导节点
	if err := service.connectToBootstrap(); err != nil {
		log.Printf("警告: 连接引导节点失败: %v", err)
	}

	log.Printf("服务发现节点启动，ID: %s", host.ID())
	log.Printf("监听地址: %v", host.Addrs())

	return service, nil
}

// connectToBootstrap 连接到引导节点
func (s *Service) connectToBootstrap() error {
	if len(s.config.BootstrapPeers) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	for _, addrStr := range s.config.BootstrapPeers {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()

			maddr, err := multiaddr.NewMultiaddr(addr)
			if err != nil {
				log.Printf("无效的引导节点地址 %s: %v", addr, err)
				return
			}

			peerInfo, err := peer.AddrInfoFromP2pAddr(maddr)
			if err != nil {
				log.Printf("解析引导节点失败 %s: %v", addr, err)
				return
			}

			ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
			defer cancel()

			if err := s.host.Connect(ctx, *peerInfo); err != nil {
				log.Printf("连接引导节点失败 %s: %v", addr, err)
			} else {
				log.Printf("成功连接引导节点: %s", peerInfo.ID)
			}
		}(addrStr)
	}
	wg.Wait()
	return nil
}

// RegisterService 注册服务
func (s *Service) RegisterService(serviceType ServiceType) error {
	s.servicesMutex.Lock()
	defer s.servicesMutex.Unlock()

	if s.registeredServices[serviceType] {
		return nil // 已经注册过
	}

	// 广告服务
	tag := string(serviceType)
	if s.config.DiscoveryTag != "" {
		tag = s.config.DiscoveryTag + "-" + string(serviceType)
	}

	interval := s.config.AdvertiseInterval
	if interval == 0 {
		interval = 10 * time.Minute
	}

	// 启动服务广告
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// 立即广告一次
		s.advertiseService(tag)

		for {
			select {
			case <-ticker.C:
				s.advertiseService(tag)
			case <-s.ctx.Done():
				return
			}
		}
	}()

	s.registeredServices[serviceType] = true
	log.Printf("注册服务: %s (标签: %s)", serviceType, tag)
	return nil
}

// advertiseService 广告服务
func (s *Service) advertiseService(tag string) {
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	_, err := s.discovery.Advertise(ctx, tag)
	if err != nil {
		log.Printf("广告服务失败 %s: %v", tag, err)
	} else {
		log.Printf("广告服务成功: %s", tag)
	}
}

// DiscoverPeers 发现指定类型的节点
func (s *Service) DiscoverPeers(serviceType ServiceType, limit int) ([]peer.AddrInfo, error) {
	tag := string(serviceType)
	if s.config.DiscoveryTag != "" {
		tag = s.config.DiscoveryTag + "-" + string(serviceType)
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	peerChan, err := s.discovery.FindPeers(ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("failed to find peers: %w", err)
	}

	var peers []peer.AddrInfo
	for peer := range peerChan {
		if peer.ID == s.host.ID() {
			continue // 跳过自己
		}
		peers = append(peers, peer)
		if len(peers) >= limit {
			break
		}
	}

	// 更新缓存
	s.peersMutex.Lock()
	s.discoveredPeers[serviceType] = peers
	s.peersMutex.Unlock()

	log.Printf("发现 %s 服务节点: %d 个", serviceType, len(peers))
	return peers, nil
}

// GetCachedPeers 获取缓存的节点
func (s *Service) GetCachedPeers(serviceType ServiceType) []peer.AddrInfo {
	s.peersMutex.RLock()
	defer s.peersMutex.RUnlock()

	peers, exists := s.discoveredPeers[serviceType]
	if !exists {
		return nil
	}

	// 返回副本
	result := make([]peer.AddrInfo, len(peers))
	copy(result, peers)
	return result
}

// StartPeriodicDiscovery 启动周期性发现
func (s *Service) StartPeriodicDiscovery(serviceType ServiceType, interval time.Duration, limit int) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		// 立即执行一次
		s.DiscoverPeers(serviceType, limit)

		for {
			select {
			case <-ticker.C:
				s.DiscoverPeers(serviceType, limit)
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// GetHost 返回libp2p主机
func (s *Service) GetHost() host.Host {
	return s.host
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
	log.Println("正在停止服务发现服务...")

	s.cancel()

	if s.dht != nil {
		if err := s.dht.Close(); err != nil {
			log.Printf("关闭DHT失败: %v", err)
		}
	}

	if s.host != nil {
		if err := s.host.Close(); err != nil {
			log.Printf("关闭libp2p主机失败: %v", err)
		}
	}

	log.Println("服务发现服务已停止")
	return nil
}
