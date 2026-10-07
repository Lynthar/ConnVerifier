# `udp-baseline` — UDP round trip, delay variation and loss

Method version **1**.

## 1. Question

On an idle path from this host to one node, what are the round-trip time and its
variation for UDP packets, and how many are lost — in total and, where it can be
told, on the way to the node and on the way back — late, duplicated or reordered?
For one packet size, one port and one sampling period.

## 2. Out of scope

- **One-way delay.** There is no synchronized clock. Loss is split by direction;
  delay is not.
- **Delay under load**, **other packet sizes and path MTU**, **long-term stability.**
- **Where a packet was lost**: the home router, a carrier NAT, the backbone or the
  node's host cannot be told apart.
- **UDP in general.** Networks may treat UDP differently by port or content; the
  result speaks for this packet type on this port only.
- **What a TCP application sees**: that is `tcp-baseline`.

## 3. Procedure

The wire format is in [../protocol.md](../protocol.md#stamp-udp).

- STAMP (RFC 8762) in authenticated mode with the session-sender identifier of
  RFC 8972. Every packet is 112 bytes; the node answers only packets that
  authenticate under the session's key and replies with the same length.
- The node is a stateful reflector: it numbers the packets it receives itself, so
  the client can tell loss on the way there from loss on the way back
  (RFC 8762 §4). At the end of the session the node reports how many authenticated
  packets it answered.
- The client sends `rate × duration` packets at times drawn uniformly at random
  over `duration` (equivalent to Poisson sampling, RFC 2330 §11.1.1). Defaults:
  50 per second for 20 seconds, 1000 packets.
- Each packet waits **3 s** for its reply (RFC 6673 §3.2); after the last one the
  client waits another 3 s before it stops listening.
- The round-trip time is measured with this host's monotonic clock from sending to
  receiving; the timestamps in the packets are used only for the node's own
  holding time.
- The check runs in the same node session as `tcp-baseline` and at the same time.

## 4. Definitions

- **Sent:** packets that left this host. Packets the operating system refused to
  send are counted in `send_errors` and left out of everything else.
- **Reply in time:** the first authenticated reply with the session's identifier
  that arrives within 3 s of its packet.
- **Lost:** no reply in time (RFC 6673 §4.3). `lost = sent − replies`.
- **Late:** a reply that arrived after its 3 s but before the client stopped
  listening. Late packets are lost (RFC 6673 §4.4) and also counted in `late`.
- **Lost on the way there / back:** with `received` the packets the node answered,
  less those it received twice, `lost.forward = sent − received` and
  `lost.return = received − replies`. A packet that reaches the node more than 3 s
  after it was sent is lost but counted by the node, so it is attributed to the way
  back.
- **Duplicate** (RFC 5560 §3.4): every reply to a packet after the first. Copies
  compared by the sender's and the reflector's sequence numbers. A copy with a new
  reflector number was duplicated on the way there (the node received the packet
  twice); a copy with a number already seen was duplicated on the way back.
- **Reordered** (RFC 4737 §3.3): a reply whose sender sequence number is below the
  next expected one; the next expected number never decreases, and only the first
  copy of a packet counts. **Reordered on the way there:** the same rule applied to
  packets in the order the node received them (its own numbering).
- **Round-trip time (`rtt`)**: from sending to the reply, this host's monotonic
  clock. **Delay variation (`pdv`)**: `rtt − min rtt` (RFC 5481 §4.2).
- **Node holding time (`node_residence`)**: the node's send time minus its receive
  time, both on the node's clock.
- **Send slip:** how long after its scheduled time a packet left.
- A run that is interrupted leaves out the packets whose 3 s had not passed.

## 5. Validity

The result is `INVALID` when something other than the network shaped it:

- more than 1% of packets left more than 10 ms late (this host could not keep up);
- more than 1% of replies show the node holding the packet more than 10 ms;
- the node dropped packets for exceeding the granted rate (`over_rate`), which
  would look like loss on the way there;
- the node received authenticated packets from a different source address of this
  host (`other_addr`): a NAT changed the mapping during the run.

The check does not start, and is `INVALID`, when the node grants fewer STAMP
packets per second than the check sends, or a session too short for the run. The
client asks for twice its sending rate, so random bunching stays within the grant. This host's CPU load and other
traffic are not checked; every result says so.

## 6. Influences

- **This host:** Wi-Fi power saving adds delay to small, sparse packets; operating
  system scheduling enters the round-trip time. On Windows, Go 1.23 and later sleep
  with 0.5 ms resolution, which affects only the sending schedule.
- **The node:** its scheduling and load enter the round-trip time; its holding time
  is reported separately.
- **Middleboxes:** port- or content-based treatment of UDP, NAT mapping timeouts,
  rate limiting and queueing.

## 7. Statistics

- Counts are exact totals.
- Quantiles of `rtt`, `pdv` and `node_residence` are exact nearest-rank quantiles of
  the raw samples. A quantile at *q* is reported only when `samples × (1 − q) ≥ 1`
  (p50 needs 2 samples, p95 20, p99 100, p99.9 1000); otherwise it is marked
  `insufficient` and carries no value.
- `pdv.p99_9` is RFC 5481's pseudo-range (§6.5); with fewer than 1000 samples read
  `pdv.p99`. At the default of 1000 packets it appears only when none was lost.
- `loss`, `loss.forward`, `loss.return`, `duplication` and `reordering` are
  percentages with a 95% Wilson score interval (`low`, `high`). The denominators
  are packets sent, sent, answered by the node, replies and replies.
- Times are measured in nanoseconds and reported to 0.001 ms.

## 8. Data

The client sends the node STAMP packets carrying sequence numbers and this host's
wall-clock time (STAMP requires NTP-format timestamps). The node sees this host's
UDP source address, as it sees the TCP one. The result holds the parameters,
counts, quantiles, intervals and what the node reported; it does not hold the raw
samples. It is printed and not sent anywhere.

## 9. Verification

Accuracy is tested between two Linux network namespaces joined by a veth pair, with
`netem` on each side's egress: the node and this tool run as separate processes,
and each result is read from the JSON output (`internal/accuracy`, build tag
`accuracy`, needs root; not part of the commit gate). "50 ms" below is 25 ms each
way. Because `netem` adds jitter of its own that depends on the host, delay is
judged against ICMP `ping` running beside the check over the same seconds.

| # | Injected | Pass condition | Last result |
|---|---|---|---|
| S1 | 50 ms | In each of 10 runs: minimum within 0.5 ms above ping's, p50 within 0.5 ms of ping's, p99 at most 1.5 ms above ping's | 10/10: minimum +0.07 to +0.12 ms, p50 within 0.12 ms, p99 never above ping's |
| S2 | 50 ms, 1% loss on the way to the node, 1500 packets | 1% inside the reported `loss.forward` interval in at least 17 of 20 runs; `lost.return` 0 in every run | 20/20; `lost.return` 0 in all |
| S3 | 50 ms, 1% loss on the way back | Mirror of S2 | 20/20; `lost.forward` 0 in all |
| S4 | 50 ms, 1% duplication on the way to the node | 1% inside the `duplication` interval in at least 17 of 20 runs; no duplicate attributed to the way back | 20/20; none on the way back |
| S5 | 25 ms ± 10 ms on the way to the node | Reordering on the way to the node detected; PDV p99 above 1 ms | 130 reordered; PDV p99 23.9 ms |
| S6 | This host limited to 1% of a CPU | `INVALID` for send slip | `INVALID`, send slip |
| S7 | Nothing | `PASS`; no loss, duplication or reordering | `PASS` |

Last run: 2026-10-07, Linux 6.8 (Ubuntu 24.04, arm64 virtual machine), Go 1.27.1.
Late replies are covered by unit tests with constructed timings; netem cannot place
a reply after a chosen waiting time.

## Status rules

Before the run:

| # | Condition | Status |
|---|---|---|
| 0a | The node cannot be reached, presents a key other than the invite's, or refuses the session | `ERROR`, naming the cause |
| 0b | The node or the invite is at its session limit | `ERROR`, with the node's message and retry hint |
| 0c | The invite names no UDP port, or the node grants no STAMP rate | `UNSUPPORTED` |
| 0d | The node grants fewer STAMP packets per second than the check sends, or a session shorter than the run | `INVALID`; the check does not run |

After the run, rules 1 and 2 decide the status on their own, in order. Otherwise
rule 3 makes it `WARN` with an inference per kind observed, and if it does not hold
the status is `PASS`.

| # | Condition | Status |
|---|---|---|
| 1 | No packet sent; or no reply at all | `ERROR` if nothing was sent, or the node received nothing (UDP did not reach it — the path or the node's firewall, which cannot be told apart), or the node did not report; `WARN` if the node received packets but no reply came back (UDP blocked on the way back) |
| 2 | A validity condition of section 5 | `INVALID`, a warning per condition |
| 3 | `lost` > 0, a duplicate, or a reordered reply | `WARN` |
| 4 | otherwise | `PASS` |

A run interrupted before the check sent anything is `INVALID`, not `ERROR`.
Every result notes that host load was not checked. A missing node report, loss in
both directions, or send errors add a note without changing the status.

## Method version changes

The method version increases when the packet format or size, the sampling, the
waiting time, any definition above, the estimators or their sufficiency rule, the
validity conditions or the status rules change.

| Version | Change |
|---|---|
| 1 | First version. |
