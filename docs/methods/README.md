# Measurement methods

Every check ConnVerifier reports has a method document here. A result names its
check `id` and `method_version`; the document below with that id is the one that
version follows.

| Check id | Method version | Document |
|---|---|---|
| `tcp-capacity` | 3 | [tcp-capacity.md](tcp-capacity.md) |
| `gateway` | 1 | [gateway.md](gateway.md) |
| `udp-baseline` | 1 | [udp-baseline.md](udp-baseline.md) |
| `tcp-baseline` | 1 | [tcp-baseline.md](tcp-baseline.md) |
| `tcp-load` | 4 | [tcp-load.md](tcp-load.md) |
| `quic-load` | 4 | [quic-load.md](quic-load.md) |

## What each method document answers

1. **Question.** What the check tries to answer.
2. **Out of scope.** What it explicitly cannot answer.
3. **Procedure.** Protocol, ports, payload sizes, timing, sample size and sampling.
4. **Definitions.** What counts as a timeout, a loss, a late, duplicate or reordered
   reply — whichever apply.
5. **Validity.** Which conditions degrade the result or make it invalid.
6. **Influences.** How the client host, the node and middleboxes can shape the result.
7. **Statistics.** Estimators, resolution, error and how confidence is expressed.
8. **Data.** What is collected, sent and stored.
9. **Verification.** How conformance to this document is shown under controlled
   network conditions, and its current state.

Each document also carries the check's **status rules** — the exact conditions for
`PASS`, `WARN`, `FAIL`, `SKIP`, `UNSUPPORTED`, `ERROR` and `INVALID` — and the
changes that require a new **method version**.

## Statuses

| Status | Meaning |
|---|---|
| `PASS` | The check completed validly and met its stated condition. |
| `WARN` | The check completed validly and observed something worth attention. |
| `FAIL` | The check completed validly and did not meet a condition the user set. |
| `SKIP` | Not selected, or a prerequisite is missing. |
| `UNSUPPORTED` | The platform, node or protocol cannot run this check. |
| `ERROR` | No valid measurement was obtained: the tool, protocol or node failed. |
| `INVALID` | Data exists, but conditions make it unfit for judging the network. |

A poor network is never reported as `ERROR`, and a tool failure is never reported
as a poor network.

## This host

Before the checks, a result names the kind of interface the route to the node
leaves by, once for each address family the checks ran on (`host.interfaces` in the
JSON). When the run ends, the tool asks the kernel for that route the way a
connection would get it — a UDP socket connected to the node's address, which sends
nothing — and classifies the interface holding the source address it chose:

| Kind | Linux | macOS | Windows |
|---|---|---|---|
| `wifi` | `phy80211` or `wireless` in sysfs, or `DEVTYPE=wlan` | functional type Wi-Fi | `IfType` 71 |
| `wired` | Ethernet backed by a device | functional type wired | `IfType` 6 |
| `tunnel` | tun or tap, WireGuard, SIT, IP-in-IP and GRE over IPv4 or IPv6 | `utun`, `ipsec`, `gif`, `stf` | `IfType` 131, or 53 (Wintun) |
| `ppp` | link type PPP | `ppp` | `IfType` 23 |
| `other` | anything else: bridges, veth, VLANs, loopback | anything else | anything else |
| `unknown` | the interface or its type could not be read | the same | the same |

Over Wi-Fi, the wireless hop's delay and loss are part of every result. Over a
tunnel, what is measured is the tunnel's path, not the bare connection. A PPP link
is broadband dial-up or a VPN, which the host cannot tell apart. None of this
changes a status. Interface names, addresses, MAC addresses and SSIDs are never
recorded.
