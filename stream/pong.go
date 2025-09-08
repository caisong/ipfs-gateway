package stream

import (
	"bufio"
	"context"
	"io"
	"log"
	"time"

	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/protocol"

	"ipfs-gateway/service"
)

const (
	// PingPongProtocol 定义PingPong协议ID
	PingPongProtocol protocol.ID = "/ipfs-gateway/pingpong/1.0.0"

	// 协议消息
	msgPing = "PING"
	msgPong = "PONG"
)

type PongService struct {
	ctx             context.Context
	cancel          context.CancelFunc
	host            host.Host
	dht             *dht.IpfsDHT
	pingInterval    time.Duration
	responseTimeout time.Duration
	provider        *service.Provider
}

// NewPongService creates a new stream communication service
func NewPongService(host host.Host, kadDHT *dht.IpfsDHT, interval time.Duration, timeout time.Duration) (*PongService, error) {
	ctx, cancel := context.WithCancel(context.Background())

	if interval == 0 {
		interval = 10 * time.Second
	}

	if timeout == 0 {
		timeout = 5 * time.Second
	}

	svc := &PongService{
		ctx:             ctx,
		cancel:          cancel,
		host:            host,
		dht:             kadDHT,
		pingInterval:    interval,
		responseTimeout: timeout,
	}

	// Set stream handler
	host.SetStreamHandler(PingPongProtocol, svc.handleStream)

	// Create provider for DHT advertising
	svc.provider = service.NewProvider(ctx, host, kadDHT, string(PingPongProtocol))

	log.Printf("Stream communication service started, protocol: %s", PingPongProtocol)

	return svc, nil
}

func (pong *PongService) Broadcast() {
	pong.provider.Broadcast()
}

// handleStream handles new stream connections
func (pong *PongService) handleStream(stream network.Stream) {
	defer stream.Close()

	peerID := stream.Conn().RemotePeer()
	reader := bufio.NewReader(stream)

	for {
		select {
		case <-pong.ctx.Done():
			return
		default:
		}

		// Set read timeout
		stream.SetReadDeadline(time.Now().Add(30 * time.Second))
		message, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Printf("Failed to read message from %s: %v", peerID, err)
			}
			return
		}

		message = message[:len(message)-1] // Remove newline
		log.Printf("Received message from %s: %s", peerID, message)

		if message == msgPing {
			stream.SetWriteDeadline(time.Now().Add(pong.responseTimeout))
			if _, err := stream.Write([]byte("PONG\n")); err != nil {
				log.Printf("Failed to write message: %v", err)
				return
			}
		}
	}
}
