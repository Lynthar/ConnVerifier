# ConnVerifier / 连接稳定性验证工具

ConnVerifier is a Go-based TCP keepalive stress tool that tests whether a client behind carrier NAT can keep a configurable number of concurrent TCP sessions alive with a cloud echo endpoint.

ConnVerifier 是一个基于 Go 的 TCP Keepalive 压测工具，用于验证运营商级 NAT 后的客户端能否维持可配置数量的并发 TCP 会话与云端回声服务持续通信。

## Components / 组成部分

- `server.go`：Minimal echo server that listens on a configurable TCP port, accepts client sockets, and immediately echoes back inbound bytes without closing connections—ideal for validating PING/PONG heartbeats.
- `server.go`：一个最简回声服务器，监听可配置端口，接受客户端后直接回写收到的字节，不主动断开连接，便于验证 PING/PONG 心跳。
- `client.go`：Launches configurable goroutines, each maintaining a TCP connection with periodic heartbeats, exponential-backoff reconnects, and per-second stats. Dialing is rate-limited to avoid exhausting bandwidth/file descriptors.
- `client.go`：启动多个可配置的 Goroutine，每个维持一个 TCP 连接，定期心跳、遇断自动指数退避重连，并输出每秒统计；拨号行为带速率限制，防止带宽或文件描述符耗尽。

## Usage / 使用方式

1. Build the binaries / 编译可执行文件：
   ```bash
   go build server.go   # produces ./server / 生成 ./server
   go build client.go   # produces ./client / 生成 ./client
   ```

2. Start the echo server on a public cloud VM / 在云端 VM 上启动回声服务：
   ```bash
   ./server -addr :9000
   ```

3. Run the local client (tune flags per environment) / 在本地运行客户端（根据实际环境调整参数）：
   ```bash
   ./client \
     -addr <server-ip>:9000 \
     -clients 10000 \
     -start-rate 500 \
     -heartbeat 30s \
     -min-backoff 500ms \
     -max-backoff 1m
   ```

## Client Flags / 客户端参数

- `-addr`：Target server address (默认 `127.0.0.1:9000`)，指定服务器地址。
- `-clients`：Goal for concurrent connections to maintain（目标并发连接数），客户端会持续补足。
- `-start-rate`：Maximum number of new connections (initial or reconnect) launched per second（每秒最大新连接数，含重连）。
- `-heartbeat`：Interval between each PING heartbeat（心跳间隔）。
- `-dial-timeout`, `-io-timeout`：拨号与 IO 操作的超时时间。
- `-min-backoff`, `-max-backoff`：指数退避策略的最小/最大间隔，控制重连节奏。

The client prints per-second stats covering active connections, total dial attempts, dropouts, reconnect attempts, and heartbeat acknowledgments—giving you real-time stability feedback without noisy per-disconnection logs.

客户端会每秒输出活跃连接数、总拨号数、掉线次数、重连次数与心跳确认数，提供实时的稳定性反馈，无需打印每次断线细节。

## Recommendations / 建议

- Raise `ulimit -n` before running the client so the OS permits 10k+ sockets.
- 运行客户端前请先提升 `ulimit -n`，确保系统支持 1 万条以上 socket。
- Smoothly ramp the load via `-start-rate` to avoid triggering cloud provider DDoS protections.
- 通过 `-start-rate` 控制每秒新连接量，平滑开启，避免触发云厂商的防护策略。