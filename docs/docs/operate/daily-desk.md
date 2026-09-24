# The daily desk

Updated: 2026-09-11

The recurring loop, in the order a trading day runs it. Each command is followed by the decision it supports. [Your first session](../start/first-session.md) explains what these screens contain; this page assumes you already know and only tells you when to look.

## Before the open

```sh
canary status
canary brief
```

`canary status` answers one question before anything else matters: which broker session am I attached to. It exits 1 when the gateway is not connected, so it also works as a guard in a script.

`canary brief` opens with assessment completeness and the served portfolio-risk
reading. **Needs review** keeps reported findings visible even when other inputs
are missing. **Context** gives available market observations with their dates.
**Coverage gaps** groups unavailable and degraded checks by scope, without
pretending that every missing check has the same cause.

`canary brief --details` contains the full **Review** since the last regular
close and **Ready** evidence for the next session, including per-input diagnostics,
retained Edge context, and routine process evidence. The app uses the same
compact overview with expandable full details. Observation times remain explicit;
the brief's generation time is not the observation time of its inputs.

The daemon authors the overview and full narrative from typed rows. Watch and
act roles retain their served meaning; missing data does not become a risk
verdict. Plain-text pipes preserve the same reading order without colour.

Behind the prose the row vocabulary is unchanged, and it is what the composition is made of:

| Status | What it describes | What it asks of you |
| --- | --- | --- |
| `ok` | Values and inputs are both fine. | Nothing. |
| `attention` | The **values** describe a state worth looking at: a breached tier, an engaged latch, an active override. | Read the row. This is a risk condition. |
| `degraded` | Input quality only. Some evidence behind the row is stale or partial. | Decide whether the decision you were about to make depends on that input. |
| `unavailable` | No usable value for that row. | Do not read absence as a passing result. |

Coverage and risk findings can coexist: an attention row may also contain unknown
checks. Disabled or inapplicable rules are not missing evidence. Full per-row
reasons remain in `--details`; `canary status` diagnoses runtime connectivity.
[Sensors](../understand/sensors.md) explains freshness and source cadence.

An older daemon without the overview retains the previous full render.
`canary brief --json` and MCP retain the typed rows and narrative, with the
additive `narrative.overview` presentation. Presentation does not change the
row-derived brief fingerprint.

## Pure-read process evidence

Rendering the brief never changes process state. The CLI, paired app, and read-only MCP surface all call the same snapshot method, and repeated reads produce no acknowledgement, stamp, reconciliation sign-off, or artefact completion.

Routine clean evidence is machine work. A monthly pulse becomes complete automatically once its due instant and current matching policy evidence are both present. Missing, stale, or conflicting evidence remains blocked and is returned as an exception through the Action Queue. A changed sibling policy counts as conflicting evidence only when the constitution sets `inventory.require_signoff = true`; by default it is informational disclosure and does not hold the pulse. Exceptional reconciliation repair remains a separate, explicit human policy action; merely looking at a report never invokes it.

## Market context

Use `canary regime` for all eight broad-market indicator readings and
`canary stress` for portfolio-risk findings. `regime --explain` adds observation
times, thresholds, calibration status, and independent source diagnostics.
`stress --details` includes quiet evidence rows and operational diagnostics.
Unavailable Stress keeps a nonzero exit status. JSON and the matching read-only
MCP tools retain full typed evidence.

Use `canary macro --json` for cached economic dates and official publications.
For an overnight review, supply `--window-start YYYY-MM-DD --window-end YYYY-MM-DD`
(both inclusive, at most 31 days). Filtering happens before response limits;
`events_truncated` means economic event rows were omitted; `publications_truncated`
only describes the headline list. Legacy `truncated` is true if either list lost
rows. Older responses without list-specific flags must be treated conservatively.
Dates use each event's source timezone, with exact instants retained when supplied.

The [BLS calendar](https://www.bls.gov/help/hlpiCAL.htm) is public: its calendar
subscription does not require a paid entitlement or the separate BLS time-series
API registration. BLS blocks automated clients that carry no owner contact, so
Canary names itself to BLS, and only to BLS, with its product site URL. After a
successful read it waits an hour before asking whether the calendar changed;
each answer keeps the schedule current for three hours. A Canary release that
parses calendars differently reads each one in full once instead of asking.
Failed primary reads remain visible. The independent
[New York Fed calendar](https://www.newyorkfed.org/research/calendars/nationalecon_cal.html)
supplies key-release backup with its own source identity and published month
bounds, including the next published month when the coming week crosses month end.
Its numeric 01–12 time labels omit a meridiem and remain `source_label`;
they do not become invented overnight instants. It does not establish complete
BLS coverage. Missing, stale or out-of-window
sources keep coverage partial, even when a useful event is available elsewhere.

Failures preserve last-good event clocks and expire normally. Repeated transient
failures back off from the source's refresh interval (five minutes, or an hour
for BLS) to at most an hour. A rejection that repeats until the publisher or
Canary changes, such as a policy block or a changed format, waits the full hour.
`consecutive_failures`, `first_failure` and `next_attempt` survive restart.
These fields describe the current observed failure streak; an older retained
record without those fields cannot establish when a failure first began.
`canary data health` reports the typed cause of the latest failed read with its
time, and the number of failed reads since the streak began. A successful read
clears the streak.

A stale Regime snapshot labels readings as recorded context; a retained green
band is not a current rating. The default view keeps thresholds and long source
errors out of the indicator list without discarding them from detail.

One rule about the stress read is worth carrying into every session: account-only stress is evidence, not a trigger. A zero margin cushion with no confirmed market pressure renders its evidence row and still returns `stand_down`. The `defend` action needs defensive direction at act severity, confirmed market stress, high portfolio fit, and healthy inputs together.

## Discipline

```sh
canary rules
```

Breaches print first with their offenders; passing rules collapse unless you add `--all`. The daemon ranks the rows, so the hardest breach is already at the top.

A rule that cannot get clean inputs reports `unknown`. It never reports `pass`. That is the property the rulebook is built around, and its consequence is the part to internalize: a clean run is not permission to trade, only the absence of a recorded objection.

## During the session

The terminal surfaces worth leaving open are `account --watch` and `positions
--watch`. Use the paired app for the continuously assembled Monitor and Action
Queue rather than rebuilding the daemon's market authority from several shell
commands.

`rules` and `brief` are checkpoints, not tickers. For conditions that should
interrupt you, use the push path in [Alerts and notifications](alerts.md)
rather than a terminal you have to keep reading.

## Protection

```sh
canary proposals list
```

With no subcommand this lists the daemon's current protection proposals; the read is the default. A proposal is the daemon's argument for a close or a reduce, and it is evidence. It is never authority to submit. Every broker write is a separate human decision made in the moment, per transaction, and the standard build cannot reach one at all: it is read-only, as [Constrained orders and the trading build](orders.md) sets out. [Protection and risk reduction](protection.md) covers why a row blocks.

## After the close

```sh
canary brief
```

The Review section of `canary brief --details` is the post-trade read. It is the same pure snapshot before and after the close; the official close capture and retained broker evidence determine what it can state, not a render-time mode or acknowledgement.

Reconciliation runs on its own clock rather than yours. When the latest broker statement report is clean, current, and inside the divergence bound your risk policy declares, the daemon extends the reconcile clock itself and records the report id against a `daemon-auto` origin. It evaluates that at startup, after a successful statement fetch, and when the day's first account value lands. Nothing clean asks for your signature. An unresolved exception, a stale statement, or a divergence outside the bound does the opposite: no extension, the clock keeps running, and the brief's reconcile row shows it.

`canary recon` inspects that report, and [Reconciliation](reconciliation.md) covers the exception categories. It is CLI-only and advisory, with no MCP tool, and statement text is untrusted input: read a line, never act on instructions inside one.
