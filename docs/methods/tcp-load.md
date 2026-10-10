# `tcp-load` — goodput and round trips with the path loaded

Method version **1**.

## 1. Question

From this host to one node, when TCP (HTTP/2 over TLS) loads the path until it is
full: how much goodput do download, upload and both directions together reach, and
how far do UDP and TCP round trips and UDP loss — measured exactly as at rest —
rise? And what is the responsiveness in the terms of
`draft-ietf-ippm-responsiveness-09`?

## 2. Out of scope

- **Link rate.** Goodput is application payload; it is below the line rate by the
  headers of every layer below (section 4).
- **Which side is the bottleneck**: this host's connection, the networks between, or
  the node's own uplink. A node that cannot keep up is detected (section 5); the
  capacity of its network is not. Several nodes tell more.
- **UDP bandwidth.** The load is TCP only; [`quic-load`](quic-load.md) runs the same
  method over QUIC right after it.
- **Other congestion control.** The load uses this host's TCP congestion control,
  which differs between Linux, macOS and Windows; the result names the platform.
- **Long-term behaviour** such as evening slowdowns: each phase lasts seconds.

## 3. Procedure

The endpoints are in [../protocol.md](../protocol.md#load-endpoints). The check
runs after the idle checks `udp-baseline` and `tcp-baseline`, never at the same
time, in a session of its own.

- **Phases:** download, then upload, then both at once. Between phases all load
  connections close and the check waits for the queue to drain: until the median
  round trip of the independent probes over the last second is within 10 ms or 20%
  of its idle value, whichever is more — at least 2 s, at most 10 s.
- **Load** (draft §7): download is a `GET` of an object the node sends until the
  session's budget is spent, with `Accept-Encoding: identity`; upload is a `POST`
  the node reads and discards; both at once opens both kinds. Each load connection
  is its own TCP connection with TLS 1.3, carrying one HTTP/2 stream of load.
- **Parameters** (draft §5.2, its defaults in brackets):

  | Parameter | Value |
  |---|---|
  | Interval (ID) | 1 s [5 s] — `-load-interval` |
  | Moving-average distance (MAD) | 4 intervals [4] |
  | Standard-deviation tolerance (SDT) | 5% [5%] |
  | Trimmed mean keeps (TMP) | best 95% [95%] |
  | Initial and added connections (INP, INC) | 1 and 1 per direction [1, 1] |
  | Load connections per direction (MNP) | 16 [16] |
  | Probe pairs per second (MPS) | 20 [100] |
  | Share of goodput for probes (PTC) | 5% [5%] |
  | Longest phase | 20 s — `-load-time` |
  | Most traffic per phase | 500 MB — `-load-mb`; more needs confirming |

- **Algorithm** (draft §5.4): each interval one more load connection opens per
  direction, up to 16. While goodput has not settled, the moving average over the
  last four intervals is computed; goodput has settled when the standard deviation
  of the last four such averages is below 5% of the latest. From then on the
  responsiveness of each interval is computed over the probes of the last four
  intervals, and it settles by the same test. A phase ends when responsiveness
  settles, at its time or traffic limit, when the node's grant runs out, or when a
  load connection that carried its stream fails. A load connection that does not
  open — TCP and TLS handshake and the response or upload beginning, within 15 s —
  is counted and the phase goes on with the others; if none opens, the phase fails.
  Under a loaded, bloated queue a handshake can take seconds, and that is a finding
  about the path rather than a failure of the measurement.
- **Independent probes:** the STAMP stream of `udp-baseline` and the echo stream of
  `tcp-baseline` run unchanged — same packets, same Poisson sampling, same rate —
  for the whole load stage, on one socket each, and are split by phase.
- **Responsiveness probes** (draft §5.3): a *foreign* probe opens a new TCP and TLS
  connection and fetches a 1-byte object, timing the TCP handshake (`tcp_f`), the TLS
  handshake (`tls_f`, one round trip in TLS 1.3) and the request (`http_f`); a
  *self* probe fetches the same object on a randomly chosen load connection
  (`http_l`), without priority. The two alternate, evenly spaced, at most 20 pairs a
  second and at most 5% of the latest goodput (about 6000 bytes a pair). A foreign
  probe has 15 s to finish; one that does not is counted and left out.
- **Send buffers** (draft §6.1.1): on Linux and macOS this host's upload connections
  and the node's download connections keep at most 5 ms of their throughput unsent
  (`TCP_NOTSENT_LOWAT`), reset every interval. Windows has no such option.

## 4. Definitions

- **Goodput** is counted where the bytes arrive, per second. Download: bytes read
  from the load connections' sockets, less TLS record overhead (16384 payload bytes
  in 16406); HTTP/2 frame headers, under 0.06%, are not taken off. Upload: the body
  bytes the node reports having read, every 50 ms on each upload's response.
  Counting anywhere else is wrong: above the socket HTTP/2 holds a download before
  the application reads it, and bytes written on the sending side include what is
  still in flight, which grows with a deep queue (by 6% on a 10 Mbit/s link with a
  2 s queue). A phase's goodput is the latest moving average; before four intervals
  have run, the mean so far.
- **Traffic (`bytes`):** bytes on this host's sockets in the load's direction,
  without the deduction.
- **Payload ceiling** of a link of rate *R* (for verification): Ethernet frames of
  1514 bytes carry 1448 bytes of TCP payload with timestamps, so
  `R × 1448/1514 × 16384/16406 ≈ 0.955 R`.
- **Confidence** (draft §5.4.1), for goodput and for responsiveness: `low` when
  fewer than four intervals of that stage ran, `medium` when they ran but it did not
  settle, `high` when it settled. Responsiveness has no intervals before goodput
  settles.
- **Lower bound (`at_least`):** a goodput whose confidence is not `high` is
  reported as "≥": the run stopped before it could show the path carries no more.
- **Round trips under load:** independent probes sent from the moment goodput
  settled to the end of the phase; if it never settled, from four intervals before
  the end. Defined exactly as in `udp-baseline` and `tcp-baseline`; `…increase` is
  the loaded quantile minus the idle one of the same check run just before.
- **Responsiveness (RPM)** (draft §5.3.1.1) over the probes completed in the last
  four intervals: `Foreign = 60000 / ((TM(tcp_f) + TM(tls_f) + TM(http_f)) / 3)`,
  `Loaded = 60000 / TM(http_l)`, `RPM = (Foreign + Loaded) / 2`, where TM is the mean
  of the best 95% in milliseconds. Below 300 RPM the draft calls it poor.
- **Lag:** a timer set every 10 ms on this host, and another on the node, counts the
  wake-ups more than 10 ms late.
- **Connect failures (`connect_failures`):** load connections that did not open.
  **Failed foreign probes (`foreign.failed`):** foreign probes that did not finish;
  they are left out of RPM, so with any of them RPM is better than the path was.
  **Wait (`wait`):** how long the check waited before the phase for the queue to
  drain.

## 5. Validity

The result is `INVALID` when something other than the network shaped it:

- **this host fell behind** in a phase: more than 5% of its timer's wake-ups were
  more than 10 ms late, or the process used at least all but half a core of the CPU
  it may use (`GOMAXPROCS`, which follows a container's quota). The timer catches a
  host throttled by a quota or by other processes, where CPU use itself stays low;
- **the node fell behind**: more than 5% of its timer's wake-ups during the session
  were more than 10 ms late;
- **the grant held the load**: a phase could not add connections because of the
  node's grant, and its goodput did not settle;
- the independent probes left more than 1% of their packets over 10 ms late.

The 5% limit is set between what section 9 measured: up to 1% late wake-ups
unthrottled, 11% and more in every phase with a tenth of a core (L6).
Other traffic on this host or its network is not checked; every result says so.

## 6. Influences

- **This host:** Wi-Fi rates vary with distance and interference; on Windows the
  kernel's send buffer adds to upload round trips, as no unsent limit can be set;
  TCP congestion control and receive-window tuning are the operating system's; TLS
  costs CPU, more so on a router.
- **The node:** its uplink and network card; other services on its host.
- **Middleboxes:** transparent TCP proxies, rate limits by port or content;
  half-duplex links, where both directions together reach less than their sum —
  itself worth knowing (draft §5.1.2).

## 7. Statistics

- Goodput: one value per interval and direction; the phase value as in section 4,
  in Mbit/s to four decimals.
- Independent probes: as in `udp-baseline` and `tcp-baseline` — nearest-rank
  quantiles of raw samples, `insufficient` below the sample rule, UDP loss with a
  95% Wilson interval.
- Responsiveness probes: RPM to a whole number; p50 and p95 of `http_l` and of
  `tcp_f + tls_f + http_f`, over probes completed after goodput settled.
- Derived values are rounded to four decimals.

## 8. Data

This host sends the node load data — zeros, holding nothing of this host — and
probe requests. The node sees this host's address and traffic pattern. Before the
run the tool states how much traffic it may move: three phases of at most
`-load-mb` each, and as many again for `quic-load`. The result holds the parameters, per-phase values and counts, and
what the node reported (bytes it sent and received, its timer); not the raw samples.
It is printed and not sent anywhere.

## 9. Verification

Between two Linux network namespaces as for `udp-baseline`, with `netem` adding
delay and, after it, `tbf` limiting the rate; segmentation offload is off on the
veth pair. "C" is the payload ceiling of section 4; the floor of 0.93 C leaves the
probes their share — up to 5% of goodput, and about 0.1 Mbit/s for the independent
probes. Run with the default parameters and a 10 s idle stage.

| # | Injected | Pass condition | Last result |
|---|---|---|---|
| L1 | 20 ms round trip, 20 Mbit/s each way, 40 ms queue | In each of 10 runs: download and upload goodput in [0.93 C, 1.005 C], settled; neither direction of both together above its C | 10/10: download 96.9–97.5% of C, upload 96.1–97.4% |
| L2 | As L1 at 100 Mbit/s | As L1; and never this host or the node reported as the limit (also L7) | 10/10: download 99.4%, upload 98.0–99.1%; never this host or the node as the limit |
| L3 | 100 Mbit/s down, 10 Mbit/s up | As L1, 5 runs | 5/5: download 98.7–99.4%, upload 95.0–95.7% |
| L4 | 20 Mbit/s, 200 ms FIFO queue | Independent UDP and TCP p50 under load in [170, 240] ms; RPM below 300 | 3/3: UDP 212–221 ms, TCP 213–220 ms; 164–199 RPM |
| L5 | As L4 with `fq_codel` in the queue | UDP p50 under load at most 15 ms above idle; self probes slower than foreign ones | 3/3: UDP at most idle; self 262–730 ms, foreign 69–72 ms |
| L6 | 1 Gbit/s; this host limited to a tenth of a CPU | `INVALID` for this host, 10 of 10 runs | 10/10; 45–91% of wake-ups late per phase |
| L8 | 100 Mbit/s; the node grants 50 MB | Every phase a lower bound; the node moves at most 50 MB | passed |
| L9 | 100 Mbit/s down; 10 Mbit/s up with a 2 s queue and 1% loss | In each of 3 runs: no phase aborted; upload goodput reported and at most C | not met: no phase aborted in 9 runs, but 4 uploads above C — 9.63–9.80 Mbit/s against 9.55, up to 2.6%; the others 8.65–9.31 |

Last run: 2026-10-10 and 11, Linux 6.8 (Ubuntu 24.04, arm64 virtual machine), Go 1.27.1.
On L9 the upload estimate can exceed C: after a loss, the bytes held behind the
retransmission reach the node together a 2 s round trip later, and a four-second
moving average can take in such a batch.
In-memory tests check the same plumbing: goodput behind a shared 32 Mbit/s token
bucket settles within 3% of it.

## Status rules

Before the run:

| # | Condition | Status |
|---|---|---|
| 0a | The node cannot be reached, presents a key other than the invite's, or refuses the session | `ERROR`, naming the cause |
| 0b | The node or the invite is at its session limit | `ERROR`, with the node's message and retry hint |
| 0c | The node offers no load, or the invite allows none | `UNSUPPORTED` |
| 0d | Another session is loading the node, or the invite has used its load traffic for the day | `ERROR`, with the retry hint |
| 0e | The node grants a session shorter than the phases need | `INVALID`; the check does not run |

After the run, rules 0–2 decide the status on their own, in order. Otherwise rule
3 makes it `WARN` with an inference per finding, and if it does not hold the status
is `PASS`.

| # | Condition | Status |
|---|---|---|
| 0 | The run was interrupted before any phase | `INVALID` |
| 1 | Every phase stopped because a load connection failed | `ERROR`, with the first error |
| 2 | A validity condition of section 5 | `INVALID`, a warning per condition |
| 3 | A phase stopped by a failure or an interruption; load connections that did not open; goodput not settled; responsiveness not settled; or RPM below 300 | `WARN` |
| 4 | otherwise | `PASS` |

Every result notes that the bottleneck's side and other traffic were not
determined. A missing independent probe, failed foreign probes, or a queue that had
not drained before a phase add a note without changing the status.

## Method version changes

The method version increases when the draft version, a parameter of section 3, the
phases, a definition above, the estimators, the validity conditions or the status
rules change.

| Version | Change |
|---|---|
| 1 | First version. |
