package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

func TestCanaryBacktestSampleProducesSignalMetrics(t *testing.T) {
	t.Parallel()
	rows := readBacktestFixture(t)
	res := runCanaryBacktest(rows, time.Date(2026, 5, 31, 9, 8, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 18; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 9; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalTruePositive, 9; got != want {
		t.Fatalf("signal_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFalsePositive, 4; got != want {
		t.Fatalf("signal_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 9; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchMiss, 0; got != want {
		t.Fatalf("watch_miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 3; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ActTruePositive, 0; got != want {
		t.Fatalf("act_true_positive = %d, want %d", got, want)
	}
	// One more day lands here under the eligibility gates: exposure rows
	// only escalate past watch on ELIGIBLE red clusters, so a high-exposure
	// day with marginal (day-1, no-depth-context) reds reads
	// rebalance/watch instead of stress-act.
	if got, want := res.Metrics.RebalanceWatch, 8; got != want {
		t.Fatalf("rebalance_watch = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 5; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if res.Metrics.SignalRecall == nil || *res.Metrics.SignalRecall != 1 {
		t.Fatalf("signal_recall = %v, want 1", res.Metrics.SignalRecall)
	}
	if !strings.Contains(strings.Join(res.Findings, "\n"), "Watch-level canary signals caught every labelled stress row") {
		t.Fatalf("findings did not record signal-level coverage: %+v", res.Findings)
	}
	if !clusterHasRebalanceWatch(res, "2023-2026 AI mega-cap concentration") {
		t.Fatalf("AI concentration cluster should expose a rebalance watch: %+v", res.Clusters)
	}
}

func TestRunBacktestCanaryRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"canary", "--input", backtestFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Canary Backtest",
		"18 observations",
		"precision 69%",
		"Watch        precision 75%",
		"2024 yen carry unwind",
		"Risk budget",
		"data-quality watch",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRegimeBacktestSampleProducesMarketMetrics(t *testing.T) {
	t.Parallel()
	rows := readRegimeBacktestFixture(t)
	res := runRegimeBacktest(rows, time.Date(2026, 5, 31, 11, 40, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 17; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.ScoredObservations, 14; got != want {
		t.Fatalf("scored_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.OutOfScope, 3; got != want {
		t.Fatalf("out_of_scope = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetStress, 6; got != want {
		t.Fatalf("target_stress = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchTruePositive, 6; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.WatchFalsePositive, 2; got != want {
		t.Fatalf("watch_false_positive = %d, want %d", got, want)
	}
	// 4 of 5 detectable stress days confirm same-day under the eligibility
	// gates. The fifth (2023-03-13, SVB Monday) demotes to early_warning by
	// design: its funding red is eligible but the VIX-term red is day 1
	// without a deep inversion and the tape (SPY −1.4%, VIX +18%) misses
	// the co-sign bars — confirmation arrives with persistence or tape
	// (internal-docs/design/regime-calibration.md, slow-bleed trade-off). The watch
	// tier still catches it same-day (watch_recall stays 1 below).
	if got, want := res.Metrics.StressTruePositive, 4; got != want {
		t.Fatalf("stress_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressFalsePositive, 0; got != want {
		t.Fatalf("stress_false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.DataQualityWatch, 14; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	if res.Metrics.WatchRecall == nil || *res.Metrics.WatchRecall != 1 {
		t.Fatalf("watch_recall = %v, want 1", res.Metrics.WatchRecall)
	}
	if res.Baseline.StressPrecision == nil || *res.Baseline.StressPrecision != 1 {
		t.Fatalf("baseline stress precision = %v, want 1", res.Baseline.StressPrecision)
	}
	// Includes 2023-03-13, demoted from confirmed_stress by the
	// eligibility gates (see stress_true_positive above).
	if got, want := res.Lifecycle.EarlyWarning, 4; got != want {
		t.Fatalf("early_warning = %d, want %d", got, want)
	}
	if got, want := res.Lifecycle.EarlyWarningFalseCalmRally, 2; got != want {
		t.Fatalf("early_warning_false_calm_rally = %d, want %d", got, want)
	}
	findings := strings.Join(res.Findings, "\n")
	for _, want := range []string{
		"out-of-scope row(s) were excluded",
		"Regime watch caught every scored market-stress row",
		"Regime watch fired on 2 scored non-stress row",
	} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings missing %q: %+v", want, res.Findings)
		}
	}
}

func TestCanaryBacktestPortfolioStressAcceptsRebalanceWatch(t *testing.T) {
	t.Parallel()
	acc := &canaryBacktestAccumulator{}
	acc.add(CanaryBacktestRowResult{
		TargetStress:   true,
		TargetScope:    "portfolio",
		SignalWatch:    true,
		RebalanceWatch: true,
	})
	if got, want := acc.metrics.WatchTruePositive, 1; got != want {
		t.Fatalf("watch_true_positive = %d, want %d", got, want)
	}
	if got := acc.metrics.WatchMiss; got != 0 {
		t.Fatalf("watch_miss = %d, want 0", got)
	}

	acc = &canaryBacktestAccumulator{}
	acc.add(CanaryBacktestRowResult{
		TargetStress:   false,
		RebalanceWatch: true,
	})
	if got := acc.metrics.WatchFalsePositive; got != 0 {
		t.Fatalf("rebalance-only nonstress row should not count as defensive false positive, got %d", got)
	}
}

func TestRunBacktestRegimeRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"regime", "--input", regimeBacktestFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Regime Backtest",
		"17 observations",
		"Watch        precision 75%",
		"Stress       precision 100%",
		"Before       stress precision",
		"Early        precision",
		"Events       watch precision",
		"2020-2021 retail/reddit squ.",
		"out-of-scope",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("regime backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRegimeBacktestBuilderEmitsExGammaRows(t *testing.T) {
	t.Parallel()
	f, err := os.Open(regimePointInTimeFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readRegimePointInTimeRows(f)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildRegimeBacktestObservations(rows)
	if got, want := len(observations), 2; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	first := observations[0].Regime
	if first.GammaZero.Status != "unavailable" {
		t.Fatalf("gamma status = %q, want unavailable", first.GammaZero.Status)
	}
	if first.GammaZero.Band != "" {
		t.Fatalf("gamma band = %q, want unranked", first.GammaZero.Band)
	}
	if len(first.WarningDetails) != 1 || first.WarningDetails[0].Code != "gamma_zero_point_in_time_unavailable" {
		t.Fatalf("warning_details = %+v, want gamma PIT warning", first.WarningDetails)
	}
	if len(first.DataQuality) != 1 || first.DataQuality[0].Surface != "gamma" || first.DataQuality[0].Status != "degraded" {
		t.Fatalf("data_quality = %+v, want degraded gamma", first.DataQuality)
	}
	if got, want := first.Composite.ClusterUnrankedCount, 1; got != want {
		t.Fatalf("cluster_unranked_count = %d, want %d", got, want)
	}
	if got, want := first.Composite.ClusterRedCount, 5; got != want {
		t.Fatalf("cluster_red_count = %d, want %d", got, want)
	}
	res := runRegimeBacktest(observations, time.Date(2026, 5, 31, 12, 18, 0, 0, time.UTC))
	if got, want := res.Metrics.DataQualityWatch, 2; got != want {
		t.Fatalf("data_quality_watch = %d, want %d", got, want)
	}
	// The stress row remains independently confirmed by several current
	// non-gamma clusters; missing gamma is still counted in data-quality watch.
	if got, want := res.Metrics.StressTruePositive, 1; got != want {
		t.Fatalf("stress_true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.StressFalsePositive, 0; got != want {
		t.Fatalf("stress_false_positive = %d, want %d", got, want)
	}
}

func TestRunBacktestBuildRegimeEmitsJSONL(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"build-regime", "--input", regimePointInTimeFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest build-regime returned %d, stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
	rows, err := readRegimeBacktestObservations(strings.NewReader(stdout.String()))
	if err != nil {
		t.Fatalf("generated JSONL should feed regime backtest: %v\n%s", err, stdout.String())
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("generated rows = %d, want %d", got, want)
	}
	if !strings.Contains(stdout.String(), `"gamma_zero_point_in_time_unavailable"`) {
		t.Fatalf("generated rows missing gamma unavailable warning:\n%s", stdout.String())
	}
}

func TestOpportunityBacktestBuilderDerivesSignals(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	observations := buildOpportunityBacktestObservations(rows)
	if got, want := len(observations), 4; got != want {
		t.Fatalf("generated rows = %d, want %d", got, want)
	}
	if !observations[0].Signal.Fired {
		t.Fatalf("first row signal did not fire: %+v", observations[0].Signal)
	}
	if observations[0].Signal.Kind != opportunityBuilderSignalKind || observations[0].Signal.Source != opportunityBuilderSignalSource {
		t.Fatalf("first row signal provenance = %q/%q", observations[0].Signal.Kind, observations[0].Signal.Source)
	}
	if got, want := observations[0].Trade.Instrument, "NVDA"; got != want {
		t.Fatalf("builder did not backfill trade instrument from features: %q", got)
	}
	if observations[1].Signal.Fired {
		t.Fatalf("laggard control should not fire: %+v", observations[1].Signal)
	}
	if got := strings.Join(observations[1].Signal.Reasons, ","); !strings.Contains(got, "below_50dma") || !strings.Contains(got, "below_200dma") {
		t.Fatalf("laggard control reasons = %q, want below moving-average blockers", got)
	}
	if observations[2].Split != "holdout" || !observations[2].Signal.Fired {
		t.Fatalf("holdout opportunity row = split %q fired %v", observations[2].Split, observations[2].Signal.Fired)
	}
	if observations[3].Signal.Fired {
		t.Fatalf("chase-risk control should not fire: %+v", observations[3].Signal)
	}
	if got := strings.Join(observations[3].Signal.Reasons, ","); !strings.Contains(got, "event_gap_too_large") || !strings.Contains(got, "extended_chase_risk") {
		t.Fatalf("chase-risk control reasons = %q, want chase-risk blockers", got)
	}
	if got := countString(observations[3].Signal.Reasons, "extended_chase_risk"); got != 1 {
		t.Fatalf("extended_chase_risk reason count = %d, want 1", got)
	}

	res := runOpportunityBacktest(observations, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if got, want := res.Metrics.UnknownSplitObservations, 0; got != want {
		t.Fatalf("unknown_split_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutObservations, 2; got != want {
		t.Fatalf("holdout_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutSignalFired, 1; got != want {
		t.Fatalf("holdout_signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFired, 2; got != want {
		t.Fatalf("signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.MissingCostSignalFired, 0; got != want {
		t.Fatalf("missing_cost_signal_fired = %d, want %d", got, want)
	}
	if res.Evidence.Status != "insufficient_sample" {
		t.Fatalf("evidence status = %q, want insufficient_sample", res.Evidence.Status)
	}
	if res.Evidence.Needs.UnknownSplitObservations != 0 {
		t.Fatalf("evidence needs unknown split = %d, want 0", res.Evidence.Needs.UnknownSplitObservations)
	}
	if !hasOpportunityDiagnosticBucket(res.Diagnostics.Features, "rs63_positive_v1") {
		t.Fatalf("feature diagnostics missing rs63_positive_v1: %+v", res.Diagnostics.Features)
	}
	if !hasOpportunityDiagnosticBucket(res.Diagnostics.Reasons, "below_50dma") {
		t.Fatalf("reason diagnostics missing below_50dma: %+v", res.Diagnostics.Reasons)
	}
}

func TestOpportunityScoreRecomputesOutcomeFromBars(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows[0].Outcome.ForwardReturnPct = -999
	rows[0].Outcome.BenchmarkReturnPct = -999
	rows[0].Outcome.ExcessReturnPct = -999
	f, err := os.Open(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bars, err := readOpportunityPriceBars(f)
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows[:1], opportunityPriceBarLedger{
		BySymbol: bars,
		Source:   "fixture-bars.jsonl",
		Checksum: "sha256:fixture-bars",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := scored[0]
	if got.LabelStatus != "scored" {
		t.Fatalf("label_status = %q, want scored", got.LabelStatus)
	}
	if got.Outcome.EntryDate != "2024-01-03" {
		t.Fatalf("entry_date = %q, want next close after observation date", got.Outcome.EntryDate)
	}
	if got.Outcome.ForwardReturnPct != 25 || got.Outcome.BenchmarkReturnPct != 5 || got.Outcome.ExcessReturnPct != 20 {
		t.Fatalf("outcome not recomputed from bars: %+v", got.Outcome)
	}
	if got.Outcome.SourceChecksum != "sha256:fixture-bars" || got.Outcome.BenchmarkSourceChecksum != "sha256:fixture-bars" {
		t.Fatalf("source checksums = %q/%q", got.Outcome.SourceChecksum, got.Outcome.BenchmarkSourceChecksum)
	}
	if !strings.Contains(got.Outcome.PriceSource, "#NVDA") || !strings.Contains(got.Outcome.BenchmarkSource, "#QQQ") {
		t.Fatalf("sources = %q/%q", got.Outcome.PriceSource, got.Outcome.BenchmarkSource)
	}
}

func TestOpportunityScoreIgnoresExistingExitDateForNextCloseHorizon(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows[0].Outcome.ExitDate = "2024-07-01"
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}

	scored, err := scoreOpportunityPointInTimeRows(rows[:1], ledger)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := scored[0].Outcome.EntryDate, "2024-01-03"; got != want {
		t.Fatalf("entry_date = %q, want %q", got, want)
	}
	if got, want := scored[0].Outcome.ExitDate, "2024-05-08"; got != want {
		t.Fatalf("exit_date = %q, want deterministic horizon exit %q", got, want)
	}
}

func TestOpportunityScoreRejectsUnscoredDegradedCapture(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	row := rows[0]
	row.LabelStatus = "unscored_forward_window_pending"
	row.Outcome = OpportunityBacktestOutcome{}
	row.Features.DataType = ""
	row.Features.QuoteQuality = ""
	row.Features.DataQuality = "technical_error"
	row.Features.TechnicalError = "context deadline exceeded"
	row.Features.Price = nil
	row.Features.SessionContext = nil
	row.FeatureProvenance = opportunityFeatureProvenance("fixture_feature_ledger:dirty", "fixture_point_in_time_features_v1", row.Features)
	f, err := os.Open(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bars, err := readOpportunityPriceBars(f)
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoreOpportunityPointInTimeRows([]OpportunityPointInTimeRow{row}, opportunityPriceBarLedger{
		BySymbol: bars,
		Source:   "fixture-bars.jsonl",
		Checksum: "sha256:fixture-bars",
	})
	if err == nil {
		t.Fatal("degraded unscored capture row scored successfully")
	}
	got := err.Error()
	for _, want := range []string{"capture row failed --require-live", "NVDA", "data_quality_technical_error", "technical_error", "price_missing", "session_context_missing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("error = %q, want %s", got, want)
		}
	}
}

func TestOpportunityScoreRejectsUnscoredPrefilledOutcomeWindow(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	row := rows[0]
	row.LabelStatus = "unscored_forward_window_pending"
	row.Outcome = OpportunityBacktestOutcome{
		EntryDate: "2024-02-01",
		ExitDate:  "2024-07-01",
	}
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoreOpportunityPointInTimeRows([]OpportunityPointInTimeRow{row}, ledger)
	if err == nil {
		t.Fatal("unscored row with prefilled outcome window scored successfully")
	}
	if got := err.Error(); !strings.Contains(got, "outcome fields must be empty") {
		t.Fatalf("error = %q, want prefilled outcome blocker", got)
	}
}

func TestOpportunityScoreNextCloseRejectsSameDayLookahead(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	row := rows[0]
	row.Outcome.EntryDate = row.Date
	err := validateOpportunityPointInTimeRowsScored([]OpportunityPointInTimeRow{row})
	if err == nil {
		t.Fatal("same-day next_close outcome passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "entry_rule next_close") {
		t.Fatalf("error = %q, want next_close chronology blocker", got)
	}
}

func TestOpportunityScoreNextCloseUsesSessionDateWhenDateMissing(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	row := rows[0]
	row.Date = ""
	row.AsOf = time.Date(2024, 1, 3, 0, 30, 0, 0, time.UTC)
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}

	scored, err := scoreOpportunityPointInTimeRows([]OpportunityPointInTimeRow{row}, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := scored[0].Outcome.EntryDate, "2024-01-03"; got != want {
		t.Fatalf("entry_date = %q, want %q from session_context.date", got, want)
	}
	if got, want := scored[0].Outcome.ForwardReturnPct, 25.0; got != want {
		t.Fatalf("forward_return_pct = %.1f, want %.1f from intended next-close window", got, want)
	}
}

func TestOpportunityRowsRejectDateSessionDateMismatch(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows[0].Date = "2024-01-03"
	err := validateOpportunityPointInTimeRowsScored(rows[:1])
	if err == nil {
		t.Fatal("PIT row with mismatched top-level date and session date passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "disagrees with session_context.date") {
		t.Fatalf("error = %q, want session date authority blocker", got)
	}
}

func TestOpportunityRowsRejectUnsupportedEntryRule(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows[0].Trade.EntryRule = "nxt_close"
	err := validateOpportunityPointInTimeRowsScored(rows[:1])
	if err == nil {
		t.Fatal("unsupported entry rule passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "unsupported trade.entry_rule") {
		t.Fatalf("error = %q, want unsupported entry rule blocker", got)
	}
}

func TestOpportunityPointInTimeLedgerKeyUsesSessionDateWhenDateMissing(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	withDate := rows[0]
	withoutDate := rows[0]
	withoutDate.Date = ""
	withoutDate.AsOf = time.Date(2024, 1, 3, 0, 30, 0, 0, time.UTC)

	want, err := opportunityPointInTimeLedgerKey(withDate)
	if err != nil {
		t.Fatal(err)
	}
	got, err := opportunityPointInTimeLedgerKey(withoutDate)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ledger key with missing date = %q, want explicit-date key %q", got, want)
	}
}

func TestOpportunityCaptureBuildsUnscoredPointInTimeRows(t *testing.T) {
	t.Parallel()
	last := 101.0
	changePct := 8.0
	volume := int64(2_000_000)
	advDollar := 250_000_000.0
	etfLast := 400.0
	scan := rpc.ScanResult{
		Preset: "top-movers",
		Type:   "TOP_PERC_GAIN",
		AsOf:   time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
		Rows: []rpc.ScanRow{
			{
				Rank:               1,
				Symbol:             "ALFA",
				SecType:            "STK",
				Exchange:           "SMART",
				Currency:           "USD",
				Last:               &last,
				ChangePct:          &changePct,
				Volume:             &volume,
				AvgDollarVolume20D: &advDollar,
				DataType:           rpc.MarketDataLive,
				PriceAsOf:          "As of: Jun 15 at 02:30:00 PM EDT",
			},
			{
				Rank:           2,
				Symbol:         "QQQ",
				SecType:        "STK",
				Last:           &etfLast,
				InstrumentTags: []string{"etf", "broad_index_etf"},
				DataType:       rpc.MarketDataLive,
			},
		},
	}
	price := 101.0
	sma50 := 95.0
	sma200 := 90.0
	pct50 := (price - sma50) / sma50
	pct200 := (price - sma200) / sma200
	rs63 := 0.12
	rs126 := 0.18
	technical := rpc.TechnicalResult{
		Benchmark:    "QQQ",
		LookbackDays: 420,
		AsOf:         scan.AsOf,
		Rows: []rpc.TechnicalRow{{
			Symbol:             "ALFA",
			Price:              &price,
			SMA50:              &sma50,
			SMA200:             &sma200,
			PctAbove50DMA:      &pct50,
			PctAbove200DMA:     &pct200,
			RS63D:              &rs63,
			RS126D:             &rs126,
			AvgDollarVolume20D: &advDollar,
			TrendState:         "uptrend",
			DataQuality:        "ok",
		}},
	}
	quoteAt := scan.AsOf.Add(-5 * time.Second)
	quotes := map[string]rpc.Quote{
		"ALFA": {
			Symbol:             "ALFA",
			Contract:           rpc.ContractParams{Symbol: "ALFA", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			QuotePrice:         &price,
			QuotePriceAt:       quoteAt,
			QuotePriceAsOf:     "As of: Jun 15 at 10:29:55 AM EDT",
			DataType:           rpc.MarketDataLive,
			FeedType:           "streaming",
			QuoteQuality:       "firm",
			AvgDollarVolume20D: &advDollar,
			AsOf:               scan.AsOf,
			SessionContext:     &rpc.MarketSession{Market: "us_equity", Date: "2026-06-15", State: "regular", IsOpen: true},
		},
	}
	rows := opportunityPointInTimeRowsFromSnapshots(scan, quotes, nil, technical, opportunityCaptureOptions{
		Split:            "holdout",
		HoldoutPlan:      "test-holdout-plan",
		Benchmark:        "QQQ",
		HorizonDays:      126,
		RoundTripCostBps: 50,
		Macro:            testOpportunityMacroContext(rpc.RegimeToneNormal),
	})
	if got, want := len(rows), 1; got != want {
		t.Fatalf("captured rows = %d, want %d", got, want)
	}
	row := rows[0]
	if got, want := row.Features.Instrument, "ALFA"; got != want {
		t.Fatalf("instrument = %q, want %q", got, want)
	}
	if got, want := row.Split, "holdout"; got != want {
		t.Fatalf("split = %q, want %q", got, want)
	}
	if got, want := row.SplitProvenance.PlanID, "test-holdout-plan"; got != want {
		t.Fatalf("holdout plan = %q, want %q", got, want)
	}
	if !row.SplitProvenance.PreRegistered || row.SplitProvenance.LabelStatusAtAssignment != "unscored_forward_window_pending" {
		t.Fatalf("split provenance not pre-registered: %+v", row.SplitProvenance)
	}
	if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, row.Features); err != nil {
		t.Fatalf("feature provenance did not validate: %v", err)
	}
	if row.Features.Macro == nil || row.Features.Macro.Tone != rpc.RegimeToneNormal {
		t.Fatalf("macro context not preserved: %+v", row.Features.Macro)
	}
	editedFeatures := row.Features
	editedMacro := *editedFeatures.Macro
	editedMacro.Tone = rpc.RegimeToneStress
	editedFeatures.Macro = &editedMacro
	if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, editedFeatures); err == nil {
		t.Fatal("feature provenance did not catch edited macro context")
	} else if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("feature provenance error = %q, want checksum mismatch", err)
	}
	if row.FeatureProvenance.Source != "capture-opportunity" || row.FeatureProvenance.Method != "scanner_features_v1" {
		t.Fatalf("feature provenance = %+v, want scanner capture source/method", row.FeatureProvenance)
	}
	if row.LabelStatus != "unscored_forward_window_pending" {
		t.Fatalf("label_status = %q, want unscored_forward_window_pending", row.LabelStatus)
	}
	if row.Features.ScanRank != 1 || row.Features.ScanType != "TOP_PERC_GAIN" {
		t.Fatalf("scan metadata not preserved: %+v", row.Features)
	}
	if row.Features.QuoteQuality != "firm" || row.Features.SessionContext == nil || !row.Features.PriceAt.Equal(quoteAt) {
		t.Fatalf("quote freshness not preserved for scanner row: %+v", row.Features)
	}
	if row.Target.Kind != "" || row.Outcome.EntryDate != "" {
		t.Fatalf("capture rows must remain unscored: target=%+v outcome=%+v", row.Target, row.Outcome)
	}
	if row.Trade.RoundTripCostBps == nil || *row.Trade.RoundTripCostBps != 50 {
		t.Fatalf("round-trip cost = %v, want 50", row.Trade.RoundTripCostBps)
	}
	signal := opportunityPointInTimeSignal(row.Features)
	if !signal.Fired {
		t.Fatalf("captured ALFA signal did not fire: %+v", signal)
	}
	if !strings.Contains(strings.Join(signal.Reasons, ","), "passed_constructive_breakout_v1") {
		t.Fatalf("signal reasons = %+v", signal.Reasons)
	}
	if err := validateOpportunityPointInTimeRowsScored(rows); err == nil {
		t.Fatal("unscored capture rows must not build into opportunity observations")
	}
	filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
	if len(skipped) != 0 {
		t.Fatalf("scanner row failed --require-live filter: %+v", skipped)
	}
	if got, want := len(filtered), 1; got != want {
		t.Fatalf("filtered scanner rows = %d, want %d", got, want)
	}
}

func TestOpportunityMacroContextFromRegime(t *testing.T) {
	t.Parallel()
	asOf := time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC)
	regime := rpc.RegimeSnapshotResult{
		AsOf:        asOf,
		Fingerprint: rpc.Fingerprint{Version: "test-fp", Key: "abc123"},
		Posture: rpc.RegimePosture{
			Label:      "Normal regime",
			Tone:       rpc.RegimeToneNormal,
			Stage:      rpc.LifecycleOpportunity,
			Severity:   "observe",
			Readiness:  "ready",
			Confidence: "high",
		},
		Composite: rpc.RegimeComposite{
			ClusterGreenCount:          5,
			ClusterYellowCount:         1,
			ClusterRedCount:            0,
			ClusterRankedCount:         6,
			ClusterEligibleRedCount:    0,
			ClusterProvisionalRedCount: 0,
		},
	}
	macro := opportunityMacroContextFromRegime(regime)
	if macro == nil {
		t.Fatal("macro context is nil")
	}
	if macro.Source != rpc.MethodRegimeSnapshot || !macro.AsOf.Equal(asOf) || macro.Fingerprint.Key != "abc123" {
		t.Fatalf("macro provenance not preserved: %+v", macro)
	}
	if macro.Tone != rpc.RegimeToneNormal || macro.Stage != rpc.LifecycleOpportunity || macro.ClusterRankedCount != 6 {
		t.Fatalf("macro posture not preserved: %+v", macro)
	}
}

func TestOpportunityCaptureDefaultsToTuningSplit(t *testing.T) {
	t.Parallel()
	price := 101.0
	sma50 := 95.0
	sma200 := 90.0
	rs := 0.12
	advDollar := 120_000_000.0
	scan := rpc.ScanResult{
		AsOf:   time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
		Preset: "top-movers",
		Type:   "TOP_PERC_GAIN",
		Rows: []rpc.ScanRow{{
			Symbol: "ALFA",
			Rank:   1,
			Last:   &price,
		}},
	}
	technical := rpc.TechnicalResult{
		AsOf: scan.AsOf,
		Rows: []rpc.TechnicalRow{{
			Symbol:             "ALFA",
			Price:              &price,
			SMA50:              &sma50,
			SMA200:             &sma200,
			RS63D:              &rs,
			RS126D:             &rs,
			AvgDollarVolume20D: &advDollar,
			DataQuality:        "ok",
		}},
	}
	quotes := map[string]rpc.Quote{"ALFA": {
		Symbol:             "ALFA",
		Contract:           rpc.ContractParams{Symbol: "ALFA", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		QuotePrice:         &price,
		QuotePriceAt:       scan.AsOf,
		DataType:           rpc.MarketDataLive,
		QuoteQuality:       "firm",
		AvgDollarVolume20D: &advDollar,
		AsOf:               scan.AsOf,
		SessionContext:     &rpc.MarketSession{Market: "us_equity", Date: "2026-06-15", State: "regular", IsOpen: true},
	}}
	rows := opportunityPointInTimeRowsFromSnapshots(scan, quotes, nil, technical, opportunityCaptureOptions{})
	if got, want := len(rows), 1; got != want {
		t.Fatalf("captured rows = %d, want %d", got, want)
	}
	if got, want := rows[0].Split, "tuning"; got != want {
		t.Fatalf("default capture split = %q, want %q", got, want)
	}
	if rows[0].SplitProvenance.Source != "capture-opportunity" || rows[0].SplitProvenance.Method != "capture_cli_tuning_default_v1" {
		t.Fatalf("default capture split provenance = %+v", rows[0].SplitProvenance)
	}
	if err := validateOpportunityFeatureProvenance(rows[0].FeatureProvenance, rows[0].Features); err != nil {
		t.Fatalf("default capture feature provenance did not validate: %v", err)
	}
	if got, want := opportunityCaptureSplit("holddout"), "tuning"; got != want {
		t.Fatalf("invalid internal capture split = %q, want safe tuning default", got)
	}
}

func TestRunBacktestCaptureOpportunityRejectsInvalidSplit(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"capture-opportunity", "--symbols", "SPY", "--split", "holddout", "--json"})
	if code == 0 {
		t.Fatalf("Run capture-opportunity accepted invalid split, stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "--split must be tuning or holdout") {
		t.Fatalf("stderr = %q, want invalid split blocker", got)
	}
}

func TestRunBacktestCaptureOpportunityRequiresHoldoutPlan(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"capture-opportunity", "--symbols", "SPY", "--split", "holdout", "--json"})
	if code == 0 {
		t.Fatalf("Run capture-opportunity accepted holdout without plan, stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "--holdout-plan is required when --split holdout") {
		t.Fatalf("stderr = %q, want holdout-plan blocker", got)
	}
}

func TestOpportunityCaptureFromScannerBlocksMissingQuote(t *testing.T) {
	t.Parallel()
	last := 101.0
	advDollar := 250_000_000.0
	scan := rpc.ScanResult{
		Preset: "top-movers",
		Type:   "TOP_PERC_GAIN",
		AsOf:   time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
		Rows: []rpc.ScanRow{{
			Rank:               1,
			Symbol:             "ALFA",
			SecType:            "STK",
			Last:               &last,
			AvgDollarVolume20D: &advDollar,
			DataType:           rpc.MarketDataLive,
		}},
	}
	sma50 := 95.0
	sma200 := 90.0
	rs := 0.12
	technical := rpc.TechnicalResult{
		AsOf: scan.AsOf,
		Rows: []rpc.TechnicalRow{{
			Symbol:             "ALFA",
			Price:              &last,
			SMA50:              &sma50,
			SMA200:             &sma200,
			RS63D:              &rs,
			RS126D:             &rs,
			AvgDollarVolume20D: &advDollar,
			DataQuality:        "ok",
		}},
	}
	rows := opportunityPointInTimeRowsFromSnapshots(scan, nil, nil, technical, opportunityCaptureOptions{
		Split:            "holdout",
		HoldoutPlan:      "test-holdout-plan",
		RoundTripCostBps: 50,
	})
	if got, want := len(rows), 1; got != want {
		t.Fatalf("captured rows = %d, want %d", got, want)
	}
	if rows[0].Features.QuoteError == "" {
		t.Fatalf("missing scanner quote did not record quote error: %+v", rows[0].Features)
	}
	filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
	if len(filtered) != 0 {
		t.Fatalf("missing scanner quote passed --require-live filter: %+v", filtered)
	}
	got := strings.Join(skipped, ";")
	for _, want := range []string{"ALFA:", "data_quality_quote_error", "quote_quality_missing", "quote_error", "session_context_missing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipped reasons = %q, want %s", got, want)
		}
	}
}

func TestOpportunityCaptureFromSymbolsPreservesQuoteFreshness(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 15, 18, 30, 0, 0, time.UTC)
	price := 101.0
	sma50 := 95.0
	sma200 := 90.0
	pct50 := (price - sma50) / sma50
	pct200 := (price - sma200) / sma200
	rs63 := 0.12
	rs126 := 0.18
	advDollar := 120_000_000.0
	technical := rpc.TechnicalResult{
		Benchmark:    "QQQ",
		LookbackDays: 420,
		AsOf:         now,
		Rows: []rpc.TechnicalRow{{
			Symbol:             "ALFA",
			Price:              &price,
			SMA50:              &sma50,
			SMA200:             &sma200,
			PctAbove50DMA:      &pct50,
			PctAbove200DMA:     &pct200,
			RS63D:              &rs63,
			RS126D:             &rs126,
			AvgDollarVolume20D: &advDollar,
			TrendState:         "uptrend",
			DataQuality:        "ok",
		}},
	}
	quoteAt := now.Add(-10 * time.Second)
	quotes := map[string]rpc.Quote{
		"ALFA": {
			Symbol:             "ALFA",
			Contract:           rpc.ContractParams{Symbol: "ALFA", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			QuotePrice:         &price,
			QuotePriceAt:       quoteAt,
			QuotePriceAsOf:     "As of: Jun 15 at 02:29:50 PM EDT",
			DataType:           rpc.MarketDataLive,
			FeedType:           "streaming",
			QuoteQuality:       "firm",
			AvgDollarVolume20D: &advDollar,
			AsOf:               now,
			SessionContext:     &rpc.MarketSession{Market: "us_equity", Date: "2026-06-15", State: "regular", IsOpen: true},
		},
	}
	rows := opportunityPointInTimeRowsFromSymbolSnapshots([]string{"ALFA"}, quotes, nil, technical, "", opportunityCaptureOptions{
		Split:            "holdout",
		HoldoutPlan:      "test-holdout-plan",
		Benchmark:        "QQQ",
		HorizonDays:      126,
		RoundTripCostBps: 50,
	})
	if got, want := len(rows), 1; got != want {
		t.Fatalf("captured rows = %d, want %d", got, want)
	}
	row := rows[0]
	if row.LabelStatus != "unscored_forward_window_pending" {
		t.Fatalf("label_status = %q, want unscored_forward_window_pending", row.LabelStatus)
	}
	if row.Features.DataType != rpc.MarketDataLive || row.Features.QuoteQuality != "firm" {
		t.Fatalf("quote context not preserved: %+v", row.Features)
	}
	if err := validateOpportunityFeatureProvenance(row.FeatureProvenance, row.Features); err != nil {
		t.Fatalf("symbol feature provenance did not validate: %v", err)
	}
	if row.FeatureProvenance.Method != "symbol_features_v1" {
		t.Fatalf("feature provenance = %+v, want symbol capture method", row.FeatureProvenance)
	}
	if !row.Features.PriceAt.Equal(quoteAt) || row.Features.PriceAsOf == "" {
		t.Fatalf("quote freshness not preserved: %+v", row.Features)
	}
	signal := opportunityPointInTimeSignal(row.Features)
	if !signal.Fired {
		t.Fatalf("live symbol capture signal did not fire: %+v", signal)
	}
	filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
	if len(skipped) != 0 {
		t.Fatalf("live symbol capture failed --require-live filter: %+v", skipped)
	}
	if got, want := len(filtered), 1; got != want {
		t.Fatalf("filtered live rows = %d, want %d", got, want)
	}
}

func TestOpportunityCaptureFromSymbolsBlocksMissingQuote(t *testing.T) {
	t.Parallel()
	price := 101.0
	sma50 := 95.0
	sma200 := 90.0
	rs := 0.12
	advDollar := 120_000_000.0
	technical := rpc.TechnicalResult{
		AsOf: time.Date(2026, 6, 15, 18, 30, 0, 0, time.UTC),
		Rows: []rpc.TechnicalRow{{
			Symbol:             "ALFA",
			Price:              &price,
			SMA50:              &sma50,
			SMA200:             &sma200,
			RS63D:              &rs,
			RS126D:             &rs,
			AvgDollarVolume20D: &advDollar,
			DataQuality:        "ok",
		}},
	}
	rows := opportunityPointInTimeRowsFromSymbolSnapshots([]string{"ALFA"}, nil, nil, technical, "", opportunityCaptureOptions{
		Split:            "holdout",
		HoldoutPlan:      "test-holdout-plan",
		RoundTripCostBps: 50,
	})
	if got, want := len(rows), 1; got != want {
		t.Fatalf("captured rows = %d, want %d", got, want)
	}
	signal := opportunityPointInTimeSignal(rows[0].Features)
	if signal.Fired {
		t.Fatalf("missing quote signal fired: %+v", signal)
	}
	if got := strings.Join(signal.Reasons, ","); !strings.Contains(got, "quote_error") {
		t.Fatalf("signal reasons = %q, want quote_error", got)
	}
	filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
	if len(filtered) != 0 {
		t.Fatalf("missing quote rows passed --require-live filter: %+v", filtered)
	}
	got := strings.Join(skipped, ";")
	for _, want := range []string{"ALFA:", "data_quality_quote_error", "data_type_missing", "quote_quality_missing", "quote_error", "session_context_missing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipped reasons = %q, want %s", got, want)
		}
	}
}

func TestOpportunityCaptureRequireLiveBlocksTechnicalError(t *testing.T) {
	t.Parallel()
	price := 101.0
	rows := []OpportunityPointInTimeRow{{
		Date: "2026-06-15",
		Features: OpportunityPointInTimeFeatures{
			Instrument:     "ALFA",
			DataType:       rpc.MarketDataLive,
			QuoteQuality:   "firm",
			DataQuality:    "technical_error",
			TechnicalError: "context deadline exceeded",
			Price:          &price,
			SessionContext: &rpc.MarketSession{
				Market: "us_equity",
				Date:   "2026-06-15",
				State:  "regular",
				IsOpen: true,
			},
		},
		Trade: OpportunityBacktestTrade{Instrument: "ALFA", EntryRule: "next_close", HorizonDays: 126},
	}}
	filtered, skipped := opportunityCaptureRowsSatisfyingLiveContext(rows)
	if len(filtered) != 0 {
		t.Fatalf("technical-error rows passed --require-live filter: %+v", filtered)
	}
	got := strings.Join(skipped, ";")
	for _, want := range []string{"ALFA:", "data_quality_technical_error", "technical_error"} {
		if !strings.Contains(got, want) {
			t.Fatalf("skipped reasons = %q, want %s", got, want)
		}
	}
}

func TestOpportunityCaptureHealthBlockersRequireScannerQuoteAndHistory(t *testing.T) {
	t.Parallel()
	blockers := opportunityCaptureHealthBlockers(rpc.HealthResult{
		Connected: true,
		Subsystems: []rpc.SubsystemHealth{
			{Name: "quote", Status: "degraded", Message: "no market-data farm connection notice observed; quotes may time out"},
			{Name: "scanner", Status: "degraded", Message: "scanner requests may time out"},
			{Name: "history", Status: "ready"},
			{Name: "chain", Status: "degraded", Message: "security-definition farm disconnected"},
			{Name: "opportunities", Status: "degraded", Message: "account unavailable"},
		},
	}, true)
	got := strings.Join(blockers, ";")
	for _, want := range []string{
		"quote degraded: no market-data farm connection notice observed; quotes may time out",
		"scanner degraded: scanner requests may time out",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("blockers = %q, want %q", got, want)
		}
	}
	for _, notWant := range []string{"chain", "opportunities"} {
		if strings.Contains(got, notWant) {
			t.Fatalf("blockers = %q, did not expect %q for opportunity capture preflight", got, notWant)
		}
	}
}

func TestOpportunityCaptureHealthBlockersForSymbolsIgnoreScanner(t *testing.T) {
	t.Parallel()
	blockers := opportunityCaptureHealthBlockers(rpc.HealthResult{
		Connected: true,
		Subsystems: []rpc.SubsystemHealth{
			{Name: "quote", Status: "ready"},
			{Name: "scanner", Status: "degraded", Message: "scanner requests may time out"},
			{Name: "history", Status: "ready"},
		},
	}, false)
	if len(blockers) != 0 {
		t.Fatalf("symbol capture blockers = %+v, want none because scanner is not required", blockers)
	}
}

func TestOpportunityCaptureHealthBlockersOffline(t *testing.T) {
	t.Parallel()
	blockers := opportunityCaptureHealthBlockers(rpc.HealthResult{
		Connected: false,
		LastError: "dial tcp 127.0.0.1:7496: connect: connection refused",
	}, true)
	if got, want := strings.Join(blockers, ";"), "gateway_unavailable: dial tcp 127.0.0.1:7496: connect: connection refused"; got != want {
		t.Fatalf("offline blockers = %q, want %q", got, want)
	}
}

func TestOpportunityCapturePreflightErrorCarriesJSONResult(t *testing.T) {
	t.Parallel()
	err := newOpportunityCapturePreflightError(true, []string{
		"quote degraded: no market-data farm connection notice observed; quotes may time out",
		"history degraded: no historical-data farm connection notice observed; history and technical screens may time out",
	})
	if !strings.Contains(err.Error(), "--require-live preflight failed") {
		t.Fatalf("error text = %q", err.Error())
	}
	if got, want := err.Result.Kind, "opportunity_capture_preflight"; got != want {
		t.Fatalf("kind = %q, want %q", got, want)
	}
	if got, want := err.Result.Status, "blocked"; got != want {
		t.Fatalf("status = %q, want %q", got, want)
	}
	if got, want := err.Result.Mode, "scanner"; got != want {
		t.Fatalf("mode = %q, want %q", got, want)
	}
	if !err.Result.RequireLive {
		t.Fatal("require_live = false, want true")
	}
	if got, want := len(err.Result.Blockers), 2; got != want {
		t.Fatalf("blockers len = %d, want %d", got, want)
	}
}

func TestOpportunityBacktestRejectsUnsourcedObservation(t *testing.T) {
	t.Parallel()
	rows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t)[:1])
	rows[0].Outcome.PriceSource = ""
	rows[0].Outcome.BenchmarkSource = ""
	err := validateOpportunityBacktestObservationsSourced(rows)
	if err == nil {
		t.Fatal("unsourced opportunity observation passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "price_source and benchmark_source") {
		t.Fatalf("error = %q, want price/benchmark source blocker", got)
	}
}

func TestOpportunityPointInTimeScoredRowsRequireProvenance(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)[:1]
	rows[0].Outcome.SourceChecksum = ""
	rows[0].Outcome.BenchmarkSourceChecksum = ""
	err := validateOpportunityPointInTimeRowsScored(rows)
	if err == nil {
		t.Fatal("partially sourced PIT row passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "source_checksum and benchmark_source_checksum") {
		t.Fatalf("error = %q, want checksum blocker", got)
	}
}

func TestOpportunityPointInTimeScoredRowsRejectDuplicateKeys(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows = append(rows[:1:1], rows[0])
	err := validateOpportunityPointInTimeRowsScored(rows)
	if err == nil {
		t.Fatal("duplicate PIT rows passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "duplicate opportunity PIT row") || !strings.Contains(got, "line 1") {
		t.Fatalf("error = %q, want duplicate PIT blocker with first line", got)
	}
}

func TestOpportunityBacktestObservationsRejectDuplicateKeys(t *testing.T) {
	t.Parallel()
	rows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	rows = append(rows[:1:1], rows[0])
	err := validateOpportunityBacktestObservationsSourced(rows)
	if err == nil {
		t.Fatal("duplicate opportunity observations passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "duplicate opportunity observation") || !strings.Contains(got, "line 1") {
		t.Fatalf("error = %q, want duplicate observation blocker with first line", got)
	}
}

func TestOpportunityHoldoutRowsRequireSplitProvenance(t *testing.T) {
	t.Parallel()
	pitRows := readOpportunityPointInTimeFixture(t)
	pitRows[2].SplitProvenance = OpportunitySplitProvenance{}
	err := validateOpportunityPointInTimeRowsScored(pitRows)
	if err == nil {
		t.Fatal("holdout PIT row without split provenance passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "split_provenance") || !strings.Contains(got, "--holdout-plan") {
		t.Fatalf("error = %q, want split provenance blocker", got)
	}

	pitRows = readOpportunityPointInTimeFixture(t)
	pitRows[2].SplitProvenance.AssignedAt = time.Date(2024, 4, 16, 0, 0, 0, 0, time.UTC)
	err = validateOpportunityPointInTimeRowsScored(pitRows)
	if err == nil {
		t.Fatal("holdout PIT row with late split provenance passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "assigned_at") {
		t.Fatalf("error = %q, want assigned_at timing blocker", got)
	}

	obsRows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	obsRows[2].SplitProvenance = OpportunitySplitProvenance{}
	err = validateOpportunityBacktestObservationsSourced(obsRows)
	if err == nil {
		t.Fatal("holdout observation without split provenance passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "split_provenance") || !strings.Contains(got, "--holdout-plan") {
		t.Fatalf("error = %q, want split provenance blocker", got)
	}

	obsRows = buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	obsRows[2].AsOf = time.Date(2024, 4, 15, 14, 30, 0, 0, time.UTC)
	obsRows[2].SplitProvenance.AssignedAt = obsRows[2].AsOf.Add(time.Second)
	err = validateOpportunityBacktestObservationsSourced(obsRows)
	if err == nil {
		t.Fatal("holdout observation with post-as_of split provenance passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "assigned_at") {
		t.Fatalf("error = %q, want assigned_at timing blocker", got)
	}
}

func TestOpportunityBacktestLateHoldoutProvenanceCountsUnknown(t *testing.T) {
	t.Parallel()
	rows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))[2:3]
	rows[0].SplitProvenance.AssignedAt = time.Date(2024, 4, 16, 0, 0, 0, 0, time.UTC)
	res := runOpportunityBacktest(rows, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))

	if res.Observations[0].Holdout {
		t.Fatal("late split provenance counted as holdout")
	}
	if got := res.Observations[0].Split; got != "unknown" {
		t.Fatalf("split = %q, want unknown", got)
	}
	if got, want := res.Metrics.UnknownSplitObservations, 1; got != want {
		t.Fatalf("unknown split observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutObservations, 0; got != want {
		t.Fatalf("holdout observations = %d, want %d", got, want)
	}
}

func TestOpportunityRowsRequireFeatureProvenance(t *testing.T) {
	t.Parallel()
	pitRows := readOpportunityPointInTimeFixture(t)
	pitRows[0].FeatureProvenance = OpportunityFeatureProvenance{}
	err := validateOpportunityPointInTimeRowsScored(pitRows)
	if err == nil {
		t.Fatal("PIT row without feature provenance passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "feature_provenance") {
		t.Fatalf("error = %q, want feature provenance blocker", got)
	}

	pitRows = readOpportunityPointInTimeFixture(t)
	pitRows[0].Features.SMA50 = new(10.0)
	err = validateOpportunityPointInTimeRowsScored(pitRows)
	if err == nil {
		t.Fatal("PIT row with edited features passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "checksum mismatch") {
		t.Fatalf("error = %q, want feature checksum mismatch", got)
	}

	obsRows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	obsRows[0].Features.RS63D = new(-0.20)
	err = validateOpportunityBacktestObservationsSourced(obsRows)
	if err == nil {
		t.Fatal("observation with edited features passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "checksum mismatch") {
		t.Fatalf("error = %q, want feature checksum mismatch", got)
	}

	obsRows = buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	obsRows[0].Signal.Fired = !obsRows[0].Signal.Fired
	err = validateOpportunityBacktestObservationsSourced(obsRows)
	if err == nil {
		t.Fatal("observation with edited signal passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "unverified signal provenance") {
		t.Fatalf("error = %q, want signal provenance blocker", got)
	}
}

func TestOpportunityRowsRequireOutcomeChronology(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	rows[0].Outcome.ExitDate = "2024-01-01"
	err := validateOpportunityPointInTimeRowsScored(rows)
	if err == nil {
		t.Fatal("PIT row with exit before entry passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "exit_date must be on or after") {
		t.Fatalf("error = %q, want exit-before-entry blocker", got)
	}

	rows = readOpportunityPointInTimeFixture(t)
	rows[0].Date = "2024-01-04"
	rows[0].Features.SessionContext.Date = "2024-01-04"
	rows[0].FeatureProvenance = opportunityFeatureProvenance("fixture_feature_ledger:ai_2026-06-15", "fixture_point_in_time_features_v1", rows[0].Features)
	err = validateOpportunityPointInTimeRowsScored(rows)
	if err == nil {
		t.Fatal("PIT row with entry before observation date passed validation")
	}
	if got := err.Error(); !strings.Contains(got, "entry_date must be on or after") {
		t.Fatalf("error = %q, want entry-before-observation blocker", got)
	}
}

func TestRunBacktestOpportunityRejectsFutureExitDate(t *testing.T) {
	t.Parallel()
	rows := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))[:1]
	rows[0].Outcome.ExitDate = time.Now().AddDate(0, 0, 7).Format("2006-01-02")
	var input bytes.Buffer
	if err := writeOpportunityBacktestObservationsJSONL(&input, rows); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "future-exit.jsonl")
	if err := os.WriteFile(path, input.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"opportunity", "--input", path})
	if code == 0 {
		t.Fatalf("Run backtest accepted future exit date, stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "after backtest run date") {
		t.Fatalf("stderr = %q, want future exit blocker", got)
	}
}

func TestOpportunityPointInTimeSignalBlocksNonLiveData(t *testing.T) {
	t.Parallel()
	price := 100.0
	sma50 := 90.0
	sma200 := 80.0
	rs := 0.10
	advDollar := 100_000_000.0
	signal := opportunityPointInTimeSignal(OpportunityPointInTimeFeatures{
		Instrument:         "ALFA",
		DataType:           rpc.MarketDataDelayed,
		DataQuality:        "ok",
		Price:              &price,
		SMA50:              &sma50,
		SMA200:             &sma200,
		RS63D:              &rs,
		RS126D:             &rs,
		AvgDollarVolume20D: &advDollar,
	})
	if signal.Fired {
		t.Fatalf("delayed data signal fired: %+v", signal)
	}
	if got := strings.Join(signal.Reasons, ","); !strings.Contains(got, "data_type_not_live") {
		t.Fatalf("signal reasons = %q, want data_type_not_live", got)
	}
}

func TestOpportunityPointInTimeSignalBlocksMissingFreshnessContext(t *testing.T) {
	t.Parallel()
	price := 100.0
	sma50 := 90.0
	sma200 := 80.0
	rs := 0.10
	advDollar := 100_000_000.0
	signal := opportunityPointInTimeSignal(OpportunityPointInTimeFeatures{
		Instrument:         "ALFA",
		DataQuality:        "ok",
		Price:              &price,
		SMA50:              &sma50,
		SMA200:             &sma200,
		RS63D:              &rs,
		RS126D:             &rs,
		AvgDollarVolume20D: &advDollar,
	})
	if signal.Fired {
		t.Fatalf("missing freshness signal fired: %+v", signal)
	}
	got := strings.Join(signal.Reasons, ",")
	for _, want := range []string{"data_type_missing", "quote_quality_missing", "session_context_missing"} {
		if !strings.Contains(got, want) {
			t.Fatalf("signal reasons = %q, want %s", got, want)
		}
	}
	if !opportunitySignalContextBlocked(signal.Reasons) {
		t.Fatalf("missing freshness reasons did not block context: %+v", signal.Reasons)
	}
}

func TestOpportunityPointInTimeSignalBlocksStaleQuote(t *testing.T) {
	t.Parallel()
	price := 100.0
	sma50 := 90.0
	sma200 := 80.0
	rs := 0.10
	advDollar := 100_000_000.0
	signal := opportunityPointInTimeSignal(OpportunityPointInTimeFeatures{
		Instrument:         "ALFA",
		DataType:           rpc.MarketDataLive,
		QuoteQuality:       "stale",
		Stale:              true,
		DataQuality:        "stale_quote",
		Price:              &price,
		SMA50:              &sma50,
		SMA200:             &sma200,
		RS63D:              &rs,
		RS126D:             &rs,
		AvgDollarVolume20D: &advDollar,
	})
	if signal.Fired {
		t.Fatalf("stale quote signal fired: %+v", signal)
	}
	got := strings.Join(signal.Reasons, ",")
	for _, want := range []string{"data_quality_not_ok", "quote_quality_stale", "quote_stale"} {
		if !strings.Contains(got, want) {
			t.Fatalf("signal reasons = %q, want %s", got, want)
		}
	}
}

func TestRunBacktestBuildOpportunityEmitsJSONL(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"build-opportunity", "--input", opportunityPointInTimeFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest build-opportunity returned %d, stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
	rows, err := readOpportunityBacktestObservations(strings.NewReader(stdout.String()))
	if err != nil {
		t.Fatalf("generated JSONL should feed opportunity backtest: %v\n%s", err, stdout.String())
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("generated rows = %d, want %d", got, want)
	}
	if rows[0].Signal.Source != opportunityBuilderSignalSource || !rows[0].Signal.Fired {
		t.Fatalf("first generated row signal = %+v", rows[0].Signal)
	}
	if !strings.Contains(stdout.String(), `"split":"holdout"`) {
		t.Fatalf("generated rows missing holdout split:\n%s", stdout.String())
	}
	if got := rows[2].SplitProvenance.PlanID; got == "" {
		t.Fatalf("generated holdout row missing split provenance: %+v", rows[2].SplitProvenance)
	}
}

func TestRegimeBacktestBuilderPreservesTapeChangeFields(t *testing.T) {
	t.Parallel()
	rows := []RegimePointInTimeRow{{
		Date: "2026-05-29",
		VIXTermStructure: RegimePointInTimeVIXTerm{
			VIX:          new(30.0),
			VIX3M:        new(24.0),
			VIXPrevClose: new(20.0),
		},
		HYGSPYDivergence: RegimePointInTimeHYGSPY{
			HYGPrice:     new(78.0),
			HYG50DMA:     new(79.0),
			SPYPrice:     new(95.0),
			SPY52WHigh:   new(110.0),
			SPYPrevClose: new(100.0),
		},
	}}
	observations := buildRegimeBacktestObservations(rows)
	got := observations[0].Regime
	if got.VIXTermStructure.VIXChangePct == nil || *got.VIXTermStructure.VIXChangePct != 50.0 {
		t.Fatalf("vix_change_pct = %v, want 50.0", got.VIXTermStructure.VIXChangePct)
	}
	if got.HYGSPYDivergence.SPYChange == nil || *got.HYGSPYDivergence.SPYChange != -5.0 {
		t.Fatalf("spy_change = %v, want -5.0", got.HYGSPYDivergence.SPYChange)
	}
	if got.HYGSPYDivergence.SPYChangePct == nil || *got.HYGSPYDivergence.SPYChangePct != -5.0 {
		t.Fatalf("spy_change_pct = %v, want -5.0", got.HYGSPYDivergence.SPYChangePct)
	}
}

// TestRegimeBacktestInputStampsTapeSession pins the replay-side session
// stamp: replay classifies exactly like the daemon does at snapshot time, so
// live closed-date gating and backtest replay can never drift. Date-only
// observations clock 15:59 ET inside their date (trading_date on trading
// days); dates outside embedded calendar coverage stay empty and fail open,
// which is what keeps the historical sourced corpora reporting identical
// metrics.
func TestRegimeBacktestInputStampsTapeSession(t *testing.T) {
	t.Parallel()

	in, _ := regimeBacktestInput(RegimeBacktestObservation{Date: "2026-07-17"}) // Friday, in coverage
	if in.TapeSessionState != rpc.TapeSessionTradingDate {
		t.Fatalf("trading-day stamp = %q, want trading_date", in.TapeSessionState)
	}

	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	saturday := time.Date(2026, 7, 18, 12, 0, 0, 0, loc)
	in, _ = regimeBacktestInput(RegimeBacktestObservation{AsOf: saturday})
	if in.TapeSessionState != rpc.TapeSessionClosedDate {
		t.Fatalf("weekend stamp = %q, want closed_date", in.TapeSessionState)
	}

	in, _ = regimeBacktestInput(RegimeBacktestObservation{Date: "2017-01-24"}) // outside coverage
	if in.TapeSessionState != "" {
		t.Fatalf("outside-coverage stamp = %q, want empty (fail-open)", in.TapeSessionState)
	}
}

func TestOpportunityBacktestSampleProducesMarketOutcomeMetrics(t *testing.T) {
	t.Parallel()
	rows := readOpportunityBacktestFixture(t)
	res := runOpportunityBacktest(rows, time.Date(2026, 5, 31, 12, 5, 0, 0, time.UTC))

	if got, want := res.Metrics.Observations, 8; got != want {
		t.Fatalf("observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TargetOpportunity, 5; got != want {
		t.Fatalf("target_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Metrics.SignalFired, 6; got != want {
		t.Fatalf("signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.TruePositive, 4; got != want {
		t.Fatalf("true_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.FalsePositive, 2; got != want {
		t.Fatalf("false_positive = %d, want %d", got, want)
	}
	if got, want := res.Metrics.Miss, 1; got != want {
		t.Fatalf("miss = %d, want %d", got, want)
	}
	if got, want := res.Metrics.PositiveExcess, 4; got != want {
		t.Fatalf("positive_excess = %d, want %d", got, want)
	}
	if res.Metrics.Recall == nil || *res.Metrics.Recall != 0.8 {
		t.Fatalf("recall = %v, want 0.8", res.Metrics.Recall)
	}
	if res.Metrics.AvgExcessReturnPct == nil || math.Abs(*res.Metrics.AvgExcessReturnPct-64.25) > 0.01 {
		t.Fatalf("avg_excess_return_pct = %v, want about 64.25", res.Metrics.AvgExcessReturnPct)
	}
	if res.Metrics.MedianExcessReturnPct == nil || math.Abs(*res.Metrics.MedianExcessReturnPct-42.05) > 0.01 {
		t.Fatalf("median_excess_return_pct = %v, want about 42.05", res.Metrics.MedianExcessReturnPct)
	}
	if res.Metrics.WorstExcessReturnPct == nil || math.Abs(*res.Metrics.WorstExcessReturnPct-(-67.9)) > 0.01 {
		t.Fatalf("worst_excess_return_pct = %v, want about -67.9", res.Metrics.WorstExcessReturnPct)
	}
	if got, want := res.Metrics.CostedSignalFired, 6; got != want {
		t.Fatalf("costed_signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.MissingCostSignalFired, 0; got != want {
		t.Fatalf("missing_cost_signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutObservations, 0; got != want {
		t.Fatalf("holdout_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.UnknownSplitObservations, 8; got != want {
		t.Fatalf("unknown_split_observations = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutSignalFired, 0; got != want {
		t.Fatalf("holdout_signal_fired = %d, want %d", got, want)
	}
	if res.Metrics.AvgExecutionCostPct == nil || math.Abs(*res.Metrics.AvgExecutionCostPct-0.5) > 0.01 {
		t.Fatalf("avg_execution_cost_pct = %v, want about 0.5", res.Metrics.AvgExecutionCostPct)
	}
	if res.Metrics.AvgNetExcessReturnPct == nil || math.Abs(*res.Metrics.AvgNetExcessReturnPct-63.75) > 0.01 {
		t.Fatalf("avg_net_excess_return_pct = %v, want about 63.75", res.Metrics.AvgNetExcessReturnPct)
	}
	if res.Metrics.MedianNetExcessReturnPct == nil || math.Abs(*res.Metrics.MedianNetExcessReturnPct-41.55) > 0.01 {
		t.Fatalf("median_net_excess_return_pct = %v, want about 41.55", res.Metrics.MedianNetExcessReturnPct)
	}
	if res.Metrics.WorstNetExcessReturnPct == nil || math.Abs(*res.Metrics.WorstNetExcessReturnPct-(-68.4)) > 0.01 {
		t.Fatalf("worst_net_excess_return_pct = %v, want about -68.4", res.Metrics.WorstNetExcessReturnPct)
	}
	if res.Metrics.BestNetExcessReturnPct == nil || math.Abs(*res.Metrics.BestNetExcessReturnPct-193.2) > 0.01 {
		t.Fatalf("best_net_excess_return_pct = %v, want about 193.2", res.Metrics.BestNetExcessReturnPct)
	}
	if res.Metrics.NetExcessHitRate == nil || math.Abs(*res.Metrics.NetExcessHitRate-(4.0/6.0)) > 0.001 {
		t.Fatalf("net_excess_hit_rate = %v, want 4/6", res.Metrics.NetExcessHitRate)
	}
	if got, want := res.Metrics.CostedCandidates, 8; got != want {
		t.Fatalf("costed_candidates = %d, want %d", got, want)
	}
	if got, want := res.Metrics.NonFiredCostedCandidates, 2; got != want {
		t.Fatalf("non_fired_costed_candidates = %d, want %d", got, want)
	}
	if res.Metrics.CandidateNetExcessHitRate == nil || math.Abs(*res.Metrics.CandidateNetExcessHitRate-(5.0/8.0)) > 0.001 {
		t.Fatalf("candidate_net_excess_hit_rate = %v, want 5/8", res.Metrics.CandidateNetExcessHitRate)
	}
	if res.Metrics.AvgCandidateNetExcessPct == nil || math.Abs(*res.Metrics.AvgCandidateNetExcessPct-53.6) > 0.01 {
		t.Fatalf("avg_candidate_net_excess_pct = %v, want about 53.6", res.Metrics.AvgCandidateNetExcessPct)
	}
	if res.Metrics.MedianCandidateNetExcessPct == nil || math.Abs(*res.Metrics.MedianCandidateNetExcessPct-41.55) > 0.01 {
		t.Fatalf("median_candidate_net_excess_pct = %v, want about 41.55", res.Metrics.MedianCandidateNetExcessPct)
	}
	if res.Metrics.AvgNonFiredCandidateNetPct == nil || math.Abs(*res.Metrics.AvgNonFiredCandidateNetPct-23.15) > 0.01 {
		t.Fatalf("avg_non_fired_candidate_net_pct = %v, want about 23.15", res.Metrics.AvgNonFiredCandidateNetPct)
	}
	if res.Metrics.FiredVsCandidateAvgLiftPct == nil || math.Abs(*res.Metrics.FiredVsCandidateAvgLiftPct-10.15) > 0.01 {
		t.Fatalf("fired_vs_candidate_avg_lift_pct = %v, want about 10.15", res.Metrics.FiredVsCandidateAvgLiftPct)
	}
	if res.Metrics.FiredVsNonFiredAvgLiftPct == nil || math.Abs(*res.Metrics.FiredVsNonFiredAvgLiftPct-40.6) > 0.01 {
		t.Fatalf("fired_vs_non_fired_avg_lift_pct = %v, want about 40.6", res.Metrics.FiredVsNonFiredAvgLiftPct)
	}
	if got, want := res.Metrics.DistinctSignalInstruments, 5; got != want {
		t.Fatalf("distinct_signal_instruments = %d, want %d", got, want)
	}
	if res.Metrics.MaxSignalInstrument != "NVDA" || res.Metrics.MaxSignalInstrumentFired != 2 || res.Metrics.MaxSignalInstrumentShare == nil || math.Abs(*res.Metrics.MaxSignalInstrumentShare-(2.0/6.0)) > 0.001 {
		t.Fatalf("max signal instrument = %q %d %v, want NVDA 2/6", res.Metrics.MaxSignalInstrument, res.Metrics.MaxSignalInstrumentFired, res.Metrics.MaxSignalInstrumentShare)
	}
	if got, want := res.Metrics.DistinctSignalClusters, 2; got != want {
		t.Fatalf("distinct_signal_clusters = %d, want %d", got, want)
	}
	if res.Metrics.MaxSignalCluster != "AI infrastructure opportunity" || res.Metrics.MaxSignalClusterFired != 4 || res.Metrics.MaxSignalClusterShare == nil || math.Abs(*res.Metrics.MaxSignalClusterShare-(4.0/6.0)) > 0.001 {
		t.Fatalf("max signal cluster = %q %d %v, want AI infrastructure opportunity 4/6", res.Metrics.MaxSignalCluster, res.Metrics.MaxSignalClusterFired, res.Metrics.MaxSignalClusterShare)
	}
	if res.Metrics.ExcessHitRateLower95 == nil || math.Abs(*res.Metrics.ExcessHitRateLower95-0.299988) > 0.0001 {
		t.Fatalf("excess_hit_rate_lower_95 = %v, want about 0.299988", res.Metrics.ExcessHitRateLower95)
	}
	if res.Metrics.AvgExcessReturnLower95Pct == nil || math.Abs(*res.Metrics.AvgExcessReturnLower95Pct-(-20.2383)) > 0.01 {
		t.Fatalf("avg_excess_return_lower_95_pct = %v, want about -20.2383", res.Metrics.AvgExcessReturnLower95Pct)
	}
	if res.Metrics.NetExcessHitRateLower95 == nil || math.Abs(*res.Metrics.NetExcessHitRateLower95-0.299988) > 0.0001 {
		t.Fatalf("net_excess_hit_rate_lower_95 = %v, want about 0.299988", res.Metrics.NetExcessHitRateLower95)
	}
	if res.Metrics.AvgNetExcessReturnLower95Pct == nil || math.Abs(*res.Metrics.AvgNetExcessReturnLower95Pct-(-20.7383)) > 0.01 {
		t.Fatalf("avg_net_excess_return_lower_95_pct = %v, want about -20.7383", res.Metrics.AvgNetExcessReturnLower95Pct)
	}
	for _, row := range res.Observations {
		if row.Split != "unknown" || row.Holdout {
			t.Fatalf("row %q split=%q holdout=%v, want unknown/non-holdout", row.Case, row.Split, row.Holdout)
		}
		if row.SignalFired {
			continue
		}
		if row.ExecutionCostPct != nil || row.NetExcessReturnPct != nil || row.PositiveNetExcess != nil {
			t.Fatalf("non-fired row %q has executed net fields: cost=%v net=%v positive=%v", row.Case, row.ExecutionCostPct, row.NetExcessReturnPct, row.PositiveNetExcess)
		}
	}
	if res.Evidence.Status != "insufficient_sample" {
		t.Fatalf("evidence status = %q, want insufficient_sample: %+v", res.Evidence.Status, res.Evidence)
	}
	if got, want := res.Evidence.Needs.AdditionalObservations, 92; got != want {
		t.Fatalf("additional_observations = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalSignalFired, 24; got != want {
		t.Fatalf("additional_signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalTargetOpportunity, 25; got != want {
		t.Fatalf("additional_target_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalNonOpportunity, 27; got != want {
		t.Fatalf("additional_non_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalSignalInstruments, 5; got != want {
		t.Fatalf("additional_signal_instruments = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalSignalClusters, 1; got != want {
		t.Fatalf("additional_signal_clusters = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutObservations, 30; got != want {
		t.Fatalf("additional_holdout_observations = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutSignalFired, 10; got != want {
		t.Fatalf("additional_holdout_signal_fired = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutTargetOpportunity, 10; got != want {
		t.Fatalf("additional_holdout_target_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutNonOpportunity, 10; got != want {
		t.Fatalf("additional_holdout_non_opportunity = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutSignalInstruments, 5; got != want {
		t.Fatalf("additional_holdout_signal_instruments = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.AdditionalHoldoutSignalClusters, 2; got != want {
		t.Fatalf("additional_holdout_signal_clusters = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.UnknownSplitObservations, 8; got != want {
		t.Fatalf("unknown_split_observations need = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Needs.MissingCostSignalFired, 0; got != want {
		t.Fatalf("missing_cost_signal_fired need = %d, want %d", got, want)
	}
	if got, want := strings.Join(res.Evidence.Reasons, "\n"), "observations 8 < 100\nfired signals 6 < 30\ntarget opportunities 5 < 30\nnon-opportunity controls 3 < 30"; got != want {
		t.Fatalf("evidence reasons = %q, want %q", got, want)
	}
	findings := strings.Join(res.Findings, "\n")
	for _, want := range []string{
		"Evidence status insufficient_sample",
		"missed 1 labelled opportunity",
		"fired on 2 non-opportunity",
		"4/6 fired signal row(s) had positive excess return",
		"median +42.0%",
		"Cost-adjusted net excess distribution",
		"Fired-signal concentration",
		"Validation split",
		"95% lower-bound check",
		"Evidence needed before alpha claims",
	} {
		if !strings.Contains(findings, want) {
			t.Fatalf("findings missing %q: %+v", want, res.Findings)
		}
	}
	if strings.Contains(findings, "missing round-trip cost") {
		t.Fatalf("findings should not claim missing costs: %+v", res.Findings)
	}
}

func TestOpportunityBacktestCostAdjustedMetrics(t *testing.T) {
	t.Parallel()
	rows := readOpportunityBacktestFixture(t)
	for i := range rows {
		if !rows[i].Signal.Fired {
			continue
		}
		costBps := 50.0
		rows[i].Trade.RoundTripCostBps = &costBps
		rows[i].Trade.CostModel = "test-flat-50bps"
	}
	res := runOpportunityBacktest(rows, time.Date(2026, 5, 31, 12, 5, 0, 0, time.UTC))

	if got, want := res.Metrics.CostedSignalFired, 6; got != want {
		t.Fatalf("costed_signal_fired = %d, want %d", got, want)
	}
	if got := res.Metrics.MissingCostSignalFired; got != 0 {
		t.Fatalf("missing_cost_signal_fired = %d, want 0", got)
	}
	if res.Metrics.AvgExecutionCostPct == nil || math.Abs(*res.Metrics.AvgExecutionCostPct-0.5) > 0.01 {
		t.Fatalf("avg_execution_cost_pct = %v, want about 0.5", res.Metrics.AvgExecutionCostPct)
	}
	if res.Metrics.AvgNetExcessReturnPct == nil || math.Abs(*res.Metrics.AvgNetExcessReturnPct-63.75) > 0.01 {
		t.Fatalf("avg_net_excess_return_pct = %v, want about 63.75", res.Metrics.AvgNetExcessReturnPct)
	}
	if res.Metrics.MedianNetExcessReturnPct == nil || math.Abs(*res.Metrics.MedianNetExcessReturnPct-41.55) > 0.01 {
		t.Fatalf("median_net_excess_return_pct = %v, want about 41.55", res.Metrics.MedianNetExcessReturnPct)
	}
	if res.Metrics.WorstNetExcessReturnPct == nil || math.Abs(*res.Metrics.WorstNetExcessReturnPct-(-68.4)) > 0.01 {
		t.Fatalf("worst_net_excess_return_pct = %v, want about -68.4", res.Metrics.WorstNetExcessReturnPct)
	}
	if res.Metrics.NetExcessHitRate == nil || math.Abs(*res.Metrics.NetExcessHitRate-(4.0/6.0)) > 0.001 {
		t.Fatalf("net_excess_hit_rate = %v, want 4/6", res.Metrics.NetExcessHitRate)
	}
	if !strings.Contains(strings.Join(res.Findings, "\n"), "Cost-adjusted net excess distribution") {
		t.Fatalf("findings missing net distribution: %+v", res.Findings)
	}
}

func TestOpportunityBacktestPortfolioSimulationMetrics(t *testing.T) {
	t.Parallel()
	rows := readOpportunityBacktestFixture(t)
	res := runOpportunityBacktest(rows, time.Date(2026, 5, 31, 12, 5, 0, 0, time.UTC))

	if got, want := res.Simulation.Model, "equal_weight_slots_v1"; got != want {
		t.Fatalf("simulation model = %q, want %q", got, want)
	}
	if got, want := res.Simulation.Signals, 6; got != want {
		t.Fatalf("simulation signals = %d, want %d", got, want)
	}
	if got, want := res.Simulation.FilledSignals, 6; got != want {
		t.Fatalf("simulation filled = %d, want %d", got, want)
	}
	if got, want := res.Simulation.SkippedSignals, 0; got != want {
		t.Fatalf("simulation skipped = %d, want %d", got, want)
	}
	if got, want := res.Simulation.MaxConcurrent, 4; got != want {
		t.Fatalf("simulation max concurrent = %d, want %d", got, want)
	}
	if res.Simulation.PortfolioReturnPct == nil || math.Abs(*res.Simulation.PortfolioReturnPct-56.0) > 0.01 {
		t.Fatalf("simulation portfolio return = %v, want about 56.0", res.Simulation.PortfolioReturnPct)
	}
	if res.Simulation.BenchmarkReturnPct == nil || math.Abs(*res.Simulation.BenchmarkReturnPct-17.7) > 0.01 {
		t.Fatalf("simulation benchmark return = %v, want about 17.7", res.Simulation.BenchmarkReturnPct)
	}
	if res.Simulation.ExcessReturnPct == nil || math.Abs(*res.Simulation.ExcessReturnPct-38.3) > 0.01 {
		t.Fatalf("simulation excess return = %v, want about 38.3", res.Simulation.ExcessReturnPct)
	}
	if res.Simulation.TurnoverPct == nil || math.Abs(*res.Simulation.TurnoverPct-60.0) > 0.01 {
		t.Fatalf("simulation turnover = %v, want about 60.0", res.Simulation.TurnoverPct)
	}
	if got, want := res.Simulation.InvestedExposureDays, 1831; got != want {
		t.Fatalf("simulation invested exposure days = %d, want %d", got, want)
	}
	if got, want := res.Simulation.CashDragDays, 5469; got != want {
		t.Fatalf("simulation cash drag days = %d, want %d", got, want)
	}
	if res.Simulation.AvgConcurrent == nil || math.Abs(*res.Simulation.AvgConcurrent-2.5082) > 0.001 {
		t.Fatalf("simulation avg concurrent = %v, want about 2.508", res.Simulation.AvgConcurrent)
	}
	if got, want := res.Simulation.WindowStart, "2023-01-04"; got != want {
		t.Fatalf("simulation window start = %q, want %q", got, want)
	}
	if got, want := res.Simulation.WindowEnd, "2025-01-02"; got != want {
		t.Fatalf("simulation window end = %q, want %q", got, want)
	}
	if got := strings.Join(res.Simulation.Limitations, "\n"); !strings.Contains(got, "not daily mark-to-market") {
		t.Fatalf("simulation limitations = %+v, want mark-to-market limitation", res.Simulation.Limitations)
	}
}

func TestOpportunityBacktestMarkToMarketSimulationMetrics(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	f, err := os.Open(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	bars, err := readOpportunityPriceBars(f)
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows, opportunityPriceBarLedger{
		BySymbol: bars,
		Source:   opportunityPriceBarsFixturePath(t),
		Checksum: "sha256:fixture-bars",
	})
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	res := runOpportunityBacktestWithSlots(observations, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), 2)
	if err := applyOpportunityMarkToMarketSimulation(&res, opportunityPriceBarLedger{
		BySymbol: bars,
		Source:   opportunityPriceBarsFixturePath(t),
		Checksum: "sha256:fixture-bars",
	}); err != nil {
		t.Fatal(err)
	}

	mtm := res.Simulation.MarkToMarket
	if mtm == nil {
		t.Fatal("mark-to-market simulation missing")
	}
	if got, want := mtm.Model, "equal_weight_slots_mtm_v1"; got != want {
		t.Fatalf("mtm model = %q, want %q", got, want)
	}
	if got, want := mtm.Bars, 15; got != want {
		t.Fatalf("mtm bars = %d, want %d", got, want)
	}
	if mtm.PortfolioReturnPct == nil || math.Abs(*mtm.PortfolioReturnPct-19.5) > 0.01 {
		t.Fatalf("mtm portfolio return = %v, want about 19.5", mtm.PortfolioReturnPct)
	}
	if mtm.BenchmarkReturnPct == nil || math.Abs(*mtm.BenchmarkReturnPct-4.0) > 0.01 {
		t.Fatalf("mtm benchmark return = %v, want about 4.0", mtm.BenchmarkReturnPct)
	}
	if mtm.MaxDrawdownPct == nil || math.Abs(*mtm.MaxDrawdownPct-(-2.8)) > 0.01 {
		t.Fatalf("mtm max drawdown = %v, want about -2.8", mtm.MaxDrawdownPct)
	}
	if mtm.BenchmarkMaxDrawdownPct == nil || math.Abs(*mtm.BenchmarkMaxDrawdownPct-(-2.4)) > 0.01 {
		t.Fatalf("mtm benchmark max drawdown = %v, want about -2.4", mtm.BenchmarkMaxDrawdownPct)
	}
	if got, want := mtm.MinTradeMarks, 3; got != want {
		t.Fatalf("mtm min trade marks = %d, want %d", got, want)
	}
	if got, want := mtm.MaxTradeMarkGapDays, 75; got != want {
		t.Fatalf("mtm max trade mark gap days = %d, want %d", got, want)
	}
	if mtm.BarReturnVolPct == nil || math.Abs(*mtm.BarReturnVolPct-5.2) > 0.01 {
		t.Fatalf("mtm bar return vol = %v, want about 5.2", mtm.BarReturnVolPct)
	}
	if mtm.SourceChecksum != "sha256:fixture-bars" || mtm.PriceBasis != "adjusted_close" {
		t.Fatalf("mtm source = %q basis=%q, want checksum and adjusted_close", mtm.SourceChecksum, mtm.PriceBasis)
	}
	if got, want := mtm.SourceQuality, "fixture_source"; got != want {
		t.Fatalf("mtm source quality = %q, want %q", got, want)
	}
	if got := strings.Join(mtm.SourceWarnings, "\n"); !strings.Contains(got, "fixture/test source") {
		t.Fatalf("mtm source warnings = %+v, want fixture/test warning", mtm.SourceWarnings)
	}
	if res.Simulation.Holdout == nil || res.Simulation.Holdout.MarkToMarket == nil {
		t.Fatal("holdout mark-to-market simulation missing")
	}
	if got, want := res.Simulation.Holdout.FilledSignals, 1; got != want {
		t.Fatalf("holdout filled signals = %d, want %d", got, want)
	}
	if res.Simulation.Holdout.MarkToMarket.ExcessReturnPct == nil {
		t.Fatalf("holdout mtm excess return missing: %+v", res.Simulation.Holdout.MarkToMarket)
	}
}

func TestOpportunityPriceBarLedgerManifestTrustContract(t *testing.T) {
	t.Parallel()
	barsPath, manifestPath := writeTrustedOpportunityBarsFixtureWithManifest(t, nil)

	unattested, err := readOpportunityPriceBarLedgerFromFile(barsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := unattested.SourceQuality, "unattested_source"; got != want {
		t.Fatalf("trusted bars without manifest source quality = %q, want %q", got, want)
	}
	if got := strings.Join(unattested.SourceWarnings, "\n"); !strings.Contains(got, "--bars-manifest") {
		t.Fatalf("warnings = %q, want bars manifest warning", got)
	}

	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ledger.SourceQuality, "ok"; got != want {
		t.Fatalf("manifest-backed source quality = %q, want %q", got, want)
	}
	if !opportunityIsSHA256Checksum(ledger.ManifestChecksum) {
		t.Fatalf("manifest checksum = %q, want sha256", ledger.ManifestChecksum)
	}
	if got := ledger.SourceProvider; got != "IBKR HMDS" {
		t.Fatalf("source provider = %q, want IBKR HMDS", got)
	}
}

func TestOpportunityPriceBarLedgerManifestRejectsMismatch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		mutate     func(*opportunityPriceBarManifest)
		wantReason string
	}{
		{
			name: "checksum",
			mutate: func(m *opportunityPriceBarManifest) {
				m.BarsSHA256 = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
			wantReason: "bars_sha256",
		},
		{
			name: "row_count",
			mutate: func(m *opportunityPriceBarManifest) {
				m.RowCount++
			},
			wantReason: "row_count",
		},
		{
			name: "exporter_id",
			mutate: func(m *opportunityPriceBarManifest) {
				m.ExporterID = "hand_edited_exporter"
			},
			wantReason: "exporter_id",
		},
		{
			name: "symbol_coverage",
			mutate: func(m *opportunityPriceBarManifest) {
				m.Symbols[0].Bars++
			},
			wantReason: "symbols[AMD]",
		},
		{
			name: "wrong_adjustment_feed",
			mutate: func(m *opportunityPriceBarManifest) {
				m.WhatToShow = "TRADES"
			},
			wantReason: "ADJUSTED_LAST",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			barsPath, manifestPath := writeTrustedOpportunityBarsFixtureWithManifest(t, tc.mutate)
			_, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("error = %v, want reason %q", err, tc.wantReason)
			}
		})
	}
}

func TestOpportunityPriceBarLedgerManifestRequiresAdjustedClose(t *testing.T) {
	t.Parallel()
	source := opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV
	content := strings.Join([]string{
		`{"symbol":"AAPL","date":"2024-01-02","close":100,"source":"` + source + `"}`,
		`{"symbol":"QQQ","date":"2024-01-02","close":100,"source":"` + source + `"}`,
	}, "\n") + "\n"
	barsPath, manifestPath := writeOpportunityBarsWithManifest(t, content, nil)
	_, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err == nil || !strings.Contains(err.Error(), "adjusted_close is required") {
		t.Fatalf("error = %v, want adjusted_close blocker", err)
	}
}

func TestExportOpportunityPriceBarsWritesManifestBackedLedger(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	barsPath := filepath.Join(dir, "bars.jsonl")
	manifestPath := filepath.Join(dir, "bars.manifest.json")
	type call struct {
		symbol     string
		days       int
		whatToShow string
	}
	var calls []call
	fetch := func(_ context.Context, symbol string, days int, whatToShow string) (rpc.HistoryDailyResult, error) {
		calls = append(calls, call{symbol: symbol, days: days, whatToShow: whatToShow})
		return rpc.HistoryDailyResult{
			Symbol:     symbol,
			Days:       days,
			WhatToShow: whatToShow,
			Bars: []rpc.HistoryBar{
				{Date: "2024-01-02", Open: 100, High: 102, Low: 99, Close: 101, Volume: 1000},
				{Date: "2024-01-03", Open: 101, High: 104, Low: 100, Close: 103, Volume: 1200},
			},
			AsOf: time.Date(2024, 1, 3, 22, 0, 0, 0, time.UTC),
		}, nil
	}
	now := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	res, err := exportOpportunityPriceBars(context.Background(), opportunityPriceBarExportOptions{
		Symbols:         []string{"amd", "NVDA", "AMD"},
		Benchmark:       "qqq",
		LookbackDays:    7,
		BarsPath:        barsPath,
		ManifestPath:    manifestPath,
		ExporterVersion: "test-exporter",
	}, fetch, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(res.Symbols, ","), "AMD,NVDA,QQQ"; got != want {
		t.Fatalf("symbols = %s, want %s", got, want)
	}
	if len(calls) != 3 {
		t.Fatalf("fetch calls = %+v, want 3", calls)
	}
	for i, wantSymbol := range []string{"AMD", "NVDA", "QQQ"} {
		if calls[i].symbol != wantSymbol || calls[i].days != 7 || calls[i].whatToShow != "ADJUSTED_LAST" {
			t.Fatalf("call[%d] = %+v, want %s/7/ADJUSTED_LAST", i, calls[i], wantSymbol)
		}
	}
	if res.RowCount != 6 {
		t.Fatalf("row count = %d, want 6", res.RowCount)
	}
	if !opportunityIsSHA256Checksum(res.BarsSHA256) || !opportunityIsSHA256Checksum(res.ManifestSHA256) {
		t.Fatalf("checksums = %q %q, want sha256", res.BarsSHA256, res.ManifestSHA256)
	}

	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ledger.SourceQuality, "ok"; got != want {
		t.Fatalf("source quality = %q, want %q", got, want)
	}
	if got := opportunityPriceBarRowCount(ledger.BySymbol); got != 6 {
		t.Fatalf("ledger rows = %d, want 6", got)
	}
	if got := ledger.BySymbol["AMD"][0].AdjustedClose; got != 101 {
		t.Fatalf("adjusted close = %g, want 101", got)
	}
	manifest, _, err := readOpportunityPriceBarManifestFromFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.WhatToShow != "ADJUSTED_LAST" || manifest.PriceBasis != "adjusted_close" {
		t.Fatalf("manifest feed/basis = %q/%q", manifest.WhatToShow, manifest.PriceBasis)
	}
	if manifest.ExporterVersion != "test-exporter" {
		t.Fatalf("manifest exporter version = %q", manifest.ExporterVersion)
	}
}

func TestExportOpportunityPriceBarsRequiresAdjustedHistoryEcho(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fetch := func(_ context.Context, symbol string, days int, _ string) (rpc.HistoryDailyResult, error) {
		return rpc.HistoryDailyResult{
			Symbol:     symbol,
			Days:       days,
			WhatToShow: "TRADES",
			Bars:       []rpc.HistoryBar{{Date: "2024-01-02", Close: 101}},
		}, nil
	}
	_, err := exportOpportunityPriceBars(context.Background(), opportunityPriceBarExportOptions{
		Symbols:      []string{"AMD"},
		BarsPath:     filepath.Join(dir, "bars.jsonl"),
		ManifestPath: filepath.Join(dir, "bars.manifest.json"),
	}, fetch, time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC))
	if err == nil || !strings.Contains(err.Error(), "expected ADJUSTED_LAST") {
		t.Fatalf("error = %v, want ADJUSTED_LAST fail-closed", err)
	}
}

func TestBuildOpportunityPointInTimeRowsFromBarsAndDeriveTargets(t *testing.T) {
	t.Parallel()
	source := opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := range 260 {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		if err := enc.Encode(OpportunityPriceBarRow{
			Symbol:        "ALFA",
			Date:          date,
			Close:         100 + float64(i)*0.30,
			AdjustedClose: 100 + float64(i)*0.30,
			Volume:        1_000_000,
			Source:        source,
		}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(OpportunityPriceBarRow{
			Symbol:        "QQQ",
			Date:          date,
			Close:         100 + float64(i)*0.10,
			AdjustedClose: 100 + float64(i)*0.10,
			Volume:        1_000_000,
			Source:        source,
		}); err != nil {
			t.Fatal(err)
		}
	}
	barsPath, manifestPath := writeOpportunityBarsWithManifest(t, buf.String(), nil)
	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := buildOpportunityPointInTimeRowsFromBars(ledger, opportunityHistoricalPanelOptions{
		Symbols:          []string{"ALFA"},
		Benchmark:        "QQQ",
		SampleStepBars:   20,
		HorizonDays:      20,
		RoundTripCostBps: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 {
		t.Fatal("expected historical PIT rows")
	}
	first := rows[0]
	if first.Features.DataType != opportunityHistoricalBarDataType || first.Features.QuoteQuality != opportunityHistoricalBarQuoteQuality {
		t.Fatalf("historical context = %q/%q", first.Features.DataType, first.Features.QuoteQuality)
	}
	if first.LabelStatus != "unscored_forward_window_pending" || first.Target.Kind != "" {
		t.Fatalf("initial label/target = %q/%+v, want pending and unlabeled", first.LabelStatus, first.Target)
	}
	if signal := opportunityPointInTimeSignal(first.Features); !signal.Fired {
		t.Fatalf("historical PIT signal did not fire: %+v", signal)
	}
	scored, err := scoreOpportunityPointInTimeRowsWithOptions(rows, ledger, opportunityScoreOptions{
		TargetPolicy: opportunityScoreTargetPolicyNetExcessPositive,
	})
	if err != nil {
		t.Fatal(err)
	}
	if scored[0].Target.Method != "net_excess_after_round_trip_cost_v1" {
		t.Fatalf("target method = %q", scored[0].Target.Method)
	}
	if !scored[0].Target.Opportunity {
		t.Fatalf("expected positive net-excess target: outcome=%+v target=%+v", scored[0].Outcome, scored[0].Target)
	}
	observations := buildOpportunityBacktestObservations(scored)
	if err := validateOpportunityBacktestObservationsSourced(observations); err != nil {
		t.Fatalf("generated observations should validate: %v", err)
	}
}

func TestBuildOpportunityPointInTimeRowsFromBarsAssignsDateHoldout(t *testing.T) {
	t.Parallel()
	source := opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	start := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := range 320 {
		date := start.AddDate(0, 0, i).Format("2006-01-02")
		if err := enc.Encode(OpportunityPriceBarRow{
			Symbol:        "ALFA",
			Date:          date,
			Close:         100 + float64(i)*0.30,
			AdjustedClose: 100 + float64(i)*0.30,
			Volume:        1_000_000,
			Source:        source,
		}); err != nil {
			t.Fatal(err)
		}
		if err := enc.Encode(OpportunityPriceBarRow{
			Symbol:        "QQQ",
			Date:          date,
			Close:         100 + float64(i)*0.10,
			AdjustedClose: 100 + float64(i)*0.10,
			Volume:        1_000_000,
			Source:        source,
		}); err != nil {
			t.Fatal(err)
		}
	}
	barsPath, manifestPath := writeOpportunityBarsWithManifest(t, buf.String(), nil)
	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	baseRows, err := buildOpportunityPointInTimeRowsFromBars(ledger, opportunityHistoricalPanelOptions{
		Symbols:          []string{"ALFA"},
		Benchmark:        "QQQ",
		SampleStepBars:   20,
		HorizonDays:      20,
		RoundTripCostBps: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(baseRows) < 3 {
		t.Fatalf("expected at least 3 rows, got %d", len(baseRows))
	}
	holdoutStart := baseRows[len(baseRows)/2].Date
	rows, err := buildOpportunityPointInTimeRowsFromBars(ledger, opportunityHistoricalPanelOptions{
		Symbols:          []string{"ALFA"},
		Benchmark:        "QQQ",
		SampleStepBars:   20,
		HorizonDays:      20,
		RoundTripCostBps: 50,
		HoldoutStartDate: holdoutStart,
		HoldoutPlan:      "walk-forward-2024q4",
	})
	if err != nil {
		t.Fatal(err)
	}
	var tuning, holdout int
	for _, row := range rows {
		if err := validateOpportunityPointInTimeSplitProvenance(row); err != nil {
			t.Fatalf("split provenance should validate for %s: %v", row.Date, err)
		}
		if row.Date < holdoutStart {
			tuning++
			if row.Split != "tuning" || !row.SplitProvenance.IsZero() {
				t.Fatalf("pre-holdout row split/provenance = %q/%+v, want tuning/no provenance", row.Split, row.SplitProvenance)
			}
			continue
		}
		holdout++
		if row.Split != "holdout" {
			t.Fatalf("row %s split = %q, want holdout", row.Date, row.Split)
		}
		if row.SplitProvenance.Source != "build-opportunity-pit" ||
			row.SplitProvenance.Method != opportunityHistoricalPanelSplitMethod ||
			row.SplitProvenance.PlanID != "walk-forward-2024q4" ||
			row.SplitProvenance.PreRegistered {
			t.Fatalf("holdout provenance = %+v", row.SplitProvenance)
		}
	}
	if tuning == 0 || holdout == 0 {
		t.Fatalf("expected both tuning and holdout rows, got tuning=%d holdout=%d", tuning, holdout)
	}
	scored, err := scoreOpportunityPointInTimeRowsWithOptions(rows, ledger, opportunityScoreOptions{
		TargetPolicy: opportunityScoreTargetPolicyNetExcessPositive,
	})
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	if err := validateOpportunityBacktestObservationsSourced(observations); err != nil {
		t.Fatalf("scored holdout observations should validate: %v", err)
	}
	res := runOpportunityBacktest(observations, time.Date(2026, 6, 16, 8, 0, 0, 0, time.UTC))
	if res.Metrics.HoldoutObservations != holdout {
		t.Fatalf("holdout observations = %d, want %d", res.Metrics.HoldoutObservations, holdout)
	}
	if res.Metrics.RetrospectiveHoldoutObservations != holdout {
		t.Fatalf("retrospective holdout observations = %d, want %d", res.Metrics.RetrospectiveHoldoutObservations, holdout)
	}
}

func TestOpportunityBacktestMarkToMarketRejectsMismatchedOutcomeBars(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows, ledger)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	res := runOpportunityBacktestWithSlots(observations, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), 2)
	ledger.Checksum = "sha256:different-bars"
	err = applyOpportunityMarkToMarketSimulation(&res, ledger)
	if err == nil || !strings.Contains(err.Error(), "do not match --bars ledger") {
		t.Fatalf("mark-to-market error = %v, want checksum mismatch", err)
	}
}

func TestOpportunityBacktestMarkToMarketRejectsTamperedOutcomeValues(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows, ledger)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	res := runOpportunityBacktestWithSlots(observations, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), 2)
	res.Observations[0].Outcome.ExcessReturnPct += 100
	err = applyOpportunityMarkToMarketSimulation(&res, ledger)
	if err == nil || !strings.Contains(err.Error(), "outcome.excess_return_pct") {
		t.Fatalf("mark-to-market error = %v, want outcome reconciliation mismatch", err)
	}
}

func TestOpportunityBacktestMarkToMarketRejectsTamperedOutcomeWindow(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	ledger.Checksum = "sha256:test-bars"
	ledger.BySymbol["QQQ"] = appendOpportunityTestBarSorted(ledger.BySymbol["QQQ"], OpportunityPriceBarRow{
		Symbol:        "QQQ",
		Date:          "2024-07-01",
		AdjustedClose: 110,
		Source:        "fixture_adjusted_ohlcv",
	})
	scored, err := scoreOpportunityPointInTimeRows(rows, ledger)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	setOpportunityOutcomeFromLedger(t, &observations[0].Outcome, ledger, "NVDA", "QQQ", "2024-01-03", "2024-07-01")
	res := runOpportunityBacktestWithSlots(observations, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), 2)

	err = applyOpportunityMarkToMarketSimulation(&res, ledger)
	if err == nil || !strings.Contains(err.Error(), "outcome.exit_date 2024-07-01 does not match trade rule 2024-05-08") {
		t.Fatalf("mark-to-market error = %v, want trade-window reconciliation mismatch", err)
	}
}

func TestOpportunityBacktestMarkToMarketFailsClosedOnMissingBars(t *testing.T) {
	t.Parallel()
	rows := []OpportunityBacktestObservation{{
		Date: "2024-01-02",
		Signal: OpportunityBacktestSignal{
			Fired: true,
		},
		Trade: OpportunityBacktestTrade{
			Instrument:       "MISSING",
			Benchmark:        "QQQ",
			RoundTripCostBps: new(50.0),
		},
		Outcome: OpportunityBacktestOutcome{
			EntryDate:               "2024-01-02",
			ExitDate:                "2024-01-03",
			Formula:                 opportunityOutcomeFormulaCloseToClose,
			PriceBasis:              "adjusted_close",
			SourceChecksum:          "sha256:fixture-bars",
			BenchmarkSourceChecksum: "sha256:fixture-bars",
		},
		Target: OpportunityBacktestTarget{
			Source: "fixture",
			Method: "fixture",
		},
	}}
	res := runOpportunityBacktestWithSlots(rows, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC), 1)
	err := applyOpportunityMarkToMarketSimulation(&res, opportunityPriceBarLedger{
		BySymbol: map[string][]OpportunityPriceBarRow{
			"QQQ": {
				{Symbol: "QQQ", Date: "2024-01-02", AdjustedClose: 100},
				{Symbol: "QQQ", Date: "2024-01-03", AdjustedClose: 101},
			},
		},
		Source:   "fixture-bars",
		Checksum: "sha256:fixture-bars",
	})
	if err == nil || !strings.Contains(err.Error(), "no price bars for MISSING") {
		t.Fatalf("mark-to-market error = %v, want missing instrument bars", err)
	}
}

func TestOpportunityPriceBarsRejectDuplicateSymbolDate(t *testing.T) {
	t.Parallel()
	input := strings.NewReader(strings.Join([]string{
		`{"symbol":"NVDA","date":"2024-01-02","adjusted_close":50}`,
		`{"symbol":"nvda","date":"2024-01-02","adjusted_close":51}`,
	}, "\n"))
	_, err := readOpportunityPriceBars(input)
	if err == nil || !strings.Contains(err.Error(), "duplicate price bar for NVDA on 2024-01-02") {
		t.Fatalf("readOpportunityPriceBars error = %v, want duplicate bar blocker", err)
	}
}

func TestOpportunitySimulationSkipsWhenSlotsFull(t *testing.T) {
	t.Parallel()
	cost := 0.0
	rows := []OpportunityBacktestRowResult{
		{
			Case:        "first",
			SignalFired: true,
			Trade:       OpportunityBacktestTrade{Instrument: "AAA"},
			Outcome: OpportunityBacktestOutcome{
				EntryDate:          "2024-01-02",
				ExitDate:           "2024-02-01",
				ForwardReturnPct:   10,
				BenchmarkReturnPct: 2,
			},
			ExecutionCostPct: &cost,
		},
		{
			Case:        "second",
			SignalFired: true,
			Trade:       OpportunityBacktestTrade{Instrument: "BBB"},
			Outcome: OpportunityBacktestOutcome{
				EntryDate:          "2024-01-03",
				ExitDate:           "2024-02-02",
				ForwardReturnPct:   20,
				BenchmarkReturnPct: 2,
			},
			ExecutionCostPct: &cost,
		},
	}
	sim := simulateOpportunityBacktestSlots(rows, 1)
	if got, want := sim.FilledSignals, 1; got != want {
		t.Fatalf("filled = %d, want %d", got, want)
	}
	if got, want := sim.SkippedSignals, 1; got != want {
		t.Fatalf("skipped = %d, want %d", got, want)
	}
	if sim.PortfolioReturnPct == nil || math.Abs(*sim.PortfolioReturnPct-10) > 0.01 {
		t.Fatalf("portfolio return = %v, want 10", sim.PortfolioReturnPct)
	}
}

func TestRunBacktestOpportunityMaxSlotsRendersCapacity(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"opportunity",
		"--input", opportunityBuiltObservationFixturePath(t),
		"--max-slots", "1",
	})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
	out := stdout.String()
	want := "Portfolio    1/2 filled · return +24.5% vs bench +5.0% · excess +19.5% · max open 1 · turnover +100.0%"
	if !strings.Contains(out, want) {
		t.Fatalf("opportunity backtest output missing max-slot simulation %q:\n%s", want, out)
	}
}

func TestRunBacktestOpportunityBarsRendersMarkToMarket(t *testing.T) {
	t.Parallel()
	rows := readOpportunityPointInTimeFixture(t)
	ledger, err := readOpportunityPriceBarLedgerFromFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows, ledger)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	var input bytes.Buffer
	if err := writeOpportunityBacktestObservationsJSONL(&input, observations); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opportunity.jsonl")
	if err := os.WriteFile(path, input.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"opportunity",
		"--input", path,
		"--max-slots", "2",
		"--bars", opportunityPriceBarsFixturePath(t),
	})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
	want := "MTM          15 bars · return +19.5% vs bench +4.0% · max DD -2.8% vs bench -2.4% · vol +5.2%"
	if !strings.Contains(stdout.String(), want) {
		t.Fatalf("opportunity backtest output missing mark-to-market line %q:\n%s", want, stdout.String())
	}
}

func TestRunBacktestOpportunityBarsManifestRendersSourceAttestation(t *testing.T) {
	t.Parallel()
	barsPath, manifestPath := writeTrustedOpportunityBarsFixtureWithManifest(t, nil)
	rows := readOpportunityPointInTimeFixture(t)
	ledger, err := readOpportunityPriceBarLedgerFromFileWithManifest(barsPath, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	scored, err := scoreOpportunityPointInTimeRows(rows, ledger)
	if err != nil {
		t.Fatal(err)
	}
	observations := buildOpportunityBacktestObservations(scored)
	var input bytes.Buffer
	if err := writeOpportunityBacktestObservationsJSONL(&input, observations); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opportunity.jsonl")
	if err := os.WriteFile(path, input.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"opportunity",
		"--input", path,
		"--max-slots", "2",
		"--bars", barsPath,
		"--bars-manifest", manifestPath,
	})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Bar source   ok · ibkr_hmds_adjusted_ohlcv_v1 · checksum sha256:",
		"manifest sha256:",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("opportunity backtest output missing %q:\n%s", want, out)
		}
	}
}

func TestOpportunityBacktestInvalidSplitCountsUnknown(t *testing.T) {
	t.Parallel()
	rows := readOpportunityBacktestFixture(t)[:1]
	rows[0].Split = "holddout"
	res := runOpportunityBacktest(rows, time.Date(2026, 5, 31, 12, 5, 0, 0, time.UTC))

	if got := res.Observations[0].Split; got != "unknown" {
		t.Fatalf("split = %q, want unknown", got)
	}
	if res.Observations[0].Holdout {
		t.Fatal("invalid split must not count as holdout")
	}
	if got, want := res.Metrics.UnknownSplitObservations, 1; got != want {
		t.Fatalf("unknown_split_observations = %d, want %d", got, want)
	}
	if res.Metrics.TuningObservations != 0 || res.Metrics.HoldoutObservations != 0 {
		t.Fatalf("invalid split counted as tuning/holdout: tuning=%d holdout=%d", res.Metrics.TuningObservations, res.Metrics.HoldoutObservations)
	}
}

func TestOpportunityBacktestDirtySignalContextBlocksEvidence(t *testing.T) {
	t.Parallel()
	cost := 50.0
	rows := []OpportunityBacktestObservation{{
		Date:            "2024-01-02",
		Split:           "holdout",
		SplitProvenance: testOpportunitySplitProvenance("dirty-sample-holdout-plan"),
		LabelStatus:     "scored",
		Signal: OpportunityBacktestSignal{
			Fired:   false,
			Kind:    opportunityBuilderSignalKind,
			Source:  opportunityBuilderSignalSource,
			Reasons: []string{"data_quality_not_ok", "technical_error", "price_missing"},
		},
		Trade: OpportunityBacktestTrade{
			Instrument:       "NVDA",
			RoundTripCostBps: &cost,
		},
		Target: OpportunityBacktestTarget{
			Opportunity: false,
			Kind:        "degraded audit control",
			Source:      "fixture_label_book:dirty",
			Method:      "manual_review_v1",
		},
		Outcome: OpportunityBacktestOutcome{
			EntryDate:               "2024-01-02",
			ExitDate:                "2024-07-01",
			PriceSource:             "fixture_adjusted_ohlcv:NVDA",
			BenchmarkSource:         "fixture_adjusted_ohlcv:QQQ",
			Formula:                 opportunityOutcomeFormulaCloseToClose,
			PriceBasis:              "adjusted_close",
			SourceChecksum:          "sha256:nvda",
			BenchmarkSourceChecksum: "sha256:qqq",
		},
	}}
	res := runOpportunityBacktest(rows, time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC))
	if got, want := res.Metrics.SignalContextBlocked, 1; got != want {
		t.Fatalf("signal_context_blocked = %d, want %d", got, want)
	}
	if got, want := res.Metrics.HoldoutSignalContextBlocked, 1; got != want {
		t.Fatalf("holdout_signal_context_blocked = %d, want %d", got, want)
	}
	if got, want := res.Evidence.Status, "dirty_sample"; got != want {
		t.Fatalf("evidence status = %q, want %q: %+v", got, want, res.Evidence)
	}
	if !strings.Contains(strings.Join(res.Findings, "\n"), "blocking alpha evidence") {
		t.Fatalf("findings = %+v, want dirty-sample warning", res.Findings)
	}
}

func TestOpportunityBacktestEvidenceStatus(t *testing.T) {
	t.Parallel()
	hitHalf, hitSixty := 0.5, 0.6
	median, avg := 5.0, 7.0
	netMedian, netAvg := 4.0, 6.0
	weakLowerHit, strongLowerHit := 0.49, 0.55
	weakLowerAvg, strongLowerAvg := -1.0, 1.0
	okInstrumentShare, crowdedInstrumentShare := 0.20, 0.30
	okClusterShare, crowdedClusterShare := 0.50, 0.70
	okHoldoutInstrumentShare, crowdedHoldoutInstrumentShare := 0.30, 0.50
	okHoldoutClusterShare := 0.70
	tests := []struct {
		name              string
		in                OpportunityBacktestMetrics
		withCandidateBase bool
		withHoldoutBase   bool
		want              string
		wantReason        string
	}{
		{name: "no data", in: OpportunityBacktestMetrics{}, want: "no_data"},
		{name: "dirty sample", in: OpportunityBacktestMetrics{Observations: 100, SignalContextBlocked: 1}, want: "dirty_sample", wantReason: "blocked signal context"},
		{name: "no signals", in: OpportunityBacktestMetrics{Observations: 100}, want: "no_signals"},
		{name: "insufficient sample", in: OpportunityBacktestMetrics{Observations: 99, SignalFired: 29}, want: "insufficient_sample"},
		{name: "no target opportunity labels", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            0,
			NonOpportunity:               100,
			SignalFired:                  30,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "insufficient_sample", wantReason: "target opportunities 0 < 30"},
		{name: "no negative controls", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            100,
			NonOpportunity:               0,
			SignalFired:                  30,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "insufficient_sample", wantReason: "non-opportunity controls 0 < 30"},
		{name: "insufficient controls", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            71,
			NonOpportunity:               29,
			SignalFired:                  30,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "insufficient_sample", wantReason: "non-opportunity controls 29 < 30"},
		{name: "unfavorable hit rate", in: OpportunityBacktestMetrics{
			Observations:          100,
			TargetOpportunity:     50,
			NonOpportunity:        50,
			SignalFired:           30,
			CostedSignalFired:     30,
			ExcessHitRate:         &hitHalf,
			MedianExcessReturnPct: &median,
			AvgExcessReturnPct:    &avg,
		}, want: "unfavorable"},
		{name: "missing costs", in: OpportunityBacktestMetrics{
			Observations:             100,
			TargetOpportunity:        50,
			NonOpportunity:           50,
			SignalFired:              30,
			CostedSignalFired:        29,
			MissingCostSignalFired:   1,
			ExcessHitRate:            &hitSixty,
			MedianExcessReturnPct:    &median,
			AvgExcessReturnPct:       &avg,
			NetExcessHitRate:         &hitSixty,
			MedianNetExcessReturnPct: &netMedian,
			AvgNetExcessReturnPct:    &netAvg,
		}, want: "missing_costs"},
		{name: "weak lower-bound hit rate", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     6,
			MaxSignalInstrumentShare:     &okInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        15,
			MaxSignalClusterShare:        &okClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &weakLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "weak_edge"},
		{name: "weak lower-bound average net", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     6,
			MaxSignalInstrumentShare:     &okInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        15,
			MaxSignalClusterShare:        &okClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &weakLowerAvg,
		}, want: "weak_edge"},
		{name: "concentrated signal instrument", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     9,
			MaxSignalInstrumentShare:     &crowdedInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        15,
			MaxSignalClusterShare:        &okClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "concentrated_sample", wantReason: "largest fired instrument NVDA is 9/30"},
		{name: "concentrated market cluster", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     6,
			MaxSignalInstrumentShare:     &okInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        21,
			MaxSignalClusterShare:        &crowdedClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "concentrated_sample", wantReason: "largest fired cluster AI infrastructure is 21/30"},
		{name: "unknown split blocks otherwise eligible aggregate", in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     6,
			MaxSignalInstrumentShare:     &okInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        15,
			MaxSignalClusterShare:        &okClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
			UnknownSplitObservations:     1,
		}, want: "unknown_split", wantReason: "1 observation(s) have unknown split"},
		{name: "retrospective holdout blocks alpha evidence", in: OpportunityBacktestMetrics{
			Observations:                     100,
			TargetOpportunity:                50,
			NonOpportunity:                   50,
			SignalFired:                      30,
			RetrospectiveHoldoutObservations: 30,
		}, want: "retrospective_holdout", wantReason: "retrospective historical date split"},
		{name: "aggregate gates passed without holdout", withCandidateBase: true, in: OpportunityBacktestMetrics{
			Observations:                 100,
			TargetOpportunity:            50,
			NonOpportunity:               50,
			SignalFired:                  30,
			DistinctSignalInstruments:    10,
			MaxSignalInstrument:          "NVDA",
			MaxSignalInstrumentFired:     6,
			MaxSignalInstrumentShare:     &okInstrumentShare,
			DistinctSignalClusters:       3,
			MaxSignalCluster:             "AI infrastructure",
			MaxSignalClusterFired:        15,
			MaxSignalClusterShare:        &okClusterShare,
			CostedSignalFired:            30,
			ExcessHitRate:                &hitSixty,
			ExcessHitRateLower95:         &strongLowerHit,
			MedianExcessReturnPct:        &median,
			AvgExcessReturnPct:           &avg,
			AvgExcessReturnLower95Pct:    &strongLowerAvg,
			NetExcessHitRate:             &hitSixty,
			NetExcessHitRateLower95:      &strongLowerHit,
			MedianNetExcessReturnPct:     &netMedian,
			AvgNetExcessReturnPct:        &netAvg,
			AvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "no_holdout", wantReason: "no holdout/out-of-sample rows were present"},
		{name: "insufficient holdout", withCandidateBase: true, in: OpportunityBacktestMetrics{
			Observations:                        100,
			TargetOpportunity:                   50,
			NonOpportunity:                      50,
			SignalFired:                         30,
			DistinctSignalInstruments:           10,
			MaxSignalInstrument:                 "NVDA",
			MaxSignalInstrumentFired:            6,
			MaxSignalInstrumentShare:            &okInstrumentShare,
			DistinctSignalClusters:              3,
			MaxSignalCluster:                    "AI infrastructure",
			MaxSignalClusterFired:               15,
			MaxSignalClusterShare:               &okClusterShare,
			CostedSignalFired:                   30,
			ExcessHitRate:                       &hitSixty,
			ExcessHitRateLower95:                &strongLowerHit,
			MedianExcessReturnPct:               &median,
			AvgExcessReturnPct:                  &avg,
			AvgExcessReturnLower95Pct:           &strongLowerAvg,
			NetExcessHitRate:                    &hitSixty,
			NetExcessHitRateLower95:             &strongLowerHit,
			MedianNetExcessReturnPct:            &netMedian,
			AvgNetExcessReturnPct:               &netAvg,
			AvgNetExcessReturnLower95Pct:        &strongLowerAvg,
			HoldoutObservations:                 29,
			HoldoutSignalFired:                  10,
			HoldoutTargetOpportunity:            10,
			HoldoutNonOpportunity:               10,
			HoldoutCostedSignalFired:            10,
			HoldoutPositiveNetExcess:            6,
			HoldoutDistinctSignalInstruments:    5,
			HoldoutMaxSignalInstrument:          "NVDA",
			HoldoutMaxSignalInstrumentFired:     3,
			HoldoutMaxSignalInstrumentShare:     &okHoldoutInstrumentShare,
			HoldoutDistinctSignalClusters:       2,
			HoldoutMaxSignalCluster:             "AI infrastructure",
			HoldoutMaxSignalClusterFired:        7,
			HoldoutMaxSignalClusterShare:        &okHoldoutClusterShare,
			HoldoutNetExcessHitRate:             &hitSixty,
			HoldoutNetExcessHitRateLower95:      &strongLowerHit,
			HoldoutAvgNetExcessReturnPct:        &netAvg,
			HoldoutAvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "insufficient_holdout", wantReason: "holdout observations 29 < 30"},
		{name: "weak holdout", withCandidateBase: true, in: OpportunityBacktestMetrics{
			Observations:                        100,
			TargetOpportunity:                   50,
			NonOpportunity:                      50,
			SignalFired:                         30,
			DistinctSignalInstruments:           10,
			MaxSignalInstrument:                 "NVDA",
			MaxSignalInstrumentFired:            6,
			MaxSignalInstrumentShare:            &okInstrumentShare,
			DistinctSignalClusters:              3,
			MaxSignalCluster:                    "AI infrastructure",
			MaxSignalClusterFired:               15,
			MaxSignalClusterShare:               &okClusterShare,
			CostedSignalFired:                   30,
			ExcessHitRate:                       &hitSixty,
			ExcessHitRateLower95:                &strongLowerHit,
			MedianExcessReturnPct:               &median,
			AvgExcessReturnPct:                  &avg,
			AvgExcessReturnLower95Pct:           &strongLowerAvg,
			NetExcessHitRate:                    &hitSixty,
			NetExcessHitRateLower95:             &strongLowerHit,
			MedianNetExcessReturnPct:            &netMedian,
			AvgNetExcessReturnPct:               &netAvg,
			AvgNetExcessReturnLower95Pct:        &strongLowerAvg,
			HoldoutObservations:                 30,
			HoldoutSignalFired:                  10,
			HoldoutTargetOpportunity:            10,
			HoldoutNonOpportunity:               10,
			HoldoutCostedSignalFired:            10,
			HoldoutPositiveNetExcess:            6,
			HoldoutDistinctSignalInstruments:    5,
			HoldoutMaxSignalInstrument:          "NVDA",
			HoldoutMaxSignalInstrumentFired:     3,
			HoldoutMaxSignalInstrumentShare:     &okHoldoutInstrumentShare,
			HoldoutDistinctSignalClusters:       2,
			HoldoutMaxSignalCluster:             "AI infrastructure",
			HoldoutMaxSignalClusterFired:        7,
			HoldoutMaxSignalClusterShare:        &okHoldoutClusterShare,
			HoldoutNetExcessHitRate:             &hitSixty,
			HoldoutNetExcessHitRateLower95:      &weakLowerHit,
			HoldoutAvgNetExcessReturnPct:        &netAvg,
			HoldoutAvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "weak_holdout", wantReason: "holdout net excess hit-rate 95% lower bound"},
		{name: "concentrated holdout instrument", withCandidateBase: true, in: OpportunityBacktestMetrics{
			Observations:                        100,
			TargetOpportunity:                   50,
			NonOpportunity:                      50,
			SignalFired:                         30,
			DistinctSignalInstruments:           10,
			MaxSignalInstrument:                 "NVDA",
			MaxSignalInstrumentFired:            6,
			MaxSignalInstrumentShare:            &okInstrumentShare,
			DistinctSignalClusters:              3,
			MaxSignalCluster:                    "AI infrastructure",
			MaxSignalClusterFired:               15,
			MaxSignalClusterShare:               &okClusterShare,
			CostedSignalFired:                   30,
			ExcessHitRate:                       &hitSixty,
			ExcessHitRateLower95:                &strongLowerHit,
			MedianExcessReturnPct:               &median,
			AvgExcessReturnPct:                  &avg,
			AvgExcessReturnLower95Pct:           &strongLowerAvg,
			NetExcessHitRate:                    &hitSixty,
			NetExcessHitRateLower95:             &strongLowerHit,
			MedianNetExcessReturnPct:            &netMedian,
			AvgNetExcessReturnPct:               &netAvg,
			AvgNetExcessReturnLower95Pct:        &strongLowerAvg,
			HoldoutObservations:                 30,
			HoldoutSignalFired:                  10,
			HoldoutTargetOpportunity:            10,
			HoldoutNonOpportunity:               10,
			HoldoutCostedSignalFired:            10,
			HoldoutPositiveNetExcess:            6,
			HoldoutDistinctSignalInstruments:    5,
			HoldoutMaxSignalInstrument:          "NVDA",
			HoldoutMaxSignalInstrumentFired:     5,
			HoldoutMaxSignalInstrumentShare:     &crowdedHoldoutInstrumentShare,
			HoldoutDistinctSignalClusters:       2,
			HoldoutMaxSignalCluster:             "AI infrastructure",
			HoldoutMaxSignalClusterFired:        7,
			HoldoutMaxSignalClusterShare:        &okHoldoutClusterShare,
			HoldoutNetExcessHitRate:             &hitSixty,
			HoldoutNetExcessHitRateLower95:      &strongLowerHit,
			HoldoutAvgNetExcessReturnPct:        &netAvg,
			HoldoutAvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "concentrated_holdout", wantReason: "holdout largest fired instrument NVDA is 5/10"},
		{name: "sample lower-bound and holdout gates passed but no portfolio", withCandidateBase: true, withHoldoutBase: true, in: OpportunityBacktestMetrics{
			Observations:                        100,
			TargetOpportunity:                   50,
			NonOpportunity:                      50,
			SignalFired:                         30,
			DistinctSignalInstruments:           10,
			MaxSignalInstrument:                 "NVDA",
			MaxSignalInstrumentFired:            6,
			MaxSignalInstrumentShare:            &okInstrumentShare,
			DistinctSignalClusters:              3,
			MaxSignalCluster:                    "AI infrastructure",
			MaxSignalClusterFired:               15,
			MaxSignalClusterShare:               &okClusterShare,
			CostedSignalFired:                   30,
			ExcessHitRate:                       &hitSixty,
			ExcessHitRateLower95:                &strongLowerHit,
			MedianExcessReturnPct:               &median,
			AvgExcessReturnPct:                  &avg,
			AvgExcessReturnLower95Pct:           &strongLowerAvg,
			NetExcessHitRate:                    &hitSixty,
			NetExcessHitRateLower95:             &strongLowerHit,
			MedianNetExcessReturnPct:            &netMedian,
			AvgNetExcessReturnPct:               &netAvg,
			AvgNetExcessReturnLower95Pct:        &strongLowerAvg,
			HoldoutObservations:                 30,
			HoldoutSignalFired:                  10,
			HoldoutTargetOpportunity:            10,
			HoldoutNonOpportunity:               10,
			HoldoutCostedSignalFired:            10,
			HoldoutPositiveNetExcess:            6,
			HoldoutDistinctSignalInstruments:    5,
			HoldoutMaxSignalInstrument:          "NVDA",
			HoldoutMaxSignalInstrumentFired:     3,
			HoldoutMaxSignalInstrumentShare:     &okHoldoutInstrumentShare,
			HoldoutDistinctSignalClusters:       2,
			HoldoutMaxSignalCluster:             "AI infrastructure",
			HoldoutMaxSignalClusterFired:        7,
			HoldoutMaxSignalClusterShare:        &okHoldoutClusterShare,
			HoldoutNetExcessHitRate:             &hitSixty,
			HoldoutNetExcessHitRateLower95:      &strongLowerHit,
			HoldoutAvgNetExcessReturnPct:        &netAvg,
			HoldoutAvgNetExcessReturnLower95Pct: &strongLowerAvg,
		}, want: "missing_portfolio", wantReason: "portfolio simulation is required"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if tc.withCandidateBase {
				applyPassingOpportunityCandidateBaseline(&in)
			}
			if tc.withHoldoutBase {
				applyPassingOpportunityHoldoutCandidateBaseline(&in)
			}
			got := opportunityBacktestEvidence(in)
			if got.Status != tc.want {
				t.Fatalf("status = %q, want %q: %+v", got.Status, tc.want, got)
			}
			if tc.wantReason != "" && !strings.Contains(strings.Join(got.Reasons, "\n"), tc.wantReason) {
				t.Fatalf("reasons = %+v, want to contain %q", got.Reasons, tc.wantReason)
			}
		})
	}
}

func TestOpportunityBacktestEvidenceRequiresPortfolioAndMTM(t *testing.T) {
	t.Parallel()
	metrics := passingOpportunityEvidenceMetrics()
	sim := OpportunityBacktestSimulation{
		Model:              "equal_weight_slots_v1",
		Signals:            30,
		FilledSignals:      30,
		PortfolioReturnPct: new(12.0),
		ExcessReturnPct:    new(6.0),
	}

	got := opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "missing_mtm" {
		t.Fatalf("status = %q, want missing_mtm: %+v", got.Status, got)
	}
	if !strings.Contains(strings.Join(got.Reasons, "\n"), "mark-to-market bar simulation is required") {
		t.Fatalf("reasons = %+v, want missing MTM reason", got.Reasons)
	}

	sim.MarkToMarket = trustedOpportunityEvidenceMTM(12.0, 4.0, 8.0, -8.0)
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "missing_holdout_portfolio" {
		t.Fatalf("status = %q, want missing_holdout_portfolio: %+v", got.Status, got)
	}

	sim.Holdout = &OpportunityBacktestSimulation{
		Model:              "equal_weight_slots_v1",
		Signals:            10,
		FilledSignals:      10,
		PortfolioReturnPct: new(8.0),
		ExcessReturnPct:    new(4.0),
	}
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "missing_holdout_mtm" {
		t.Fatalf("status = %q, want missing_holdout_mtm: %+v", got.Status, got)
	}

	sim.Holdout.MarkToMarket = trustedOpportunityEvidenceMTM(8.0, 3.0, 5.0, -5.0)
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "promising_diagnostic" {
		t.Fatalf("status = %q, want promising_diagnostic: %+v", got.Status, got)
	}

	sim.MarkToMarket.MaxTradeMarkGapDays = opportunityEvidenceMaxMTMGapDays + 1
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "weak_mtm" || !strings.Contains(strings.Join(got.Reasons, "\n"), "bar coverage") {
		t.Fatalf("status/reasons = %q %+v, want weak sparse MTM", got.Status, got.Reasons)
	}

	sim.MarkToMarket.MaxTradeMarkGapDays = opportunityEvidenceMaxMTMGapDays
	sim.MarkToMarket.MaxDrawdownPct = new(-30.0)
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "weak_mtm" || !strings.Contains(strings.Join(got.Reasons, "\n"), "max drawdown") {
		t.Fatalf("status/reasons = %q %+v, want weak max-drawdown MTM", got.Status, got.Reasons)
	}

	sim.MarkToMarket.MaxDrawdownPct = new(-20.0)
	sim.MarkToMarket.ExcessReturnPct = new(5.0)
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "weak_mtm" || !strings.Contains(strings.Join(got.Reasons, "\n"), "excess/drawdown") {
		t.Fatalf("status/reasons = %q %+v, want weak excess/drawdown MTM", got.Status, got.Reasons)
	}

	sim.MarkToMarket.MaxDrawdownPct = new(-8.0)
	sim.MarkToMarket.ExcessReturnPct = new(8.0)
	sim.Holdout.MarkToMarket.MaxTradeMarkGapDays = opportunityEvidenceMaxMTMGapDays + 1
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "weak_holdout_mtm" || !strings.Contains(strings.Join(got.Reasons, "\n"), "holdout-only mark-to-market bar coverage") {
		t.Fatalf("status/reasons = %q %+v, want weak sparse holdout MTM", got.Status, got.Reasons)
	}

	sim.Holdout.MarkToMarket.MaxTradeMarkGapDays = opportunityEvidenceMaxMTMGapDays
	sim.Holdout.MarkToMarket.MaxDrawdownPct = new(-30.0)
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "weak_holdout_mtm" || !strings.Contains(strings.Join(got.Reasons, "\n"), "holdout-only mark-to-market max drawdown") {
		t.Fatalf("status/reasons = %q %+v, want weak holdout MTM drawdown", got.Status, got.Reasons)
	}
}

func TestOpportunityBacktestEvidenceRequiresCandidateBaselineLift(t *testing.T) {
	t.Parallel()
	metrics := passingOpportunityEvidenceMetrics()
	negativeLift := -0.25

	metrics.FiredVsCandidateAvgLiftPct = &negativeLift
	got := opportunityBacktestEvidence(metrics)
	if got.Status != "weak_candidate_baseline" || !strings.Contains(strings.Join(got.Reasons, "\n"), "all-candidate baseline") {
		t.Fatalf("status/reasons = %q %+v, want weak aggregate candidate baseline", got.Status, got.Reasons)
	}

	metrics = passingOpportunityEvidenceMetrics()
	metrics.FiredVsNonFiredAvgLiftPct = &negativeLift
	got = opportunityBacktestEvidence(metrics)
	if got.Status != "weak_candidate_baseline" || !strings.Contains(strings.Join(got.Reasons, "\n"), "non-fired candidate baseline") {
		t.Fatalf("status/reasons = %q %+v, want weak non-fired candidate baseline", got.Status, got.Reasons)
	}

	metrics = passingOpportunityEvidenceMetrics()
	metrics.HoldoutFiredVsCandidateAvgLiftPct = &negativeLift
	got = opportunityBacktestEvidence(metrics)
	if got.Status != "weak_holdout_baseline" || !strings.Contains(strings.Join(got.Reasons, "\n"), "holdout all-candidate baseline") {
		t.Fatalf("status/reasons = %q %+v, want weak holdout aggregate candidate baseline", got.Status, got.Reasons)
	}
}

func TestOpportunityBacktestEvidenceRejectsWeakBarProvenance(t *testing.T) {
	t.Parallel()
	metrics := passingOpportunityEvidenceMetrics()
	sim := passingOpportunityEvidenceSimulation()

	sim.MarkToMarket.SourceQuality = "fixture_source"
	sim.MarkToMarket.SourceWarnings = []string{"42/42 price bar(s) use fixture/test source"}
	got := opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "fixture/test source") {
		t.Fatalf("status/reasons = %q %+v, want fixture bar provenance blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.SourceQuality = ""
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "missing source provenance") {
		t.Fatalf("status/reasons = %q %+v, want missing bar provenance blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.Holdout.MarkToMarket.SourceQuality = "untrusted_source"
	sim.Holdout.MarkToMarket.SourceWarnings = []string{"12/12 price bar(s) use an unrecognized source"}
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_holdout_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "holdout-only mark-to-market bars use unrecognized source") {
		t.Fatalf("status/reasons = %q %+v, want holdout bar provenance blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.SourceChecksum = ""
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "sha256 source checksum") {
		t.Fatalf("status/reasons = %q %+v, want missing checksum blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.SourceManifestChecksum = ""
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "source manifest checksum") {
		t.Fatalf("status/reasons = %q %+v, want missing manifest blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.BarSources = nil
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "missing source provenance") {
		t.Fatalf("status/reasons = %q %+v, want missing bar-source blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.BarSources = []string{"ibkr_fake"}
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "unrecognized source") {
		t.Fatalf("status/reasons = %q %+v, want spoofed source blocker", got.Status, got.Reasons)
	}

	sim = passingOpportunityEvidenceSimulation()
	sim.MarkToMarket.PriceBasis = "close"
	got = opportunityBacktestEvidenceWithSimulation(metrics, &sim)
	if got.Status != "untrusted_bars" || !strings.Contains(strings.Join(got.Reasons, "\n"), "expected adjusted_close") {
		t.Fatalf("status/reasons = %q %+v, want price-basis blocker", got.Status, got.Reasons)
	}
}

func TestOpportunityPriceBarSourceClassRequiresExactTrustedIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		source string
		want   string
	}{
		{source: opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV, want: "trusted"},
		{source: "ibkr", want: "untrusted"},
		{source: "ibkr_fake", want: "untrusted"},
		{source: "polygon_spoof", want: "untrusted"},
		{source: "fixture_adjusted_ohlcv", want: "fixture"},
		{source: "", want: "missing"},
	}
	for _, tc := range tests {
		if got := opportunityPriceBarSourceClass(tc.source); got != tc.want {
			t.Fatalf("source class for %q = %q, want %q", tc.source, got, tc.want)
		}
	}
}

func applyPassingOpportunityCandidateBaseline(m *OpportunityBacktestMetrics) {
	hitRate := 0.55
	candidateAvg, candidateMedian := 2.0, 2.0
	nonFiredAvg, nonFiredMedian := 1.0, 1.0
	liftAllAvg, liftAllMedian := 4.0, 2.0
	liftNonFiredAvg, liftNonFiredMedian := 5.0, 3.0
	m.CostedCandidates = m.Observations
	m.PositiveCandidateNetExcess = 55
	m.NegativeCandidateNetExcess = max(m.CostedCandidates-m.PositiveCandidateNetExcess, 0)
	m.CandidateNetExcessHitRate = &hitRate
	m.AvgCandidateNetExcessPct = &candidateAvg
	m.MedianCandidateNetExcessPct = &candidateMedian
	m.NonFiredCostedCandidates = max(m.Observations-m.SignalFired, 1)
	m.AvgNonFiredCandidateNetPct = &nonFiredAvg
	m.MedianNonFiredCandidateNetPct = &nonFiredMedian
	m.FiredVsCandidateAvgLiftPct = &liftAllAvg
	m.FiredVsCandidateMedianLiftPct = &liftAllMedian
	m.FiredVsNonFiredAvgLiftPct = &liftNonFiredAvg
	m.FiredVsNonFiredMedianLiftPct = &liftNonFiredMedian
}

func applyPassingOpportunityHoldoutCandidateBaseline(m *OpportunityBacktestMetrics) {
	hitRate := 0.55
	candidateAvg, candidateMedian := 2.0, 2.0
	nonFiredAvg, nonFiredMedian := 1.0, 1.0
	liftAllAvg, liftAllMedian := 4.0, 2.0
	liftNonFiredAvg, liftNonFiredMedian := 5.0, 3.0
	m.HoldoutCostedCandidates = m.HoldoutObservations
	m.HoldoutPositiveCandidateNetExcess = 17
	m.HoldoutNegativeCandidateNetExcess = max(m.HoldoutCostedCandidates-m.HoldoutPositiveCandidateNetExcess, 0)
	m.HoldoutCandidateNetExcessHitRate = &hitRate
	m.HoldoutAvgCandidateNetExcessPct = &candidateAvg
	m.HoldoutMedianCandidateNetExcessPct = &candidateMedian
	m.HoldoutNonFiredCostedCandidates = max(m.HoldoutObservations-m.HoldoutSignalFired, 1)
	m.HoldoutAvgNonFiredCandidateNetPct = &nonFiredAvg
	m.HoldoutMedianNonFiredCandidateNetPct = &nonFiredMedian
	m.HoldoutFiredVsCandidateAvgLiftPct = &liftAllAvg
	m.HoldoutFiredVsCandidateMedianLiftPct = &liftAllMedian
	m.HoldoutFiredVsNonFiredAvgLiftPct = &liftNonFiredAvg
	m.HoldoutFiredVsNonFiredMedianLiftPct = &liftNonFiredMedian
}

func passingOpportunityEvidenceMetrics() OpportunityBacktestMetrics {
	hitSixty := 0.6
	strongLowerHit := 0.55
	median, avg := 5.0, 7.0
	netMedian, netAvg := 4.0, 6.0
	strongLowerAvg := 1.0
	okInstrumentShare := 0.20
	okClusterShare := 0.50
	okHoldoutInstrumentShare := 0.30
	okHoldoutClusterShare := 0.70
	m := OpportunityBacktestMetrics{
		Observations:                        100,
		TargetOpportunity:                   50,
		NonOpportunity:                      50,
		SignalFired:                         30,
		DistinctSignalInstruments:           10,
		MaxSignalInstrument:                 "NVDA",
		MaxSignalInstrumentFired:            6,
		MaxSignalInstrumentShare:            &okInstrumentShare,
		DistinctSignalClusters:              3,
		MaxSignalCluster:                    "AI infrastructure",
		MaxSignalClusterFired:               15,
		MaxSignalClusterShare:               &okClusterShare,
		CostedSignalFired:                   30,
		ExcessHitRate:                       &hitSixty,
		ExcessHitRateLower95:                &strongLowerHit,
		MedianExcessReturnPct:               &median,
		AvgExcessReturnPct:                  &avg,
		AvgExcessReturnLower95Pct:           &strongLowerAvg,
		NetExcessHitRate:                    &hitSixty,
		NetExcessHitRateLower95:             &strongLowerHit,
		MedianNetExcessReturnPct:            &netMedian,
		AvgNetExcessReturnPct:               &netAvg,
		AvgNetExcessReturnLower95Pct:        &strongLowerAvg,
		HoldoutObservations:                 30,
		HoldoutSignalFired:                  10,
		HoldoutTargetOpportunity:            10,
		HoldoutNonOpportunity:               10,
		HoldoutCostedSignalFired:            10,
		HoldoutPositiveNetExcess:            6,
		HoldoutDistinctSignalInstruments:    5,
		HoldoutMaxSignalInstrument:          "NVDA",
		HoldoutMaxSignalInstrumentFired:     3,
		HoldoutMaxSignalInstrumentShare:     &okHoldoutInstrumentShare,
		HoldoutDistinctSignalClusters:       2,
		HoldoutMaxSignalCluster:             "AI infrastructure",
		HoldoutMaxSignalClusterFired:        7,
		HoldoutMaxSignalClusterShare:        &okHoldoutClusterShare,
		HoldoutNetExcessHitRate:             &hitSixty,
		HoldoutNetExcessHitRateLower95:      &strongLowerHit,
		HoldoutAvgNetExcessReturnPct:        &netAvg,
		HoldoutAvgNetExcessReturnLower95Pct: &strongLowerAvg,
	}
	applyPassingOpportunityCandidateBaseline(&m)
	applyPassingOpportunityHoldoutCandidateBaseline(&m)
	return m
}

func passingOpportunityEvidenceSimulation() OpportunityBacktestSimulation {
	return OpportunityBacktestSimulation{
		Model:              "equal_weight_slots_v1",
		Signals:            30,
		FilledSignals:      30,
		PortfolioReturnPct: new(12.0),
		ExcessReturnPct:    new(6.0),
		MarkToMarket:       trustedOpportunityEvidenceMTM(12.0, 4.0, 8.0, -8.0),
		Holdout: &OpportunityBacktestSimulation{
			Model:              "equal_weight_slots_v1",
			Signals:            10,
			FilledSignals:      10,
			PortfolioReturnPct: new(8.0),
			ExcessReturnPct:    new(4.0),
			MarkToMarket:       trustedOpportunityEvidenceMTM(8.0, 3.0, 5.0, -5.0),
		},
	}
}

func trustedOpportunityEvidenceMTM(portfolio, benchmark, excess, drawdown float64) *OpportunityMarkToMarketSimulation {
	return &OpportunityMarkToMarketSimulation{
		Model:                  "equal_weight_slots_mtm_v1",
		PriceSource:            "testdata/trusted-opportunity-bars.jsonl",
		SourceChecksum:         "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SourceManifest:         "testdata/trusted-opportunity-bars.manifest.json",
		SourceManifestChecksum: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		SourceQuality:          "ok",
		BarSources:             []string{opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV},
		PriceBasis:             "adjusted_close",
		PortfolioReturnPct:     new(portfolio),
		BenchmarkReturnPct:     new(benchmark),
		ExcessReturnPct:        new(excess),
		MaxDrawdownPct:         new(drawdown),
		MaxTradeMarkGapDays:    opportunityEvidenceMaxMTMGapDays,
	}
}

func TestRunBacktestScoreOpportunityEmitsSourcedPITJSONL(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"score-opportunity",
		"--input", opportunityPointInTimeFixturePath(t),
		"--bars", opportunityPriceBarsFixturePath(t),
	})
	if code != 0 {
		t.Fatalf("Run backtest score-opportunity returned %d, stderr:\n%s", code, stderr.String())
	}
	rows, err := readOpportunityPointInTimeRows(strings.NewReader(stdout.String()))
	if err != nil {
		t.Fatalf("generated JSONL should feed PIT reader: %v\n%s", err, stdout.String())
	}
	if got, want := len(rows), 4; got != want {
		t.Fatalf("generated rows = %d, want %d", got, want)
	}
	if rows[0].LabelStatus != "scored" || rows[0].Outcome.Formula != opportunityOutcomeFormulaCloseToClose {
		t.Fatalf("first scored row = %+v", rows[0])
	}
	if rows[0].Outcome.ForwardReturnPct != 25 || rows[0].Outcome.ExcessReturnPct != 20 {
		t.Fatalf("first scored outcome = %+v", rows[0].Outcome)
	}
	if rows[0].Outcome.SourceChecksum == "" || rows[0].Outcome.BenchmarkSourceChecksum == "" {
		t.Fatalf("missing checksums: %+v", rows[0].Outcome)
	}
	if err := validateOpportunityFeatureProvenance(rows[0].FeatureProvenance, rows[0].Features); err != nil {
		t.Fatalf("scored row feature provenance did not validate: %v", err)
	}
	observations := buildOpportunityBacktestObservations(rows)
	if err := validateOpportunityBacktestObservationsSourced(observations); err != nil {
		t.Fatalf("scored rows should validate as sourced observations: %v", err)
	}
}

func TestAppendOpportunityCaptureLedgerSkipsDuplicateConfigurations(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "opportunity-ledger.jsonl")
	existing := testOpportunityCapturePointInTimeRow("NVDA", "2026-06-12", 126)
	var buf bytes.Buffer
	if err := writeOpportunityPointInTimeRowsJSONL(&buf, []OpportunityPointInTimeRow{existing}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.TrimSuffix(buf.Bytes(), []byte("\n")), 0o600); err != nil {
		t.Fatal(err)
	}

	duplicate := existing
	cost := 25.0
	duplicate.Split = "tuning"
	duplicate.Theme = "relabelled"
	duplicate.MarketCluster = "different-cluster"
	duplicate.Trade.Benchmark = "SPY"
	duplicate.Trade.RoundTripCostBps = &cost
	fresh := testOpportunityCapturePointInTimeRow("ANET", "2026-06-12", 126)
	res, appended, err := appendOpportunityPointInTimeRowsJSONL(path, []OpportunityPointInTimeRow{duplicate, fresh})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := res.Captured, 2; got != want {
		t.Fatalf("captured = %d, want %d", got, want)
	}
	if got, want := res.Appended, 1; got != want {
		t.Fatalf("appended = %d, want %d", got, want)
	}
	if got, want := res.SkippedDuplicates, 1; got != want {
		t.Fatalf("skipped = %d, want %d", got, want)
	}
	if got, want := len(appended), 1; got != want {
		t.Fatalf("appended rows = %d, want %d", got, want)
	}
	if got, want := appended[0].Trade.Instrument, "ANET"; got != want {
		t.Fatalf("appended instrument = %q, want %q", got, want)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readOpportunityPointInTimeRows(f)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(rows), 2; got != want {
		t.Fatalf("ledger rows = %d, want %d", got, want)
	}
	if got, want := rows[0].Trade.Instrument, "NVDA"; got != want {
		t.Fatalf("first ledger instrument = %q, want %q", got, want)
	}
	if got, want := rows[1].Trade.Instrument, "ANET"; got != want {
		t.Fatalf("second ledger instrument = %q, want %q", got, want)
	}
}

func TestAppendOpportunityCaptureLedgerRejectsMalformedExistingRow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "opportunity-ledger.jsonl")
	var buf bytes.Buffer
	if err := writeOpportunityPointInTimeRowsJSONL(&buf, []OpportunityPointInTimeRow{{Date: "2026-06-12"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := appendOpportunityPointInTimeRowsJSONL(path, []OpportunityPointInTimeRow{
		testOpportunityCapturePointInTimeRow("ANET", "2026-06-13", 126),
	})
	if err == nil {
		t.Fatal("expected malformed existing ledger row to fail")
	}
	if !strings.Contains(err.Error(), "existing row 1: instrument is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunBacktestOpportunityRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"opportunity", "--input", opportunityBuiltObservationFixturePath(t)})
	if code != 0 {
		t.Fatalf("Run backtest returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Opportunity Backtest",
		"4 observations",
		"Evidence     insufficient_sample",
		"Verdict      not alpha evidence",
		"min 100 obs / 30 fires / 30 opp / 30 controls",
		"Diversity    2 instruments (min 10) · max ANET 50% (limit 25%) · 1 clusters (min 3) · max AI infrastructure opportunity 100% (limit 60%)",
		"Validation   holdout 2 obs / 1 fires / 1 opp / 1 controls · unknown split 0",
		"Need         +96 obs / +28 fires / +28 opp / +28 controls · +8 inst / +2 clusters",
		"Holdout need +28 obs / +9 fires / +9 opp / +9 controls · +4 inst / +1 clusters",
		"Holdout div  1 instruments (min 5) · max ANET 100% (limit 40%) · 1 clusters (min 2) · max AI infrastructure opportunity 100% (limit 75%)",
		"Signal       precision 100%",
		"median excess +16.0%",
		"Net outcome  costed 2/2",
		"Baseline     all 4/4 · hit 50% · avg -6.8% · median -1.0% · non-fired 2 avg -29.0% median -29.0%",
		"Lift         vs all avg +22.2% median +16.5% · vs non-fired avg +44.5% median +44.5%",
		"Holdout base all 2/2 avg -16.5% median -16.5% · lift avg +28.0% median +28.0%",
		"Holdout sel  non-fired 1 avg -44.5% median -44.5% · lift avg +56.0% median +56.0%",
		"Confidence   gross hit lb95 34% · avg gross lb95 +8.2% · net hit lb95 34% · avg net lb95 +7.7%",
		"Portfolio    2/2 filled · return +3.9% vs bench +0.8% · excess +3.1% · max open 2 · turnover +20.0%",
		"Cost model   flat-50bps-diagnostic",
		"AI infrastructure opportunity",
		"AI mega-cap fragility",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("opportunity backtest output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRunBacktestResearchOpportunityRendersText(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"research-opportunity",
		"--input", opportunityPointInTimeFixturePath(t),
		"--plan", "all",
	})
	if code != 0 {
		t.Fatalf("Run backtest research-opportunity returned %d, stderr:\n%s", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"Opportunity Research",
		"4 scored rows",
		"ranked by tuning_fired_vs_candidate_avg_lift_pct",
		"constructive_breakout_v1",
		"rs63_positive_v1",
		"holdout metrics are audit evidence",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("research output missing %q:\n%s", want, out)
		}
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestOpportunityResearchPullbackMacroVetoPlan(t *testing.T) {
	t.Parallel()
	plan := testOpportunityResearchPlan(t, "pullback_uptrend_rs63_macro_veto_v1")

	normal := testOpportunityPullbackMacroFeature(rpc.RegimeToneNormal)
	if signal := plan.Evaluate(normal); !signal.Fired {
		t.Fatalf("normal regime macro-veto signal did not fire: %+v", signal)
	}

	watch := testOpportunityPullbackMacroFeature(rpc.RegimeToneWatch)
	if signal := plan.Evaluate(watch); !signal.Fired {
		t.Fatalf("watch regime macro-veto signal did not fire: %+v", signal)
	}

	stress := testOpportunityPullbackMacroFeature(rpc.RegimeToneStress)
	if signal := plan.Evaluate(stress); signal.Fired || !slices.Contains(signal.Reasons, "macro_stress_veto") {
		t.Fatalf("stress regime macro-veto signal = %+v, want macro_stress_veto", signal)
	}

	missing := testOpportunityPullbackMacroFeature(rpc.RegimeToneNormal)
	missing.Macro = nil
	if signal := plan.Evaluate(missing); signal.Fired || !slices.Contains(signal.Reasons, "macro_context_missing") || !opportunitySignalContextBlocked(signal.Reasons) {
		t.Fatalf("missing macro signal = %+v, want blocked macro_context_missing", signal)
	}

	dataQuality := testOpportunityPullbackMacroFeature(rpc.RegimeToneDataQuality)
	if signal := plan.Evaluate(dataQuality); signal.Fired || !slices.Contains(signal.Reasons, "macro_data_quality_veto") || !opportunitySignalContextBlocked(signal.Reasons) {
		t.Fatalf("data-quality macro signal = %+v, want blocked macro_data_quality_veto", signal)
	}
}

func TestRunBacktestResearchOpportunityListPlansIncludesMacroVeto(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"research-opportunity",
		"--list-plans",
	})
	if code != 0 {
		t.Fatalf("Run backtest research-opportunity --list-plans returned %d, stderr:\n%s", code, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "pullback_uptrend_rs63_macro_veto_v1") {
		t.Fatalf("list-plans output missing macro-veto plan:\n%s", out)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRunBacktestResearchOpportunityJSON(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"research-opportunity",
		"--input", opportunityPointInTimeFixturePath(t),
		"--plan", "rs63_positive_v1,constructive_breakout_v1",
		"--json",
	})
	if code != 0 {
		t.Fatalf("Run backtest research-opportunity --json returned %d, stderr:\n%s", code, stderr.String())
	}
	var res OpportunityResearchResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("decode research json: %v\n%s", err, stdout.String())
	}
	if got, want := res.Rows, 4; got != want {
		t.Fatalf("rows = %d, want %d", got, want)
	}
	if got, want := res.PlansEvaluated, 2; got != want {
		t.Fatalf("plans_evaluated = %d, want %d", got, want)
	}
	if res.RankedBy != opportunityResearchRankMetric {
		t.Fatalf("ranked_by = %q, want %q", res.RankedBy, opportunityResearchRankMetric)
	}
	if len(res.Plans) != 2 || res.Plans[0].Rank != 1 || res.Plans[1].Rank != 2 {
		t.Fatalf("unexpected plan ranks: %+v", res.Plans)
	}
	if res.Plans[0].TuningMetrics.Observations == 0 {
		t.Fatalf("top plan missing tuning metrics: %+v", res.Plans[0])
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr should be empty, got:\n%s", stderr.String())
	}
}

func TestRunBacktestResearchOpportunityRejectsUnknownPlan(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{
		"research-opportunity",
		"--input", opportunityPointInTimeFixturePath(t),
		"--plan", "curve_fit_v9000",
	})
	if code == 0 {
		t.Fatalf("Run backtest research-opportunity accepted unknown plan, stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "unknown research plan") {
		t.Fatalf("stderr = %q, want unknown research plan", got)
	}
}

func TestRunBacktestOpportunityRejectsUnverifiedDirectSignals(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	code := Run(context.Background(), env, "backtest", []string{"opportunity", "--input", opportunityBacktestFixturePath(t)})
	if code == 0 {
		t.Fatalf("Run backtest accepted featureless direct observation signals, stdout=%s", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "feature_provenance") {
		t.Fatalf("stderr = %q, want feature provenance blocker", got)
	}
}

func readBacktestFixture(t *testing.T) []CanaryBacktestObservation {
	t.Helper()
	f, err := os.Open(backtestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readCanaryBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readRegimeBacktestFixture(t *testing.T) []RegimeBacktestObservation {
	t.Helper()
	f, err := os.Open(regimeBacktestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readRegimeBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func readOpportunityBacktestFixture(t *testing.T) []OpportunityBacktestObservation {
	t.Helper()
	f, err := os.Open(opportunityBacktestFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readOpportunityBacktestObservations(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func appendOpportunityTestBarSorted(bars []OpportunityPriceBarRow, bar OpportunityPriceBarRow) []OpportunityPriceBarRow {
	out := make([]OpportunityPriceBarRow, 0, len(bars)+1)
	inserted := false
	for _, existing := range bars {
		if !inserted && existing.Date > bar.Date {
			out = append(out, bar)
			inserted = true
		}
		out = append(out, existing)
	}
	if !inserted {
		out = append(out, bar)
	}
	return out
}

func setOpportunityOutcomeFromLedger(t *testing.T, outcome *OpportunityBacktestOutcome, ledger opportunityPriceBarLedger, instrument, benchmark, entryDate, exitDate string) {
	t.Helper()
	entry, ok := opportunityBarOnDate(ledger.BySymbol[instrument], entryDate)
	if !ok {
		t.Fatalf("missing %s entry bar on %s", instrument, entryDate)
	}
	exit, ok := opportunityBarOnDate(ledger.BySymbol[instrument], exitDate)
	if !ok {
		t.Fatalf("missing %s exit bar on %s", instrument, exitDate)
	}
	benchEntry, ok := opportunityBarOnDate(ledger.BySymbol[benchmark], entryDate)
	if !ok {
		t.Fatalf("missing %s entry bar on %s", benchmark, entryDate)
	}
	benchExit, ok := opportunityBarOnDate(ledger.BySymbol[benchmark], exitDate)
	if !ok {
		t.Fatalf("missing %s exit bar on %s", benchmark, exitDate)
	}
	entryClose := opportunityBarClose(entry)
	exitClose := opportunityBarClose(exit)
	benchEntryClose := opportunityBarClose(benchEntry)
	benchExitClose := opportunityBarClose(benchExit)
	forward := roundOpportunityPct(opportunityPctReturn(entryClose, exitClose))
	bench := roundOpportunityPct(opportunityPctReturn(benchEntryClose, benchExitClose))
	adverse, favorable := opportunityExcursions(ledger.BySymbol[instrument], entryDate, exitDate, entryClose)
	outcome.EntryDate = entryDate
	outcome.ExitDate = exitDate
	outcome.EntryPrice = &entryClose
	outcome.ExitPrice = &exitClose
	outcome.PriceSource = opportunityBarSource(ledger.Source, instrument)
	outcome.BenchmarkSource = opportunityBarSource(ledger.Source, benchmark)
	outcome.Formula = opportunityOutcomeFormulaCloseToClose
	outcome.PriceBasis = "adjusted_close"
	outcome.SourceChecksum = ledger.Checksum
	outcome.BenchmarkSourceChecksum = ledger.Checksum
	outcome.ForwardReturnPct = forward
	outcome.BenchmarkReturnPct = bench
	outcome.ExcessReturnPct = roundOpportunityPct(forward - bench)
	outcome.MaxAdverseExcursionPct = roundOpportunityPct(adverse)
	outcome.MaxFavorableExcursionPct = roundOpportunityPct(favorable)
}

func opportunityBuiltObservationFixturePath(t *testing.T) string {
	t.Helper()
	observations := buildOpportunityBacktestObservations(readOpportunityPointInTimeFixture(t))
	var buf bytes.Buffer
	if err := writeOpportunityBacktestObservationsJSONL(&buf, observations); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "opportunity-built.jsonl")
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testOpportunityCapturePointInTimeRow(symbol, date string, horizonDays int) OpportunityPointInTimeRow {
	cost := 50.0
	features := OpportunityPointInTimeFeatures{
		Instrument:  symbol,
		SecType:     "STK",
		Exchange:    "SMART",
		Currency:    "USD",
		DataType:    rpc.MarketDataLive,
		DataQuality: "ok",
	}
	return OpportunityPointInTimeRow{
		Date:              date,
		AsOf:              time.Date(2026, 6, 12, 15, 30, 0, 0, time.UTC),
		Split:             "holdout",
		SplitProvenance:   testOpportunitySplitProvenance("test-holdout-plan"),
		FeatureProvenance: opportunityFeatureProvenance("test-ledger", "test_features_v1", features),
		LabelStatus:       "unscored_forward_window_pending",
		MarketCluster:     "test-cluster",
		Theme:             "test-theme",
		Features:          features,
		Trade: OpportunityBacktestTrade{
			Instrument:       symbol,
			EntryRule:        "next_close",
			HorizonDays:      horizonDays,
			Benchmark:        "QQQ",
			RoundTripCostBps: &cost,
			CostModel:        "flat-50bps-test",
		},
	}
}

func testOpportunityResearchPlan(t *testing.T, id string) opportunitySignalPlan {
	t.Helper()
	for _, plan := range opportunitySignalPlans() {
		if plan.ID == id {
			return plan
		}
	}
	t.Fatalf("research plan %q not registered", id)
	return opportunitySignalPlan{}
}

func testOpportunityPullbackMacroFeature(tone string) OpportunityPointInTimeFeatures {
	price := 100.0
	sma50 := 98.0
	sma200 := 80.0
	pct50 := (price - sma50) / sma50
	pct200 := (price - sma200) / sma200
	rs63 := 0.10
	rs126 := 0.08
	advDollar := 120_000_000.0
	return OpportunityPointInTimeFeatures{
		Instrument:         "ALFA",
		SecType:            "STK",
		Exchange:           "SMART",
		Currency:           "USD",
		DataType:           rpc.MarketDataLive,
		QuoteQuality:       "firm",
		DataQuality:        "ok",
		SessionContext:     &rpc.MarketSession{Market: "us_equity", Date: "2026-06-15", State: "regular", IsOpen: true},
		Price:              &price,
		SMA50:              &sma50,
		SMA200:             &sma200,
		PctAbove50DMA:      &pct50,
		PctAbove200DMA:     &pct200,
		RS63D:              &rs63,
		RS126D:             &rs126,
		AvgDollarVolume20D: &advDollar,
		Macro:              testOpportunityMacroContext(tone),
	}
}

func testOpportunityMacroContext(tone string) *OpportunityMacroContext {
	stage := rpc.LifecycleOpportunity
	if tone == rpc.RegimeToneStress {
		stage = rpc.LifecycleConfirmedStress
	}
	if tone == rpc.RegimeToneRiskOff {
		stage = rpc.LifecyclePanic
	}
	if tone == rpc.RegimeToneDataQuality {
		stage = rpc.LifecycleDataQuality
	}
	return &OpportunityMacroContext{
		Source:                     rpc.MethodRegimeSnapshot,
		AsOf:                       time.Date(2026, 6, 15, 14, 30, 0, 0, time.UTC),
		Fingerprint:                rpc.Fingerprint{Version: "test-regime", Key: "test-" + strings.ReplaceAll(tone, "_", "-")},
		Label:                      "Test regime",
		Tone:                       tone,
		Stage:                      stage,
		Severity:                   "observe",
		Readiness:                  "ready",
		Confidence:                 "high",
		ClusterGreenCount:          5,
		ClusterYellowCount:         1,
		ClusterRedCount:            0,
		ClusterRankedCount:         6,
		ClusterEligibleRedCount:    0,
		ClusterProvisionalRedCount: 0,
	}
}

func testOpportunitySplitProvenance(planID string) OpportunitySplitProvenance {
	return OpportunitySplitProvenance{
		Source:                  "test-ledger",
		Method:                  "test_explicit_holdout_v1",
		PlanID:                  planID,
		AssignedAt:              time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		LabelStatusAtAssignment: "unscored_forward_window_pending",
		PreRegistered:           true,
	}
}

func readOpportunityPointInTimeFixture(t *testing.T) []OpportunityPointInTimeRow {
	t.Helper()
	f, err := os.Open(opportunityPointInTimeFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := readOpportunityPointInTimeRows(f)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func backtestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "canary_backtest_sample.jsonl")
}

func regimeBacktestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "regime_backtest_sample.jsonl")
}

func regimePointInTimeFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "regime_pit_panel_sample.jsonl")
}

func opportunityBacktestFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "opportunity_backtest_sample.jsonl")
}

func opportunityPointInTimeFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "opportunity_pit_panel_sample.jsonl")
}

func opportunityPriceBarsFixturePath(t *testing.T) string {
	t.Helper()
	return filepath.Join("testdata", "opportunity_price_bars_sample.jsonl")
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func hasOpportunityDiagnosticBucket(buckets []OpportunityBacktestDiagnosticBucket, name string) bool {
	for _, bucket := range buckets {
		if bucket.Name == name {
			return true
		}
	}
	return false
}

func writeTrustedOpportunityBarsFixtureWithManifest(t *testing.T, mutate func(*opportunityPriceBarManifest)) (string, string) {
	t.Helper()
	raw, err := os.ReadFile(opportunityPriceBarsFixturePath(t))
	if err != nil {
		t.Fatal(err)
	}
	content := strings.ReplaceAll(string(raw), "fixture_adjusted_ohlcv", opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV)
	return writeOpportunityBarsWithManifest(t, content, mutate)
}

func writeOpportunityBarsWithManifest(t *testing.T, content string, mutate func(*opportunityPriceBarManifest)) (string, string) {
	t.Helper()
	dir := t.TempDir()
	barsPath := filepath.Join(dir, "bars.jsonl")
	if err := os.WriteFile(barsPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	checksum, err := sha256FileHex(barsPath)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(barsPath)
	if err != nil {
		t.Fatal(err)
	}
	bars, err := readOpportunityPriceBars(f)
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	manifest := opportunityPriceBarManifest{
		SchemaVersion:    opportunityPriceBarManifestSchemaV1,
		SourceID:         opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		ExporterID:       opportunityTrustedBarSourceIBKRHMDSAdjustedOHLCV,
		ExporterVersion:  "test-v1",
		Provider:         "IBKR HMDS",
		Method:           "fixture-backed adjusted daily bars for manifest contract tests",
		WhatToShow:       "ADJUSTED_LAST",
		PriceBasis:       "adjusted_close",
		BarSize:          "1 day",
		AdjustmentPolicy: "IBKR HMDS adjusted historical daily close",
		CreatedAt:        time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC),
		Command:          "ibkr backtest export-opportunity-bars --test",
		BarFile:          filepath.Base(barsPath),
		BarsSHA256:       "sha256:" + checksum,
		RowCount:         opportunityPriceBarRowCount(bars),
		Symbols:          opportunityPriceBarManifestSymbolsFromBars(bars),
	}
	if mutate != nil {
		mutate(&manifest)
	}
	manifestPath := filepath.Join(dir, "bars.manifest.json")
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return barsPath, manifestPath
}

func clusterHasRebalanceWatch(res CanaryBacktestResult, name string) bool {
	for _, cluster := range res.Clusters {
		if cluster.Name == name && cluster.Metrics.RebalanceWatch > 0 {
			return true
		}
	}
	return false
}

func TestBacktestDateAsOfStampsInsideRegularSession(t *testing.T) {
	t.Parallel()
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	got := backtestDateAsOf("2020-03-09")
	want := time.Date(2020, 3, 9, 15, 59, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("backtestDateAsOf = %v, want %v (15:59 ET inside the observation date)", got, want)
	}
	if !backtestDateAsOf("").IsZero() || !backtestDateAsOf("not-a-date").IsZero() {
		t.Fatal("empty/invalid dates must stay zero")
	}
}
