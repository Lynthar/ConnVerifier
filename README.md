# ConnVerifier

[![license](https://img.shields.io/github/license/Lynthar/ConnVerifier)](LICENSE)

Two Go binaries that hold thousands of idle TCP connections to find where NAT drops them, with RTT percentiles

English | [简体中文](README.zh-CN.md)

> **Under construction.** It does what's described below and the tests pass, but
> there's no release, no CI and no version flag — you build it from source.

Carriers and home routers quietly forget idle TCP connections. This finds out
when. One binary echoes bytes back; the other opens as many connections as you
ask for, keeps them alive with a small heartbeat, and reports what percentage
survived and for how long — with each disconnect attributed to a timeout, a
close from the peer, or an error. Every second it prints p50, p95 and p99 RTT
alongside the connection counts.

Standard library only, no dependencies.

## Build

There's no release and `go install` won't work — the module path isn't the
repository path. Clone and build; you need Go 1.21 or newer:

```bash
git clone https://github.com/Lynthar/ConnVerifier.git
cd ConnVerifier
go build -o bin/connverifier-server ./cmd/server
go build -o bin/connverifier-client ./cmd/client
```

Raise the file descriptor limit on **both** machines before running anything
sizeable — one connection is one descriptor.

## Usage

```bash
ulimit -n 20480
./bin/connverifier-server -addr :9000 -max-conns 20000 -idle-timeout 2m
```

On the other side:

```bash
./bin/connverifier-client -addr <server>:9000 -clients 10000 -start-rate 500 -heartbeat 30s
```

For an unattended run that stops on its own:

```bash
./bin/connverifier-client -addr <server>:9000 -clients 1000 -duration 1h
```

Output looks like this, once a second, then a summary at the end:

```
stats target=50 active=50 dial_attempts=50 connects=50 dial_errors=0 drops=0
  heartbeats=100 ack=100 rtt_p50=159µs rtt_p95=255µs rtt_p99=255µs
```

Everything is a flag; there's no config file and no environment variables.
Client: `-addr`, `-clients` (1000), `-start-rate` (100), `-heartbeat` (30s),
`-dial-timeout` (5s), `-io-timeout` (5s), `-min-backoff` (500ms),
`-max-backoff` (1m), `-tcp-keepalive` (0), `-duration` (0 = until interrupted),
`-log-drops`. Server: `-addr` (:9000), `-max-conns` (10000), `-idle-timeout`
(2m), `-tcp-keepalive` (0), `-log-connections`. Zero or negative means disabled
or unlimited.

**`-tcp-keepalive` is 0 on purpose** — kernel keepalives would refresh the NAT
mapping and quietly turn every result into "the NAT is fine".

## Limitations

- **TCP only.** No UDP, no STUN, no DNS, no bandwidth measurement.
- **No JSON output and no stable output format.** Anything parsing the text
  will break when the text changes.
- **The exit code doesn't judge your network.** Zero means the run finished;
  non-zero means the tool itself failed. Gating CI on it will always pass.
- **Drops are found on the heartbeat, not instantly.** A `FIN` arriving between
  beats isn't noticed until the next one, so reported lifetimes are an upper
  bound and `active` runs slightly high.
- **A full server looks like a client-side failure.** The TCP handshake
  completes before the rejection, so you see successful dials and failed first
  heartbeats, with `dial_errors` at zero. Check the server's `rejected=` count
  to tell them apart.
- **About 28,000 ephemeral ports** per source IP against one destination
  `ip:port` caps how many connections one client machine can hold.

## Differences from upstream

This is a fork of [codeberg.org/woq/ConnVerifier](https://codeberg.org/woq/ConnVerifier).
What I added: the `cmd/` layout with tests, RTT percentiles, drop attribution,
reconnect with exponential backoff and jitter, a server with a connection cap
and idle reaping, and `-duration` for unattended soak runs.

## Security

**The server is an unauthenticated echo endpoint.** It has no rate limiting, so
anyone who can reach it can fill `-max-conns` and keep legitimate clients out,
and it will echo back whatever bytes it receives. Put it behind a firewall that
only admits the client you're testing from, and don't leave it on the open
internet.

## License

MIT — see [LICENSE](LICENSE). The copyright line reads "2025 woq", the upstream
author.
