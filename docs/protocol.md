# Node protocol, version 2

A node serves clients on **one TCP port**, and answers STAMP on a UDP port — by
default the same number. The first byte a client sends on TCP decides what the
connection is:

| First byte | Connection |
|---|---|
| `0x16` (TLS handshake record) | HTTPS control plane: TLS 1.3, ALPN `h2` or `http/1.1` |
| `0x43` (`C`, start of the magic `CVD2`) | Data plane: one long-lived measured connection |
| anything else | closed without a reply |

The TLS handshake or the data-plane HELLO must finish within 10 seconds of the
connection being accepted. Neither side enables TCP keepalive.

## Node identity

A node has an Ed25519 key and a self-signed certificate for it. Its identity is
the SHA-256 of the certificate's SubjectPublicKeyInfo — the **pin**. A client
completes a TLS handshake only with the certificate whose key matches the pin in
its invite; CA chains and host names are not used.

## Invite

```text
cvi1_<base64url without padding of the JSON below>
{"l": "<label>", "a": ["<host:port>", …], "p": "<pin>", "t": "<token>", "u": <port>}
```

| Field | Content |
|---|---|
| `l` | Label chosen by the node operator, 1–64 bytes of printable UTF-8 |
| `a` | 1–4 addresses, `host:port`, host an IP literal or a DNS name |
| `p` | Pin, 32 bytes, base64url |
| `t` | Access token, 32 random bytes, base64url |
| `u` | Optional. UDP port of the node's STAMP reflector, on the hosts of `a`; absent when the node offers none |

An invite is at most 2048 characters. Unknown JSON fields are ignored. The node
stores only the SHA-256 of each token, with the invite's label and limits.

## Control plane

Every request carries `Authorization: Bearer <token>`. Bodies are JSON of at most
4096 bytes; headers at most 8 KiB.

### `POST /v2/sessions`

```json
{
  "client": {"name": "connverifier", "version": "…"},
  "check": "tcp-capacity",
  "want": {"connections": 10000, "dial_rate": 500, "duration_s": 3600, "idle_timeout_s": 120}
}
```

`check` is `tcp-capacity` or `baseline`. A `baseline` session may also ask for
`stamp_rate`, STAMP packets per second; a node without a reflector, or a
`tcp-capacity` session, is granted none.

`duration_s` 0 asks for as long as the invite allows. The node grants each value
up to the invite's limit: `connections`, `dial_rate` and `idle_timeout_s` are the
smaller of asked and allowed; `duration_s` is the smaller of asked and allowed, or
the allowed maximum when 0 was asked.

`201 Created`:

```json
{
  "session_id": "<16 bytes, base64url>",
  "secret": "<32 bytes, base64url>",
  "expires_at": "2026-01-01T00:00:00Z",
  "observed_addr": "198.51.100.7:51234",
  "granted": {"connections": 5000, "dial_rate": 500, "duration_s": 3600, "idle_timeout_s": 120},
  "node": {"version": "…", "load": {"sessions": 1, "connections": 0, "max_connections": 20000, "uptime_s": 86400}}
}
```

`observed_addr` is the client's address as the node saw it on this request, or
empty when unknown. A session granted a STAMP rate also carries
`"stamp": {"ssid": N}`, its non-zero STAMP session-sender identifier. Connection
limits are on **live** connections, not on how many a session opens over its
lifetime.

### `DELETE /v2/sessions/{session_id}`

Ends a session created with the same token. Its data-plane connections receive
`CLOSE(session_ended)`. `200 OK` returns `{"node": {…}}` with the load afterwards,
and for a session that had a STAMP grant,
`"stamp": {"received": N, "over_rate": N, "other_addr": N}`: the authenticated
packets the reflector answered, and those it dropped for exceeding the granted
rate or for coming from another source address.

### Refusals

Every refusal is `{"reason": "…", "retry_after_s": N, "message": "…"}`; `message` is
at most 256 bytes and only explains.

| Status | `reason` | Meaning |
|---|---|---|
| 400 | `bad_request` | Body malformed or out of range, or unsupported check |
| 401 | `auth` | Token unknown or revoked |
| 404 | `bad_request` | No such session for this token |
| 429 | `quota` | The invite has its maximum number of sessions, or too many requests from this address |
| 503 | `busy` | The node has its maximum number of sessions, or is shutting down |

## Data plane

All integers are big-endian. The client opens with a fixed 56-byte HELLO:

| Offset | Size | Field |
|---|---|---|
| 0 | 4 | Magic `CVD2` |
| 4 | 1 | Version, `1` |
| 5 | 1 | Type `0x01` |
| 6 | 2 | Reserved, `0` |
| 8 | 16 | Session ID |
| 24 | 16 | Nonce, random per connection |
| 40 | 16 | First 16 bytes of HMAC-SHA256(secret, `"CVD2 hello"` ‖ version ‖ session ID ‖ nonce) |

After the HELLO both sides send frames: type (1 byte), payload length (2 bytes),
payload. Each type has exactly one payload length; any other type or length closes
the connection.

| Type | Direction | Payload | Meaning |
|---|---|---|---|
| `0x02` ACCEPT | node → client | idle timeout ms (4) | Admitted |
| `0x03` REJECT | node → client | reason (1), retry after s (2) | Refused; the node closes |
| `0x10` PING | client → node | sequence (8) | Heartbeat |
| `0x11` PONG | node → client | sequence (8) | Echo of the PING's sequence |
| `0x20` PROBE | node → client | sequence (8) | Node-initiated probe |
| `0x21` PROBE_ACK | client → node | sequence (8) | Answer to PROBE |
| `0x30` CLOSE | either | reason (1), retry after s (2) | Closing, with the reason |

| Reason | Name | Sent when |
|---|---|---|
| 1 | `busy` | The node has its maximum of live connections |
| 2 | `quota` | The session has its granted live connections, or exceeds its granted dial rate |
| 3 | `auth` | Unknown or expired session, or a HELLO whose MAC does not verify |
| 4 | `version` | The HELLO's version is not supported |
| 5 | `idle_timeout` | Nothing arrived for the granted idle period |
| 6 | `session_ended` | The session expired or was ended |
| 7 | `shutting_down` | The node is stopping |

A client must not count a REJECT or a CLOSE as a network failure: the node said
why. The node sends nothing before the HELLO and nothing unprompted afterwards
except CLOSE and PROBE.

## STAMP (UDP)

The node is a stateful STAMP reflector (RFC 8762) in authenticated mode with the
session-sender identifier of RFC 8972. Sender and reflector packets are both 112
bytes and carry no TLVs:

| Offset | Size | Sender packet | Reflector packet |
|---|---|---|---|
| 0 | 4 | Sequence number, from 0 | The reflector's own sequence number, from 0 per session |
| 16 | 8 | Timestamp T1 | Timestamp T3, just before sending |
| 24 | 2 | Error estimate | Error estimate |
| 26 | 2 | SSID | SSID |
| 32 | 8 | — | Receive timestamp T2 |
| 48 | 4 | — | The sender's sequence number |
| 64 | 8 | — | The sender's timestamp |
| 72 | 2 | — | The sender's error estimate |
| 80 | 1 | — | The sender's TTL, `0` (not read) |
| 96 | 16 | HMAC | HMAC |

All other bytes are zero. Timestamps are NTPv4 64-bit; the error estimate is
unsynchronized, NTP format, one second. The HMAC is HMAC-SHA-256 over bytes 0–95,
truncated to 16 bytes, keyed with HKDF-SHA256 of the session secret with info
`connverifier stamp v1` (32 bytes).

The reflector answers a packet only when its SSID names a live session, its HMAC
verifies, it comes from the source address of that session's first authenticated
packet, and it fits the granted rate (a token bucket holding one second's worth).
Everything else is dropped without an answer. The reply has the length of the
request.

## Limits

| Limit | Default | Set by |
|---|---|---|
| Live data-plane connections on the node | 20 000 | `serve -max-conns` |
| Sessions on the node | 64 | `serve -max-sessions` |
| Sessions per invite | 2 | `invite create -max-sessions` |
| Live connections per session | 20 000 | `invite create -max-connections` |
| New connections per second per session | 1 000 | `invite create -max-dial-rate` |
| Session length | 24 h | `invite create -max-duration` |
| Idle period a session may ask for | 1 h | `invite create -max-idle` |
| STAMP packets per second per session | 100 | `invite create -max-stamp-rate` |
| Connections not yet past TLS or HELLO | 32 per address, 1 024 in total | fixed |
| Open control-plane connections | 8 per address, 512 in total | fixed |
| Control-plane requests | 10 per second per address | fixed |
