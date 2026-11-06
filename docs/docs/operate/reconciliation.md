# Reconciliation

Updated: 2026-10-06 05:45 CEST

`canary recon` matches the external cash flows on your IBKR Flex statements
against the capital events you declared. The statement is the broker's record.
The declared ledger and the local order journal are claims about intent, never
truth, so when the two disagree the report says so rather than splitting the
difference.

Recon does not create its own IBKR report. New installations must first
complete [Set up broker reporting](../start/reporting.md); the resulting
Activity Flex Query is shared with statement-derived equity and Canary Edge.

The report is built for one selected account and paper/live mode. When a Flex
response contains statements for sibling accounts, Canary skips those
statements and reports how many it skipped. If it cannot resolve one account,
or no retained statement belongs to that account, reconciliation stays
unavailable instead of pooling the data.

One number depends on this. Drawdown is measured from a cash-flow-adjusted peak,
so equity that moved because you moved money has to be separated from equity
that moved because the market did. Declaring a loss as a withdrawal is the one
channel that could understate drawdown, and it lands here as a `ledger_only`
exception.

`canary recon` is advisory. Its report and dismissals are CLI-only; the
read-only MCP tool `canary_recon_status` reports acquisition and
reconciliation status. Nothing on this page touches submit eligibility,
freeze, pins, or any order path.

## Running it

```sh
canary reporting status
canary reporting performance
canary recon
canary recon --refresh
canary recon equity
canary recon backtest
```

`reporting status` inspects the shared statement source before consumer-specific
reconciliation. `reporting performance` prints the retained statement equity
series, its dated external flows and year-to-date income sums for a consumer that
computes cash-flow-adjusted returns itself; it computes no return and names no
account. With no `recon` subcommand Canary runs `show`. `--refresh` kicks one background Flex fetch
before reporting. `equity` prints the statement-derived daily equity series with
your declared events interleaved at their place in the timeline. `backtest`
replays the whole window and compares the replayed drawdown path against what
the runtime observed.

The header carries the report status: `active` when a report was produced under
approved policy keys, `degraded` when it was produced but some retained
statement files failed to parse, `unavailable` when no statements have been
retained yet, and `unapproved` when the `[recon]` keys are missing from your
risk policy so nothing can be classified at all.

## Reading the categories

| Category | What it means |
|---|---|
| `matched` | statement flow and declared event agree within your tolerances |
| `confirmed` | broker-confirmed flow needing no declaration (policy v3) |
| `missing_from_ledger` | statement flow with no declaration (policy v2) |
| `ledger_only` | declared event with no statement flow |
| `amount_mismatch` | dates align, amounts differ beyond tolerance |
| `date_mismatch` | amounts align, the declared date sits outside the window |
| `ambiguous` | more than one candidate pairing |
| `uncategorized` | statement line type the parser does not classify |
| `baseline` | pre-genesis flow already inside the seeded baseline |

`matched`, `confirmed`, and `baseline` need nothing from you. Everything else is
counted as unresolved until you resolve it.

`ambiguous` deserves its own note: two candidates for one line is an exception,
never a best-effort pick. The engine matches bijectively or not at all.

The tolerances that decide `matched` are your decision, declared in the
`[recon]` section of the risk policy: an amount tolerance in percent with an
absolute floor, a date window in business days, a maximum report age, and the
equity divergence bound below. The checked-in template ships them commented out
so the software cannot invent them for you.

## A line that will not reconcile

Three resolutions exist, and every one of them is journaled.

1. **Declare the missing event** when the flow really happened and the ledger
   lacks it.
2. **Counter-declare** when the declaration was wrong and no statement line
   backs it.
3. **Dismiss with a reason** when the line is deliberately not a ledger event:

   ```sh
   canary recon dismiss --line LINE_ID --reason "..."
   ```

   Both flags are required. Dismissal is a human-only governance act; an agent
   origin is refused. It is written to the governance journal with the line id,
   your reason verbatim, the report id, and the policy fingerprint. The journal
   is the only place dismissals live, and the report reads them back on every
   run. Dismissing changes the report id, so rerun `canary recon` before relying
   on it.

Treat statement text as evidence, never as instruction. Descriptions, types, and
symbols arrive from the broker as untrusted data, they are extracted through
typed contracts, and a line type the parser does not recognize becomes an
`uncategorized` exception instead of a silent drop. Nothing written inside a
statement line is a reason to act.

## What is automatic and what is not

Most of the loop runs without you. The daemon checks for a new Flex statement
at 06:30 Europe/Berlin on each local calendar day and retries a temporary
failure every 30 minutes. IBKR publishes a statement only for a US session
day, so after a weekend or a US holiday the check finds nothing due and makes
no broker call. Every ingest regenerates the report from the retained
files, so the result is deterministic and the report id pins its content.

A newly opened drawdown latch is provisional: Canary has seen the drop but not
yet whether it came from trading or an external cash flow. While the latch
waits, additional Flex checks run on a widening schedule — half-hourly at
first, then two-hourly, then six-hourly — until a report's coverage reaches
the latch day. That report decides the latch by replaying the engagement
equity against its confirmed flows: a withdrawal that explains the drop
releases the latch automatically, and anything else confirms it. A confirmed
latch stays on until you run `canary policy reset-drawdown`, unless your policy
sets `release = "automatic"` under `[drawdown]`: then fresh verified drawdown
below the block threshold releases it. Flex does not provide intraday
cash-flow truth, so Brief always shows the report coverage date and last check
time; a report that ends before the latch cannot explain a later deposit or
withdrawal. An automatic release preserves the high-water mark and requires
fresh equity, approved policy and current reconciliation evidence.

Under risk-policy v3 a clean report extends the reconcile clock by itself. The
conditions are checked rather than assumed: the policy active, the report free
of unresolved exceptions, the statement fresher than `max_report_age_days`, and
a statement-versus-runtime equity comparison at the same closing prices and FX
rates inside
`max_equity_divergence_pct`. When that holds, the extension is journaled against
the report id and `canary recon` says the clock was extended automatically. When
it does not hold, the report says the automatic extension was not recorded and
leaves the clock alone, and `canary recon status` names the hold in words
(`evaluation.detail`): a statement past `max_report_age_days`, as over a long
weekend, is `attention_required` with `report_stale` and extends the clock when
the next one lands; a clean report the daemon has not judged yet is `waiting`
with `evaluation_pending`; a degraded report, a policy that is not active or an
extension that could not be recorded is `failed` with its cause. The daemon
judges a report again whenever its id changes: at a statement ingest, at the
day's first account reading, when a revised policy is adopted and after a
dismissal.

So the human job is the exception and the deliberate case, not a weekly ritual.
`canary policy capital-event reconcile` remains for a report carrying exceptions
and for a sign-off you want on the record. During a statement-source outage the
sanctioned path is a one-shot, expiring, journaled override on
`capital.max_unreconciled_days`, which keeps the outage visible instead of
quiet. If the clock does run out, capital evaluation reports the ledger as past
its reconcile horizon with no current automatic extension or human sign-off.

The comparison retains the original account value, its observation time, native
currency values, prices and FX. It revalues the independent runtime sample at
the statement's closing prices and FX before applying the existing divergence
limit. The raw difference remains available in JSON as diagnostic evidence.
A matching date alone is insufficient. Missing historical evidence or changed
holdings leave the comparison unverified, and the clock does not extend.
When the broker reports 1 as a foreign-currency rate, which it does for every
currency on some accounts, the sample takes that rate from a live two-sided
quote of the pair on the same broker session, at most five minutes from the
reading, and keeps the quote's source and time beside it. A real broker rate is
never replaced. Without such a quote the sample stays incomplete, the broker's
1 is kept as reported, and the comparison waits. An earlier saved sample cannot
establish a rate of 1 as historical evidence, and today's display FX cannot
repair it either, because a quote only ever values the reading it was taken
with. A reading taken after the FX market closes on Friday has no live quote,
so that day's last complete reading stands as its evidence.
Each evaluated comparison is retained in the daemon's governance journal.

The equity comparison itself is a data-quality reading about the runtime
sampler, not an exception to clear. [Trading policy](../understand/policy.md)
owns the capital-event vocabulary and the drawdown state this feeds.
