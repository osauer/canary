# Writing a risk policy

Updated: 2026-07-25 12:09 CEST

The personal risk policy is one TOML file you write by hand. It holds the
capital numbers, drawdown ladder, exception cap, reconciliation tolerances, and
cadence declarations the daemon evaluates against your account.
[Trading policy](policy.md) covers who owns each control and what the system
does with the result; this page covers how to write those decisions down.

Every number in it is yours. There is no embedded default and no recommended
value, and nothing here proposes one. A key you do not write does not exist,
and the control that depends on it stays `unapproved`.

## Where the file lives

The daemon reads one path, a constant rather than a config key, so the file
that governs risk numbers cannot be relocated:

```text
~/.config/ibkr/policies/risk-policy.toml
```

Start from the template at
[`examples/risk-policy.toml`](../../../examples/risk-policy.toml). Every
material key arrives commented out. The few values that are filled in are
either structural, where the schema accepts only one or two values anyway, or
copied from the author's own setup, like the base currency and the
`[inventory]` pins. Nothing in that file is a recommendation.

Only the envelope is fixed:

```toml
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1
```

`kind` and `schema_version` must be exactly those values. `policy_id` is any
non-empty identity string and `policy_version` any positive integer you raise
on each revision.

## What each section governs

| Section | Keys | What the numbers govern |
|---|---|---|
| `[capital]` | `base_currency`, `protected_floor`, `declared_risk_capital`, `max_equity_age_minutes`, `max_unreconciled_days` | The currency every figure is stated in, equity that is never risk capital, the money you have authorized to be at risk, and how stale an equity reading or a reconciliation may get |
| `[drawdown]` | `warn_consumed_pct`, `block_consumed_pct`, `block_enforcement` | Two tiers, each a percent of declared risk capital consumed from the cash-flow-adjusted equity peak, plus the enforcement class of the block tier |
| `[override]` | `max_duration_hours` | The longest a one-shot exception may live |
| `[recon]` | `amount_tolerance_pct`, `amount_tolerance_min`, `date_window_business_days`, `max_report_age_days`, `max_equity_divergence_pct` | Which statement-versus-declared-event differences you want to look at, and how old the statement evidence may be |
| `[cadence]` | `morning.class`, `eod.class`, `weekly.class` | Which routine reviews get completion journaling |
| `[inventory]` | `rulebook`, `protection`, `stress` pins | The sibling policy versions this constitution was approved against, identity only |

The schema bounds the shape of these numbers, never the level. Percentages must
sit in `(0, 100]`, `warn_consumed_pct` must be below `block_consumed_pct`,
`declared_risk_capital` must be positive, and `protected_floor` must not be
negative. Choosing the values inside those bounds is your decision alone.

`ibkr policy show --explain` is the field inventory: it prints every key with
its meaning, current value, source (`file`, `default`, or `unapproved`), and
enforcement class. The [configuration reference](../reference/config.md)
covers `config.toml`, the protection and opportunity policies, and runtime
settings; the risk policy is not among them.

Effective risk capital is the lesser of `declared_risk_capital` and equity above
`protected_floor`, so a deposit raises equity without raising the budget; only a
revision does that. If `capital.base_currency` differs from the account's base
currency, the equity observation is reported as unusable for capital math rather
than converted.

## What the file cannot do

Schema version 1 accepts `shadow` (the default when the key is empty) and
`advisory` for `drawdown.block_enforcement`. It rejects `"hard"` with a
targeted error, so this file cannot block an order today. A warn or block tier
attaches an advisory `capital_drawdown` cause to a risk-increasing order
preview and leaves submit eligibility untouched; close and reduce previews, and
a long put on the Rulebook's hedge index list, never carry it. Cadence classes
accept `advisory` only.

The schema has no key for account or route pins, freeze, preview tokens, broker
WhatIf, or origin gating, so no revision can express a change to them, and an
override naming a key outside the constitution is refused with "safety
invariants have no keys and cannot be overridden". Unknown keys fail the load
outright rather than being ignored. `trading.freeze` and the trading limits are
runtime settings, not policy: they change only through `ibkr settings set` from
an interactive human terminal, and agent and paired-device origins are
rejected.

## Making a revision effective

1. Edit the file and raise `policy_version`. Identical content at the accepted
   version keeps running. Changed content at the same or a lower version
   reports `drift`, and the daemon keeps the last accepted version in memory.
2. Wait. The manager rereads the file every 30 seconds.
3. Run `ibkr policy show`. The header gives the status, policy id, version,
   short fingerprint, and path; add `--explain` for the limit rows or `--json`
   for the typed result. It works without gateway connectivity, falling back to
   the persisted last equity reading.
   - `error` means bad syntax, an unknown key, or failed validation. The last
     good policy stays active and the message names the problem.
   - `absent` means the file is missing. A previously loaded policy stays
     active; deleting the file is not how you retire one.
4. Read the "Waiting on your decisions" list. Any material key missing there
   makes the whole capital tier `unapproved`, not only the control that needs
   it.

Raising `policy_version` past certain points enlarges the decision set.
`recon.max_equity_divergence_pct` is accepted only at version 3 or above and
becomes material there; `[cadence.nudges]` and `[cadence.monthly]` behave the
same way at version 4. A version bump made for an unrelated edit can therefore
move a complete policy to `unapproved`.

Evaluation is account-wide: the ladder measures current equity against the
cash-flow-adjusted peak, so a revision changes the answer for everything you
hold at once. No position carries the policy that was in force when it was
opened.

## Bounded exceptions

```sh
ibkr policy override --control <key> --reason "..." --hours <n>
```

`--hours` must be positive and at most `override.max_duration_hours`; until you
declare that cap, overrides are unavailable. `--control` must name a key that
`--explain` lists. Each grant is journaled with its reason and the policy
fingerprint and expires on its own. A durable change is a version bump, not a
longer override.

Only an override on `capital.max_unreconciled_days` currently reaches
evaluation, and it can extend that deadline, never shorten it. Others are
recorded and displayed.

A latched drawdown block is not an override case. It clears through
`ibkr policy reset-drawdown --reason "..."`, which re-bases the adjusted peak.

`ibkr policy` is a CLI surface with no MCP tool. That command and the other
governance verbs (`capital-event`, `override`, `correct-peak`, `artefact`) are
human-origin only, so an agent session can read the policy result and never
operate this file.
