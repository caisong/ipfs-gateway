package config

import (
	"flag"
	"log"
)

// AppMode 应用模式
type AppMode string

const (
	ModeAll      AppMode = "all"      // 全功能模式
	ModeGateway  AppMode = "gateway"  // 仅网关模式
	ModeStream   AppMode = "stream"   // 仅流通信模式
	ModeTransfer AppMode = "transfer" // 仅文件传输模式
)

// Config 应用配置
type Config struct {
	Mode              AppMode
	Port              int
	RemoteGateway     string
	ListenAddress     string
	BootstrapPeers    []string
	PingInterval      int
	EnableHealthCheck bool
	LogLevel          string
}

// LoadConfig 加载配置
func LoadConfig() *Config {
	var cfg Config

	// 命令行参数解析
	mode := flag.String("mode", "gateway+stream", "运行模式: all, gateway, stream, transfer, gateway+stream")
	port := flag.Int("port", 8080, "HTTP服务端口（网关模式）")
	remoteGateway := flag.String("remote", "https://trustless-gateway.link", "远程IPFS网关地址")
	listenAddress := flag.String("listen", "/ip4/0.0.0.0/tcp/0", "P2P监听地址")
	pingInterval := flag.Int("ping-interval", 3, "Ping间隔（秒）")
	enableHealthCheck := flag.Bool("health-check", true, "启用健康检查端点")
	logLevel := flag.String("log-level", "info", "日志级别: debug, info, warn, error")

	flag.Parse()

	cfg.Mode = AppMode(*mode)
	cfg.Port = *port
	cfg.RemoteGateway = *remoteGateway
	cfg.ListenAddress = *listenAddress
	cfg.PingInterval = *pingInterval
	cfg.EnableHealthCheck = *enableHealthCheck
	cfg.LogLevel = *logLevel

	// 验证配置
	if cfg.Mode != ModeAll && cfg.Mode != ModeGateway &&
		cfg.Mode != ModeStream && cfg.Mode != ModeTransfer &&
		cfg.Mode != "gateway+stream" {
		log.Fatalf("无效的运行模式: %s", cfg.Mode)
	}

	if cfg.Port <= 0 || cfg.Port > 65535 {
		log.Fatalf("无效的端口号: %d", cfg.Port)
	}

	return &cfg
}

// IsGatewayEnabled 检查是否启用网关功能
func (c *Config) IsGatewayEnabled() bool {
	return c.Mode == ModeAll || c.Mode == ModeGateway || c.Mode == "gateway+stream"
}

// IsStreamEnabled 检查是否启用流通信功能
func (c *Config) IsStreamEnabled() bool {
	return c.Mode == ModeAll || c.Mode == ModeStream || c.Mode == "gateway+stream"
}

// IsTransferEnabled 检查是否启用文件传输功能
func (c *Config) IsTransferEnabled() bool {
	return c.Mode == ModeAll || c.Mode == ModeTransfer || c.Mode == "gateway+stream"
}

// PrintConfig 打印配置信息
func (c *Config) PrintConfig() {
	log.Printf("应用配置:")
	log.Printf("  运行模式: %s", c.Mode)
	log.Printf("  HTTP端口: %d", c.Port)
	log.Printf("  远程网关: %s", c.RemoteGateway)
	log.Printf("  监听地址: %s", c.ListenAddress)
	log.Printf("  Ping间隔: %d秒", c.PingInterval)
	log.Printf("  健康检查: %v", c.EnableHealthCheck)
	log.Printf("  日志级别: %s", c.LogLevel)
}
