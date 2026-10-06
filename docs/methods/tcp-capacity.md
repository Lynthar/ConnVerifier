# `tcp-capacity` — long-lived TCP connections

Method version **2**.

## 1. Question

Can this client hold a given number of idle TCP connections to one node, and how
do the connections that drop end — silently, closed by the far side, or with an
error?

## 2. Out of scope

- **Where a drop happened.** A silent drop may be a home router, a carrier-grade
  NAT, another middlebox or the node; the check cannot tell them apart.
- **Node-side rejection versus network drop.** When the node is at its connection
  limit it closes connections right after accepting them; the client sees the same
  thing a network reset would produce.
- **The exact NAT idle timeout.** The heartbeat interval is fixed for the run, so the
  result says whether connections survive *that* interval, not the largest interval
  they would survive.
- **Throughput, packet loss or latency under load.** The heartbeat is four bytes.
- **Mixed address families.** The path's family is taken from the first connection;
  a host name that resolves to both IPv4 and IPv6 may mix them, and the check does
  not separate them.

## 3. Procedure

- Protocol: plain TCP to the node's echo service; the node returns every byte it
  receives.
- The client keeps a fixed pool of `target_connections` slots. Each slot dials,
  holds the connection, and redials after a drop. New dials are capped at
  `start_rate` per second.
- Each connection sends a 4-byte heartbeat (`PING`) immediately after connecting,
  then every `heartbeat_ms`. A heartbeat succeeds when exactly those four bytes come
  back within `io_timeout_ms`.
- Each slot carries a backoff that starts at `min_backoff_ms`. A failed dial, or a
  drop before any interval heartbeat was acknowledged, doubles it up to
  `max_backoff_ms`; a drop after one resets it to `min_backoff_ms`. The slot then
  waits a time drawn uniformly from half to all of the backoff before redialing.
- TCP keepalive is off on the client and on the node unless `tcp_keepalive_ms` is
  set: keepalive probes would refresh NAT state and hide the timeout under test.
- The run lasts `duration_ms`, or until interrupted.

## 4. Definitions

- **Connect:** a dial that completed the TCP handshake.
- **Dial error:** a dial that failed for a reason other than the run ending.
  `dial_errors.host_resource` counts the ones where this host ran out of file
  descriptors, ephemeral ports or socket buffers.
- **Drop:** an established connection that broke before the run ended. Each drop
  falls in exactly one bucket, so `drops` is their sum:
  - `drops.timeout` — the heartbeat reply did not arrive within `io_timeout_ms`;
  - `drops.closed` — the far side closed the connection;
  - `drops.error` — any other error, such as a reset;
  - `drops.bad_ack` — the reply was not the four bytes sent.
- **Interval heartbeat:** any heartbeat after the first on a connection. A connection
  with one acknowledged interval heartbeat has survived at least one full
  `heartbeat_ms` of idleness.
- **Echo round-trip time (`echo_rtt`):** from the start of the heartbeat write to the
  end of the four-byte read, measured in the client process. It includes scheduling
  on both hosts and is not a network-layer round-trip time.
- **Dropped session lifetime:** from connect to the moment the drop was noticed.

## 5. Validity

- A run that ends before any connection completes, with no dial errors, carries no
  data and is `INVALID`.
- Drops are only noticed when a heartbeat is due, so lifetimes are upper bounds by
  up to one `heartbeat_ms`, and the number of live connections reads high in between.
- If the node's idle timeout is not longer than `heartbeat_ms`, the node itself
  closes every connection and the result describes the node, not the network.
- Before the run the client reads its open-file limit (`host.fd_limit`) and its
  ephemeral port range (`host.ephemeral_ports`) where the platform reports them;
  an unreported value is absent from the result, not zero. If the open-file limit
  is below `target_connections` + 32, or the port range below `target_connections`,
  or any dial fails for lack of host resources, the result is `INVALID`: it shows
  the limit of the machine running the tool, not of the network. The Go runtime
  raises the soft open-file limit to the hard limit at startup, so only a lower
  hard limit (`ulimit -Hn`) constrains a run.

## 6. Influences

- **Client host:** each connection needs a file descriptor, and one source address
  has a limited number of ephemeral ports per destination address and port (about
  28,000 on Linux and 16,000 on macOS and Windows by default). Reaching either
  limit shows up as dial errors.
- **Node:** its connection limit and idle timeout, as above.
- **Middleboxes:** NAT and firewall state timeouts are what the check exists to
  expose; some middleboxes inject resets or close connections instead of dropping
  them silently, which moves drops from `drops.timeout` to `drops.closed` or
  `drops.error`.

## 7. Statistics

- Counts are exact totals over the run.
- `echo_rtt` quantiles come from a histogram with buckets about 10% apart from
  10 µs to 600 s; a quantile is reported as the upper bound of its bucket, so it
  can overstate the true value by up to about 10%. Quantiles use the nearest-rank
  definition.
- A quantile *q* is only reported when at least one sample lies above it:
  `samples × (1 − q) ≥ 1`, so p50 needs 2 samples, p95 needs 20 and p99 needs 100.
  Below that the metric is marked `insufficient` and carries no value.
- `dropped_session.mean` is the arithmetic mean over all drops and inherits the
  upper-bound bias above.

## 8. Data

The client sends only heartbeat bytes to the node. The result contains the node
address as given, the parameters, the counts and the timings; it is printed and
not sent anywhere.

## 9. Verification

No controlled-environment accuracy test exists for this method yet. The status
rules and metric construction are covered by unit tests.

## Status rules

Rules 1 to 3 decide the status on their own, in order. Otherwise rules 4 and 5 are
each checked, the status is `WARN` if either holds, and both contribute their
messages; if neither holds the status is `PASS`.

| # | Condition | Status | Messages |
|---|---|---|---|
| 1 | `connects` = 0 and `dial_errors` > 0 | `ERROR` | error: no connection could be made — naming the host as the cause when every failed dial was a host resource error |
| 2 | `connects` = 0 and `dial_errors` = 0 | `INVALID` | warning: the run ended before any connection completed |
| 3 | `dial_errors.host_resource` > 0, or `host.fd_limit` < `target_connections` + 32, or `host.ephemeral_ports` < `target_connections` | `INVALID` | a warning per condition that holds; no inferences, since the counts below reflect the host's limit |
| 4 | `drops` > 0 | `WARN` | an inference per non-empty drop bucket; not proven: node rejection versus network drop; not proven: exact lifetimes |
| 5 | `heartbeats.interval_acked` = 0 | `WARN` | not proven: no connection was seen idle for a full heartbeat interval |
| 6 | otherwise | `PASS` | — |

## Method version changes

The method version increases when the heartbeat payload or timing, the drop
definitions or buckets, the quantile estimator or its sufficiency rule, or the
status rules change.

| Version | Change |
|---|---|
| 2 | Host resource limits make the result `INVALID` (rule 3). |
| 1 | First version. |
