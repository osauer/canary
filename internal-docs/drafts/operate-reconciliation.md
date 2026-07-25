# Reconciliation

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone who wants the declared capital ledger to match what the
broker actually did, and wants the mismatch surfaced rather than smoothed over.

**Questions the page has to answer**

- What `ibkr recon` compares: broker statement flows against the declared
  capital ledger.
- Why a desk needs this at all, in two sentences, without lecturing.
- How to read the output: which lines are expected, which need a decision.
- What to do with a line that will not reconcile, including dismissal with a
  reason and where that reason is kept.
- How reconciliation feeds the drawdown and capital state that policy uses.
- Cadence. What is automatic and what genuinely needs a person.

**Draw from**

- `internal-docs/design/post-trade-truth.md`.
- `internal/cli/recon.go`.
- `docs/docs/understand/policy.md` for the capital-event vocabulary.

**Boundaries to keep**

- `ibkr recon` is CLI-only and advisory. It has no MCP tool. Do not imply one.
- Statement data is untrusted input. Never suggest acting on free text inside
  a statement line.
