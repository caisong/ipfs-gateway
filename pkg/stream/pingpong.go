package stream

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"github.com/multiformats/go-multiaddr"

	"ipfs-gateway/pkg/discovery"
)

const (
	// PingPongProtocol 定义PingPong协议ID
	PingPongProtocol protocol.ID = "/ipfs-gateway/pingpong/1.0.0"

	// 协议消息
	msgPing      = "PING"
	msgPong      = "PONG"
	msgHeartbeat = "HEARTBEAT"
)

// Config 流通信配置
type Config struct {
	ListenAddress    string
	PingInterval     time.Duration
	ResponseTimeout  time.Duration
	MaxConnections   int
	HeartbeatEnabled bool
}

// PeerStats 节点统计信息
type PeerStats struct {
	PeerID       string
	ConnectedAt  time.Time
	LastPingTime time.Time
	LastPongTime time.Time
	PingCount    int64
	PongCount    int64
	Latency      time.Duration
	IsActive     bool
}

// Service 流通信服务
type Service struct {
	config    Config
	discovery *discovery.Service
	host      host.Host
	ctx       context.Context
	cancel    context.CancelFunc

	// 连接管理
	connections map[peer.ID]network.Stream
	connMutex   sync.RWMutex

	// 统计信息
	stats      map[peer.ID]*PeerStats
	statsMutex sync.RWMutex

	// 消息处理
	messageHandlers map[string]func(peer.ID, string)
	handlerMutex    sync.RWMutex
}

// NewService 创建新的流通信服务
func NewService(cfg Config, discoveryService *discovery.Service) (*Service, error) {
	ctx, cancel := context.WithCancel(context.Background())

	if discoveryService == nil {
		cancel()
		return nil, fmt.Errorf("discovery service is required")
	}

	// 设置默认值
	if cfg.PingInterval == 0 {
		cfg.PingInterval = 10 * time.Second
	}
	if cfg.ResponseTimeout == 0 {
		cfg.ResponseTimeout = 5 * time.Second
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 100
	}

	service := &Service{
		config:          cfg,
		discovery:       discoveryService,
		host:            discoveryService.GetHost(),
		ctx:             ctx,
		cancel:          cancel,
		connections:     make(map[peer.ID]network.Stream),
		stats:           make(map[peer.ID]*PeerStats),
		messageHandlers: make(map[string]func(peer.ID, string)),
	}

	// 设置流处理器
	service.host.SetStreamHandler(PingPongProtocol, service.handleStream)

	// 注册默认消息处理器
	service.RegisterMessageHandler(msgPing, service.handlePing)
	service.RegisterMessageHandler(msgPong, service.handlePong)
	service.RegisterMessageHandler(msgHeartbeat, service.handleHeartbeat)

	// 注册服务到发现模块
	if err := service.discovery.RegisterService(discovery.ServiceStream); err != nil {
		log.Printf("注册流通信服务失败: %v", err)
	}

	log.Printf("流通信服务已启动，协议: %s", PingPongProtocol)
	return service, nil
}

// handleStream 处理新的流连接
func (s *Service) handleStream(stream network.Stream) {
	peerID := stream.Conn().RemotePeer()
	log.Printf("新的流连接来自: %s", peerID)

	// 检查连接限制
	s.connMutex.Lock()
	if len(s.connections) >= s.config.MaxConnections {
		s.connMutex.Unlock()
		log.Printf("连接数已达上限，拒绝连接: %s", peerID)
		stream.Close()
		return
	}
	s.connections[peerID] = stream
	s.connMutex.Unlock()

	// 初始化统计信息
	s.statsMutex.Lock()
	s.stats[peerID] = &PeerStats{
		PeerID:      peerID.String(),
		ConnectedAt: time.Now(),
		IsActive:    true,
	}
	s.statsMutex.Unlock()

	// 启动消息读取
	go s.readMessages(stream, peerID)

	// 启动心跳（如果启用）
	if s.config.HeartbeatEnabled {
		go s.startHeartbeat(peerID)
	}
}

// readMessages 读取来自流的消息
func (s *Service) readMessages(stream network.Stream, peerID peer.ID) {
	defer func() {
		stream.Close()
		s.removeConnection(peerID)
	}()

	reader := bufio.NewReader(stream)
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		// 设置读取超时
		stream.SetReadDeadline(time.Now().Add(30 * time.Second))

		message, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("读取消息失败 %s: %v", peerID, err)
			}
			return
		}

		message = message[:len(message)-1] // 移除换行符
		log.Printf("收到消息来自 %s: %s", peerID, message)

		// 处理消息
		s.processMessage(peerID, message)
	}
}

// processMessage 处理接收到的消息
func (s *Service) processMessage(peerID peer.ID, message string) {
	s.handlerMutex.RLock()
	handler, exists := s.messageHandlers[message]
	s.handlerMutex.RUnlock()

	if exists {
		handler(peerID, message)
	} else {
		log.Printf("未知消息类型: %s 来自 %s", message, peerID)
	}
}

// RegisterMessageHandler 注册消息处理器
func (s *Service) RegisterMessageHandler(messageType string, handler func(peer.ID, string)) {
	s.handlerMutex.Lock()
	s.messageHandlers[messageType] = handler
	s.handlerMutex.Unlock()
}

// handlePing 处理PING消息
func (s *Service) handlePing(peerID peer.ID, message string) {
	log.Printf("收到PING来自: %s", peerID)

	// 更新统计
	s.statsMutex.Lock()
	if stats, exists := s.stats[peerID]; exists {
		stats.PingCount++
		stats.LastPingTime = time.Now()
	}
	s.statsMutex.Unlock()

	// 发送PONG响应
	if err := s.SendMessage(peerID, msgPong); err != nil {
		log.Printf("发送PONG失败 %s: %v", peerID, err)
	}
}

// handlePong 处理PONG消息
func (s *Service) handlePong(peerID peer.ID, message string) {
	log.Printf("收到PONG来自: %s", peerID)

	// 更新统计
	s.statsMutex.Lock()
	if stats, exists := s.stats[peerID]; exists {
		stats.PongCount++
		stats.LastPongTime = time.Now()
		// 计算延迟
		if !stats.LastPingTime.IsZero() {
			stats.Latency = time.Since(stats.LastPingTime)
		}
	}
	s.statsMutex.Unlock()
}

// handleHeartbeat 处理心跳消息
func (s *Service) handleHeartbeat(peerID peer.ID, message string) {
	log.Printf("收到心跳来自: %s", peerID)

	// 更新活跃状态
	s.statsMutex.Lock()
	if stats, exists := s.stats[peerID]; exists {
		stats.IsActive = true
	}
	s.statsMutex.Unlock()
}

// SendMessage 发送消息到指定节点
func (s *Service) SendMessage(peerID peer.ID, message string) error {
	s.connMutex.RLock()
	stream, exists := s.connections[peerID]
	s.connMutex.RUnlock()

	if !exists {
		return fmt.Errorf("no connection to peer %s", peerID)
	}

	// 设置写入超时
	stream.SetWriteDeadline(time.Now().Add(s.config.ResponseTimeout))

	_, err := stream.Write([]byte(message + "\n"))
	if err != nil {
		return fmt.Errorf("write message failed: %w", err)
	}

	log.Printf("发送消息到 %s: %s", peerID, message)
	return nil
}

// ConnectToPeer 连接到指定节点
func (s *Service) ConnectToPeer(peerInfo peer.AddrInfo) error {
	// 首先连接到节点
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()

	if err := s.host.Connect(ctx, peerInfo); err != nil {
		return fmt.Errorf("connect to peer failed: %w", err)
	}

	// 打开流
	stream, err := s.host.NewStream(ctx, peerInfo.ID, PingPongProtocol)
	if err != nil {
		return fmt.Errorf("open stream failed: %w", err)
	}

	// 处理流
	go s.handleStream(stream)

	log.Printf("成功连接到节点: %s", peerInfo.ID)
	return nil
}

// StartPing 开始向所有连接的节点发送ping
func (s *Service) StartPing() {
	go func() {
		ticker := time.NewTicker(s.config.PingInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				s.pingAllPeers()
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

// pingAllPeers 向所有连接的节点发送ping
func (s *Service) pingAllPeers() {
	s.connMutex.RLock()
	peers := make([]peer.ID, 0, len(s.connections))
	for peerID := range s.connections {
		peers = append(peers, peerID)
	}
	s.connMutex.RUnlock()

	for _, peerID := range peers {
		if err := s.SendMessage(peerID, msgPing); err != nil {
			log.Printf("发送ping失败 %s: %v", peerID, err)
			// 连接可能已断开，清理
			s.removeConnection(peerID)
		}
	}
}

// startHeartbeat 为指定节点启动心跳
func (s *Service) startHeartbeat(peerID peer.ID) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := s.SendMessage(peerID, msgHeartbeat); err != nil {
				log.Printf("发送心跳失败 %s: %v", peerID, err)
				return
			}
		case <-s.ctx.Done():
			return
		}
	}
}

// removeConnection 移除连接
func (s *Service) removeConnection(peerID peer.ID) {
	s.connMutex.Lock()
	if stream, exists := s.connections[peerID]; exists {
		stream.Close()
		delete(s.connections, peerID)
	}
	s.connMutex.Unlock()

	s.statsMutex.Lock()
	if stats, exists := s.stats[peerID]; exists {
		stats.IsActive = false
	}
	s.statsMutex.Unlock()

	log.Printf("移除连接: %s", peerID)
}

// GetStats 获取所有节点统计信息
func (s *Service) GetStats() map[string]*PeerStats {
	s.statsMutex.RLock()
	defer s.statsMutex.RUnlock()

	result := make(map[string]*PeerStats)
	for peerID, stats := range s.stats {
		// 复制统计信息
		statsCopy := *stats
		result[peerID.String()] = &statsCopy
	}
	return result
}

// GetConnectedPeers 获取已连接的节点列表
func (s *Service) GetConnectedPeers() []string {
	s.connMutex.RLock()
	defer s.connMutex.RUnlock()

	peers := make([]string, 0, len(s.connections))
	for peerID := range s.connections {
		peers = append(peers, peerID.String())
	}
	return peers
}

// GetPeerID 返回本节点ID
func (s *Service) GetPeerID() string {
	return s.host.ID().String()
}

// GetAddresses 返回监听地址
func (s *Service) GetAddresses() []multiaddr.Multiaddr {
	return s.host.Addrs()
}

// Stop 停止服务
func (s *Service) Stop() error {
	log.Println("正在停止流通信服务...")

	s.cancel()

	// 关闭所有连接
	s.connMutex.Lock()
	for _, stream := range s.connections {
		stream.Close()
	}
	s.connections = make(map[peer.ID]network.Stream)
	s.connMutex.Unlock()

	log.Println("流通信服务已停止")
	return nil
}
