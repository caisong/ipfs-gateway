package pubsub

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ipfs/boxo/bitswap"
	"github.com/ipfs/boxo/blockservice"
	"github.com/ipfs/boxo/blockstore"
	"github.com/ipfs/boxo/ipld/merkledag"
	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	format "github.com/ipfs/go-ipld-format"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/host"
)

// PushMessage represents a file push message (task info only, no file content)
type PushMessage struct {
	TaskID     string `gob:"task_id"`     // Task identifier
	FilePath   string `gob:"file_path"`   // File path (for file pushes)
	CID        string `gob:"cid"`         // IPFS CID (for CID pushes)
	PushType   string `gob:"push_type"`   // "file" or "cid"
	TargetNode string `gob:"target_node"` // Target node peer ID
	SenderNode string `gob:"sender_node"` // Sender node peer ID
	Timestamp  int64  `gob:"timestamp"`   // Push timestamp
}

// FileMessage represents a file transfer message with CID instead of raw data
type FileMessage struct {
	CID        string `gob:"cid"`         // IPFS CID of the file
	Filename   string `gob:"filename"`    // Original filename
	Size       int64  `gob:"size"`        // File size
	TargetNode string `gob:"target_node"` // Target node peer ID
	SenderNode string `gob:"sender_node"` // Sender node peer ID
}

// Topic naming constants
const (
	TopicFileTransfer = "ipfs-gateway-files" // Original file transfer topic
	TopicPushMessage  = "ipfs-gateway-push"  // File push message topic
)

// IPFSComponents represents shared IPFS components for file operations
type IPFSComponents struct {
	Host         host.Host
	DHT          *dht.IpfsDHT
	Datastore    datastore.Datastore
	Blockstore   blockstore.Blockstore
	Bitswap      *bitswap.Bitswap
	BlockService blockservice.BlockService
	DAG          format.DAGService
}

// PublishFile publishes a file CID to a specific target node
func PublishFile(ctx context.Context, components *IPFSComponents, topic string, filePath string, targetNodeID string) error {
	// Add file to IPFS and get CID
	fileCID, fileSize, err := addFileToIPFS(ctx, components, filePath)
	if err != nil {
		return fmt.Errorf("failed to add file to IPFS: %w", err)
	}

	// Create file message with CID and metadata
	fileMsg := FileMessage{
		CID:        fileCID,
		Filename:   filepath.Base(filePath),
		Size:       fileSize,
		TargetNode: targetNodeID,
		SenderNode: components.Host.ID().String(),
	}

	// Serialize message using gob
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(fileMsg); err != nil {
		return fmt.Errorf("failed to encode message: %w", err)
	}

	fmt.Printf("Publishing file CID %s to target node %s\n", fileCID, targetNodeID)
	return Publish(ctx, components.Host, topic, buf.Bytes())
}

// PublishCID publishes a CID to a specific target node
func PublishCID(ctx context.Context, components *IPFSComponents, topic string, cidStr string, targetNodeID string) error {
	// Create file message with CID only
	fileMsg := FileMessage{
		CID:        cidStr,
		Filename:   cidStr + ".dat", // Default filename for CID
		Size:       0,               // Size unknown for CID-only pushes
		TargetNode: targetNodeID,
		SenderNode: components.Host.ID().String(),
	}

	// Serialize message using gob
	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(fileMsg); err != nil {
		return fmt.Errorf("failed to encode message: %w", err)
	}

	fmt.Printf("Publishing CID %s to target node %s\n", cidStr, targetNodeID)
	return Publish(ctx, components.Host, topic, buf.Bytes())
}

// addFileToIPFS adds a file to IPFS and returns its CID
func addFileToIPFS(ctx context.Context, components *IPFSComponents, filePath string) (string, int64, error) {
	// Read file data
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read file: %w", err)
	}

	// Create a block from the file data
	block := blocks.NewBlock(data)

	// Add block to blockstore
	if err := components.Blockstore.Put(ctx, block); err != nil {
		return "", 0, fmt.Errorf("failed to put block to blockstore: %w", err)
	}

	// Create a simple DAG node with the data
	node := merkledag.NewRawNode(data)

	// Add node to DAG
	if err := components.DAG.Add(ctx, node); err != nil {
		return "", 0, fmt.Errorf("failed to add node to DAG: %w", err)
	}

	return node.Cid().String(), int64(len(data)), nil
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

// SubscribeToFiles subscribes to a topic for file CID messages and downloads files for this node
func SubscribeToFiles(ctx context.Context, components *IPFSComponents, topic string, downloadDir string) error {
	msgCh, err := Subscribe(ctx, components.Host, topic)
	if err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}

	// Ensure download directory exists
	if err := os.MkdirAll(downloadDir, 0755); err != nil {
		return fmt.Errorf("failed to create download directory: %w", err)
	}

	// Get current node ID for comparison
	currentNodeID := components.Host.ID().String()

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

				fmt.Printf("Received file CID %s from %s, downloading...\n",
					fileMsg.CID, fileMsg.SenderNode)

				// Download file using CID
				filePath := filepath.Join(downloadDir, fileMsg.Filename)
				if err := downloadFileFromCID(ctx, components, fileMsg.CID, filePath); err != nil {
					fmt.Printf("Failed to download file %s: %v\n", fileMsg.Filename, err)
					continue
				}

				fmt.Printf("Successfully downloaded file from %s: %s (%d bytes)\n",
					fileMsg.SenderNode, filePath, fileMsg.Size)
			}
		}
	}()

	return nil
}

// downloadFileFromCID downloads a file from IPFS using its CID
func downloadFileFromCID(ctx context.Context, components *IPFSComponents, cidStr string, filePath string) error {
	// Parse CID
	targetCID, err := cid.Decode(cidStr)
	if err != nil {
		return fmt.Errorf("invalid CID: %w", err)
	}

	// Create download context with timeout
	downloadCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// Get the node from IPFS
	node, err := components.DAG.Get(downloadCtx, targetCID)
	if err != nil {
		return fmt.Errorf("failed to get node from IPFS: %w", err)
	}

	// Create output file
	outFile, err := os.Create(filePath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer outFile.Close()

	// Get file reader from UnixFS node
	fileReader, err := getFileReaderFromNode(node)
	if err != nil {
		return fmt.Errorf("failed to get file reader: %w", err)
	}

	// Copy data to file
	bytesWritten, err := io.Copy(outFile, fileReader)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	fmt.Printf("File downloaded: %s (%d bytes) -> %s\n", cidStr, bytesWritten, filePath)
	return nil
}

// getFileReaderFromNode extracts file data from IPFS node
func getFileReaderFromNode(node format.Node) (io.Reader, error) {
	// For simplicity, we'll use the raw data of the node
	// In a full implementation, you'd want to handle UnixFS properly
	return bytes.NewReader(node.RawData()), nil
}

// PublishPushMessage publishes a push message
func PublishPushMessage(ctx context.Context, host host.Host, taskID, targetNode, senderNode, pushType, filePath, cid string) error {
	pushMsg := PushMessage{
		TaskID:     taskID,
		FilePath:   filePath,
		CID:        cid,
		PushType:   pushType,
		TargetNode: targetNode,
		SenderNode: senderNode,
		Timestamp:  time.Now().Unix(),
	}

	var buf bytes.Buffer
	encoder := gob.NewEncoder(&buf)
	if err := encoder.Encode(pushMsg); err != nil {
		return fmt.Errorf("failed to encode push message: %w", err)
	}

	return Publish(ctx, host, TopicPushMessage, buf.Bytes())
}

// SubscribeToPushMessages subscribes to push messages
func SubscribeToPushMessages(ctx context.Context, components *IPFSComponents, transferService interface{}) error {
	msgCh, err := Subscribe(ctx, components.Host, TopicPushMessage)
	if err != nil {
		return fmt.Errorf("failed to subscribe to push messages: %w", err)
	}

	currentNodeID := components.Host.ID().String()

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case data := <-msgCh:
				if data == nil {
					return
				}

				// Deserialize push message
				var pushMsg PushMessage
				buf := bytes.NewBuffer(data)
				decoder := gob.NewDecoder(buf)
				if err := decoder.Decode(&pushMsg); err != nil {
					fmt.Printf("Failed to decode push message: %v\n", err)
					continue
				}

				// Check if this message is for current node
				if pushMsg.TargetNode != currentNodeID {
					continue
				}

				fmt.Printf("Received push message from %s: type=%s\n", pushMsg.SenderNode, pushMsg.PushType)

				// Handle push message asynchronously
				go handlePushMessage(ctx, components, &pushMsg, transferService)
			}
		}
	}()

	return nil
}

// handlePushMessage handles a push message
func handlePushMessage(ctx context.Context, components *IPFSComponents, pushMsg *PushMessage, transferService interface{}) {
	var err error
	switch pushMsg.PushType {
	case "cid":
		// For CID pushes, download directly using bsnet
		if pushMsg.CID != "" {
			err = downloadFileFromCID(ctx, components, pushMsg.CID, fmt.Sprintf("./downloads/%s_%s", pushMsg.TaskID, pushMsg.CID+".dat"))
		}
	case "file":
		// For file pushes, the sender should use transfer service to upload
		fmt.Printf("Received file push notification for %s, waiting for transfer\n", pushMsg.FilePath)
		// No immediate action needed - transfer will be initiated by sender
	default:
		err = fmt.Errorf("unknown push type: %s", pushMsg.PushType)
	}

	// Push message handling completed - no notification needed
	if err != nil {
		fmt.Printf("Push message handling failed: %v\n", err)
	} else {
		fmt.Printf("Push message handled successfully: %s\n", pushMsg.TaskID)
	}
}
