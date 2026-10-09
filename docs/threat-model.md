# Threat model

What ConnVerifier protects, against whom, and how. The wire details are in
[protocol.md](protocol.md).

## Assets

| Asset | Why it matters |
|---|---|
| Third parties | Neither a node nor a client may be turned against anyone else |
| Node availability and host resources | CPU, descriptors, bandwidth and the operator's traffic allowance |
| The node operator's address and reputation | A node used as a reflector draws abuse complaints |
| The user's own network | A stress check can briefly cut off other devices on it |
| Credentials | Invite tokens and the node's private key |
| Privacy | A node necessarily sees the client's public address; local results hold node and client addresses |
| Measurement integrity | A result must not be made to look better or worse than the path was |

## Adversaries

| | Adversary | Capabilities |
|---|---|---|
| A1 | Anyone on the internet | Connects to the node port, scans, holds connections open, sends malformed data |
| A2 | An invite holder | Uses the node within the invite's limits, carelessly or maliciously |
| A3 | Someone with a leaked invite | Like A2, without the operator knowing |
| A4 | An on-path party | Observes, delays, drops, injects or alters unencrypted traffic |
| A5 | A malicious node | Answers a client that was given its invite with anything at all |

## Threats and mitigations

| Threat | Adversary | Mitigation |
|---|---|---|
| The node is used to reflect or amplify traffic | A1 | The node forwards nothing, fetches nothing and never connects anywhere on a client's behalf. Its STAMP reflector answers only packets that authenticate under a live session's key, from that session's first source address and within its granted rate, and the reply is the size of the request |
| Unauthenticated connections exhaust the node | A1 | 10-second budget to finish TLS or the HELLO; per-address and total caps on such connections and on open control connections; per-address request rate; hard limits on frame, body and header sizes |
| An invite holder takes more than their share | A2, A3 | Per-invite limits on sessions, live connections, dial rate and session length; a node-wide connection and session cap; refusals say which limit was hit |
| An invite holder spends the node's traffic allowance | A2, A3 | The load endpoints send only to a live session with a load grant, within a per-session byte budget and a per-invite daily one; one load session at a time by default. The daily count is kept in memory: restarting the node resets it |
| A load session's extra connections exhaust the node | A2 | The extra allowance is the session's granted load connections, only from the address that created it, and ends with the session |
| An invite leaks | A3 | The operator revokes it (`invite revoke`); revocation applies to the next session. Clients never write invites into results; results carry the label only |
| A client talks to an impostor | A4 | TLS 1.3 with the invite's key pin; on mismatch the client stops before sending the token |
| A data-plane ticket is replayed | A4 | A ticket is bound to a session with limits and an expiry, so a replay spends the same session's allowance. Data-plane traffic itself is not integrity-protected: an on-path party can already drop and delay it, which is what is being measured |
| A STAMP packet is replayed | A4 | The HMAC does not prevent replay. A replayed packet from the session's address is answered and counted as a duplicate; from any other address it is dropped. An on-path party can already drop and delay the same packets |
| A node misleads or attacks its client | A5 | Every node reply is bounded in size and range; the protocol has no field that points the client at another address; node-reported numbers are kept apart from the client's measurements; the client's own limits and deadlines apply regardless of what the node claims, including its own traffic limit per load phase; all decoders are fuzz-tested |
| A node's logs expose its users | A2 | Aggregate counts only by default; per-session logging with client addresses is opt-in; tokens are never logged or stored |
| The node key leaks | A1 | On systems with Unix permissions the node refuses a state directory, key or invite file that others can read; a new key invalidates every invite |
| A node holds a client hostage | A5 | Every read and write has a deadline; the run has a time budget; cancellation releases every connection and goroutine |

## Out of scope

- A compromised client or node host.
- Hiding the client's public address from the node: impossible, and stated.
- Volumetric denial of service against the node's host.
- Results gathered while an on-path party actively tampers with the path: the
  result describes the path as it was, and cannot prove it was not tampered with.
