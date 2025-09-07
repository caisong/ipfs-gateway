# IPFS 统一网关项目

这个项目整合了四个IPFS相关功能：服务发现、HTTP网关代理、基于libp2p的流通信（pingpong）和基于Bitswap的文件传输。

**新特性：默认启动网关+PingPong服务，支持直接文件传输命令！**

## 快速开始

```bash
# 编译项目
go build -o ipfs-gateway .

# 默认启动（网关+PingPong）
./ipfs-gateway

# 或指定端口
./ipfs-gateway -port=8080
```

现在您可以：
- 访问 IPFS 内容：`http://localhost:8080/ipfs/QmHash...`
- 接收其他节点的 ping 请求并自动回复 pong
- 使用 API 进行文件传输：`POST /api/v1/transfer/upload`

## 项目结构

```
/home/caisong/gateway/
├── main.go                # 统一主入口文件
├── go.mod                 # 项目依赖管理
├── config/
│   └── config.go          # 统一配置管理
├── pkg/
│   ├── discovery/         # 服务发现模块
│   │   └── service.go
│   ├── gateway/           # HTTP网关模块
│   │   └── gateway.go
│   ├── stream/           # 流通信模块
│   │   └── pingpong.go
│   └── transfer/         # 文件传输模块
│       └── transfer.go
```

## 功能模块

### 1. 服务发现 (Discovery)
- 基于DHT的分布式服务发现
- 支持服务注册和自动发现
- 提供网关、流通信、文件传输服务的发现机制

### 2. HTTP网关 (Gateway)
- 基于Echo Web框架
- 提供IPFS HTTP网关代理功能
- 支持 `/ipfs/*` 和 `/ipns/*` 路径
- 包含健康检查端点 `/health`
- 集成文件传输API

### 3. 流通信 (Stream)
- 基于libp2p的P2P网络通信
- 实现PingPong协议
- 支持节点发现和连接
- 自动响应ping请求

### 4. 文件传输 (Transfer)
- 基于Bitswap协议的文件传输
- 使用DHT进行节点发现
- 支持IPFS内容寻址
- 提供文件上传下载API

## 编译

```bash
cd /home/caisong/gateway
go build -o ipfs-gateway .
```

## 运行方式

### 默认模式：网关 + PingPong（推荐）

```bash
# 默认启动网关和PingPong服务
./ipfs-gateway

# 指定端口
./ipfs-gateway -port=8081

# 指定P2P监听地址
./ipfs-gateway -listen="/ip4/0.0.0.0/tcp/9000"
```

### 其他运行模式

```bash
# 全功能模式（所有服务）
./ipfs-gateway -mode=all

# 仅网关模式
./ipfs-gateway -mode=gateway

# 仅流通信模式
./ipfs-gateway -mode=stream

# 仅文件传输模式
./ipfs-gateway -mode=transfer
```



## 命令行参数

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-mode` | `gateway+stream` | 运行模式: all, gateway, stream, transfer, gateway+stream |
| `-port` | `8080` | HTTP服务端口（网关模式） |
| `-remote` | `https://trustless-gateway.link` | 远程IPFS网关地址 |
| `-listen` | `/ip4/0.0.0.0/tcp/0` | P2P监听地址 |
| `-ping-interval` | `3` | Ping间隔（秒） |
| `-health-check` | `true` | 启用健康检查端点 |
| `-log-level` | `info` | 日志级别: debug, info, warn, error |

## 使用示例

### PingPong 自动响应

启动服务后，系统会自动：
- 监听P2P网络的ping请求
- 对收到的ping自动回复pong
- 显示连接的节点信息

### 文件传输API

网关服务提供直接的文件传输API：

```bash
# 1. 上传文件到IPFS网络
curl -X POST -F "file=@test.txt" \
  http://localhost:8080/api/v1/transfer/upload

# 响应：
# {
#   "message": "upload completed successfully",
#   "cid": "QmHash...",
#   "filename": "test.txt",
#   "status": "completed"
# }

# 2. 下载文件通过CID
curl -X POST "http://localhost:8080/api/v1/transfer/download/QmHash?filename=downloaded.txt"

# 3. 查询传输状态
curl http://localhost:8080/api/v1/transfer/status/QmHash

# 4. 查看本地文件列表
curl http://localhost:8080/api/v1/files
```

### 网关功能测试
```bash
# 启动网关服务
./ipfs-gateway -mode=gateway -port=8081

# 在另一个终端测试
curl http://localhost:8081/health
curl http://localhost:8081/ipfs/QmHash...
```

### 流通信功能测试
```bash
# 启动流通信服务
./ipfs-gateway -mode=stream

# 查看节点ID和监听地址
# 可以在日志中看到节点信息
```

### 文件传输功能测试
```bash
# 启动文件传输服务
./ipfs-gateway -mode=transfer

# 服务将启动DHT和Bitswap功能
```

## 技术栈

- **Go**: 1.24.0+
- **IPFS**: github.com/ipfs/boxo v0.34.0
- **libp2p**: github.com/libp2p/go-libp2p v0.43.0
- **Web框架**: github.com/labstack/echo/v4 v4.13.4
- **DHT**: github.com/libp2p/go-libp2p-kad-dht v0.34.0

## 依赖管理

项目使用Go Modules管理依赖，主要依赖包括：
- IPFS Boxo生态系统组件
- libp2p网络库
- Echo Web框架
- 各种IPFS和libp2p相关工具库

运行 `go mod tidy` 自动下载和整理依赖。

## 开发说明

项目采用模块化架构：
- 每个功能都封装为独立的包
- 支持统一配置管理
- 提供统一的服务接口
- 支持多种运行模式

## 测试

项目已通过全面功能测试：
- ✅ 统一版本编译成功
- ✅ 默认模式（gateway+stream）启动测试
- ✅ 网关模式启动测试
- ✅ 流通信模式启动测试
- ✅ 文件传输模式启动测试
- ✅ 命令行参数解析测试
- ✅ PingPong自动响应测试
- ✅ 文件传输API测试
- ✅ 服务发现和注册测试
- ✅ 模块间协作测试

运行测试脚本：
```bash
./test_transfer.sh
```