# Internal docs index

Updated: 2026-11-04 12:55 CET

Reader's contract: **Current authority** binds today's behavior — read it before
changing the surface it names. **Approved, not yet implemented** is operator-
approved design with no code behind it. **Proposed, awaiting decision** has not
been approved and binds nothing; do not implement from it without the operator's
go-ahead. **Historical records** explain how we got here; Git history is their
authority, and they must not be read as current instructions. Each design doc
carries its own `Status:` line; when this index and a doc disagree, the doc wins
and this index needs the fix.

## Current authority

| Doc | Owns |
|---|---|
| [design/platform-settings.md](design/platform-settings.md) | Settings, config, and state authority — required reading per AGENTS.md |
| [design/trading-rulebook.md](design/trading-rulebook.md) | Rulebook semantics and authority |
| [design/budget-governor.md](design/budget-governor.md) | Premium budget governor (`budget_reduction`): basis, ranking, candidates and plan |
| [design/cash-sweep.md](design/cash-sweep.md) | Cash sweep (`cash_sweep`): idle cash into same-currency bills, and its post-install proof |
| [design/risk-policy.md](design/risk-policy.md) | Risk-policy constitution and enforcement phases |
| [design/agent-origin-gating.md](design/agent-origin-gating.md) | Agent-origin classification for broker writes |
| [design/alert-regime-production.md](design/alert-regime-production.md) | Source-neutral alert inbox and Web Push delivery |
| [design/post-trade-truth.md](design/post-trade-truth.md) | Statement-authoritative post-trade reporting |
| [design/protection-trailing-stop-tif.md](design/protection-trailing-stop-tif.md) | Protection proposal trailing-stop and TIF semantics |
| [design/protective-stop-guard.md](design/protective-stop-guard.md) | Protective stock exit exemption from the notional and short gates, and the stop guard |
| [design/regime-calibration.md](design/regime-calibration.md) | Regime indicator calibration and journal contract |
| [design/tws-order-control.md](design/tws-order-control.md) | Cancelling and changing orders placed by hand in TWS over a one-request client-0 connection: the switch, the paper proof, the gates, the journal and the failure semantics |
| [design/option-chain-recorder.md](design/option-chain-recorder.md) | Daily option-chain recorder: the switch, selection, schedule, line and pacing limits, coverage, `option-chains.db` data dictionary and read surfaces |
| [design/fill-quotes.md](design/fill-quotes.md) | Fill quotes: the quote retained at every order event and fill, the reqExecutions poll for fills Canary did not place, `reporting.costs` (half-spread paid) and its coverage verdict |
| [design/regime-input-currency.md](design/regime-input-currency.md) | Regime input currency model (`regime-currency-v1`) |
| [design/documentation-ia.md](design/documentation-ia.md) | Public handbook information architecture |
| [guides/trading-harness-development.md](guides/trading-harness-development.md) | Risk-harness development guide |
| [guides/canary-spa-dev.md](guides/canary-spa-dev.md) | SPA development guide |

## Approved, not yet implemented

| Doc | State |
|---|---|

## Proposed, awaiting decision

| Doc | State |
|---|---|
| [design/authority-contract-cache-bloat.md](design/authority-contract-cache-bloat.md) | Written 2026-07-31; 5.1 GB of unread contract-cache observations, prune plan not approved |
| [design/broker-opend.md](design/broker-opend.md) | Written 2026-11-04; Futu OpenD as the second broker: the `internal/broker` read seam, `pkg/futu`, the simulator, the mode pin and the additive instrument identity; three decisions proposed for the owner's ack |
| [design/broker-opend-build-brief.md](design/broker-opend-build-brief.md) | Written 2026-11-04; the ordered work packages, tests and gates for the OpenD build; reviewed in [reviews/2026-11-04-broker-opend-design-review.md](reviews/2026-11-04-broker-opend-design-review.md) |

`drafts/` is intentionally empty between documentation rounds; its README says
what belongs there.
