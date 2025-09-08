package pubsub

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"os"
	"path/filepath"

	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
)

// FileMessage represents a file transfer message
type FileMessage struct {
	CID        string `gob:"cid"`
	Filename   string `gob:"filename"`
	Size       int64  `gob:"size"`
	Data       []byte `gob:"data"`
	TargetNode string `gob:"target_node"` // Target node peer ID
	SenderNode string `gob:"sender_node"` // Sender node peer ID
}

// PublishFile publishes a file to a specific target node
func PublishFile(ctx context.Context, host host.Host, topic string, filePath string, targetNodeID string) error {
	// Read file
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	// Create file message with target node information
	fileMsg := FileMessage{
		Filename:   filepath.Base(filePath),
		Size:       int64(len(data)),
		Data:       data,
		TargetNode: targetNodeID,
		SenderNode: host.ID().String(),
	}

	// Serialize message using gob
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(fileMsg); err != nil {
		return fmt.Errorf("failed to encode message: %w", err)
	}

	return Publish(ctx, host, topic, buf.Bytes())
}

// Publish publishes a message to a topic
func Publish(ctx context.Context, host host.Host, topic string, msg []byte) error {
	ps, err := pubsub.NewGossipSub(ctx, host)
	if err != nil {
		return err
	}

	_topic, err := ps.Join(topic)
	if err != nil {
		return err
	}

	return _topic.Publish(ctx, msg)
}

// Subscribe subscribes to a topic and returns a channel for file messages
func Subscribe(ctx context.Context, host host.Host, topic string) (<-chan []byte, error) {
	ps, err := pubsub.NewGossipSub(ctx, host)
	if err != nil {
		return nil, err
	}

	_topic, err := ps.Join(topic)
	if err != nil {
		return nil, err
	}

	sub, err := _topic.Subscribe()
	if err != nil {
		return nil, err
	}

	msgCh := make(chan []byte)
	go func() {
		defer close(msgCh)
		for {
			msg, err := sub.Next(ctx)
			if err != nil {
				return
			}
			// Ignore messages from ourselves
			if msg.ReceivedFrom == host.ID() {
				continue
			}
			msgCh <- msg.Data
		}
	}()

	return msgCh, nil
}

// SubscribeToFiles subscribes to a topic for file messages and saves received files for this node
func SubscribeToFiles(ctx context.Context, host host.Host, topic string, downloadDir string) error {
	msgCh, err := Subscribe(ctx, host, topic)
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Ensure download directory exists
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	// Get current node ID for comparison
	currentNodeID := host.ID().String()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-msgCh:
				if data == nil {
					return
				}

				// Deserialize file message using gob
				var fileMsg FileMessage
				buf := bytes.NewBuffer(data)
				decoder := gob.NewDecoder(buf)
				if err := decoder.Decode(&fileMsg); err != nil {
					fmt.Printf("Failed to decode message: %v\n", err)
					continue
				}

				// Check if this message is for current node
				if fileMsg.TargetNode != currentNodeID {
					fmt.Printf("Message not for this node (target: %s, current: %s)\n",
						fileMsg.TargetNode, currentNodeID)
					continue
				}

				// Save file to local directory
				filePath := filepath.Join(downloadDir, fileMsg.Filename)
				if err := os.WriteFile(filePath, fileMsg.Data, 0644); err != nil {
					fmt.Printf("Failed to save file %s: %v\n", fileMsg.Filename, err)
					continue
				}

				fmt.Printf("Received and saved file from %s: %s (%d bytes)\n",
					fileMsg.SenderNode, filePath, fileMsg.Size)
			}
		}
	}()

	return nil
}
