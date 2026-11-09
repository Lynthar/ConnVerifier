# `gateway` — default gateway round trip and loss

Method version **1**.

## 1. Question

From this host to the gateway of its default route, for each address family, what
is the round-trip time and how many probes go unanswered? It needs no node.

## 2. Out of scope

- **The path beyond the gateway.**
- **Whether the gateway is the broadband connection's own end**: behind a router
  and a modem, or several routers, it is only the first of them.
- **The gateway's address.** Results name only its class (section 8).

## 3. Procedure

- **The gateway** is the next hop of the family's default route, the one with the
  lowest metric when there are several — on Linux from `/proc/net/route` and
  `/proc/net/ipv6_route`, on macOS from the kernel's routing table (the unscoped
  default route), on Windows from the forwarding table, ranked by route metric plus
  interface metric. A link-local IPv6 gateway is reached through its interface.
- **ICMP first**: 100 echo requests of 56 data bytes, one every 50 ms (20 a second),
  each waiting **900 ms** for its reply, over an unprivileged ICMP socket. Linux
  grants one only to groups listed in `net.ipv4.ping_group_range`; Windows has none,
  so there the check goes straight to TCP.
- **TCP when ICMP is unavailable or gets no answer at all**: a connection to port
  53, then 80, then 443, one attempt each, until one is answered. A SYN-ACK and a
  reset both answer. The first answer is the first sample; 99 more connections to
  that port follow at the same rate, each closed once established. The handshake
  time is the round trip.
- The families run one after the other, IPv4 first.

## 4. Definitions

- **Sent:** probes sent. For TCP, counting starts at the port's first answer:
  before it, a lost probe and a filtered port cannot be told apart.
- **Reply:** an echo reply with the probe's sequence number from the gateway, or an
  answered handshake, within 900 ms. The TCP wait has to end before the first SYN
  retransmission (1 s), or that second would enter the round trip; ICMP waits as
  long, so both methods count loss alike.
- **Lost:** `sent − replies`. A probe still waiting when the run is interrupted is
  left out, not lost.
- **Round-trip time (`rtt`)** and **delay variation (`pdv`)** as in
  [udp-baseline](udp-baseline.md#4-definitions), on this host's monotonic clock.

## 5. Validity

A handshake is not an echo: it includes the gateway's connection handling, so a
TCP round trip does not compare directly with an ICMP one, and the result says
which method and port it used.

## 6. Influences

- **The gateway:** it answers ICMP with its own CPU and drops these packets first
  when busy, so loss and delay here need not be on the forwarding path. Rate limits
  on ICMP replies look like loss.
- **This host:** Wi-Fi, power saving and scheduling enter the round trip, as for
  `udp-baseline`.
- **A tunnel or proxy** that takes the default route measures its own next hop.

## 7. Statistics

- Counts are exact totals.
- Quantiles and their sufficiency rule are those of
  [udp-baseline](udp-baseline.md#7-statistics); 100 samples suffice up to p99.
- `loss` is a percentage with a 95% Wilson score interval over the probes sent.

## 8. Data

Probes go to the gateway only. The result holds the family, the method, the TCP
port when one was used, and the class of the gateway's address — `loopback`,
`link_local`, `private` (RFC 1918 and IPv6 unique local), `shared` (RFC 6598),
`benchmarking` (198.18.0.0/15, where proxy tools put fake-ip addresses), `public` or
`other` — never the address itself. It is printed and not sent anywhere.

## 9. Verification

No controlled-environment accuracy test exists for this method yet.

## Status rules

Before probing:

| # | Condition | Status |
|---|---|---|
| 0a | No default route for this family | `SKIP` |
| 0b | The default route has no gateway: it leaves straight by an interface, as on a point-to-point link or a tunnel | `SKIP` |
| 0c | The routing table cannot be read | `UNSUPPORTED` |

After probing, the first rule that holds decides:

| # | Condition | Status |
|---|---|---|
| 1 | Neither ICMP nor a handshake on port 53, 80 or 443 was answered | `UNSUPPORTED`, saying what was tried — not "gateway unreachable", as many gateways answer none of these |
| 2 | `lost` > 0 | `WARN` |
| 3 | otherwise | `PASS` |

A run interrupted before any answer is `INVALID`. A TCP result notes that its
round trips are handshakes. Delay has no threshold.

## Method version changes

The method version increases when the probes, their number, rate or waiting time,
the choice of method or port, any definition above, the estimators or the status
rules change.

| Version | Change |
|---|---|
| 1 | First version. |
