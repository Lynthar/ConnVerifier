# ConnVerifier

[![license](https://img.shields.io/github/license/Lynthar/ConnVerifier)](LICENSE)

A Go tool that holds thousands of idle TCP connections to find where NAT drops them, with RTT percentiles

English | [简体中文](README.zh-CN.md)

> **Under construction.** It does what's described below and the tests pass, but
> there's no release yet — you build it from source.

Carriers and home routers quietly forget idle TCP connections. This finds out
when. You run a **node** on a server you control and hand out **invites** to it;
`connverifier capacity` opens as many connections to the node as you ask for,
keeps them alive with a small heartbeat, and reports how many survived and for
how long — each disconnect attributed to a silent timeout, a close, a reset, or
the node closing it with a stated reason. When the node is full it says so, and
the result says the node, not your network, set the limit.

`connverifier check` measures the same path's round-trip time and its variation
over UDP and TCP side by side, and UDP loss — split into the way to the node and
the way back, without synchronized clocks.

One binary, one dependency (`golang.org/x/term`).

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

## Usage

On the server, start a node and create an invite for each person who will test
against it. The node listens on one TCP port (7443 by default) for everything,
and answers UDP probes on the same port number — open both in the firewall:

```bash
ulimit -n 20480
./bin/connverifier serve -max-conns 20000
./bin/connverifier invite create -label alice -addr <server-ip>:7443 > alice.invite
```

The first `serve` or `invite` creates the node's key under the user's config
directory (`-state-dir` to change it). `invite list` shows the invites and
`invite revoke -label alice` withdraws one.

On the machine whose network you want to test, give `capacity` the invite — from a
file or the `CONNVERIFIER_NODE` variable keeps it out of your shell history. It
first says how many connections it will open and asks for confirmation, because
filling the NAT table can cut off other devices on the same network; `-yes` skips
the question and is required when stdin is not a terminal:

```bash
ulimit -n 20480
./bin/connverifier capacity -node @alice.invite -clients 10000 -start-rate 500 -heartbeat 30s
./bin/connverifier capacity -node @alice.invite -clients 1000 -duration 1h -yes
```

`check` needs no confirmation: it sends 50 small packets a second over UDP and
over TCP for 20 seconds (`-rate`, `-duration`). An invite made before UDP probes
existed has no UDP port; the UDP half then reports `UNSUPPORTED`.

```bash
./bin/connverifier check -node @alice.invite
```

```
UDP round trip and loss: attention (WARN)
Node: 192.0.2.10:7443 · IPv4 · UDP
Observed
  Probes sent: 1000
  Lost (no reply in time): 21
  Loss: 2.1% (95% interval 1.38–3.19%, samples: 1000)
  Lost on the way to the node: 10
  Lost on the way back: 11
  Round trip p50: 34 ms (979 samples)
Inferred
  - Probes without a reply in time: 21, of which replies that came late: 1.
```

While `capacity` runs, a progress line goes to stderr once a second. When it stops, the
result goes to stdout — as text, or as JSON with `-format json`. Shortened:

```
TCP long-lived connections: attention (WARN)
Node: 192.0.2.10:7443 · IPv4 · TCP
Reported by the node
  Name: tokyo-test
  Granted: connections 50 · new per second 100 · length 10.3 min · idle 2 min
Observed
  Connections made: 73
  Drops: 31
  Drops (no reply in time): 3
  Drops (closed by the node, with a reason): 25
  Echo round trip p95: 309 µs (40 samples)
  Echo round trip p99: too few samples (40)
Inferred
  - Connections that stopped answering without a close or reset: 3. This is the
    usual sign of a NAT or other middlebox silently dropping connection state.
  - Connections the node closed and said why: 25 (idle_timeout=25). These are not
    evidence of network drops.
```

The text is in Chinese or English, following `LC_ALL`, `LC_MESSAGES` or `LANG`;
any other language gets Chinese, and `-lang en` or `-lang zh-CN` overrides it.
What each status and number means, and when not to trust it, is in
[docs/methods](docs/methods/README.md); the node protocol is in
[docs/protocol.md](docs/protocol.md).

Everything is a flag. `check`: `-node`, `-rate` (50), `-duration` (20s),
`-dial-timeout` (5s), `-format`, `-lang`. `capacity`: `-node`, `-clients` (1000), `-start-rate` (100),
`-heartbeat` (30s), `-dial-timeout` (5s), `-io-timeout` (5s), `-min-backoff`
(500ms), `-max-backoff` (1m), `-duration` (0 = until interrupted), `-log-drops`,
`-format` (text), `-lang`, `-yes`. `serve`: `-listen` (:7443), `-listen-udp` (the
`-listen` address), `-state-dir`, `-max-conns` (20000), `-max-sessions` (64),
`-log-connections`. `invite create`: `-label`, `-addr` (repeatable),
`-udp-port` (the first `-addr`'s port), `-max-sessions` (2), `-max-connections`
(20000), `-max-dial-rate` (1000), `-max-stamp-rate` (100), `-max-duration` (24h),
`-max-idle` (1h).

Neither side enables TCP keepalive: keepalive probes would refresh the NAT
mapping and quietly turn every result into "the NAT is fine".

## Limitations

- **Idle path only.** No bandwidth or delay under load, no STUN, no DNS, one UDP
  packet size; delay is round trip only, never one way.
- **The JSON schema is `v0`.** It can still change between builds; pin a build
  if you parse it, and don't parse the text.
- **The exit code doesn't judge your network.** `0` means the run completed,
  whatever the statuses. `1` means a check obtained no valid measurement (status
  `ERROR` — for example, the node was unreachable) or the tool failed. `2` means
  nothing ran: invalid flags, or the confirmation was declined or missing. To
  gate on network quality, read the JSON.
- **Silent drops are found on the heartbeat.** Closes and resets are seen the
  moment they arrive, but a connection a NAT forgets silently is only noticed when
  a heartbeat goes unanswered, so those lifetimes are upper bounds.
- **Host limits bound the result.** One connection is one file descriptor, and one
  source address has about 28,000 ephemeral ports (16,000 on macOS and Windows)
  per destination. If the client's limit is below the target, or dials fail for
  lack of either, the result is marked `INVALID` rather than reported as a network
  limit.

## Differences from upstream

This is a fork of [codeberg.org/woq/ConnVerifier](https://codeberg.org/woq/ConnVerifier).
What I added: a single `connverifier` binary with tests, an authenticated node
protocol with invites and per-invite limits, RTT percentiles, drop attribution,
reconnect with exponential backoff and jitter, and `-duration` for unattended
soak runs.

## Security

- **An invite works like a password.** Anyone holding it can use the node within
  that invite's limits. Send it privately; revoke it with `invite revoke`.
- **The node only accepts invite holders.** Connections that don't finish TLS or a
  valid ticket within 10 seconds are dropped, and each address may hold only a few
  of those at once. The node never connects anywhere on a client's behalf, and
  answers only UDP probes signed with a live session's key, no larger than they
  came.
- **Clients check the node's key.** An invite pins the node's public key; a node
  presenting any other key is refused before the token is sent.
- **The node sees your public address** — any server you connect to does. Results
  record it locally; they never contain the invite.

The full threat model is in [docs/threat-model.md](docs/threat-model.md).

## License

MIT — see [LICENSE](LICENSE). The copyright line reads "2025 woq", the upstream
author.
