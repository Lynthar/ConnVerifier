# `quic-load` — goodput and round trips with the path loaded over QUIC

Method version **4**.

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
- **Sender loss (`sender_loss`):** packets quic-go declared lost over packets
  sent, summed over a direction's load connections, on any system: by this host at
  the end of the phase for an upload, by the node as each download ends for a
  download, reported at the end of the session. A packet declared lost may still
  have arrived: QUIC declares one lost once a packet sent three later is
  acknowledged, or 9/8 of a round trip after a later one was (RFC 9002 §6.1).
- Confidence, lower bounds, round trips under load, lag, connect failures, failed
  foreign probes and wait are as in `tcp-load`.

## 5. Validity

As in `tcp-load`. QUIC costs this host roughly twice the CPU per byte that TCP and
TLS do, so a slow host reaches the lag limit sooner; the limit is the same.
UDP buffers below what quic-go asks for do not make the result invalid: the result
adds a note that a fast or long path may have dropped packets on this host.

**Something held QUIC that did not hold TCP** in the download or the upload phase
when, in the same check, `tcp-load` measured that phase too, and over the same path

- QUIC's goodput is below 0.7 of TCP's, and
- QUIC's sender loss is below TCP's or more than 5 times it. A TCP that resent
  nothing counts as having resent one segment: its loss is below that share, not
  known to be zero.

A path's capacity holds QUIC back as it does TCP, and a flow it holds meets its
losses. Not always the same losses: behind a queue that marks ECN instead of
dropping, TCP asked for ECN and resent nothing while QUIC met drops (section 9,
E4); there only the goodput condition kept the rule from applying.
Behind deep, shallow, flow-queued and marking queues and a policer, no download or
upload met both conditions, nor an upload behind random loss.

Both at once is not judged on its own. Each direction's acknowledgements then wait
behind the other direction's load, and how far that holds a flow differs between
the protocols. Behind a 10 Mbit/s uplink with a 2 s queue (section 9, L9 and L10),
TCP's download with both at once reached 0.15–0.74 of its download by itself and
QUIC's 6–36 Mbit/s, in 5 of 6 runs losing nothing: judged on its own, it met both
conditions in 5 of the 6 runs.
A direction held by itself is taken as held with both at once too: its goodput
there is a lower bound.

When both conditions are met, what held QUIC is QUIC at one end — the sender's
CPU or congestion control, a network adapter that reorders packets across its
queues, which makes QUIC declare packets lost that arrived, or a receiving host that
drops UDP before it reaches the socket — or the path treating UDP apart, such as a
rate or burst limit for UDP alone; which one is not determined. That direction's
goodput is a lower bound. Losses 1 to 5 times apart are not judged: the path may
have changed between the two loads, or QUIC's congestion control may fare worse on
the same losses. Neither is a QUIC that lost no more than 5 times one segment's
share where TCP resent nothing: no loss tells them apart.
The rule is not applied without sender loss on both sides (an older node reports
none for downloads); that, and a phase it could not judge, each add a note.

**A queue apart from the path's** in the download or the upload phase, when
`tcp-load` in the same check measured that phase and both loads' UDP probe has
an idle round trip to rise from:

- QUIC's goodput is below 0.7 of TCP's, and
- the UDP probe's p50 rose at least 3 times as much under QUIC's load as under
  TCP's, and by at least 10 ms more. A rise below idle counts as none.

A lighter load meeting a deeper queue does not come from the path's shared
bottleneck. The queue QUIC met is at one end — this host or the node — or where
the path holds UDP apart, and that phase's round trips under load say more about
that queue than about the path. Which one is not determined; the result adds a
note and the status does not change. With the bottleneck on the path, in virtual
machines and between servers, no phase met it; a Mac receiving QUIC faster than
it can (section 6) met it in most phases.

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
- **This host's receive path on macOS:** macOS hands received UDP to the sockets
  one packet at a time, and with a socket per load connection the kernel caps QUIC
  whatever the CPU has left: near 1.4 Gbit/s on the Mac measured, lower on slower
  ones. Above that the load queues in the
  kernel ahead of every socket: the goodput is the Mac's limit, the round trips
  under load include up to tens of milliseconds of that queue, and a connection
  added in an interval fills it until the kernel drops thousands of packets at
  once, which QUIC counts as sender loss. Sharing sockets among connections did
  not lift the cap. TCP is not affected the same way.
- Otherwise as in `tcp-load`.

## 7. Statistics

As in `tcp-load`, with `quic_f + http_f` in place of `tcp_f + tls_f + http_f` for
the foreign probes' quantiles.

## 8. Data

As in `tcp-load`. The traffic stated before the run covers both load checks: eight
phase directions of at most `-load-mb` each.

## 9. Verification

The scenarios of `tcp-load` section 9 run with both loads in each check; "C" here
is the QUIC payload ceiling of section 4.

| # | Injected | Pass condition | Last result |
|---|---|---|---|
| L1 | 20 ms round trip, 20 Mbit/s each way, 40 ms queue | In each of 10 runs: download and upload goodput in [0.93 C, 1.005 C], settled; neither direction of both together above its C | 10/10: download 96.2–97.0% of C, upload 94.1–95.5% |
| L2 | As L1 at 100 Mbit/s | As L1; and never this host or the node reported as the limit | 10/10: download 98.7–99.1%, upload 98.1–98.7% |
| L3 | 100 Mbit/s down, 10 Mbit/s up | As L1, 5 runs | 5/5: download 99.0–99.3%, upload 93.5–94.2% |
| L4 | 20 Mbit/s, 200 ms FIFO queue | Independent UDP and TCP p50 under load in [170, 240] ms; RPM below 300 | 3/3: UDP 191–213 ms, TCP 202–213 ms; 277–287 RPM |
| L5 | As L4 with `fq_codel` in the queue | UDP p50 under load at most 15 ms above idle; self and foreign probes recorded, no bound (section 6) | 3/3: UDP at most 0.2 ms above idle; self 44–55 ms, foreign 44 ms |
| L6 | 1 Gbit/s; this host limited to a tenth of a CPU | `WARN` with this host the possible limit and every goodput a lower bound, 10 of 10 runs | 10/10; 31–76% of wake-ups late per phase |
| L8 | 100 Mbit/s; the node grants 50 MB | Every phase a lower bound; the node moves at most 50 MB | passed |
| L9 | 100 Mbit/s down; 10 Mbit/s up with a 2 s queue and 1% loss | In each of 3 runs: no phase aborted; upload goodput reported and at most C; nothing inferred held QUIC and not TCP | 3/3: upload 8.78–8.86 Mbit/s (C 9.50), none inferred held; with both at once TCP's download 37–70 Mbit/s, QUIC's 27–36 |
| L10 | As L9 without the loss | In each of 3 runs: upload goodput reported and at most C; below 0.93 C, not of `high` confidence; nothing inferred held QUIC and not TCP | 3/3: upload 8.95–8.97 Mbit/s (C 9.50), `high`, none inferred held; with both at once TCP's download 14–23 Mbit/s, QUIC's 6.3–6.8 |
| B1 | The node's QUIC port dropped, STAMP and TCP open | `ERROR` with the inference that QUIC was blocked; `tcp-load` unaffected | passed; `tcp-load` `WARN` |
| B2 | Unprivileged client, Linux default buffers, 100 ms round trip | Buffers reported below 7 MiB with the note; nothing from quic-go on stderr | passed: 416 KiB, the note, a clean stderr; `PASS` |
| C1 | As L2 | QUIC/TCP goodput ratio recorded, no bound: what the two reach without any rate limit | download 0.988–0.992, upload 0.984–0.993 |
| E1 | 300 Mbit/s each way, 20 ms round trip, 200 ms queue; this host limited to a tenth of a CPU | In each of 3 runs: `tcp-load`'s upload at least 0.93 of its C; `quic-load` infers something held QUIC and not TCP, its upload a lower bound | 3/3: `tcp-load` upload 98.5–99.7% of C; QUIC upload ≥ 78–116 Mbit/s, sender loss 0.02–0.07% against TCP's 0.14–0.22%; QUIC's download 0.73–0.98 of TCP's, not inferred held |
| E2 | 300 Mbit/s each way, 20 ms round trip: a policer with 5 ms of burst; a token bucket with 2 ms of queue | In none of 3 runs each does `quic-load` infer, in any direction, something held QUIC and not TCP | 6/6 none; sender loss QUIC 1.5–2.4% and TCP 1.5–3.0% behind the policer, QUIC 0.55–0.83% and TCP 0.32–0.78% behind the token bucket |
| E3 | 100 Mbit/s each way, 40 ms round trip, 100 ms queue under `fq_codel`, which marks ECN, TCP asking for ECN; 5% of UDP dropped at random at either end | In each of 3 runs: `quic-load` infers in the download and the upload phase something held QUIC and not TCP; every QUIC goodput a lower bound | 3/3: TCP resent nothing of 75 000–85 000 segments in any direction; QUIC 0.48–0.55 of TCP's goodput by itself, 0.34–0.47 with both at once, sender loss 4.5–5.1% |
| E4 | As E3 without the drops | In none of 3 runs does `quic-load` infer, in any direction, something held QUIC and not TCP | 3/3 none: TCP resent at most one of 72 000–85 000 segments, QUIC 0.99–1.00 of TCP's goodput with sender loss 0.16–0.21% |

Last run: 2026-11-02–03, Linux 6.8 (Ubuntu 24.04, arm64 virtual machine), Go
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
is not open. Rule 3 also holds when something held QUIC that did not hold TCP in the
download or the upload phase (section 5).
Buffers below what quic-go asks for, a phase the rule could not judge, and a
queue apart from the path's (section 5), add a note without changing the status.

## Method version changes

The method version increases when anything that changes `tcp-load`'s version
changes here, when the quic-go version changes its congestion control, or when a
definition above changes. Each result records the quic-go version (`quic_go`).

| Version | Change |
|---|---|
| 1 | First version. |
| 2 | As `tcp-load` version 2, and: an upload that something held apart from TCP (section 5) is a lower bound with `WARN`; sender loss is reported. |
| 3 | As `tcp-load` version 3. |
| 4 | As `tcp-load` version 4, and: downloads are judged by the rule of section 5 too, from the node's sender loss; a TCP that resent nothing counts as having resent one segment instead of leaving the direction unjudged; both at once is not judged on its own, a direction held by itself bounds its goodput there too. |
