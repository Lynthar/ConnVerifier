# ConnVerifier

ConnVerifier is a small Go tool for testing whether a host behind NAT or
carrier-grade NAT can keep a large number of long-lived TCP connections alive
with a public endpoint. It ships as two binaries: an echo **server** and a
load-generating **client**.

ConnVerifier 是一个小巧的 Go 工具，用于检验处于 NAT 或运营商级 NAT 之后的主机，能否与
公网端点长期维持大量 TCP 长连接。它由两个程序组成：回声**服务端**与压测**客户端**。

## What it does / 功能

- The client holds a target number of concurrent connections, dials at a bounded
  rate, and reconnects with jittered exponential backoff.
- Every connection runs `PING`/echo heartbeats. The client reports per-interval
  round-trip latency percentiles (p50/p95/p99) and attributes each drop to a
  cause — timeout, peer-closed, or error. OS-level TCP keepalive is optional.
- The server echoes traffic with a concurrent-connection cap, idle-connection
  reaping, per-second statistics, and graceful shutdown.

- 客户端维持目标数量的并发连接，以受限速率建连，断开后采用带抖动的指数退避重连。
- 每条连接运行 `PING`/回声心跳。客户端按周期报告往返延迟百分位（p50/p95/p99），并将每次
  掉线归因为超时、对端关闭或错误。系统级 TCP keepalive 可选。
- 服务端回显流量，支持并发连接上限、空闲连接回收、每秒统计与优雅退出。

## Usage / 使用

Build the two binaries / 编译两个程序：

```bash
go build -o bin/connverifier-server ./cmd/server
go build -o bin/connverifier-client ./cmd/client
```

Run the echo server on a public host, then point the client at it / 在公网主机
运行回声服务端，再让客户端连接它：

```bash
./bin/connverifier-server -addr :9000 -max-conns 20000 -idle-timeout 2m
./bin/connverifier-client -addr <server>:9000 -clients 10000 -start-rate 500 -heartbeat 30s
```

Both binaries print statistics once per second and shut down cleanly on
`SIGINT`/`SIGTERM`; run either with `-h` for the full set of flags. The server is
an unauthenticated echo endpoint — restrict it with a firewall and do not expose
it on the open internet.

两个程序均每秒打印一次统计，并在 `SIGINT`/`SIGTERM` 时干净退出；完整参数请以 `-h`
查看。服务端是无鉴权的回声端点，请用防火墙限制访问，切勿暴露于公网。

## Forked from / 来源

Forked from woq's ConnVerifier (<https://codeberg.org/woq/ConnVerifier>), used
under the MIT License (© 2025 woq; see [LICENSE](LICENSE)). This repository adds
RTT percentiles, drop-cause classification, reconnect jitter, and a graceful,
observable server.

源自 woq 的 ConnVerifier（<https://codeberg.org/woq/ConnVerifier>，MIT 许可，
© 2025 woq，见 [LICENSE](LICENSE)）。本仓库在其基础上增加了 RTT 百分位、掉线归因、
重连抖动，以及可优雅退出、可观测的服务端。
