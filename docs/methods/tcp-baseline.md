# `tcp-baseline` — TCP round trip and delay variation

Method version **1**.

## 1. Question

On an idle path, inside one established TCP connection to a node, what are the
round-trip time of an application-level echo and its variation — the delay an
application running over TCP experiences?

## 2. Out of scope

- **Loss, reordering and duplication.** TCP retransmits and reorders before the
  application sees anything; a lost segment shows up only as some echoes coming back
  late (the retransmission timeout plus head-of-line blocking), in the upper
  quantiles. The check reports no loss rate.
- **Connection setup time**, **one-way delay**, **delay under load.**

## 3. Procedure

The wire format is in [../protocol.md](../protocol.md#data-plane).

- The session grants one data-plane connection; after its HELLO is accepted the
  client sends PING frames and the node answers each with a PONG carrying the same
  sequence number.
- PINGs follow the same sampling as `udp-baseline` — `rate × duration` at times
  drawn uniformly at random over `duration`, by default 1000 over 20 seconds — in
  the same session and at the same time. They are pipelined: a PING never waits for
  the previous PONG.
- Nagle's algorithm is off and TCP keepalive is off.
- Each PING waits 3 s for its PONG; after the last one the client waits another 3 s.

## 4. Definitions

- **Echo round-trip time (`rtt`)**: from writing a PING to reading its PONG, this
  host's monotonic clock. It includes scheduling on both hosts; it is not a
  network-layer round-trip time. **Delay variation (`pdv`)**: `rtt − min rtt`.
- **Stall:** a PING whose PONG did not arrive within 3 s.
- **Break:** the connection closed or failed during the run; the check ends there
  and keeps the samples so far.
- **Send slip:** as in `udp-baseline`.

## 5. Validity

`INVALID` when more than 1% of PINGs left more than 10 ms late, or (before the run)
when the node granted a session too short for it. The node's own handling time is
not separated from the round trip, and host load is not checked; every result says
both.

## 6. Influences

As for `udp-baseline`. In addition, a transparent TCP proxy or optimizer on the
path answers in the node's place, so the echo measures the distance to it.

## 7. Statistics

As for `udp-baseline` for `rtt` and `pdv`; stalls are an exact count.

## 8. Data

The client sends PING frames carrying sequence numbers. The result holds the
parameters, counts, quantiles and what the node reported, not raw samples.

## 9. Verification

Tested with `udp-baseline` in the same runs; see that document's table. In S1 the
TCP echo met the same conditions against ping (10/10: minimum +0.08 to +0.13 ms,
p50 within 0.07 ms, p99 at most +0.13 ms). With 1% loss injected (S2, S3) the check
stayed `PASS` with p99 mostly 100–135 ms against a 50 ms path: lost segments show up
as retransmission delay, as section 2 says.

## Status rules

Before the run, rules 0a, 0b and 0d of `udp-baseline` apply. A connection the node
does not admit follows `tcp-capacity` rule 1: refused for the node's own limits is
`INVALID`; a refused ticket, a failed dial or no answer to the ticket is `ERROR`.

A run interrupted before the connection was admitted is `INVALID`, not `ERROR`.

After the run, rules 1 and 2 decide the status on their own, in order. Otherwise
rule 3 makes it `WARN`, and if it does not hold the status is `PASS`.

| # | Condition | Status |
|---|---|---|
| 1 | The connection broke before any echo came back | `ERROR` |
| 2 | More than 1% of PINGs left more than 10 ms late | `INVALID` |
| 3 | The connection broke during the run, or `stalls` > 0 | `WARN`, an inference each |
| 4 | otherwise | `PASS` |

## Method version changes

As for `udp-baseline`.

| Version | Change |
|---|---|
| 1 | First version. |
