package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ipfs/go-datastore"
	dssync "github.com/ipfs/go-datastore/sync"
	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	"ipfs-gateway/gateway"
	"ipfs-gateway/pubsub"
	"ipfs-gateway/service"
	"ipfs-gateway/stream"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create libp2p host
	host, err := createHost()
	if err != nil {
		log.Fatalf("Failed to create libp2p host: %v", err)
	}
	defer host.Close()

	log.Printf("Node ID: %s", host.ID())
	log.Printf("Listening on: %v", host.Addrs())

	// Create DHT
	kadDHT, err := createDHT(ctx, host)
	if err != nil {
		log.Fatalf("Failed to create DHT: %v", err)
	}

	// Bootstrap DHT
	if err := kadDHT.Bootstrap(ctx); err != nil {
		log.Printf("Warning: DHT bootstrap failed: %v", err)
	}

	// Start services
	var wg sync.WaitGroup

	// 1. Start Gateway Service (IPFS proxy with file download)
	gatewayService, err := gateway.NewService(ctx, host, kadDHT, 8080)
	if err != nil {
		log.Fatalf("Failed to create gateway service: %v", err)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Println("Starting gateway service...")
		if err := gatewayService.Start(); err != nil {
			log.Printf("Gateway service error: %v", err)
		}
	}()

	// 2. Start Ping-Pong Service
	pongService, err := stream.NewPongService(host, kadDHT, 10*time.Second, 5*time.Second)
	if err != nil {
		log.Fatalf("Failed to create pong service: %v", err)
	}

	// Start broadcasting ping-pong service
	pongService.Broadcast()
	log.Println("Ping-pong service started and broadcasting")

	// 3. Start Pubsub File Subscription
	wg.Add(1)
	go func() {
		defer wg.Done()
		log.Printf("Starting pubsub file subscription on topic: %s", "ipfs-gateway-files")
		if err := pubsub.SubscribeToFiles(ctx, host, "ipfs-gateway-files", "./downloads"); err != nil {
			log.Printf("Pubsub subscription error: %v", err)
		}
	}()

	// 4. Start Gateway Service Provider (DHT advertising)
	gatewayProvider := service.NewProvider(ctx, host, kadDHT, "ipfs-gateway")
	gatewayProvider.Broadcast()
	log.Println("Gateway service provider started and broadcasting")

	log.Println("All services started successfully")
	log.Println("Gateway available at: http://localhost:8080")

	// Wait for interrupt signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	log.Println("Shutting down services...")
	cancel()

	// Stop gateway service
	if err := gatewayService.Stop(); err != nil {
		log.Printf("Error stopping gateway service: %v", err)
	}

	// Wait for all goroutines to finish
	wg.Wait()
	log.Println("All services stopped")
}

// createHost creates a libp2p host
func createHost() (host.Host, error) {
	// Parse listen addresses
	listenAddr, err := multiaddr.NewMultiaddr("/ip4/0.0.0.0/tcp/4001")
	if err != nil {
		return nil, fmt.Errorf("invalid listen address: %w", err)
	}

	// Create host
	host, err := libp2p.New(
		libp2p.ListenAddrs(listenAddr),
		libp2p.DefaultSecurity,
		libp2p.DefaultMuxers,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create libp2p host: %w", err)
	}

	return host, nil
}

// createDHT creates and configures a Kademlia DHT
func createDHT(ctx context.Context, host host.Host) (*dht.IpfsDHT, error) {
	// Create datastore for DHT
	ds := dssync.MutexWrap(datastore.NewMapDatastore())

	// Create DHT
	kadDHT, err := dht.New(ctx, host, dht.Datastore(ds))
	if err != nil {
		return nil, fmt.Errorf("failed to create DHT: %w", err)
	}

	// Connect to bootstrap peers (simplified)
	bootstrapPeers := []string{
		"/dnsaddr/bootstrap.libp2p.io/p2p/QmNnooDu7bfjPFoTZYxMNLWUQJyrVwtbZg5gBMjTezGAJN",
	}

	var wg sync.WaitGroup
	for _, peerAddr := range bootstrapPeers {
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			if err := connectToPeer(ctx, host, addr); err != nil {
				log.Printf("Failed to connect to bootstrap peer %s: %v", addr, err)
			}
		}(peerAddr)
	}
	wg.Wait()

	return kadDHT, nil
}

// connectToPeer connects to a peer given its multiaddr
func connectToPeer(ctx context.Context, host host.Host, peerAddr string) error {
	addr, err := multiaddr.NewMultiaddr(peerAddr)
	if err != nil {
		return fmt.Errorf("invalid peer address: %w", err)
	}

	peerInfo, err := peer.AddrInfoFromP2pAddr(addr)
	if err != nil {
		return fmt.Errorf("failed to get peer info: %w", err)
	}

	if err := host.Connect(ctx, *peerInfo); err != nil {
		return fmt.Errorf("failed to connect to peer: %w", err)
	}

	log.Printf("Connected to bootstrap peer: %s", peerInfo.ID)
	return nil
}
