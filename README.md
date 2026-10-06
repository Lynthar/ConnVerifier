# ConnVerifier

[![license](https://img.shields.io/github/license/Lynthar/ConnVerifier)](LICENSE)

A Go tool that holds thousands of idle TCP connections to find where NAT drops them, with RTT percentiles

English | [简体中文](README.zh-CN.md)

> **Under construction.** It does what's described below and the tests pass, but
> there's no release yet — you build it from source.

Carriers and home routers quietly forget idle TCP connections. This finds out
when. `connverifier serve` echoes bytes back; `connverifier capacity` opens as
many connections as you ask for, keeps them alive with a small heartbeat, and reports what percentage
survived and for how long — with each disconnect attributed to a timeout, a
close from the peer, or an error. Every second it prints p50, p95 and p99 RTT
alongside the connection counts.

Standard library only, no dependencies.

## Build

There's no release yet. With Go 1.26 or newer:

```bash
go install github.com/Lynthar/ConnVerifier/cmd/connverifier@latest
```

Or clone and build:

```bash
git clone https://github.com/Lynthar/ConnVerifier.git
cd ConnVerifier
go build -o bin/connverifier ./cmd/connverifier
```

Raise the file descriptor limit on **both** machines before running anything
sizeable — one connection is one descriptor. If the client's limit or its
ephemeral port range is below the target, or dials fail for lack of either, the
result is marked `INVALID`: it would show the client machine's limit, not the
network's.

## Usage

```bash
ulimit -n 20480
./bin/connverifier serve -addr :9000 -max-conns 20000 -idle-timeout 2m
```

On the other side. `capacity` first says how many connections it will open and
asks for confirmation on the terminal, because filling the NAT table can cut off
other devices on the same network; `-yes` skips the question and is required when
stdin is not a terminal:

```bash
./bin/connverifier capacity -addr <server>:9000 -clients 10000 -start-rate 500 -heartbeat 30s
```

For an unattended run that stops on its own:

```bash
./bin/connverifier capacity -addr <server>:9000 -clients 1000 -duration 1h -yes
```

While it runs, a progress line goes to stderr once a second:

```
stats target=50 active=50 dial_attempts=50 connects=50 dial_errors=0 drops=0
  heartbeats=100 ack=100 rtt_p50=159µs rtt_p95=255µs rtt_p99=255µs
```

When it stops, the result goes to stdout — as text, or as JSON with
`-format json`. Shortened:

```
TCP long-lived connections: attention (WARN)
Node: 192.0.2.10:9000 · IPv4 · TCP
Observed
  Connections made: 73
  Drops: 31
  Drops (no reply in time): 3
  Echo round trip p95: 309 µs (40 samples)
  Echo round trip p99: too few samples (40)
Inferred
  - Connections that stopped answering without a close or reset: 3. This is
    the usual sign of a NAT or other middlebox silently dropping connection state.
Not proven
  - A node turning connections away at its limit cannot be told apart from the
    network dropping them; the client sees the same thing.
```

The text is in Chinese or English, following `LC_ALL`, `LC_MESSAGES` or `LANG`;
any other language gets Chinese, and `-lang en` or `-lang zh-CN` overrides it.
What each status and number means, and when not to trust it, is in
[docs/methods](docs/methods/README.md).

Everything is a flag; there's no config file and no environment variables.
`capacity`: `-addr`, `-clients` (1000), `-start-rate` (100), `-heartbeat` (30s),
`-dial-timeout` (5s), `-io-timeout` (5s), `-min-backoff` (500ms),
`-max-backoff` (1m), `-tcp-keepalive` (0), `-duration` (0 = until interrupted),
`-log-drops`, `-format` (text), `-lang`, `-yes`. `serve`: `-addr` (:9000), `-max-conns` (10000), `-idle-timeout`
(2m), `-tcp-keepalive` (0), `-log-connections`. Zero or negative means disabled
or unlimited.

**`-tcp-keepalive` is 0 on purpose** — kernel keepalives would refresh the NAT
mapping and quietly turn every result into "the NAT is fine".

## Limitations

- **TCP only.** No UDP, no STUN, no DNS, no bandwidth measurement.
- **The JSON schema is `v0`.** It can still change between builds; pin a build
  if you parse it, and don't parse the text.
- **The exit code doesn't judge your network.** `0` means the run completed,
  whatever the statuses. `1` means a check obtained no valid measurement (status
  `ERROR` — for example, the node was unreachable) or the tool failed. `2` means
  nothing ran: invalid flags, or the confirmation was declined or missing. To
  gate on network quality, read the JSON.
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
What I added: a single `connverifier` binary with tests, RTT percentiles, drop attribution,
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
