# Internal docs index

Updated: 2026-07-30 11:55 CEST

Reader's contract: **Current authority** binds today's behavior — read it before
changing the surface it names. **Approved, not yet implemented** is operator-
approved design with no code behind it. **Historical records** explain how we
got here; Git history is their authority, and they must not be read as current
instructions. Each design doc carries its own `Status:` line; when this index
and a doc disagree, the doc wins and this index needs the fix.

## Current authority

| Doc | Owns |
|---|---|
| [design/platform-settings.md](design/platform-settings.md) | Settings, config, and state authority — required reading per AGENTS.md |
| [design/trading-rulebook.md](design/trading-rulebook.md) | Rulebook semantics and authority |
| [design/risk-policy.md](design/risk-policy.md) | Risk-policy constitution and enforcement phases |
| [design/agent-origin-gating.md](design/agent-origin-gating.md) | Agent-origin classification for broker writes |
| [design/alert-regime-production.md](design/alert-regime-production.md) | Source-neutral alert inbox and Web Push delivery |
| [design/daemon-sqlite-authority.md](design/daemon-sqlite-authority.md) | daemon.db as the single mutable state authority |
| [design/history-index.md](design/history-index.md) | History read surfaces on daemon.db |
| [design/post-trade-truth.md](design/post-trade-truth.md) | Statement-authoritative post-trade reporting |
| [design/protection-trailing-stop-tif.md](design/protection-trailing-stop-tif.md) | Protection proposal trailing-stop and TIF semantics |
| [design/regime-calibration.md](design/regime-calibration.md) | Regime indicator calibration and journal contract |
| [design/operator-ergonomics.md](design/operator-ergonomics.md) | Operator interview outcomes shaping CLI/app ergonomics |
| [design/documentation-ia.md](design/documentation-ia.md) | Public handbook information architecture |
| [guides/trading-harness-development.md](guides/trading-harness-development.md) | Risk-harness development guide |
| [guides/canary-spa-dev.md](guides/canary-spa-dev.md) | SPA development guide |

## Approved, not yet implemented

| Doc | State |
|---|---|
| [design/risk-governance-nudges.md](design/risk-governance-nudges.md) | Approved 2026-07-18; no code yet |

## Historical records

| Doc | Why it is kept |
|---|---|
| [design/mobile-app-mvp.md](design/mobile-app-mvp.md) | MVP baseline for the paired PWA; linked from README as history |
| [design/trading-paper-smoke.md](design/trading-paper-smoke.md) | Live-gate decision record; pinned by `internal/cli/skill_drift_test.go` |

`drafts/` is intentionally empty between documentation rounds; its README says
what belongs there.
