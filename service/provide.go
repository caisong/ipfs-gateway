package service

import (
	"context"
	"log"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
)

type Provider struct {
	tag       string
	host      host.Host
	dht       *dht.IpfsDHT
	discovery *routing.RoutingDiscovery
	interval  time.Duration
	ctx       context.Context
}

func NewProvider(ctx context.Context, host host.Host, kadDHT *dht.IpfsDHT, tag string) *Provider {
	// Create routing discovery
	routingDiscovery := routing.NewRoutingDiscovery(kadDHT)

	service := &Provider{
		tag:       tag,
		host:      host,
		dht:       kadDHT,
		discovery: routingDiscovery,
		interval:  10 * time.Minute,
		ctx:       ctx,
	}

	return service
}

// Broadcast starts broadcasting the service
func (p *Provider) Broadcast() {
	// Start service advertisement
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		// Advertise immediately
		p.advertiseService()

		for {
			select {
			case <-ticker.C:
				p.advertiseService()
			case <-p.ctx.Done():
				return
			}
		}
	}()
}

func (p *Provider) advertiseService() {
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()

	_, err := p.discovery.Advertise(ctx, p.tag)
	if err != nil {
		log.Printf("广告服务失败 %s: %v", p.tag, err)
	} else {
		log.Printf("广告服务成功: %s", p.tag)
	}
}
