# Writing a risk policy

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone who has decided the limits they want to trade within and
now has to write them down in a form the system will enforce.

**Shape.** Work from a real question to a real file. Start with the decisions a
trader has to make, then show the policy file those decisions produce, then the
commands that prove it is in force.

**Questions the page has to answer**

- Which decisions are the trader's alone: risk unit, position and portfolio
  limits, the drawdown ladder, review cadence.
- What the policy file looks like, field by field, with a worked example.
- How a policy version becomes effective, and what happens to positions opened
  under the previous one.
- Overrides: how to grant one, why they expire, and what is recorded.
- What the system will not let a policy do.
- How to read `ibkr policy show --explain`.

**Draw from**

- `docs/docs/understand/policy.md`, which covers who decides. This page covers how to write
  it, and should link rather than repeat.
- `docs/design/risk-policy.md`.
- `examples/risk-policy.toml`.
- `docs/docs/reference/config.md` for the authoritative field list.

**Boundaries to keep**

- Freeze and limit changes are human-only. Never present a path that weakens a
  guardrail as routine configuration.
- Do not invent thresholds. Where a number is the reader's decision, say that
  it is the reader's decision.
- This is not investment advice, and the page must not read as a recommended
  risk level.
