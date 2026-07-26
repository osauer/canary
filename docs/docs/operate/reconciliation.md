# Reconciliation

Updated: 2026-07-25 12:07 CEST

`canary recon` matches the external cash flows on your IBKR Flex statements
against the capital events you declared. The statement is the broker's record.
The declared ledger and the local order journal are claims about intent, never
truth, so when the two disagree the report says so rather than splitting the
difference.

One number depends on this. Drawdown is measured from a cash-flow-adjusted peak,
so equity that moved because you moved money has to be separated from equity
that moved because the market did. Declaring a loss as a withdrawal is the one
channel that could understate drawdown, and it lands here as a `ledger_only`
exception.

`canary recon` is CLI-only and advisory. It has no MCP tool, and nothing on this
page touches submit eligibility, freeze, pins, or any order path.

## Running it

```sh
canary recon
canary recon --refresh
canary recon equity
canary recon backtest
```

With no subcommand it runs `show`. `--refresh` kicks one background Flex fetch
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

Most of the loop runs without you. The daemon makes its first Flex fetch attempt
at 06:30 Europe/Berlin on each local calendar day and retries a temporary
failure every 30 minutes. Every ingest regenerates the report from the retained
files, so the result is deterministic and the report id pins its content.

Under risk-policy v3 a clean report extends the reconcile clock by itself. The
conditions are checked rather than assumed: the policy active, the report free
of unresolved exceptions, the statement fresher than `max_report_age_days`, and
a same-day statement-versus-runtime equity comparison inside
`max_equity_divergence_pct`. When that holds, the extension is journaled against
the report id and `canary recon` says the clock was extended automatically. When
it does not hold, the report says the automatic extension was not recorded and
leaves the clock alone.

So the human job is the exception and the deliberate case, not a weekly ritual.
`canary policy capital-event reconcile` remains for a report carrying exceptions
and for a sign-off you want on the record. During a statement-source outage the
sanctioned path is a one-shot, expiring, journaled override on
`capital.max_unreconciled_days`, which keeps the outage visible instead of
quiet. If the clock does run out, capital evaluation reports the ledger as past
its reconcile horizon with no current automatic extension or human sign-off.

The equity comparison itself is a data-quality reading about the runtime
sampler, not an exception to clear. [Trading policy](../understand/policy.md)
owns the capital-event vocabulary and the drawdown state this feeds.
