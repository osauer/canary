# Unified Action Policy Post-MVP

Updated: 2026-06-13 10:34 CEST

The opportunities MVP deliberately keeps `opportunity-policy.toml` separate
from `protection-policy.toml`. That avoids coupling protection proposal health
to option-exercise threshold mistakes while the detector thresholds and
exercise-specific policy behavior mature.

## Target Shape

A future migration can introduce a unified policy kind, likely
`ibkr.action_policy`, with protection and opportunities as sibling buckets:

- `buckets.protection.theta_hygiene`
- `buckets.protection.risk_reduction`
- `buckets.protection.trailing_stop`
- `buckets.opportunities.option_exercise`

Shared authority fields should stay explicit. Do not infer exercise authority
from protection close/reduce authority unless the unified schema defines the
mapping and validation rules.

## Migration Requirements

- Preserve the current protection and opportunity policy loaders during the
  migration window.
- Define fingerprint behavior before moving users. The unified policy
  fingerprint must not silently replace existing protection/opportunity
  fingerprints without a compatibility story for stale previews, ignored rows,
  daemon health, CLI JSON, MCP responses, and SPA snapshots.
- Keep bucket-level errors isolated. A malformed opportunity threshold should
  not make protection proposals unhealthy unless the unified policy explicitly
  declares the whole file invalid.
- Keep defaults conservative and versioned. Moving a user from separate files
  to a unified file must require either an explicit generated migration or a
  higher `policy_version` with clear status output.

## Non-Goals

This note does not start the migration. The MVP remains on
`~/.config/ibkr/policies/opportunity-policy.toml` plus the existing protection
policy loader.
