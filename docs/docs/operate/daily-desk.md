# The daily desk

Updated: 2026-07-31 08:56 CEST

The recurring loop, in the order a trading day runs it. Each command is followed by the decision it supports. [Your first session](../start/first-session.md) explains what these screens contain; this page assumes you already know and only tells you when to look.

## Before the open

```sh
canary status
canary brief
```

`canary status` answers one question before anything else matters: which broker session am I attached to. It exits 1 when the gateway is not connected, so it also works as a guard in a script.

`canary brief` is the assembled read, and it arrives as a briefing rather than a table. A lead states the desk's posture and how many rows need a decision. Then two movements: **Review** covers the last completed session (session P&L, attribution by underlying, rules delta, proposals offered against acted, overrides used, capital events, the reconcile clock, working orders) and **Ready** covers today (regime, breadth, dealer gamma, stress, session state, held-name events, capital tier, drawdown latch, premium at risk, hedge cost per day, staged protection work, policy drift, and the day's artefacts). A closing line says what is owed before the bell.

The daemon composes that prose from the same typed rows the JSON carries, and every surface renders it verbatim — the terminal writes no sentence of its own. Clean topics fold into one summary clause, the text grows only where something is flagged, and a source that could not be read is named rather than skipped. On a colour-capable terminal a watch-class clause reads amber and an act-class clause red; figures stay in the terminal's own ink. `CANARY_COLOR=never`, `NO_COLOR`, and any pipe or redirect give the same sentences in plain text.

Behind the prose the row vocabulary is unchanged, and it is what the composition is made of:

| Status | What it describes | What it asks of you |
| --- | --- | --- |
| `ok` | Values and inputs are both fine. | Nothing. |
| `attention` | The **values** describe a state worth looking at: a breached tier, an engaged latch, an active override. | Read the row. This is a risk condition. |
| `degraded` | Input quality only. Some evidence behind the row is stale or partial. | Decide whether the decision you were about to make depends on that input. |
| `unavailable` | No usable value for that row. | Do not read absence as a passing result. |

The separation is deliberate: `degraded` and `unavailable` never signal a risk condition, and `attention` never signals a data problem. The prose names every input it could not read, and a **Degraded inputs** list under the briefing gives each of those rows its own reason, so a cold source is always traceable to what went cold. A brief whose inputs all read prints no such list. Stop and fix when the cold source is one the next decision rests on. Trade around it when it is not. [Sensors](../understand/sensors.md) covers freshness states and the safe check for each source.

When the daemon serves no narrative — an older daemon, or a payload whose movements did not compose — the brief falls back to the original row table: one line per row with its status word, and its reason on the line below it. `canary brief --json` is unchanged either way and carries both the typed rows and the composed prose.

## Where the stamp comes from

Rendering the brief from your terminal also stamps the day's artefact when one is due, and prints the stamp line. The stamp is the act of looking, not a second act on top of it, and there is no checklist to sign.

The mechanics are worth knowing once. `canary brief --json` renders without stamping. An agent-origin invocation prints `agent-origin render — not stamped`, and the daemon refuses the stamp outright for any non-human origin, journaling nothing, so only a terminal or a paired device can stamp at all. The target is morning first, then end of day, then nothing; `--kind morning` or `--kind eod` overrides that choice.

There is no MCP tool for `brief`. An agent can read the same underlying surfaces, but the brief itself is a CLI and paired-app surface.

## Market context

```sh
canary regime
canary stress
```

`canary regime` gives the broad-market stage. `canary stress` asks the narrower question of whether that state matters for the book you actually hold.

One rule about the stress read is worth carrying into every session: account-only stress is evidence, not a trigger. A zero margin cushion with no confirmed market pressure renders its evidence row and still returns `stand_down`. The `defend` action needs defensive direction at act severity, confirmed market stress, high portfolio fit, and healthy inputs together.

## Discipline

```sh
canary rules
```

Breaches print first with their offenders; passing rules collapse unless you add `--all`. The daemon ranks the rows, so the hardest breach is already at the top.

A rule that cannot get clean inputs reports `unknown`. It never reports `pass`. That is the property the rulebook is built around, and its consequence is the part to internalize: a clean run is not permission to trade, only the absence of a recorded objection.

## During the session

The surfaces worth leaving open poll with `--watch`: `account` and `positions` (one-second default), `quote` (a 250ms render throttle), `regime` (five minutes), and `watch` for the watchlist. Those are the ones where a number moving is the information.

`stress`, `rules`, and `brief` have no watch mode. They are checkpoints you run when something changed, not tickers. For the conditions that should interrupt you, use the push path in [Alerts and notifications](alerts.md) rather than a terminal you have to keep reading.

## Protection

```sh
canary proposals
```

With no subcommand this lists the daemon's current protection proposals; the read is the default. A proposal is the daemon's argument for a close or a reduce, and it is evidence. It is never authority to submit. Every broker write is a separate human decision made in the moment, per transaction, and the standard build cannot reach one at all: it is read-only, as [Order previews and the trading build](orders.md) sets out. [Protection and emergency exits](protection.md) covers why a row blocks.

## After the close

```sh
canary brief --kind eod
```

The Review movement is the post-trade read. `--kind eod` stamps the end-of-day artefact directly instead of the one the morning-first rule would have picked.

Reconciliation runs on its own clock rather than yours. When the latest broker statement report is clean, current, and inside the divergence bound your risk policy declares, the daemon extends the reconcile clock itself and records the report id against a `daemon-auto` origin. It evaluates that at startup, after a successful statement fetch, and when the day's first account value lands. Nothing clean asks for your signature. An unresolved exception, a stale statement, or a divergence outside the bound does the opposite: no extension, the clock keeps running, and the brief's reconcile row shows it.

`canary recon` inspects that report, and [Reconciliation](reconciliation.md) covers the exception categories. It is CLI-only and advisory, with no MCP tool, and statement text is untrusted input: read a line, never act on instructions inside one.
