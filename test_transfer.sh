#!/bin/bash

echo "=== IPFS网关 + PingPong + 文件传输测试 ==="
echo

# 启动服务（后台运行）
echo "1. 启动IPFS网关和PingPong服务..."
./ipfs-gateway -port=8082 &
SERVER_PID=$!

# 等待服务启动
sleep 3

echo "2. 测试健康检查..."
curl -s http://localhost:8082/health | jq .
echo

echo "3. 查看服务统计..."
curl -s http://localhost:8082/api/v1/stats | jq .
echo

echo "4. 创建测试文件..."
echo "Hello, IPFS World! 这是一个测试文件。" > /tmp/test.txt

echo "5. 上传文件到IPFS..."
curl -X POST -F "file=@/tmp/test.txt" \
  http://localhost:8082/api/v1/transfer/upload | jq .
echo

echo "6. 查看本地文件列表..."
curl -s http://localhost:8082/api/v1/files | jq .
echo

echo "7. 查看服务节点信息..."
curl -s http://localhost:8082/api/v1/peers | jq .
echo

# 清理
echo "8. 清理测试环境..."
kill $SERVER_PID 2>/dev/null
rm -f /tmp/test.txt
wait $SERVER_PID 2>/dev/null

echo "测试完成！"