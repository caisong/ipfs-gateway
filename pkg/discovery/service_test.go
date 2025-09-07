package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewService(t *testing.T) {
	tests := []struct {
		name        string
		config      Config
		expectError bool
	}{
		{
			name: "valid config",
			config: Config{
				ListenAddress: "/ip4/127.0.0.1/tcp/0",
			},
			expectError: false,
		},
		{
			name:        "empty config uses defaults",
			config:      Config{},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			service, err := NewService(ctx, tt.config)

			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, service)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, service)

				// 验证默认值
				if tt.config.ListenAddress == "" {
					assert.Contains(t, service.host.Addrs()[0].String(), "/tcp/")
				}

				// 清理
				service.Stop()
			}
		})
	}
}

func TestServiceRegistration(t *testing.T) {
	ctx := context.Background()
	config := Config{
		ListenAddress: "/ip4/127.0.0.1/tcp/0",
	}

	service, err := NewService(ctx, config)
	require.NoError(t, err)
	defer service.Stop()

	tests := []struct {
		name        string
		serviceType ServiceType
		expectError bool
	}{
		{
			name:        "register gateway service",
			serviceType: ServiceGateway,
			expectError: false,
		},
		{
			name:        "register stream service",
			serviceType: ServiceStream,
			expectError: false,
		},
		{
			name:        "register transfer service",
			serviceType: ServiceTransfer,
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := service.RegisterService(tt.serviceType)
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPeerDiscovery(t *testing.T) {
	ctx := context.Background()
	config := Config{
		ListenAddress: "/ip4/127.0.0.1/tcp/0",
	}

	service, err := NewService(ctx, config)
	require.NoError(t, err)
	defer service.Stop()

	// 等待DHT启动
	time.Sleep(2 * time.Second)

	t.Run("discover peers with limit", func(t *testing.T) {
		peers, err := service.DiscoverPeers(ServiceGateway, 5)
		assert.NoError(t, err)
		assert.LessOrEqual(t, len(peers), 5)
	})

	t.Run("get cached peers", func(t *testing.T) {
		peers := service.GetCachedPeers(ServiceGateway)
		assert.NotNil(t, peers)
	})
}

func TestHostMethods(t *testing.T) {
	ctx := context.Background()
	config := Config{
		ListenAddress: "/ip4/127.0.0.1/tcp/0",
	}

	service, err := NewService(ctx, config)
	require.NoError(t, err)
	defer service.Stop()

	t.Run("get host", func(t *testing.T) {
		host := service.GetHost()
		assert.NotNil(t, host)
		assert.NotEmpty(t, host.ID().String())
	})

	t.Run("get peer ID", func(t *testing.T) {
		peerID := service.GetPeerID()
		assert.NotEmpty(t, peerID)
		assert.True(t, len(peerID) > 10) // PeerID should be reasonably long
	})

	t.Run("get addresses", func(t *testing.T) {
		addrs := service.GetAddresses()
		assert.NotEmpty(t, addrs)
		assert.Contains(t, addrs[0].String(), "/tcp/")
	})
}

func TestStopService(t *testing.T) {
	ctx := context.Background()
	config := Config{
		ListenAddress: "/ip4/127.0.0.1/tcp/0",
	}

	service, err := NewService(ctx, config)
	require.NoError(t, err)

	// 验证服务运行
	assert.NotNil(t, service.GetHost())

	// 停止服务
	err = service.Stop()
	assert.NoError(t, err)

	// 验证上下文已取消
	select {
	case <-service.ctx.Done():
		// 正常，上下文已取消
	default:
		t.Error("context should be cancelled after Stop()")
	}
}
