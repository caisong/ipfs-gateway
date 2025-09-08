package stream

import (
	"bufio"
	"context"
	"fmt"
	"log"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

func Ping(host host.Host, service peer.ID) error {
	stream, err := host.NewStream(context.Background(), service, PingPongProtocol)
	if err != nil {
		return fmt.Errorf("打开流失败: %w", err)
	}
	defer stream.Close()

	// 发送PING消息
	_, err = stream.Write([]byte(msgPing + "\n"))
	if err != nil {
		return fmt.Errorf("发送PING失败: %w", err)
	}

	// 读取响应
	reader := bufio.NewReader(stream)
	response, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	log.Printf("收到响应: %s", response)
	return nil
}
