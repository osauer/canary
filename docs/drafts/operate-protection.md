# Protection and emergency exits

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone holding positions who wants to know what the system will
offer to do, what it will refuse to do, and what happens in a hurry.

**Questions the page has to answer**

- What a protection proposal is: daemon-owned, close or reduce only, with a
  per-row blocker when something makes it unsafe.
- The three families: trailing stop, theta hygiene, risk reduction.
- Why a proposal can be blocked, including an active halt or LULD pause, and
  why a blocked row is the system working.
- What a protective stop that no longer matches its position looks like, and
  the exact reduce-to-position quantity that fixes it.
- `ibkr purge` and `ibkr purge restore`: what they cover, what they do not
  cover, and the dry run that shows the plan first.
- What a human still has to do at every one of these steps.

**Draw from**

- `docs/design/protection-trailing-stop-tif.md`.
- `internal/cli/proposals.go` and `internal/cli/purge.go`.
- `docs/design/trading-rulebook.md` for the interaction with rule breaches.

**Boundaries to keep**

- Nothing on this page is a submit path. Every broker write stays an explicit,
  transaction-specific human decision through the gated CLI.
- Purge restore submission is currently unavailable and has to be handled in
  TWS. Say so plainly rather than implying it works.
