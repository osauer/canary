# The daily desk

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone running the desk every trading day who wants a routine
rather than a tour of features.

**Shape.** A routine in time order, with the command for each step and one line
on the decision it supports. Short. This is the page a reader keeps open.

**The routine**

- Before the open: `ibkr brief --kind morning`, what a degraded source in that
  brief means, and when to stop and fix rather than trade around it.
- Context: `ibkr regime`, then `ibkr canary`, and the rule that account-only
  risk is evidence rather than a DEFEND trigger.
- Discipline: `ibkr rules`, hardest breach first.
- During the session: what is worth watching live and what is not.
- Protection: `ibkr proposals`, and the rule that a proposal is a proposal.
- After the close: `ibkr brief --kind eod`, then reconciliation cadence.

**Draw from**

- `internal/cli/brief.go` for what the brief actually assembles.
- `docs/sensors.md` for freshness semantics, linked rather than repeated.
- `docs/design/operator-ergonomics.md` for the ergonomics rationale.

**Boundaries to keep**

- A proposal, alert, preview, or clean rulebook run is evidence, never submit
  authority. Every broker write is a separate explicit human decision.
- Do not turn the routine into a sign-off ritual. Where the system can absorb a
  routine check, say that it does.
