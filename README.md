# ConnVerifier / 连接稳定性验证工具

ConnVerifier is a Go-based TCP connection stability tester. It verifies whether a client behind NAT or carrier-grade NAT can keep a configurable number of concurrent TCP sessions alive with a public echo endpoint.

ConnVerifier 是一个基于 Go 的 TCP 连接稳定性验证工具，用于测试 NAT 或运营商级 NAT 后的客户端能否长期维持大量到云端回声服务的 TCP 连接。

The client uses both application-level heartbeats (`PING` echoed by the server) and optional OS TCP keepalive. Application heartbeats provide measurable acknowledgments; TCP keepalive helps the kernel detect broken idle connections.

客户端同时支持应用层心跳和可选的系统 TCP keepalive。应用层心跳用于统计可确认的回包；TCP keepalive 用于帮助内核发现异常空闲连接。

## Components / 组成部分

- `cmd/server`: TCP echo server. It listens on a configurable address, echoes inbound bytes, supports idle timeouts, TCP keepalive, and a maximum concurrent connection limit.
- `cmd/client`: TCP stability client. It maintains a target number of concurrent connections, sends periodic heartbeats, validates heartbeat acknowledgments, reconnects with exponential backoff, and prints per-second stats.

## Build / 编译

```bash
go build -o bin/connverifier-server ./cmd/server
go build -o bin/connverifier-client ./cmd/client
```

Run checks:

```bash
go test ./...
go vet ./...
```

## Usage / 使用方式

Start the echo server on a public VM:

```bash
./bin/connverifier-server \
  -addr :9000 \
  -max-conns 10000 \
  -idle-timeout 2m \
  -tcp-keepalive 30s
```

Run the local client:

```bash
./bin/connverifier-client \
  -addr <server-ip>:9000 \
  -clients 10000 \
  -start-rate 500 \
  -heartbeat 30s \
  -dial-timeout 5s \
  -io-timeout 5s \
  -min-backoff 500ms \
  -max-backoff 1m \
  -tcp-keepalive 30s
```

## Client Flags / 客户端参数

- `-addr`: target server address. Default: `127.0.0.1:9000`.
- `-clients`: target number of concurrent connections. Must be between `1` and `1000000`.
- `-start-rate`: maximum new connection attempts per second. Must be between `1` and `100000`.
- `-heartbeat`: interval between application-level `PING` heartbeats. Must be positive.
- `-dial-timeout`: TCP dial timeout. Must be positive.
- `-io-timeout`: deadline applied independently to each heartbeat write and read. Must be positive.
- `-min-backoff`: initial retry backoff. Must be positive.
- `-max-backoff`: maximum retry backoff. Must be greater than or equal to `-min-backoff`. Actual reconnect waits are randomized with equal jitter (each wait is a random value in `[delay/2, delay]`) to desynchronize reconnect waves after a mass drop.
- `-tcp-keepalive`: TCP keepalive probe interval. Set `<=0` to disable OS TCP keepalive.
- `-log-drops`: log every connection drop with its reason. Disabled by default to avoid log pressure during large tests; drop counts are always summarized in the per-second stats.

The client prints per-second stats:

- `target`: requested concurrent connection count.
- `active`: currently active TCP connections.
- `dial_attempts`: total dial attempts.
- `connected`: successful TCP connections.
- `dial_errors`: failed dial attempts.
- `drops`: established connections that later failed (sum of `drop_timeout`, `drop_closed`, `drop_error`, and `bad_ack`).
- `drop_timeout`: drops where a heartbeat exceeded `-io-timeout`, i.e. no reply arrived in time (often a silent NAT/middlebox drop).
- `drop_closed`: drops where the peer closed the connection (EOF/FIN).
- `drop_error`: drops from connection resets and other I/O errors.
- `heartbeats`: application heartbeats sent.
- `ack`: validated heartbeat acknowledgments.
- `bad_ack`: heartbeat responses that did not match `PING`.
- `retries`: scheduled retry attempts after dial failures or connection drops.
- `rtt_samples`: successful heartbeat round-trips measured during the last interval (the sample count behind the percentiles; low values mean the percentiles are noisy).
- `rtt_p50` / `rtt_p95` / `rtt_p99`: application-level heartbeat round-trip latency percentiles for the last interval. Measured only on successful heartbeats (failed or timed-out ones are counted under `drops`/`drop_timeout` instead). Percentiles are histogram-bucketed with ~10% resolution and reset every interval, so they reflect current latency rather than the whole run.

## Server Flags / 服务端参数

- `-addr`: TCP listen address. Default: `:9000`.
- `-max-conns`: maximum concurrent connections. Default: `10000`; set `<=0` for unlimited.
- `-idle-timeout`: idle timeout per connection. Default: `2m`; set `<=0` to disable.
- `-tcp-keepalive`: TCP keepalive probe interval. Default: `30s`; set `<=0` to disable.
- `-log-connections`: log every per-connection event (open, close-with-reason, and rejection). Disabled by default to avoid log pressure during large tests; aggregate counts are always reported in the per-second stats line.

The server prints per-second stats and shuts down cleanly on `SIGINT`/`SIGTERM` (it stops accepting and logs a final summary):

- `active`: currently open connections.
- `accepted`: total connections accepted (i.e. that passed the `-max-conns` limit).
- `rejected`: total connections refused because `-max-conns` was reached.
- `closed`: total connections that have since finished.

## Recommendations / 建议

- Raise `ulimit -n` on both client and server before testing 10k+ sockets.
- Use `-start-rate` to ramp up gradually and avoid triggering provider anti-DDoS systems.
- Do not expose the server broadly on the public internet. Prefer firewall rules that only allow known test client IPs.
- Keep `-max-conns` and `-idle-timeout` enabled for public VM tests.
- Treat this as a controlled diagnostics tool, not a general-purpose public echo service.
