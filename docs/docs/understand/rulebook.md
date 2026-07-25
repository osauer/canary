# The rulebook

`ibkr rules` evaluates fourteen discipline checks against the book you are
holding and reports which are breached. Nothing it produces reaches the
broker.

The [Trading Rulebook](../../../internal-docs/design/trading-rulebook.md)
design document is the semantic authority for every threshold and edge case,
and stays in the repository. [Sensors](sensors.md#rulebook) covers the same
component as a measurement: authority, freshness, and evidence reuse.

## The fourteen rules

| # | Rule | What it is protecting against |
|---|---|---|
| 1 | Per-name exposure cap | One name growing large enough to decide the account's outcome by itself. |
| 2 | Single option line premium | One line holding so much premium that a volatility crush becomes a portfolio event. Hedge lines get their own higher tier. |
| 3 | Negative cash sell-only posture | Adding risk on borrowed cash, where margin interest is negative carry on top of the position risk. |
| 4 | Portfolio extrinsic budget | Paying decay on too much of the book at once. Classified hedge legs are excluded; rules 2 and 12 govern those. |
| 5 | Expiry runway | Long options carried into the final-week decay cliff. Deep in-the-money legs are exempt, disclosed per row. |
| 6 | Option outlives its catalyst | An out-of-the-money long that expires before the earnings gap it was bought for. |
| 7 | Overwrite spans earnings | A short option alive through an earnings print: capped upside or forced assignment through the exact event that pays. |
| 8 | At size before earnings | Carrying an oversized name into an earnings print instead of trimming to what you would buy fresh today. |
| 9 | Relative weakness on a green tape | Missing that the market is naming your exits, when a held name is red while the tape is green. |
| 10 | Trim winners into strength | Failing to sell an oversized winner while the bid is still there. |
| 11 | Green day is an execution day | Deferring an open de-risk list to a hypothetical better day. |
| 12 | Hedge sized to the book | A hedge too small to protect anything, or so large it is a directional short wearing a hedge's clothing. |
| 13 | Exit the dead thesis | Letting theta decide an exit the thesis should have decided. |
| 14 | Non-base currency exposure | Currency risk taken by accident rather than on purpose. Watch-only by design, because a permanent alarm here would be pure fatigue. |

Rules 3, 4, and 12 take their thresholds from the classified regime stage, so
the same book can pass in a calm regime and breach in a confirmed one. A stale
or never-observed stage is evaluated against both its own threshold set and
the calm set, keeping the worse verdict: old market state may tighten a rule,
never relax it.

Thresholds ship compiled as policy `rulebook-v2`, and every row carries its
`observed` value, `threshold`, and an evidence string, so you can check the
arithmetic instead of trusting the verdict. A compiled model is not itself
proof that a threshold has your approval.

## Advisory by construction

The rulebook has no path to the broker. Its verdicts never touch submit
eligibility or broker-write authorization, and turning it off with
`features.rulebook.enabled=false` cannot affect broker-write gating either. An
order preview may carry matching advisory `rule_*` warnings, but they are
annotations and change nothing about whether it is submittable.

That is deliberate. A hard block on a discipline heuristic fails in the
direction nobody wants: it stops a correct trade at the worst moment, and it
teaches you to route around it. The rules are stated so you can disagree with
one in a specific case and still see the flag.

## A rule that cannot see does not pass

**A rule that cannot get clean data reports that it could not evaluate. It
never passes.**

When positions or account evidence is unhealthy, the affected rows report
`unknown` with the evidence line "not asserting a pass" rather than quietly
returning a clean result. Partial data may indict but never acquit: where a
provable minimum alone breaches a cap, the breach is reported as a disclosed
lower bound; where it does not, the row degrades to `unknown` instead of
passing on incomplete arithmetic.

Row outcomes are `pass`, `info`, `watch`, `act`, `unknown`, and
`not_evaluated`. The last two differ. `unknown` means the rule applies but its
inputs are not trustworthy: an unresolved earnings date, a missing delta, an
absent currency report. `not_evaluated` means the rule does not apply right
now, as with the tape rules outside the US regular session or the hedge rule
with no long book. Neither is a pass, and the summary line counts them
separately from passes for that reason.

Read `input_health` before you count passes. It is the result-level gate, with
one row each for account, positions, earnings, regime stage, and tape.

## Hardest breach first

Rows are ranked `act`, `watch`, `unknown`, `info`, `not_evaluated`, `pass`.
Ties break on base-currency impact, then rule number. `unknown` outranks
`info` on purpose, so a rule you cannot evaluate surfaces above one that is
merely noting something.

Plain `ibkr rules` shows breaches and hides passes. `--all` prints the full
checklist, `--symbol` narrows offender lists to one underlying, and `--json`
returns the ranking and input health alongside the rows.

## What a clean run means

It means fourteen specific checks did not fire on the book as the daemon last
saw it. That is all.

A clean rulebook run is not permission to trade and carries no submit
authority. It says nothing about whether the thesis holds, or whether these
are the right rules for your account. Every submission stays a
transaction-specific human decision behind the separate controls described in
[Trading policy](policy.md).

## What history is kept

A transition is recorded when a row's status changes, not on every evaluation,
so the timeline reads as a sequence of events rather than a log of minutes.

```sh
ibkr rules history
ibkr rules history --rule single_name_exposure --since 2026-01-01 --json
```

The window defaults to the last 7 days and returns 50 rows newest first, up to
500 with `--limit`; `--until` closes it. Each row carries the transition, the
evidence string at the time, and the policy identity behind it, so a verdict
from an older policy version is not read as a current one. Evidence strings
are free text for display and are never parsed back into authority.
