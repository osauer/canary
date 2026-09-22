# Handoff: The Bounded Desk, phase 1 in Canary

Written: 2026-09-22 07:05 CEST. Audience: the next session (Codex or Claude) continuing
"Phase 1 · Reduce by rule". Status: **built and gated on branch
`bounded-phase-1`; not merged to main; not pushed; not deployed; the owner's
policy edits not made.** Nothing here has placed, previewed or cancelled a
broker order.

## Read this first

The product view of record is the owner's private page "The Bounded Desk"
(claude.ai artifact `AbaUoTaRz8NBkcHU3jpbrC`, 2026-09-21). Its "Plan and
resources" section holds the phase tables; the owner executes phases in
order and pauses after each for corrections. Phase 0 (the Desk posture) is
built and deployed in the Desk repository on branch `bounded-phase-0`
(Desk 8250f4d; its note is `docs/active-runtime-source.md` there). This
document is the Canary side of phase 1.

Owner decisions this phase implements, taken 2026-09-21:

- **D1:** the risk constitution's protected floor and declared risk capital
  stand as written (`~/.config/ibkr/policies/risk-policy.toml`, v4).
- **D3:** "Reduce-only automation: Canary places the protective orders it
  already proposes: GTC trailing stops on uncovered stock and ETF lines,
  option loss exits, and, while the brake is latched, reductions back to
  budget. Phone notice first, thirty-minute veto, then it runs."
- **D4:** the options mandate "a bit more aggressive" than the page's
  proposal; the product manager set 40 % of declared risk capital in total
  premium at risk and 15 % per line (the page's third number, 20 % new
  premium per calendar month, is a rule for a later phase and is not built).

These are the explicit human decisions AGENTS.md requires for a guardrail
change. They authorise daemon-originated submission only of proposals in
buckets the owner lists as pre-authorised in the protection policy file.
They do not authorise any opening, adding or flipping order, or any change
to freeze, limits or pins.

## Branch state

`bounded-phase-1` = Canary main `f2cb26d2` (v3.9.2)
+ `p1-governor` (6 commits, Fable agent; its own full `make test` passed at
  d8e6da27)
+ `p1-preauth` (5 commits, Fable agent; the fifth, the act notice, was
  committed by the orchestrator from the agent's working tree after the
  agent hit its model limit; the agent's `make check` on its branch failed
  only on the two doc checks fixed below)
+ merge resolution (three files, see the merge commit message)
+ `954f338f` docs regen + doc-comment names (`docs-check`, `go-doc-check`)
+ `08e0be60` **the seam fix**: the scheduler predated the governor's flags;
  shadow rows are now never scheduled and `never_skip_veto` rows keep the
  full window under the latch (two witnesses in
  `proposal_automatic_test.go`)
+ `aefae4ad` `trade_proposals.veto` timing entry (the only `make test`
  failure at 954f338f).

Gates, by commit (logs in the orchestrator's scratchpad, not in the repo):

| Commit | `make check` | `make test-daemon` | `make test` |
|---|---|---|---|
| d8e6da27 (governor) | pass | pass | pass |
| 4a588972 (pre-auth) | fail: docs-check, go-doc-check only | not run | not run |
| 954f338f | pass | fail: veto timing entry | not run |
| 08e0be60 | pass | (in `make test`) | see next session's log |
| aefae4ad | not yet | not yet | **run it first** |

First action for the next session: from this worktree (or a fresh one at
`bounded-phase-1`), run `make test` (the binding full gate; about 10
minutes; background it with a log). Expect green. If not, the failing test
names the seam.

## What was built

### Pre-authorised protection (rows 1.1, 1.2; `p1-preauth`)

- Schema: `[authority] pre_authorised = [...]` (closed vocabulary
  `trailing_stop`, `option_loss_exit`, `option_profit_trail`,
  `budget_reduction`), `veto_window = "30m"` (default 30m, minimum 5m);
  `auto_submit = true` still refused; embedded default has an empty list.
  `internal/daemon/protection_policy.go`.
- Durable automatic-submission record in daemon.db per proposal key and
  revision: states pending → submitting → submitted | vetoed | superseded |
  failed; intent persisted before the broker call; restart recovery reads
  the order journal and never places twice.
  `internal/daemon/proposal_automatic.go` (+ two test files).
- Origin `daemon-preauthorised`, accepted by `brokerWriteAuthorization`
  only for a submission the scheduler itself is running and whose bucket is
  in the active policy's list; every other gate unchanged.
  `internal/daemon/broker_write_transaction.go`.
- Veto: `canary proposals veto KEY` (human-terminal or paired-device origin
  only; agent origin refused), the app endpoint and a Veto button on the
  Protection panel. A veto holds until the revision changes.
- Notice: one Protection alert episode per record at severity `act`
  (`protection_auto_<bucket>` presentation codes, `_now` variant when the
  latched brake skipped the window). No symbol or quantity in the push.
- Surfaces: `TradeProposal.automatic` on list/JSON/MCP; a short column in
  `canary proposals list`; `canary proposals status` shows the buckets and
  the pending count.

### The budget governor and the capital row (row 1.3; `p1-governor`)

- `rpc.BriefCapitalRow` gains `warn_pct`, `block_pct`,
  `protected_floor_base`, `declared_risk_capital_base`,
  `effective_risk_capital_base`; `ready.premium_at_risk` gains
  `pct_of_risk_capital`. Nil when the constitution is absent or unapproved.
- Bucket `[buckets.budget_reduction]`: `enabled` (default false, absent
  from the embedded default), `mode = "shadow" | "active"` (default
  shadow), `premium_at_risk_pct_of_risk_capital`,
  `per_line_pct_of_risk_capital`, `max_order_notional`. Generates rows only
  when the constitution is active, the block tier is latched or breached,
  and enforcement is advisory or stronger; otherwise a typed status says
  why. `internal/daemon/proposal_budget.go`.
- Selection: premium at risk = long option legs Canary does not derive as
  protection; per line first, then total, largest unrealised loss first,
  whole contracts. Rows carry `budget` arithmetic, `shadow`,
  `never_skip_veto`, and `(TradeProposal).AutomaticEligible()`.
- Shadow rows are listed under their own heading, journaled, refused by
  preview and submit with `shadow_mode`, and (since 08e0be60) never
  scheduled.
- Design record: `internal-docs/design/budget-governor.md`. Docs:
  `docs/docs/operate/protection.md` ("Budget reduction"),
  `docs/docs/understand/risk-policy.md`, references regenerated.

## What is NOT done (in order)

1. **Full gate at aefae4ad** (see above).
2. **Pre-authorised protection docs.** The pre-auth agent stopped before
   its docs: no `internal-docs/design/pre-authorised-protection.md` (use
   `.agents/docs/risk-policy-contract.md` as the template and quote D3
   verbatim), no "Pre-authorised buckets" section in
   `docs/docs/operate/protection.md`, no paragraph in `orders.md`, and
   SECURITY.md has no sentence on the `daemon-preauthorised` origin.
   `config.md` and `cli.md` were regenerated and are current.
3. **CHANGELOG.md**: add an Unreleased entry (Keep a Changelog style; see
   the v3.9.2 entry for register). Proposed lines: Added: pre-authorised
   protection buckets with a veto window and a phone notice; the budget
   governor in shadow; the constitution's ladder and money-at-risk figures
   on the brief's capital row. Changed: `canary proposals` gains `veto`.
4. **Merge to main and push** are the owner's (the orchestrating session's
   classifier blocks merges in the shared checkout). Then `make
   restart-daemon` (owner; blocked for Claude sessions), then re-pin Canary
   in Desk and restart Desk ([[canary-client-architecture]] rule).
5. **Desk row 1.4**: Risk › Protective orders shows the pre-authorised
   buckets, pending rows with their countdown, acted rows with broker
   status; a veto goes through the passkey path. Needs the Canary re-pin
   (the typed `TradeProposal.automatic` and the capital-row fields reach
   Desk's observers only after a re-pin and rebuild). Desk's constitution
   strip can then print the warn and block lines and the money at risk.
6. **Owner actions to activate** (each is a policy edit; nothing activates
   on merge):
   - `~/.config/ibkr/policies/risk-policy.toml`: `block_enforcement =
     "advisory"`, `policy_version = 5` (row 1.5; lets the governor
     generate rows and puts the ladder's cause on previews).
   - `~/.config/ibkr/policies/protection-policy.toml`, bump
     `policy_version` (currently 8):

     ```toml
     [authority]
     close_reduce_only = true
     auto_submit = false
     pre_authorised = ["trailing_stop", "option_loss_exit", "option_profit_trail"]
     veto_window = "30m"

     [buckets.budget_reduction]
     enabled = true
     mode = "shadow"
     premium_at_risk_pct_of_risk_capital = 40.0
     per_line_pct_of_risk_capital = 15.0
     max_order_notional = 10000.0
     ```

   - Read the shadow rows for a few sessions (`canary proposals list`),
     then, as a separate decision, `mode = "active"` and add
     `"budget_reduction"` to `pre_authorised`.
   - The drawdown brake stays latched until `canary policy
     reset-drawdown --reason "..."`; leave it latched until the shadow
     rows have been read.

## Known consequences to say plainly to the owner

- The live brief on 21 Sept put premium at risk at roughly three times the
  declared risk capital, so once the governor is active it will propose
  cutting most of the current option book while the brake is latched.
  That is the constitution as written (D1), not a defect.
- A pre-authorised stop or exit under a latched brake submits without the
  window. The governor's rows never do.

## Rules that bind this work

AGENTS.md (trading and data safety, no exceptions), the agent-origin gating
design, the risk-policy contract template, "Fix what you find, no homework",
and the Desk memory notes on the product view. Never propose purpose
declarations, self-tuning thresholds, forecasts or a second risk engine in
Desk.
