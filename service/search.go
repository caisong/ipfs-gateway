package service

import (
	"context"
	"fmt"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
)

func DiscoverPeers(ctx context.Context, host host.Host, kadDHT *dht.IpfsDHT, tag string, limit int) ([]peer.AddrInfo, error) {
	_ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// 创建路由发现
	discovery := routing.NewRoutingDiscovery(kadDHT)

	peerChan, err := discovery.FindPeers(_ctx, tag)
	if err != nil {
		return nil, fmt.Errorf("failed to find peers: %w", err)
	}

	var peers []peer.AddrInfo
	for peer := range peerChan {
		if peer.ID == host.ID() {
			continue // Skip ourselves
		}
		peers = append(peers, peer)
		if limit > 0 && len(peers) >= limit {
			break
		}
	}
	return peers, nil
}
