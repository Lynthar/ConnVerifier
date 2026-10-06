# Measurement methods

Every check ConnVerifier reports has a method document here. A result names its
check `id` and `method_version`; the document below with that id is the one that
version follows.

| Check id | Method version | Document |
|---|---|---|
| `tcp-capacity` | 3 | [tcp-capacity.md](tcp-capacity.md) |

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
