# Opportunity research harness

Updated: 2026-07-25 09:45 CEST

The opportunity research harness tests stock-candidate strategies before you
trust them. It is diagnostic only: it captures point-in-time evidence, scores
later outcomes, compares predeclared strategy plans, and reports whether the
evidence is still too weak.

It does not place orders, preview orders, or convert a passing signal into
investment advice.

## Mental model

The harness has four stages:

1. Capture point-in-time rows.
2. Wait until the forward outcome window is observable.
3. Score the captured rows against adjusted bars and benchmark returns.
4. Run research plans over the scored rows.

The important design rule is immutability. A row's `features` are the
point-in-time evidence. `feature_provenance.checksum` proves those features
were not edited after capture. If a strategy needs macro context, quote
freshness, or technical state, that context must be captured into `features`
before the checksum is computed.

## Where it lives

The harness is a local CLI research surface:

- `internal/cli/backtest_opportunity_build.go` owns PIT row construction,
  feature checksums, live capture, and scored observation validation.
- `internal/cli/backtest_opportunity_score.go` labels captured rows once the
  forward window can be observed.
- `internal/cli/backtest_opportunity_research.go` registers predeclared
  strategy plans and ranks their diagnostics.
- The daemon is only a read-only data source during live capture. Strategy
  policy stays in the harness, not in `internal/daemon`, MCP, or broker-write
  paths, so a strategy can be researched without becoming execution policy.

## Capture candidates

Scanner capture collects current market candidates:

```sh
canary backtest capture-opportunity \
  --preset top-movers \
  --append build/backtest/opportunity-pit.jsonl \
  --json
```

Swap `--preset` for `--symbols NVDA,MSFT,AVGO` when you already have a
watchlist. Two further flags change what a captured row carries:

| Flag | Effect |
| --- | --- |
| `--include-regime` | Captures the macro/regime context a plan will consume. |
| `--split holdout --holdout-plan 2026q3-opportunity-research` | Pre-registers the row as protected holdout evidence at capture time. |

Rows captured this way are intentionally unscored. Do not edit feature fields
after capture.

## Score rows

After the forward window is observable, export bars and score the PIT ledger:

```sh
canary backtest export-opportunity-bars \
  --symbols NVDA,MSFT,AVGO,QQQ \
  --bars build/backtest/opportunity-bars.jsonl \
  --bars-manifest build/backtest/opportunity-bars.manifest.json \
  --benchmark QQQ \
  --json

canary backtest score-opportunity \
  --input build/backtest/opportunity-pit.jsonl \
  --bars build/backtest/opportunity-bars.jsonl \
  --bars-manifest build/backtest/opportunity-bars.manifest.json \
  --target-policy net-excess-positive \
  > build/backtest/opportunity-scored.jsonl
```

Scoring turns captured candidates into rows with forward return, benchmark
return, excess return, adverse excursion, favorable excursion, and a target
label.

## Compare plans

List registered plans:

```sh
canary backtest research-opportunity --list-plans
```

Compare selected plans, or pass `--plan all` for every registered plan. Add
`--json` for notebooks, spreadsheets, or repeatable notes:

```sh
canary backtest research-opportunity \
  --input build/backtest/opportunity-scored.jsonl \
  --plan pullback_uptrend_rs63_v1,pullback_uptrend_rs63_macro_veto_v1
```

Treat the output as evidence triage. Prefer plans only when holdout behavior
improves net excess return, hit rate, drawdown behavior, and sample quality
without collapsing the number of fired samples. Tuning lift alone is not alpha.

## Strategy shape

A strategy plan is a small function that maps
`OpportunityPointInTimeFeatures` to an `OpportunityBacktestSignal`.

Good plans should:

- reuse shared freshness, session, quote-quality, and liquidity gates
- emit stable reason tokens
- distinguish dirty context from feature filters
- keep thresholds simple enough to falsify
- run only against point-in-time features
- have tests for pass, fail, and dirty-context cases

Avoid plans that quietly depend on current data, mutate features, scrape
external context during evaluation, or rank by information unavailable at the
capture date.

## Example: macro-veto pullback

`pullback_uptrend_rs63_macro_veto_v1` is the first concrete macro-aware
example. It starts with `pullback_uptrend_rs63_v1`:

- price is above the 200-day moving average
- price is no more than 5% above the 50-day moving average
- RS63 versus the benchmark is positive
- shared context and liquidity gates pass

Then it requires captured regime context. It allows `normal` and `watch` tones,
and blocks `stress`, `risk_off`, `data_quality`, missing macro context, regime
RPC errors, or unknown tones.

Useful reason tokens:

- `passed_pullback_uptrend_rs63_macro_veto_v1`: the strategy fired
- `macro_context_missing`: row was not captured with regime context
- `macro_context_error`: regime snapshot failed during capture
- `macro_data_quality_veto`: regime context was present but degraded
- `macro_stress_veto`: broad-market stress blocked the candidate
- `macro_risk_off_veto`: risk-off context blocked the candidate

The research question is narrow: does vetoing confirmed broad-market stress
improve the pullback rule's forward excess returns compared with the plain
pullback rule?

## Candidate ranking today

The harness does not yet emit a live ranked buy list. It captures candidates
and later measures strategy quality.

If several same-day candidates need manual review, keep ranking simple:

- clean live context before dirty context
- plans with no context blockers before blocked rows
- stronger RS63 before weaker RS63
- higher dollar liquidity before thinner names
- less extension above the 50-day moving average before stretched entries
- non-degraded macro context before `data_quality` macro tone

The next useful extension is a read-only candidate scorer that applies selected
plans to captured rows and prints a shortlist, still without order preview or
broker writes.

## Adding another strategy

Add one plan at a time:

1. Name the hypothesis in plain English.
2. Add the plan to `opportunitySignalPlans()`.
3. Reuse `opportunityResearchContextReasons()` unless the plan has a deliberate
   reason not to.
4. Use stable reason tokens for every blocker.
5. Add focused tests for pass, intended veto, dirty context, and list-plan
   discovery.
6. Compare against a simple baseline and holdout before trusting it.

Keep the bar boring and measurable. A strategy is useful only when the harness
can show where it passed, where it failed, and whether the gain survived
holdout evidence.
