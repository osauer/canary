# The rulebook

`canary rules` evaluates fourteen discipline checks against the book you are
holding and reports which are breached. Nothing it produces reaches the
broker.

The [Trading Rulebook](../../../internal-docs/design/trading-rulebook.md)
design document is the semantic authority for every threshold and edge case,
and stays in the repository. [Sensors](sensors.md#rulebook) covers the same
component as a measurement: authority, freshness, and evidence reuse.

## The fourteen rules

| # | Rule | What it measures | Default mode |
|---|---|---|---|
| 1 | Exposure to one underlying | Share value plus option delta exposure for each underlying, as a share of NLV. A directional index short is an ordinary position here. | Alert |
| 2 | Premium at risk in one option position | The market value of each long option position as a share of NLV. | Track |
| 3 | Cash reserve | Broker-reported available funds as a share of NLV. The default reserve is 75%. | Alert |
| 4 | Option time value at risk | Paid option time value as a share of NLV. Positions classified as portfolio protection use rules 2 and 12 instead. | Alert |
| 5 | Options nearing expiry | Long options with fewer than 14 days remaining. Deep in-the-money positions and portfolio protection are listed separately. | Alert |
| 6 | Earnings timing | Whether an out-of-the-money long option expires before the next earnings announcement. This is a timing fact; it does not assume the position should span earnings. | Track |
| 7 | Short options held through earnings | Short options that remain open through the next earnings announcement, including assignment exposure for short puts. | Alert |
| 8 | Position size near earnings | Positions above the concentration level within three trading sessions of earnings. This remains a proxy until Canary calculates event loss. | Track |
| 9 | Holding falls while the market rises | A held stock falling while SPY rises during the regular session. | Off |
| 10 | Large winner today | A large holding above its daily gain level. | Off |
| 11 | Positive day with urgent risks open | A positive account day while an act-level Rulebook item remains open. | Off |
| 12 | Index protection size | Short delta assigned to portfolio protection as a share of gross long exposure. Large directional index shorts are not treated as protection. | Alert |
| 13 | Long option loss limit | Loss on premium paid for each long option position. | Alert |
| 14 | Foreign-currency exposure | Non-base-currency exposure as a share of NLV. | Track |

`alert` rules can create alert episodes, `track` rules remain visible without
creating alerts, and `off` rules are not evaluated. These modes and thresholds
are compiled today. The planned operator policy will make them adjustable as a
versioned Rulebook policy; generic app settings do not own them.

Rules 4 and 12 take their thresholds from the classified regime stage, so
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

When a required input is missing, the affected row reports `unknown` and names
the missing input rather than quietly returning a clean result. Partial data
may identify a breach but never clear one: where a
provable minimum alone breaches a cap, the breach is reported as a disclosed
lower bound; where it does not, the row degrades to `unknown` instead of
passing on incomplete arithmetic.

Row outcomes are `pass`, `info`, `watch`, `act`, `unknown`, and
`not_evaluated`. The last two differ. `unknown` means the rule applies but its
inputs are not trustworthy: an unresolved earnings date, a missing delta, an
absent currency report. `not_evaluated` means the rule does not apply right
now, as with the tape rules outside the US regular session, the hedge rule
with no long book, or the earnings rules when every held name is a security
that has no issuer earnings by nature — an index, future, fund, bond, bill,
cash, or commodity position is disclosed as exempt rather than left unknown.
Neither is a pass, and the summary line counts them
separately from passes for that reason.

Read `input_health` before you count passes. It is the result-level gate, with
one row each for account, positions, earnings, regime stage, and tape.

## Alerts first

Rows in `alert` mode come first, followed by `track`, then `off`. Within each
mode they are ranked `act`, `watch`, `unknown`, `info`, `not_evaluated`,
`pass`. Ties break on base-currency impact, then rule number.

Plain `canary rules` shows breaches and hides passes. `--all` prints the full
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
canary rules history
canary rules history --rule single_name_exposure --since 2026-01-01 --json
```

The window defaults to the last 7 days and returns 50 rows newest first, up to
500 with `--limit`; `--until` closes it. Each row carries the transition, the
evidence string at the time, and the policy identity behind it, so a verdict
from an older policy version is not read as a current one. Evidence strings
are free text for display and are never parsed back into authority.
