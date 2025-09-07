package config

import (
	"flag"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		expectedMode   AppMode
		expectedPort   int
		expectedRemote string
		expectedListen string
	}{
		{
			name:           "default values",
			args:           []string{},
			expectedMode:   "gateway+stream",
			expectedPort:   8080,
			expectedRemote: "https://trustless-gateway.link",
			expectedListen: "/ip4/0.0.0.0/tcp/0",
		},
		{
			name: "custom values",
			args: []string{
				"-mode=all",
				"-port=9090",
				"-remote=https://ipfs.io",
				"-listen=/ip4/127.0.0.1/tcp/4001",
			},
			expectedMode:   ModeAll,
			expectedPort:   9090,
			expectedRemote: "https://ipfs.io",
			expectedListen: "/ip4/127.0.0.1/tcp/4001",
		},
		{
			name: "gateway mode",
			args: []string{
				"-mode=gateway",
				"-port=8081",
			},
			expectedMode:   ModeGateway,
			expectedPort:   8081,
			expectedRemote: "https://trustless-gateway.link",
			expectedListen: "/ip4/0.0.0.0/tcp/0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 重置flag.CommandLine
			flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

			// 备份原始命令行参数
			oldArgs := os.Args
			defer func() { os.Args = oldArgs }()

			// 设置测试参数
			os.Args = append([]string{"test"}, tt.args...)

			// 加载配置
			cfg := LoadConfig()

			// 验证结果
			assert.Equal(t, tt.expectedMode, cfg.Mode)
			assert.Equal(t, tt.expectedPort, cfg.Port)
			assert.Equal(t, tt.expectedRemote, cfg.RemoteGateway)
			assert.Equal(t, tt.expectedListen, cfg.ListenAddress)
		})
	}
}

func TestConfigMethods(t *testing.T) {
	tests := []struct {
		name           string
		mode           AppMode
		expectGateway  bool
		expectStream   bool
		expectTransfer bool
	}{
		{
			name:           "all mode",
			mode:           ModeAll,
			expectGateway:  true,
			expectStream:   true,
			expectTransfer: true,
		},
		{
			name:           "gateway mode",
			mode:           ModeGateway,
			expectGateway:  true,
			expectStream:   false,
			expectTransfer: false,
		},
		{
			name:           "stream mode",
			mode:           ModeStream,
			expectGateway:  false,
			expectStream:   true,
			expectTransfer: false,
		},
		{
			name:           "transfer mode",
			mode:           ModeTransfer,
			expectGateway:  false,
			expectStream:   false,
			expectTransfer: true,
		},
		{
			name:           "gateway+stream combination",
			mode:           "gateway+stream",
			expectGateway:  true,
			expectStream:   true,
			expectTransfer: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &Config{Mode: tt.mode}

			assert.Equal(t, tt.expectGateway, cfg.IsGatewayEnabled())
			assert.Equal(t, tt.expectStream, cfg.IsStreamEnabled())
			assert.Equal(t, tt.expectTransfer, cfg.IsTransferEnabled())
		})
	}
}

func TestPrintConfig(t *testing.T) {
	cfg := &Config{
		Mode:              ModeAll,
		Port:              8080,
		RemoteGateway:     "https://ipfs.io",
		ListenAddress:     "/ip4/0.0.0.0/tcp/0",
		PingInterval:      3,
		EnableHealthCheck: true,
		LogLevel:          "info",
	}

	// 测试打印配置不会panic
	assert.NotPanics(t, func() {
		cfg.PrintConfig()
	})
}
