# The rulebook

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** A trader deciding whether to trust the daily checklist, and
wanting to know what each rule is actually protecting against.

**Shape.** One block per rule: what it checks, the failure it exists to
prevent, what a breach looks like in the output, and what it does not mean.
Fourteen rules, so the page needs a compact table at the top and the detail
below it.

**Questions the page has to answer**

- Why an advisory checklist rather than a hard block.
- How a rule reaches a verdict, and why a rule that cannot get clean data
  reports that instead of passing.
- Severity ordering, and why the hardest breach is shown first.
- What history is kept and how to query it.

**Draw from**

- `internal-docs/design/trading-rulebook.md`, which is the authority. This page is the
  public distillation of it, not a copy. The design document stays in the
  repository.
- `internal/cli/rules.go` for the output shape.

**Boundaries to keep**

- A rule never passes because evidence was missing. Never-false-pass is the
  property worth stating explicitly.
- A clean rulebook run is not permission to trade and not submit authority.
