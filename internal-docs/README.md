# Internal docs index

Updated: 2026-08-10 08:25 CEST

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
| [design/risk-policy.md](design/risk-policy.md) | Risk-policy constitution and enforcement phases |
| [design/agent-origin-gating.md](design/agent-origin-gating.md) | Agent-origin classification for broker writes |
| [design/alert-regime-production.md](design/alert-regime-production.md) | Source-neutral alert inbox and Web Push delivery |
| [design/post-trade-truth.md](design/post-trade-truth.md) | Statement-authoritative post-trade reporting |
| [design/protection-trailing-stop-tif.md](design/protection-trailing-stop-tif.md) | Protection proposal trailing-stop and TIF semantics |
| [design/regime-calibration.md](design/regime-calibration.md) | Regime indicator calibration and journal contract |
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

`drafts/` is intentionally empty between documentation rounds; its README says
what belongs there.
