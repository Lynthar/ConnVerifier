# `tcp-capacity` — long-lived TCP connections

Method version **3**.

## 1. Question

Can this client hold a given number of idle TCP connections to one node, and how
do the connections that drop end — silently, closed by the far side, closed by the
node with a stated reason, or with an error?

## 2. Out of scope

- **Where a silent drop happened.** It may be a home router, a carrier-grade NAT,
  another middlebox or the node's host; the check cannot tell them apart.
- **The exact NAT idle timeout.** The heartbeat interval is fixed for the run, so the
  result says whether connections survive *that* interval, not the largest interval
  they would survive.
- **Throughput, packet loss or latency under load.** A heartbeat frame is 11 bytes.
- **Mixed address families.** When the node address is a host name, the path's
  family is taken from the first connection; a name that resolves to both IPv4 and
  IPv6 may mix them, and the check does not separate them.

## 3. Procedure

The wire format is in [../protocol.md](../protocol.md).

- The client asks the node for a session with `target_connections` live
  connections, `start_rate` new connections per second, an idle period of twice a
  heartbeat plus its reply timeout (at least 60 s), and `duration_ms` plus a
  margin. The node grants each up to the invite's limits.
- The client keeps a pool of as many slots as the node granted connections. Each
  slot dials, presents the session ticket, and once admitted holds the connection,
  redialing after a drop. New connections are capped at the smaller of
  `start_rate` and the granted rate.
- Each connection sends a heartbeat immediately after admission, then
  `heartbeat_ms` after each acknowledged one. A heartbeat succeeds when its
  sequence number comes back within `io_timeout_ms`.
- The client reads every connection continuously, so a close, a reset or a CLOSE
  from the node is recorded when it arrives.
- Each slot carries a backoff that starts at `min_backoff_ms`. A failed dial or
  refusal, or a drop before any interval heartbeat was acknowledged, doubles it up
  to `max_backoff_ms`; a drop after one resets it to `min_backoff_ms`. The slot then
  waits a time drawn uniformly from half to all of the backoff, and at least as long
  as the node's retry hint after a refusal.
- TCP keepalive is off on both sides: keepalive probes would refresh NAT state and
  hide the timeout under test.
- The run lasts `duration_ms`, until interrupted, or until 5 s (at most a fifth of
  the session) before the granted session ends — whichever comes first. The client
  then closes its connections and ends the session.

## 4. Definitions

- **Connect:** a connection the node admitted (TCP handshake, then ACCEPT).
- **Dial error:** a dial that failed for a reason other than the run ending.
  `dial_errors.host_resource` counts the ones where this host ran out of file
  descriptors, ephemeral ports or socket buffers.
- **Handshake error:** a connection that was dialed but got no valid answer to its
  ticket within `io_timeout_ms`.
- **Rejection:** a REJECT from the node, by reason: `rejected.busy` (node at its
  connection limit), `rejected.quota` (session limit or dial rate),
  `rejected.auth` (ticket refused), `rejected.other`. A rejection is not a drop.
- **Drop:** an admitted connection that broke before the run ended. Each drop falls
  in exactly one bucket, so `drops` is their sum:
  - `drops.timeout` — a heartbeat reply did not arrive within `io_timeout_ms`;
  - `drops.closed` — the far side closed the connection without a reason;
  - `drops.error` — a reset or any other error;
  - `drops.bad_ack` — the reply did not carry the sequence number sent;
  - `drops.node_closed` — the node sent CLOSE with a reason (idle timeout, session
    ended, shutting down).
- **Interval heartbeat:** any heartbeat after the first on a connection. A
  connection with one acknowledged interval heartbeat has survived at least one full
  `heartbeat_ms` of idleness.
- **Echo round-trip time (`echo_rtt`):** from writing a heartbeat to reading its
  reply, measured in the client process. It includes scheduling on both hosts and is
  not a network-layer round-trip time.
- **Dropped session lifetime:** from admission to the moment the drop was noticed.

## 5. Validity

- Closes, resets and node CLOSEs are noticed when they arrive. A silent drop is
  noticed only when a heartbeat reply times out, so its lifetime is an upper bound by
  up to one `heartbeat_ms` plus `io_timeout_ms`.
- A result is `INVALID` when something other than the network bounded it: the client
  host (any dial failing for lack of host resources, an open-file limit
  `host.fd_limit` below the target + 32, or an ephemeral port range
  `host.ephemeral_ports` below the target) or the node (any `busy` or `quota`
  rejection, or fewer connections granted than asked for). Host values the platform
  does not report are absent, not zero. The Go runtime raises the soft open-file
  limit to the hard limit at startup, so only a lower hard limit (`ulimit -Hn`)
  constrains a run.
- If the node grants an idle period shorter than a heartbeat plus its reply
  timeout, it would close every connection itself; the run does not start.

## 6. Influences

- **Client host:** each connection needs a file descriptor, and one source address
  has a limited number of ephemeral ports per destination address and port (about
  28,000 on Linux and 16,000 on macOS and Windows by default).
- **Node:** its connection limit and the invite's limits; both are reported.
- **Middleboxes:** NAT and firewall state timeouts are what the check exists to
  expose; some middleboxes inject resets or closes instead of dropping silently,
  which moves drops from `drops.timeout` to `drops.closed` or `drops.error`.

## 7. Statistics

- Counts are exact totals over the run.
- `echo_rtt` quantiles come from a histogram with buckets about 10% apart from
  10 µs to 600 s; a quantile is reported as the upper bound of its bucket, so it
  can overstate the true value by up to about 10%. Quantiles use the nearest-rank
  definition.
- A quantile *q* is only reported when at least one sample lies above it:
  `samples × (1 − q) ≥ 1`, so p50 needs 2 samples, p95 needs 20 and p99 needs 100.
  Below that the metric is marked `insufficient` and carries no value.
- `dropped_session.mean` is the arithmetic mean over all drops.

## 8. Data

The client sends the node its token over TLS, its requested limits, and heartbeat
frames. The result contains the node address and label (never the invite), the
parameters, the counts and timings, and in a separate `node` object what the node
reported: its version, the client address it saw, what it granted and its load at
the start and end. The result is printed and not sent anywhere.

## 9. Verification

No controlled-environment accuracy test exists for this method yet. The status
rules, metric construction and the node interaction are covered by unit tests and by
in-memory and loopback runs against a real node.

## Status rules

Before any connection is made:

| # | Condition | Status |
|---|---|---|
| 0a | The node cannot be reached, presents a key other than the invite's, or refuses the session (`auth`, `version`, `bad_request`) | `ERROR`, naming the cause |
| 0b | The node refuses the session because it or the invite is at its limit (`busy`, `quota`) | `ERROR`, with the node's message and retry hint |
| 0c | The granted idle period is shorter than `heartbeat_ms` + `io_timeout_ms` | `INVALID`; the run does not start |

After the run, rules 1 and 2 decide the status on their own, in order. Otherwise
each of rules 3 to 5 that holds makes the status `WARN` and adds its messages; if
none holds the status is `PASS`.

| # | Condition | Status | Messages |
|---|---|---|---|
| 1 | No connection admitted | `ERROR` if tickets were rejected, dials failed (naming the host as the cause when every failure was a host resource error) or no handshake finished; otherwise `INVALID` if the node refused for `busy` or `quota`, else `INVALID` because the run ended first | the cause |
| 2 | A bound outside the network: a host resource dial error, `host.fd_limit` < target + 32, `host.ephemeral_ports` < target, any `rejected.busy` or `rejected.quota`, or fewer connections granted than asked | `INVALID` | a warning per condition that holds |
| 3 | `drops` > 0 | `WARN` | an inference per non-empty drop bucket; not proven: exact lifetimes, when `drops.timeout` > 0 |
| 4 | The node granted a lower dial rate, or its session length ended the run | `WARN` | a warning each |
| 5 | `heartbeats.interval_acked` = 0 | `WARN` | not proven: no connection was seen idle for a full heartbeat interval |
| 6 | otherwise | `PASS` | — |

Handshake errors on some connections add a not-proven note without changing the
status.

## Method version changes

The method version increases when the heartbeat payload or timing, the drop
definitions or buckets, the quantile estimator or its sufficiency rule, or the
status rules change.

| Version | Change |
|---|---|
| 3 | Runs against a node session (protocol v2): rejections and node closes are counted apart from drops; closes are detected on arrival; node limits make the result `INVALID`. |
| 2 | Host resource limits make the result `INVALID`. |
| 1 | First version. |
