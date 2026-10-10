# `quic-load` — goodput and round trips with the path loaded over QUIC

Method version **1**.

This is the [`tcp-load`](tcp-load.md) method with the load and the responsiveness
probes carried by HTTP/3 over QUIC instead of HTTP/2 over TCP. Only what differs
is written here; everything else — phases, parameters, algorithm, independent
probes, validity — is as in `tcp-load`, section by section.

## 1. Question

From this host to one node, when QUIC (HTTP/3) loads the path until it is full: how
much goodput do download, upload and both directions together reach, and how far
do UDP and TCP round trips and UDP loss — measured exactly as at rest — rise? Read
beside the `tcp-load` of the same run, it is the bandwidth row of "TCP and UDP side
by side".

## 2. Out of scope

- As for `tcp-load`: the link rate, which side is the bottleneck, long-term
  behaviour.
- **How the network treats UDP in general.** A network that treats traffic by port
  (443 or not) or recognizes QUIC treats this check by those; the node's port is
  usually not 443, so a rate limit that keys on port 443 may not apply.
- **The difference between TCP and UDP as such.** The load uses quic-go's
  congestion control, NewReno, on both ends; the TCP of `tcp-load` uses each
  operating system's (Linux defaults to CUBIC, and a node may run BBR). Without
  any rate limit, two congestion controls can differ several times over a long,
  lossy path. A gap between `quic-load` and `tcp-load` is a gap between QUIC and
  TCP goodput, not proof that the network limits UDP; the node reports its TCP
  congestion control (`tcp_congestion`) so the two can be read together.
- **UDP capacity without congestion control**, which only flooding at a fixed rate
  would show.

## 3. Procedure

The endpoints are those of `tcp-load` ([../protocol.md](../protocol.md#load-endpoints)),
served over HTTP/3 on the node's QUIC port. The check runs after `tcp-load`, never
at the same time, in a session of its own, and first waits for that load's queue
to drain exactly as between phases.

- **Load connections:** up to 16 per direction, each its own QUIC connection (ALPN
  `h3`, TLS 1.3, the node's key checked against the invite's pin) from its own UDP
  socket, carrying one HTTP/3 stream of load. No request carries a `Priority`
  header, so the node interleaves the streams of a connection.
- **Responsiveness probes:** a *foreign* probe opens a new UDP socket and QUIC
  connection, timing the handshake (`quic_f`: from its first Initial until the
  handshake completes, transport and TLS together), then fetches the 1-byte object
  (`http_f`); a *self* probe fetches it on a new stream of a randomly chosen load
  connection (`http_l`).
- A load connection or foreign probe has 15 s for its QUIC handshake, and the node
  waits as long.
- **No send-buffer step:** QUIC's send queue is in the application, held by its
  congestion window and pacing; there is no kernel buffer to cap, so the Windows
  caveat of `tcp-load` does not apply.
- No keepalive, no 0-RTT, no datagrams on any connection.

## 4. Definitions

- **Goodput:** download counts the HTTP/3 body bytes read; upload counts, as in
  `tcp-load`, the body bytes the node reports having read every 50 ms. QUIC packets
  carry varying header and frame overhead, so body bytes are counted rather than
  socket bytes.
- **Traffic (`bytes`):** the body bytes moved in the load's direction.
- **Payload ceiling** of a link of rate *R* (for verification): quic-go's largest
  packet carries 1452 bytes of UDP payload in a 1494-byte Ethernet frame; less about
  7 bytes of short header, a 16-byte authentication tag and about 9 bytes of
  STREAM frame header, `R × 1420/1494 ≈ 0.950 R`.
- **Responsiveness (RPM):** the draft gives no formula for QUIC, whose handshake
  does in one round trip what TCP and TLS do in two. Following its §5.3.1.2, each
  measured part of the foreign probe weighs equally:
  `Foreign = 60000 / ((TM(quic_f) + TM(http_f)) / 2)`, `Loaded = 60000 / TM(http_l)`,
  `RPM = (Foreign + Loaded) / 2`. Other responsiveness tools may not compute it this
  way.
- **UDP buffers** (`host.udp_receive_buffer`, `host.udp_send_buffer`): the smallest
  receive and send buffers any of the check's QUIC sockets got after quic-go asked
  for 7 MiB, as the kernel reports them (Linux doubles what was granted).
- Confidence, lower bounds, round trips under load, lag, connect failures, failed
  foreign probes and wait are as in `tcp-load`.

## 5. Validity

As in `tcp-load`. QUIC costs this host roughly twice the CPU per byte that TCP and
TLS do, so a slow host reaches the lag limit sooner; the limit is the same.
UDP buffers below what quic-go asks for do not make the result invalid: the result
adds a note that a fast or long path may have dropped packets on this host.

## 6. Influences

- **Congestion control:** NewReno in quic-go, at the node for downloads and at this
  host for uploads (section 2).
- **ECN:** quic-go reads ECN marks; where a queue marks instead of dropping, QUIC
  and TCP see congestion differently.
- **UDP buffers:** an unprivileged process on Linux gets 416 KiB by default
  (`net.core.rmem_max`, `net.core.wmem_max`); on a 100 ms, 100 Mbit/s path that
  cost about a fifth of the goodput in one measurement.
- **Port and middleboxes:** networks that block QUIC or large UDP flows, NATs that
  limit UDP mappings, rate limits by port.
- **Streams:** a loss on a load stream does not hold up another stream of the same
  connection, and the send queue is QUIC's own, held by its congestion window. A
  self probe therefore waits only for its connection's share of the bottleneck
  queue, and over QUIC it comes much closer to a foreign probe than over TCP
  (section 9, L5).
- **CPU:** routers reach their limit sooner over QUIC than over TCP.
- Otherwise as in `tcp-load`.

## 7. Statistics

As in `tcp-load`, with `quic_f + http_f` in place of `tcp_f + tls_f + http_f` for
the foreign probes' quantiles.

## 8. Data

As in `tcp-load`. The traffic stated before the run covers both load checks: six
phases of at most `-load-mb` each.

## 9. Verification

The scenarios of `tcp-load` section 9 run with both loads in each check; "C" here
is the QUIC payload ceiling of section 4.

| # | Injected | Pass condition | Last result |
|---|---|---|---|
| L1 | 20 ms round trip, 20 Mbit/s each way, 40 ms queue | In each of 10 runs: download and upload goodput in [0.93 C, 1.005 C], settled; neither direction of both together above its C | 10/10: download 96.4–97.1% of C, upload 94.6–96.1% |
| L2 | As L1 at 100 Mbit/s | As L1; and never this host or the node reported as the limit | 10/10: download 98.6–99.1%, upload 97.0–98.6% |
| L3 | 100 Mbit/s down, 10 Mbit/s up | As L1, 5 runs | 5/5: download 98.5–99.1%, upload 93.3–95.0% |
| L4 | 20 Mbit/s, 200 ms FIFO queue | Independent UDP and TCP p50 under load in [170, 240] ms; RPM below 300 | 3/3: UDP 202–210 ms, TCP 203–213 ms; 280–285 RPM |
| L5 | As L4 with `fq_codel` in the queue | UDP p50 under load at most 15 ms above idle; self probes slower than foreign ones | 3/3: UDP at most idle; self 46–55 ms, foreign 46–47 ms |
| L6 | 1 Gbit/s; this host limited to a tenth of a CPU | `INVALID` for this host, 10 of 10 runs | 10/10; 41–79% of wake-ups late per phase |
| L8 | 100 Mbit/s; the node grants 50 MB | Every phase a lower bound; the node moves at most 50 MB | passed |
| L9 | 100 Mbit/s down; 10 Mbit/s up with a 2 s queue and 1% loss | In each of 3 runs: no phase aborted; upload goodput reported and at most C | 3/3: upload 8.71–8.89 Mbit/s (C 9.50) |
| B1 | The node's QUIC port dropped, STAMP and TCP open | `ERROR` with the inference that QUIC was blocked; `tcp-load` unaffected | passed; `tcp-load` `WARN` |
| B2 | Unprivileged client, Linux default buffers, 100 ms round trip | Buffers reported below 7 MiB with the note; nothing from quic-go on stderr | passed: 416 KiB, the note, a clean stderr; `PASS` |
| C1 | As L2 | QUIC/TCP goodput ratio recorded, no bound: what the two reach without any rate limit | download 0.987–0.992, upload 0.981–0.997 |

Last run: 2026-10-10 and 11, Linux 6.8 (Ubuntu 24.04, arm64 virtual machine), Go
1.27.1, quic-go v0.63.0. In-memory tests check the same plumbing: goodput behind a
shared 32 Mbit/s token bucket settles at 96% of it, the share of the datagrams'
bytes that is body.

## Status rules

Before the run, as in `tcp-load` (0a–0e), and:

| # | Condition | Status |
|---|---|---|
| 0f | The node offers no HTTP/3 load (its grant names no QUIC port) | `UNSUPPORTED` |

After the run, as in `tcp-load`. On rule 1, when not one QUIC handshake completed
while the independent UDP probe to the same node got replies, the result adds the
inference that something on the path may block QUIC, or that the node's QUIC port
is not open. Buffers below what quic-go asks for add a note without changing the
status.

## Method version changes

The method version increases when anything that changes `tcp-load`'s version
changes here, when the quic-go version changes its congestion control, or when a
definition above changes. Each result records the quic-go version (`quic_go`).

| Version | Change |
|---|---|
| 1 | First version. |
