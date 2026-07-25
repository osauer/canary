# Troubleshooting

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Anyone whose install has stopped behaving, at the moment it stops
behaving. Written to be scanned, not read.

**Shape.** Symptom, cause, fix. One block per symptom, symptom as the heading so
it matches what someone would search for.

**Symptoms to cover**

- Gateway not reachable, and the difference between a daemon still connecting
  and a gateway that is down.
- Quotes are delayed or frozen when the reader expected real-time.
- Breadth or gamma reports `computing` and stays there.
- The daemon will not start, or two daemons are fighting over a socket.
- Claude Desktop does not list the tools after installing the bundle.
- A stale or wedged subscription after an overnight gateway reset.
- Where the logs are and which one answers which question.

**Draw from**

- `README.md`, "Troubleshooting".
- The gateway-unavailable hint text in `internal/cli/cli.go`.
- `docs/docs/internals/architecture.md`, "Observability".

**Boundaries to keep**

- Log paths and command names must be verified against the current binary
  before publishing. This page rots faster than any other.
