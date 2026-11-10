# `dns` — DNS resolver round trip and answers

Method version **1**.

## 1. Question

Does each DNS resolver this host is configured to use answer within a set time,
and how long does it take? One result per resolver. It needs no node.

## 2. Out of scope

- **Whether answers are right**: hijacking and poisoning are not looked for.
- **DNS over HTTPS or TLS**, and resolvers built into browsers.
- **How long a full recursive lookup takes**: the query is one every resolver has
  cached, so what is timed is the way to the resolver and its answer, not its
  work upstream.

## 3. Procedure

- **The resolvers** are those the system uses, in its order: on Linux the
  `nameserver` lines of `/etc/resolv.conf`, or, when systemd-resolved's stub
  `127.0.0.53` is among them, those of `/run/systemd/resolve/resolv.conf` instead —
  asking the stub would time its cache; on macOS the nameservers of the default
  resolver in `scutil --dns`; on Windows the DNS servers of every adapter that is up,
  without the `fec0:0:0:ffff::1`–`3` placeholders Windows lists when none is set.
- **The query** is the root zone's NS records (`. NS IN`), recursion desired, with an
  EDNS0 record offering 1232 bytes. It names nothing about this host, and every
  resolver already holds the answer: it needs it to resolve anything at all.
- **20 queries** per resolver, one every 100 ms (10 a second), each with its own
  random ID, sent straight to the resolver over UDP port 53 without waiting for
  answers. Each answer is matched to its query by ID and question. Each query waits
  **2 s**; after the last one the check waits out that query's 2 s, so a resolver
  that never answers adds 2 s, not 20 × 2 s.
- Resolvers are asked one after another, IPv4 ones first.

## 4. Definitions

- **Sent:** queries sent, including those the system refused to send (no route, or a
  "port unreachable" reported for an earlier one).
- **Answer:** a response with the query's ID and question that arrives within 2 s.
  **Error answer:** an answer whose response code is not NOERROR — it counts as an
  answer, and separately in `error_replies`.
- **Lost:** `sent − replies`. A query still within its 2 s when the run is
  interrupted is left out.
- **Round-trip time (`rtt`)**, over answers only, and **delay variation (`pdv`)** as in
  [udp-baseline](udp-baseline.md#4-definitions).

## 5. Validity

When the route to a resolver leaves by a tunnel interface, the result notes that a
proxy in TUN mode takes over all UDP port 53, so the answers may be the proxy's own.
A resolver in `198.18.0.0/15` is usually a proxy's fake-ip resolver, and one on
`loopback` a forwarder on this host: their round trips say little about the network.

## 6. Influences

- **The resolver:** its load and rate limits; a resolver that drops repeated queries
  for the same name looks like loss.
- **This host:** as for `udp-baseline`.
- **Middleboxes:** anything on the path that intercepts port 53 answers in the
  resolver's place.

## 7. Statistics

- Counts are exact totals.
- Quantiles and their sufficiency rule are those of
  [udp-baseline](udp-baseline.md#7-statistics); 20 answers suffice for p95.
- `loss` is a percentage with a 95% Wilson score interval over the queries sent.

## 8. Data

Queries go only to the configured resolvers, which see this host's address and a
query for the root zone. The result holds the family, the resolver's place in the
host's list and the class of its address (as for [gateway](gateway.md#8-data)),
never the address. It is printed and not sent anywhere.

## 9. Verification

No controlled-environment accuracy test exists for this method yet.

## Status rules

Before the queries:

| # | Condition | Status |
|---|---|---|
| 0a | No resolver of this family is configured | `SKIP`, one for the family |
| 0b | The DNS configuration cannot be read | `UNSUPPORTED`, one per family |

For each resolver, after its queries:

| # | Condition | Status |
|---|---|---|
| 1 | A query not answered within 2 s, or an error answer | `WARN`, with an inference per kind |
| 2 | otherwise | `PASS` |

A resolver that answers nothing at all is `WARN`, not `ERROR`: no answer is a valid
measurement. A run interrupted before any query is `INVALID`. Delay has no
threshold.

## Method version changes

The method version increases when the query, the number of queries, their rate or
waiting time, how resolvers are found, any definition above, the estimators or the
status rules change.

| Version | Change |
|---|---|
| 1 | First version. |
