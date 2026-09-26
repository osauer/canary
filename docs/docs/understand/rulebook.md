# The rulebook

`canary rules` evaluates eighteen discipline checks against the book you are
holding, using limits you can set, and reports which are breached. Nothing it produces reaches the
broker.

The [Trading Rulebook](../../../internal-docs/design/trading-rulebook.md)
design document is the semantic authority for every threshold and edge case,
and stays in the repository. [Sensors](sensors.md#rulebook) covers the same
component as a measurement: authority, freshness, and evidence reuse.

## The eighteen rules

| # | Rule | What it measures | Default mode |
|---|---|---|---|
| 1 | Worst-case loss on one issuer | The most one issuer can lose at any price, every leg on it netted from current marks, as a share of NLV. Watch at 30%, act (the cap) at 40%; an issuer that takes more than three days to exit at 20% of its 20-day volume uses 20/30. A long option counts as protection only if it expires after the issuer's next earnings and at least 14 days out; short stock and uncovered short calls are sized at a 100% rise and flagged unbounded. | Alert |
| 2 | Premium at risk in one option position | Each long option position at the higher of the price paid and its value, as a share of NLV. A losing position keeps counting at what you paid, so its fall frees no room to buy more. | Track |
| 3 | Cash reserve | Broker-reported available funds as a share of NLV. The default reserve is 75%. | Alert |
| 4 | Option time value at risk | Paid option time value as a share of NLV. Positions classified as portfolio protection use rules 2 and 12 instead. | Alert |
| 5 | Options nearing expiry | Long options with 14 days or fewer remaining (act at 7 or fewer). Deep in-the-money positions and portfolio protection are listed separately. | Alert |
| 6 | Earnings timing | Whether an out-of-the-money long option expires before the next earnings announcement. This is a timing fact; it does not assume the position should span earnings. | Track |
| 7 | Short options held through earnings | Short options that remain open through the next earnings announcement, including assignment exposure for short puts. | Alert |
| 8 | Position size near earnings | Positions on an issuer at or above rule 1's watch level within three trading sessions of earnings. | Track |
| 9 | Holding falls while the market rises | A held stock falling while SPY rises during the regular session. | Off |
| 10 | Large winner today | A large holding above its daily gain level. | Off |
| 11 | Positive day with urgent risks open | A positive account day while an act-level Rulebook item remains open. | Off |
| 12 | Index protection size | Short delta assigned to portfolio protection as a share of gross long exposure, against a band that depends on the regime. It acts above twice the band's top (`overhedge_multiple`), and index puts above that multiple of the widest band count as directional shorts, not protection. | Alert |
| 13 | Long option loss limit | Loss on premium paid for each long option position. | Alert |
| 14 | Foreign-currency exposure | Non-base-currency exposure as a share of NLV. | Track |
| 15 | Net market exposure | The whole book's signed stock-equivalent exposure, index protection included, as a share of NLV: how far the book moves with the market. Watch at 100% (fully invested, unlevered), act at 150%. | Track |
| 16 | Delta swing on one issuer | One issuer's dollar delta as a share of NLV: what a 10% move costs, with gamma named when it bends that materially. Watch at 30%; it never acts. | Track |
| 17 | Cluster falling together | Every issuer in a cluster you declare falls 30% together, each netted like rule 1. Watch when the cluster loses 15% of NLV; it never acts. Not evaluated until you declare a cluster. | Track |
| 18 | Issuer loss against risk capital | One issuer's worst-case loss against the constitution's effective risk capital. Watch at 100% of it; it never acts. Unknown, never a pass, until the constitution carries the numbers. | Alert |

`alert` rules can create alert episodes, `track` rules remain visible without
creating alerts, and `off` rules are not evaluated. Rules 16 to 18 only watch:
they never act, never count toward act totals, and never drive a trim.

## One issuer, one measure

Rule 1 is Canary's single definition of concentration. An issuer is an
underlying joined with the share classes and ADR or ordinary lines you list
under `issuer_groups` (Canary has no issuer data, so an unlisted symbol is its
own issuer). Its worst-case loss comes from valuing every leg on intrinsic
payoffs at zero, every strike, today's price and a 100% rise
(`takeover_gap_pct`), from current marks: long stock can lose its value, a long
option its premium, a short put its strike notional less what it is worth
now, and a covered call credits only its premium. An early assignment realizes
exactly a short leg's intrinsic value, so the netting holds if any short leg is
assigned. Index options and options on other underlyings give an issuer no
credit. The risk-reduction trim starts at the act level and goes back to the
watch level on this same measure, and the stress read uses rule 1 and rule 16
for its concentration row rather than measuring concentration itself.

## Set your own limits

Every threshold and mode in the table is yours to change. They live in
`~/.config/ibkr/policies/rulebook-policy.toml` (or `[rulebook].policy_file`),
which the installer and each daemon start write from Canary's defaults, policy
`rulebook-v4`, when it is missing. Until you delete its
`# Canary defaults, not yet reviewed.` line it reads `default, unreviewed`.
See the limits in force:

```sh
canary rules policy
```

Change one, turn a rule off or up, or return to Canary's defaults:

```sh
canary rules policy set cash_reserve_min_pct=70
canary rules policy set modes.net_exposure=alert
canary rules policy reset cash_reserve_min_pct
canary rules policy reset --all
```

`set` edits the file in place: it changes only the lines of the keys you
name, keeps every other value and comment, raises `policy_version`, and refuses
an unknown key or an invalid value before writing anything. `reset KEY` writes
Canary's current default for that key, and `reset --all` rewrites the file
from Canary's template after keeping the old one as a backup. Declare an
issuer group or a cluster with `set issuer_groups.NAME=AAA,AAB` and remove it
with `reset issuer_groups.NAME`. `set` refuses `cash_sell_only_pct`, which no
rule reads; a file that still carries it loads with the key ignored, a note in
`policy_status` names it, and any `set` or `reset` removes it. The daemon
applies the file within 30 seconds. Hand edits work as well and apply only
with a higher `policy_version`. A key missing from the file follows Canary's
default, `canary rules policy` lists it under `not in file`, and the next
upgrade adds it at that default. A file the daemon cannot read or validate
never replaces the limits in force, and `canary rules` names the problem. A
deleted file comes back as Canary's template at the next daemon start. Every result says where its limits came from:
`policy_status` names the baseline or your file, and `policy` carries every
threshold. Agent sessions can read the limits; only you can change them.

Rules 4 and 12 take their thresholds from the classified regime stage, so
the same book can pass in a calm regime and breach in a confirmed one. A stale
or never-observed stage is evaluated against both its own threshold set and
the calm set, keeping the worse verdict: old market state may tighten a rule,
never relax it.

The baseline ships as policy `rulebook-v4`, and every row carries its
`observed` value, `threshold`, and an evidence string, so you can check the
arithmetic instead of trusting the verdict. `threshold` is the limit for the
row's status: the act level on an `act` row, the watch level otherwise, and the
evidence quotes that same number. Rules with a watch and an act level also
carry both as `watch_threshold` and `act_threshold`. A reading exactly at a
level is in that level. Two rules read differently: expiry runway counts
down and triggers at its limits (watch at 14 days or fewer, act at 7 or
fewer), and index protection is a range whose edges are inside it. A baseline value is not itself proof that the
threshold has your approval; set the ones you have decided.

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

It means eighteen specific checks did not fire on the book as the daemon last
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
