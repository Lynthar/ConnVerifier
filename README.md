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
- Every connection runs `PING`/echo heartbeats. The client reports per-second
  round-trip latency percentiles (p50/p95/p99), attributes each drop to a cause
  — timeout, peer-closed, or error — and prints a cumulative summary on exit.
  OS-level TCP keepalive is off by default on both ends (see notes below).
- The server echoes traffic with a concurrent-connection cap, idle-connection
  reaping, and per-second statistics; on shutdown it stops accepting and prints
  final totals.

- 客户端维持目标数量的并发连接，以受限速率建连，断开后采用带抖动的指数退避重连。
- 每条连接运行 `PING`/回声心跳。客户端按秒报告往返延迟百分位（p50/p95/p99），将每次
  掉线归因为超时、对端关闭或错误，并在退出时打印全程汇总。系统级 TCP keepalive 两端
  默认关闭（见下文注意事项）。
- 服务端回显流量，支持并发连接上限、空闲连接回收与每秒统计；退出时停止接受连接并
  打印终局统计。

## Usage / 使用

Build the two binaries / 编译两个程序：

```bash
go build -o bin/connverifier-server ./cmd/server
go build -o bin/connverifier-client ./cmd/client
```

Run the echo server on a public host, then point the client at it / 在公网主机
运行回声服务端，再让客户端连接它：

```bash
ulimit -n 20480   # raise above -clients / -max-conns on both ends / 两端都要高于连接数
./bin/connverifier-server -addr :9000 -max-conns 20000 -idle-timeout 2m
./bin/connverifier-client -addr <server>:9000 -clients 10000 -start-rate 500 -heartbeat 30s
```

Both binaries print statistics once per second and shut down cleanly on
`SIGINT`/`SIGTERM`; run either with `-h` for the full set of flags. The server is
an unauthenticated echo endpoint — restrict it with a firewall and do not expose
it on the open internet.

两个程序均每秒打印一次统计，并在 `SIGINT`/`SIGTERM` 时干净退出；完整参数请以 `-h`
查看。服务端是无鉴权的回声端点，请用防火墙限制访问，切勿暴露于公网。

## Operational notes / 运行注意

- **File descriptors**: every connection holds one fd on each end, so raise
  `ulimit -n` above `-clients` on both hosts first; a too-low limit surfaces as
  climbing `dial_errors`. One source IP also has only ~28k ephemeral ports
  toward a single server `ip:port`, which caps `-clients` per client machine.
- **Measuring NAT idle timeouts**: keep `-tcp-keepalive 0` (the default) on both
  ends — any keepalive traffic refreshes the NAT mapping between heartbeats, so
  you would measure the keepalive interval instead of `-heartbeat`. Keep
  `-heartbeat` clearly below the server's `-idle-timeout`, otherwise you measure
  the server's idle reaping, not the path.
- **Server-full rejections look like client errors**: the TCP handshake
  completes before the server can refuse, so a rejected connection shows up as a
  successful dial whose first heartbeat fails (`drop_error`/`drop_closed` with
  near-zero session time while `dial_errors` stays 0). Check the server's
  `rejected=` counter before blaming the network.
- **Drops are detected at the next heartbeat**: the client does not read the
  socket between heartbeats, so `dropped after X` is time-to-detection (an upper
  bound on the true lifetime) and `active` can briefly overcount.
- **Reading the stats**: counters (`connects=`, `drops=`, …) are cumulative
  since start; `rtt_*` percentiles cover the last second only; lock-free updates
  mean related counters in one line can disagree by ±1. The exit summary covers
  the whole run. Reconnect delays are jittered to 50–100% of the current
  backoff, so waits can be half of `-min-backoff`.
- **Automation**: `-duration 1h` ends the run and prints the summary. Exit code
  0 means the run completed (timer or signal); non-zero means the tool itself
  failed — it does not judge network quality.

- **文件描述符**：每条连接在两端各占一个 fd，先把两端 `ulimit -n` 提到 `-clients`
  之上；上限过低表现为 `dial_errors` 持续攀升。单一源 IP 对单一服务端 `ip:port` 也
  只有约 2.8 万个临时端口，这限制了单台客户机的 `-clients`。
- **测 NAT 空闲超时**：两端保持 `-tcp-keepalive 0`（默认值）——任何 keepalive 流量都会
  在心跳间隙刷新 NAT 映射，实测的就成了 keepalive 间隔而非 `-heartbeat`；同时
  `-heartbeat` 必须明显小于服务端 `-idle-timeout`，否则测到的是服务端回收行为。
- **服务端满载拒绝会伪装成客户端错误**：TCP 握手在服务端拒绝之前就已完成，被拒连接
  表现为 dial 成功、首个心跳立即失败（`drop_error`/`drop_closed` 且会话时长近零、
  `dial_errors` 恒为 0）。先对照服务端的 `rejected=` 计数，再怀疑网络。
- **掉线在下一次心跳才被发现**：心跳间隙客户端不读 socket，`dropped after X` 是发现
  耗时（真实存活时长的上界），`active` 可能短暂虚高。
- **读数说明**：计数器（`connects=`、`drops=` 等）自启动起累计；`rtt_*` 百分位只覆盖
  最近一秒；无锁更新使同一行相关计数可能相差 ±1。退出 summary 覆盖全程。重连延迟带
  抖动，实际等待为当前 backoff 的 50–100%，可能短至 `-min-backoff` 的一半。
- **自动化**：`-duration 1h` 到点结束并打印 summary。退出码 0 表示测试完成（计时或
  信号触发均是），非 0 表示工具自身失败——退出码不评判网络质量。

## Project direction / 项目方向

ConnVerifier is planned to evolve beyond the current long-lived-connection
tester into a reproducible and auditable broadband quality diagnostics toolkit
for technical users.

ConnVerifier 计划从当前的长连接验证器，逐步演进为面向技术用户、可复现且可审计的
宽带质量诊断工具。

## Forked from / 来源

Forked from woq's ConnVerifier (<https://codeberg.org/woq/ConnVerifier>), used
under the MIT License (© 2025 woq; see [LICENSE](LICENSE)). This repository adds
RTT percentiles, drop-cause classification, reconnect jitter, and a graceful,
observable server.

源自 woq 的 ConnVerifier（<https://codeberg.org/woq/ConnVerifier>，MIT 许可，
© 2025 woq，见 [LICENSE](LICENSE)）。本仓库在其基础上增加了 RTT 百分位、掉线归因、
重连抖动，以及可优雅退出、可观测的服务端。
