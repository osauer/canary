package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// regimeFixture returns the realistic mid-session envelope used across
// every renderer test: VIX OK + live, HYG stale + yellow band, USD/JPY
// OK + weekly change, gamma computing + ETA, breadth structurally
// unavailable. Mirrors the cmd/_preview fixture so visual and test
// expectations stay synchronised.
func regimeFixture() *rpc.RegimeSnapshotResult {
	asOf := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	authorityAge := int64(0)
	vix := 18.43
	vix3m := 21.36
	ratio := vix / vix3m
	hyg := 79.55
	spy := 737.34
	hyg50 := 80.10
	spy52 := 780.00
	usdjpy := 158.7285
	close7 := 158.05
	weekly := 0.43
	return &rpc.RegimeSnapshotResult{
		AsOf:    asOf,
		SpecDoc: "docs/docs/internals/regime-dashboard.md",
		AuthorityHealth: &rpc.RegimeAuthorityHealth{
			Status:                rpc.RegimeAuthorityFresh,
			LastSuccessAt:         &asOf,
			LastSuccessAgeSeconds: &authorityAge,
		},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				AsOf: &rpc.RegimeAsOfSummary{Label: "live"},
			},
			Status:   rpc.RegimeStatusOK,
			VIX:      &vix,
			VIX3M:    &vix3m,
			Ratio:    &ratio,
			DataType: rpc.MarketDataLive,
			Notes:    "VIX/VIX3M notes",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				AsOf: &rpc.RegimeAsOfSummary{Label: "frozen"},
			},
			Status:     rpc.RegimeStatusStale,
			HYGPrice:   &hyg,
			HYG50DMA:   &hyg50,
			SPYPrice:   &spy,
			SPY52WHigh: &spy52,
			Notes:      "HYG/SPY notes",
		},
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				AsOf: &rpc.RegimeAsOfSummary{Label: "15m delayed"},
			},
			Status:       rpc.RegimeStatusOK,
			Symbol:       "USD.JPY",
			Last:         &usdjpy,
			Close7DAgo:   &close7,
			WeeklyChange: &weekly,
			Notes:        "USD/JPY notes",
		},
		GammaZero: rpc.RegimeGammaZero{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				AsOf: &rpc.RegimeAsOfSummary{Label: "computing"},
			},
			Status: rpc.RegimeStatusComputing,
			Envelope: rpc.GammaZeroSPXResult{
				Status:     rpc.GammaZeroStatusComputing,
				EtaSeconds: 42,
				Progress:   40,
			},
			Notes: "gamma notes",
		},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{
				AsOf: &rpc.RegimeAsOfSummary{Label: "unavailable"},
			},
			Status: rpc.RegimeStatusUnavailable,
			Notes:  "breadth notes",
		},
	}
}

func rankableRegimeGammaQuality() *rpc.GammaSignalQuality {
	return &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}
}

func regimeRenderedRowHits(out, name string) int {
	hits := 0
	for line := range strings.SplitSeq(out, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(line, name) &&
			(strings.HasPrefix(trimmed, "●") || strings.HasPrefix(trimmed, "✕") ||
				strings.HasPrefix(trimmed, "○") || strings.HasPrefix(trimmed, "◌")) {
			hits++
		}
	}
	return hits
}

// TestRenderRegime_CompositeVerdictAndCount pins the headline: bold
// verdict line per the spec's interpretation table, usable-coverage
// summary, and decision-readout lines instead of raw band counts.
func TestRenderRegime_CompositeVerdictAndCount(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Risk Regime",
		"Normal regime",
		"3/6 evidence groups usable",
		"Read:",
		"Input health:",
		"Support:",
		"VIX/VIX3M (volatility term structure) and USD/JPY (FX carry proxy) are calm",
		"Watch:",
		"HYG vs SPY (ETF credit proxy) is on watch from frozen market context",
		"Set aside:",
		"γ-zero (SPY+SPX) (dealer gamma) is still building",
		"are not in this read yet",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("composite header missing %q\n%s", want, out)
		}
	}
	if strings.Contains(out, "Use:") {
		t.Errorf("default render should not use meta prompt label Use:\n%s", out)
	}
}

func TestRenderRegime_TapeSitsBetweenDescriptionAndIndicators(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	read := strings.Index(out, "Read:")
	spy := strings.Index(out, "SPY 737.34")
	vix := strings.Index(out, "VIX 18.43")
	header := strings.Index(out, "SIGNAL")
	if read < 0 || spy < 0 || vix < 0 || header < 0 {
		t.Fatalf("missing expected blocks: read=%d spy=%d vix=%d header=%d\n%s", read, spy, vix, header, out)
	}
	if !(read < spy && spy < header && read < vix && vix < header) {
		t.Errorf("SPY/VIX tape should sit after the description block and before indicators:\n%s", out)
	}
}

func TestRenderRegime_DataQualityLine(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	fix.DataQuality = []rpc.DataQualityHealth{
		{Surface: "gamma", Status: "degraded", Summary: "degraded: SPX excluded", DegradedClusters: []string{"gamma"}},
		{Surface: "regime", Status: "stale", Summary: "stale: vol, credit", StaleClusters: []string{"vol", "credit"}},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, fix); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"Data context:", "gamma context", "SPX excluded", "regime stale"} {
		if !strings.Contains(out, want) {
			t.Fatalf("regime output missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "Set aside:") > strings.Index(out, "Data context:") {
		t.Fatalf("data-quality context line should follow decision lines:\n%s", out)
	}
}

func TestRenderRegime_DefaultOmitsLifecycleInternals(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	fix.Lifecycle = rpc.LifecycleState{
		Scope:       "market",
		Stage:       rpc.LifecycleEarlyWarning,
		Severity:    "watch",
		Readiness:   "degraded",
		Timing:      "forward_warning",
		Confidence:  "medium",
		Unconfirmed: []string{"credit"},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, fix); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, notWant := range []string{"Stage:", "early warning", "forward warning", "unconfirmed credit"} {
		if strings.Contains(out, notWant) {
			t.Errorf("default render leaked lifecycle internals %q:\n%s", notWant, out)
		}
	}
}

func TestRenderRegime_InputHealthColorSeparatesContextFromProblems(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	fix.DataQuality = []rpc.DataQualityHealth{
		{Surface: "regime", Status: "stale", Summary: "stale: off-hours vol", StaleClusters: []string{"vol"}},
	}
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}, Color: true}
	if code := renderRegimeText(env, fix); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, ansiYellow+"Needs confirmation") {
		t.Errorf("technical input gaps should color Input health yellow:\n%s", out)
	}
	if strings.Contains(out, ansiYellow+"regime stale") {
		t.Errorf("data context detail should stay dim rather than inherit the health color:\n%s", out)
	}
	if !strings.Contains(out, ansiDim+"regime stale") {
		t.Errorf("stale data context should be rendered plainly and dim, got:\n%s", out)
	}
}

// TestRenderRegime_RowsFitOneLineEach pins the compressed layout: each
// of the eight indicators occupies exactly one row in the rendered body
// (excluding header, composite, blank, and footer lines). The old
// renderer used 4 lines per indicator; the redesign collapses to one.
func TestRenderRegime_RowsFitOneLineEach(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	indicators := []string{"VIX/VIX3M", "HYG vs SPY", "HY/IG OAS", "USD/JPY", "γ-zero (SPY+SPX)", "SPX breadth"}
	for _, name := range indicators {
		hits := regimeRenderedRowHits(stdout.String(), name)
		if hits != 1 {
			t.Errorf("%s should appear on exactly one row (got %d):\n%s", name, hits, stdout.String())
		}
	}
}

func TestRenderRegime_NarrowWidthWrapsDefaultOutput(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	fix := regimeFixture()
	hygPrice := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 740.0
	fix.HYGSPYDivergence.HYGPrice = &hygPrice
	fix.HYGSPYDivergence.HYG50DMA = &hyg50
	fix.HYGSPYDivergence.SPYPrice = &spy
	fix.HYGSPYDivergence.SPY52WHigh = &spy52

	if code := renderRegimeTextWidthWithOptions(env, &stdout, fix, regimeRenderOptions{}, 80); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	if !strings.Contains(out, "READING / WHY") {
		t.Fatalf("80-column render should use compact table header:\n%s", out)
	}
	for i, line := range strings.Split(out, "\n") {
		if got := visibleLen(line); got > 80 {
			t.Fatalf("line %d visible width = %d, want <= 80:\n%s\n\nfull output:\n%s", i+1, got, line, out)
		}
	}
}

// TestRenderRegime_CallWordsAppearOnUsableRows pins the call column:
// default output uses trader-facing calm/watch/stress/no-vote wording,
// not raw JSON band enums.
func TestRenderRegime_CallWordsAppearOnUsableRows(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"CALL", "calm", "watch", "building", "skip"} {
		if !strings.Contains(out, want) {
			t.Errorf("call word %q missing:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"INDICATOR", "BAND", "unranked"} {
		if strings.Contains(out, notWant) {
			t.Errorf("default render leaked raw table word %q:\n%s", notWant, out)
		}
	}
}

func TestRenderRegime_WhenColumnShowsFriendlyFreshnessLabels(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"WHEN", "live", "frozen", "delayed 15m", "building", "missing"} {
		if !strings.Contains(out, want) {
			t.Errorf("regime table missing as-of label %q:\n%s", want, out)
		}
	}
}

func regimeAuthorityTestHealth(status rpc.RegimeAuthorityStatus, refreshing bool, failure rpc.RegimeAuthorityFailureCode) *rpc.RegimeAuthorityHealth {
	health := &rpc.RegimeAuthorityHealth{Status: status, Refreshing: refreshing, FailureCode: failure}
	if status == rpc.RegimeAuthorityFresh || status == rpc.RegimeAuthorityStale {
		lastSuccess := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
		age := int64(60)
		health.LastSuccessAt = &lastSuccess
		health.LastSuccessAgeSeconds = &age
	}
	return health
}

func TestRegimeAuthorityHealthOverridesHealthyLookingRows(t *testing.T) {
	t.Parallel()
	c := regimeComposite{green: 6, ranked: 6, total: 6, clusterGreen: 6, clusterRanked: 6, clusterTotal: 6}
	rows := []regimeRow{{name: "vol", status: rpc.RegimeStatusOK, band: bandGreen, asOf: "live"}}
	base := rpc.RegimeMonitorResult{
		Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleQuiet},
		Composite: rpc.RegimeComposite{Verdict: "Normal regime", ClusterGreenCount: 6, ClusterRankedCount: 6},
	}
	tests := []struct {
		name          string
		health        *rpc.RegimeAuthorityHealth
		wantVerdict   string
		wantHealth    string
		wantRead      string
		forbidHealth  string
		forbidVerdict string
	}{
		{name: "fresh", health: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, false, rpc.RegimeAuthorityFailureNone), wantVerdict: "Normal regime", wantHealth: "OK", wantRead: "Constructive tape"},
		{name: "stale refreshing", health: regimeAuthorityTestHealth(rpc.RegimeAuthorityStale, true, rpc.RegimeAuthorityFailureNone), wantVerdict: "Market state undefined", wantHealth: "Blocked", wantRead: "retained context only", forbidHealth: "OK", forbidVerdict: "Normal regime"},
		{name: "clock invalid", health: regimeAuthorityTestHealth(rpc.RegimeAuthorityStale, false, rpc.RegimeAuthorityFailureClockInvalid), wantVerdict: "Market state undefined", wantHealth: "daemon clock is behind", wantRead: "retained context only", forbidHealth: "OK", forbidVerdict: "Normal regime"},
		{name: "unavailable", health: regimeAuthorityTestHealth(rpc.RegimeAuthorityUnavailable, false, rpc.RegimeAuthorityFailureNoLastGood), wantVerdict: "Market state undefined", wantHealth: "no complete retained snapshot", wantRead: "retained context only", forbidHealth: "OK", forbidVerdict: "Normal regime"},
		{name: "invalid", health: &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityFresh}, wantVerdict: "Market state undefined", wantHealth: "metadata is invalid", wantRead: "retained context only", forbidHealth: "OK", forbidVerdict: "Normal regime"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			monitor := base
			monitor.AuthorityHealth = tc.health
			verdict := regimeServedVerdict(monitor, c)
			health := regimeInputHealthLine(c, rows, monitor)
			read := regimeReadLine(c, monitor)
			if !strings.Contains(verdict, tc.wantVerdict) || !strings.Contains(health, tc.wantHealth) || !strings.Contains(read, tc.wantRead) {
				t.Fatalf("verdict=%q health=%q read=%q", verdict, health, read)
			}
			if tc.forbidHealth != "" && strings.Contains(health, tc.forbidHealth) {
				t.Fatalf("health=%q must not contain %q", health, tc.forbidHealth)
			}
			if tc.forbidVerdict != "" && strings.Contains(verdict, tc.forbidVerdict) {
				t.Fatalf("verdict=%q must not contain %q", verdict, tc.forbidVerdict)
			}
		})
	}
}

func TestRegimeAuthorityRefreshBeforeExpiryKeepsCurrentLastGoodUsable(t *testing.T) {
	t.Parallel()
	c := regimeComposite{green: 6, ranked: 6, total: 6, clusterGreen: 6, clusterRanked: 6, clusterTotal: 6}
	monitor := rpc.RegimeMonitorResult{
		AuthorityHealth: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, true, rpc.RegimeAuthorityFailureNone),
		Lifecycle:       rpc.LifecycleState{Stage: rpc.LifecycleQuiet},
		Composite:       rpc.RegimeComposite{Verdict: "Normal regime", ClusterGreenCount: 6, ClusterRankedCount: 6},
	}
	health := regimeInputHealthLine(c, nil, monitor)
	if !strings.Contains(health, "OK") || !strings.Contains(health, "current snapshot remains usable while a Regime refresh runs") {
		t.Fatalf("refresh-ahead health=%q", health)
	}
	if got := regimeReadLine(c, monitor); !strings.Contains(got, "Constructive tape") {
		t.Fatalf("refresh-ahead read=%q", got)
	}
}

func TestRegimeFreshRefreshFailurePreservesConfirmedStress(t *testing.T) {
	t.Parallel()
	c := regimeComposite{red: 2, ranked: 6, total: 6, clusterRed: 2, clusterRanked: 6, clusterTotal: 6}
	monitor := rpc.RegimeMonitorResult{
		AuthorityHealth: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, false, rpc.RegimeAuthorityFailureRefreshFailed),
		Lifecycle:       rpc.LifecycleState{Stage: rpc.LifecycleConfirmedStress},
		Composite: rpc.RegimeComposite{
			Verdict: "Confirmed stress regime", ClusterRedCount: 2,
			ClusterEligibleRedCount: 2, ClusterRankedCount: 6,
		},
	}
	verdict := regimeServedVerdict(monitor, c)
	if !strings.Contains(verdict, "Confirmed stress regime") || !strings.Contains(verdict, "refresh degraded") {
		t.Fatalf("fresh failed-refresh verdict=%q", verdict)
	}
	health := regimeInputHealthLine(c, nil, monitor)
	if strings.Contains(health, "OK") || !strings.Contains(health, "Degraded") || !strings.Contains(health, "refresh failed") {
		t.Fatalf("fresh failed-refresh health=%q", health)
	}
	if got := regimeReadLine(c, monitor); !strings.Contains(got, "still confirms stress") || !strings.Contains(got, "refresh failed") {
		t.Fatalf("fresh failed-refresh read=%q", got)
	}
}

func TestRegimeFreshnessLabelsKeepStaleFrozenAndNotDueDistinct(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		label string
		want  string
	}{{"stale", "stale"}, {"frozen", "frozen"}, {"not_due", "not due"}, {"overdue", "overdue"}} {
		if got := regimeWhenLabel(regimeRow{asOf: tc.label}); got != tc.want {
			t.Fatalf("when(%q)=%q, want %q", tc.label, got, tc.want)
		}
	}
	c := regimeComposite{ranked: 3, total: 3, clusterRanked: 3, clusterTotal: 3}
	monitor := rpc.RegimeMonitorResult{
		AuthorityHealth: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, false, rpc.RegimeAuthorityFailureNone),
		Lifecycle:       rpc.LifecycleState{Stage: rpc.LifecycleQuiet},
	}
	stale := regimeInputHealthLine(c, []regimeRow{{name: "vol", status: rpc.RegimeStatusStale, asOf: "stale"}}, monitor)
	frozen := regimeInputHealthLine(c, []regimeRow{{name: "vol", status: rpc.RegimeStatusStale, asOf: "frozen"}}, monitor)
	notDue := regimeInputHealthLine(c, []regimeRow{{name: "vol", status: rpc.RegimeStatusStale, asOf: "not_due"}}, monitor)
	if !strings.Contains(stale, "Needs confirmation") || !strings.Contains(stale, "stale") {
		t.Fatalf("stale health=%q", stale)
	}
	if !strings.Contains(frozen, "OK") || !strings.Contains(frozen, "frozen rows are closed-session context") || strings.Contains(frozen, "stale or overdue") {
		t.Fatalf("frozen health=%q", frozen)
	}
	if !strings.Contains(notDue, "OK") || !strings.Contains(notDue, "not due yet") || strings.Contains(notDue, "stale or overdue") {
		t.Fatalf("not-due health=%q", notDue)
	}
}

func TestRegimeReadLineDoesNotConfirmThreeProvisionalReds(t *testing.T) {
	t.Parallel()
	c := regimeComposite{red: 3, ranked: 6, total: 6, clusterRed: 3, clusterRanked: 6, clusterTotal: 6}
	monitor := rpc.RegimeMonitorResult{
		AuthorityHealth: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, false, rpc.RegimeAuthorityFailureNone),
		Lifecycle:       rpc.LifecycleState{Stage: rpc.LifecycleEarlyWarning},
		Composite: rpc.RegimeComposite{
			ClusterRedCount: 3, ClusterProvisionalRedCount: 3, ClusterRankedCount: 6,
		},
	}
	got := regimeReadLine(c, monitor)
	if !strings.Contains(got, "has not confirmed broad stress") || strings.Contains(got, "Broad stress is confirmed") {
		t.Fatalf("three provisional reds read=%q", got)
	}
}

func TestRegimeReadLineProvisionalRedWithBrokenEvidenceIsDataQuality(t *testing.T) {
	t.Parallel()
	c := regimeComposite{red: 1, ranked: 6, total: 6, clusterRed: 1, clusterRanked: 6, clusterTotal: 6}
	monitor := rpc.RegimeMonitorResult{
		AuthorityHealth: regimeAuthorityTestHealth(rpc.RegimeAuthorityFresh, false, rpc.RegimeAuthorityFailureNone),
		Lifecycle:       rpc.LifecycleState{Stage: rpc.LifecycleDataQuality, Readiness: "blocked"},
		Composite: rpc.RegimeComposite{
			ClusterRedCount: 1, ClusterProvisionalRedCount: 1, ClusterRankedCount: 6,
		},
	}
	got := regimeReadLine(c, monitor)
	if !strings.Contains(got, "Market state is undefined") || !strings.Contains(got, "required evidence is incomplete") || strings.Contains(got, "early_warning") {
		t.Fatalf("provisional data-quality read=%q", got)
	}
	if health := regimeInputHealthLine(c, nil, monitor); !strings.Contains(health, "Blocked") || !strings.Contains(health, "undefined") {
		t.Fatalf("provisional data-quality health=%q", health)
	}
}

// TestRenderRegime_GammaComputingInlinesETA pins the computing-state
// row: ETA + progress sit in the value cell, not on a separate "re-run"
// hint line. The old renderer wrote three lines for this state; the
// redesign collapses to one.
func TestRenderRegime_GammaComputingInlinesETA(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	for _, want := range []string{"building", "42s ETA", "40%"} {
		if !strings.Contains(out, want) {
			t.Errorf("gamma row missing %q on the same line:\n%s", want, out)
		}
	}
}

// TestRenderRegime_BreadthUnavailableReasonInline pins the structurally-
// missing row: the reason (currently "breadth engine offline") sits in
// the dim parenthetical, the value cell shows "unavailable". No fake
// zero.
func TestRenderRegime_BreadthUnavailableReasonInline(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if !strings.Contains(out, "unavailable") {
		t.Errorf("breadth row should surface unavailable in value cell:\n%s", out)
	}
	if !strings.Contains(out, "no breadth snapshot yet") {
		t.Errorf("breadth row should surface the reason inline:\n%s", out)
	}
	if strings.Contains(out, "0.0%") {
		t.Errorf("unavailable breadth must not render a fake zero value:\n%s", out)
	}
}

// TestRenderRegime_NoSpecDocLine pins the removed-feature: the old
// renderer echoed the spec-doc path on stdout, which served no human
// purpose (the JSON envelope still carries it for programmatic
// consumers).
func TestRenderRegime_NoSpecDocLine(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	if strings.Contains(stdout.String(), "Spec doc:") {
		t.Errorf("spec-doc path should not echo to stdout in the default render:\n%s", stdout.String())
	}
}

// TestRenderRegime_ExplainModeUsesCompactAuditNotes pins the --explain
// footer: it renders compact thresholds/provenance/read notes instead
// of dumping the daemon's long methodology prose verbatim.
func TestRenderRegime_ExplainModeUsesCompactAuditNotes(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, regimeFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Explain",
		"Source:",
		"FRED/St. Louis Fed official daily ICE BofA",
		"Federal Reserve commercial-paper release plus U.S. Treasury Daily Treasury Bill Rates.",
		"Read:",
		"Volatility term structure",
		"Dealer-gamma model",
		"produces data_quality instead",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{
		"VIX/VIX3M notes", "HYG/SPY notes", "USD/JPY notes", "gamma notes", "breadth notes",
		"Full methodology", "Inputs:", "Raw source:", "BAMLH0A0HYM2",
		"visible, drives early_warning",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("--explain should not dump fixture note %q:\n%s", notWant, out)
		}
	}
	explain := out[strings.Index(out, "  Explain"):]
	for line := range strings.SplitSeq(explain, "\n") {
		if got := visibleLen(line); got > 112 {
			t.Fatalf("--explain line too wide (%d):\n%s\n\nfull output:\n%s", got, line, out)
		}
	}
}

// TestRenderRegime_DefaultOmitsNotesHasHint pins the default-mode
// shape: the giant spec-prose notes do not appear, replaced by a
// one-line "pass --explain" hint at the bottom.
func TestRenderRegime_DefaultOmitsNotesHasHint(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, regimeFixture())
	out := stdout.String()
	if strings.Contains(out, "VIX/VIX3M notes") {
		t.Errorf("default render should omit the full notes prose:\n%s", out)
	}
	if !strings.Contains(out, "--explain") {
		t.Errorf("default render should hint at --explain:\n%s", out)
	}
}

// ----- SPY + VIX tape above the indicator rows -----

// TestRenderRegime_HeadlineShowsSPYAndVIX pins the headline contract:
// when both SPY and VIX carry their prev-close anchors, the dashboard
// surfaces "SPY <price> <±$> (<±%>)    VIX <price> (<±%>)" between the
// verdict block and the audit rows. The line never invents numbers — a
// missing prev close shows the price with a dim "(—)" placeholder.
func TestRenderRegime_HeadlineShowsSPYAndVIX(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	spyPrev := 736.14
	spyChg := *fix.HYGSPYDivergence.SPYPrice - spyPrev
	spyPct := spyChg / spyPrev * 100
	fix.HYGSPYDivergence.SPYPrevClose = &spyPrev
	fix.HYGSPYDivergence.SPYChange = &spyChg
	fix.HYGSPYDivergence.SPYChangePct = &spyPct
	vixPrev := 18.85
	vixPct := (*fix.VIXTermStructure.VIX - vixPrev) / vixPrev * 100
	fix.VIXTermStructure.VIXPrevClose = &vixPrev
	fix.VIXTermStructure.VIXChangePct = &vixPct

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, fix); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// SPY half: price + signed dollar change + parenthesised signed %.
	for _, want := range []string{"SPY 737.34", "+1.20", "+0.16%"} {
		if !strings.Contains(out, want) {
			t.Errorf("headline missing %q:\n%s", want, out)
		}
	}
	// VIX half: price + parenthesised signed %. Unicode minus on a
	// negative change.
	for _, want := range []string{"VIX 18.43", "−2.23%"} {
		if !strings.Contains(out, want) {
			t.Errorf("headline missing %q:\n%s", want, out)
		}
	}
}

// TestRenderRegime_HeadlineDimsWhenPrevCloseMissing pins the honest-
// fallback contract: the headline still surfaces the spot price when
// the prev-close anchor didn't land, but the change cell is dim "(—)"
// rather than a fabricated zero.
func TestRenderRegime_HeadlineDimsWhenPrevCloseMissing(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	// Leave SPYPrevClose / SPYChange / SPYChangePct nil; VIXPrevClose /
	// VIXChangePct nil too. The fixture's SPYPrice + VIX should still
	// surface.
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	_ = renderRegimeText(env, fix)
	out := stdout.String()
	if !strings.Contains(out, "SPY 737.34") || !strings.Contains(out, "VIX 18.43") {
		t.Errorf("headline must surface spot even without prev-close:\n%s", out)
	}
	// Two dim "(—)" placeholders — one per missing change leg.
	if got := strings.Count(out, "(—)"); got < 2 {
		t.Errorf("expected 2+ dim (—) placeholders, got %d:\n%s", got, out)
	}
}

// TestSignedFloat pins the signed-formatter helper: positive numbers
// carry "+", negatives carry the Unicode minus (U+2212) so the visual
// width matches the plus sign and column alignment stays clean.
func TestSignedFloat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    float64
		dec  int
		want string
	}{
		{1.20, 2, "+1.20"},
		{-1.20, 2, "−1.20"},
		{0, 2, "+0.00"},
		{-0.01, 2, "−0.01"},
	}
	for _, tc := range cases {
		if got := signedFloat(tc.v, tc.dec); got != tc.want {
			t.Errorf("signedFloat(%v, %d) = %q, want %q", tc.v, tc.dec, got, tc.want)
		}
	}
}

// ----- shared headline wording table (rpc.RegimeHeadline) -----
// The renderer no longer owns a verdict copy: it shows the served verdict,
// and the wording table lives once in internal/rpc. This table pins the
// new eligible/provisional semantics through the shared function.

func TestRegimeComposite_VerdictTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		green       int
		yellow      int
		red         int
		eligible    int
		provisional int
		ranked      int
		unranked    int
		stage       string
		want        string
	}{
		{"all green = normal", 5, 0, 0, 0, 0, 5, 0, rpc.LifecycleQuiet, "Normal regime"},
		{"two yellow = normal", 3, 2, 0, 0, 0, 5, 0, rpc.LifecycleQuiet, "Normal regime"},
		{"three yellow = elevated", 2, 3, 0, 0, 0, 5, 0, rpc.LifecycleEarlyWarning, "Elevated stress watch"},
		{"one eligible red = stress signal", 2, 2, 1, 1, 0, 5, 0, rpc.LifecycleEarlyWarning, "Stress signal present"},
		{"one provisional red = stress signal", 2, 2, 1, 0, 1, 5, 0, rpc.LifecycleEarlyWarning, "Stress signal present"},
		// Two PROVISIONAL reds stay a stress signal — the 2026-06-12
		// incident headline ("Broad stress regime" at red==2) is gone.
		{"two provisional reds = stress signal", 1, 2, 2, 0, 2, 5, 0, rpc.LifecycleEarlyWarning, "Stress signal present"},
		// Two ELIGIBLE reds confirm — new explicit label.
		{"two eligible reds = confirmed stress", 1, 2, 2, 2, 0, 5, 0, rpc.LifecycleConfirmedStress, "Confirmed stress regime"},
		{"three eligible reds = broad stress", 0, 2, 3, 3, 0, 5, 0, rpc.LifecyclePanic, "Broad stress regime"},
		{"four eligible reds = broad stress", 0, 1, 4, 4, 0, 5, 0, rpc.LifecyclePanic, "Broad stress regime"},
		{"five eligible reds full ranked = full risk-off", 0, 0, 5, 5, 0, 5, 0, rpc.LifecyclePanic, "Full risk-off conditions"},
		// Coverage edge: 3 eligible red but two unranked → broad, not full.
		{"three eligible reds with two unranked", 0, 0, 3, 3, 0, 3, 2, rpc.LifecyclePanic, "Broad stress regime"},
		{"all unranked", 0, 0, 0, 0, 0, 0, 5, rpc.LifecycleDataQuality, "Market state undefined — data incomplete"},
		// Honesty floor: below verdictFloor ranked clusters no positive
		// claim is made, even with reds visible.
		{"one green ranked = insufficient", 1, 0, 0, 0, 0, 1, 4, rpc.LifecycleDataQuality, "Market state undefined — data incomplete"},
		{"two green ranked = insufficient", 2, 0, 0, 0, 0, 2, 3, rpc.LifecycleDataQuality, "Market state undefined — data incomplete"},
		{"one red + one yellow = insufficient", 0, 1, 1, 1, 0, 2, 3, rpc.LifecycleDataQuality, "Market state undefined — data incomplete"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := rpc.RegimeComposite{
				ClusterGreenCount:          tc.green,
				ClusterYellowCount:         tc.yellow,
				ClusterRedCount:            tc.red,
				ClusterEligibleRedCount:    tc.eligible,
				ClusterProvisionalRedCount: tc.provisional,
				ClusterRankedCount:         tc.ranked,
				ClusterUnrankedCount:       tc.unranked,
			}
			if got := rpc.RegimeHeadline(c, tc.stage); got != tc.want {
				t.Errorf("headline for %v = %q, want %q", tc, got, tc.want)
			}
		})
	}
}

func TestRegimeComposite_IsolatedVVIXRedCountsClusterYellow(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	vvix := 112.0
	fix.VolOfVol = rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Last: &vvix}
	rows := []regimeRow{
		{name: "VIX/VIX3M", cluster: "equity_vol", band: bandGreen},
		{name: "VVIX", cluster: "equity_vol", band: bandRed},
		{name: "HYG vs SPY", cluster: "credit", band: bandGreen},
		{name: "HY/IG OAS", cluster: "credit", band: bandGreen},
		{name: "funding spread", cluster: "funding", band: bandGreen},
		{name: "USD/JPY", cluster: "fx_carry", band: bandGreen},
		{name: "γ-zero (SPY+SPX)", cluster: "dealer_gamma", band: bandGreen},
		{name: "SPX breadth", cluster: "breadth", band: bandGreen},
	}
	c := tallyCompositeFromSnapshot(fix, rows)
	if c.red != 1 {
		t.Fatalf("indicator red count = %d, want isolated VVIX row still red", c.red)
	}
	if c.clusterRed != 0 || c.clusterYellow != 1 {
		t.Fatalf("isolated non-severe VVIX red should be a yellow cluster, got %+v", c)
	}
}

// TestRegimeRow_VIXBands pins the VIX/VIX3M row's threshold mapping
// against the spec's three-band split (<0.92 green; 0.92-1.00 yellow;
// >1.00 red). The renderer's defaults track the spec verbatim.
func TestRegimeRow_VIXBands(t *testing.T) {
	t.Parallel()
	mk := func(r float64) rpc.RegimeVIXTerm {
		v, v3 := 18.0, r*18.0
		return rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK, VIX: &v, VIX3M: &v3, Ratio: &r}
	}
	cases := []struct {
		ratio float64
		want  regimeBand
	}{
		{0.85, bandGreen},
		{0.91, bandGreen},
		{0.92, bandYellow},
		{0.99, bandYellow},
		{1.00, bandRed},
		{1.10, bandRed},
	}
	for _, tc := range cases {
		got := rowVIXTerm(time.Now(), mk(tc.ratio)).band
		if got != tc.want {
			t.Errorf("ratio=%.2f band=%v want %v", tc.ratio, got, tc.want)
		}
	}
}

// TestRegimeRow_USDJPYWatchesYenStrengthening pins the asymmetric
// banding: yen *strengthening* (USD/JPY *falling*) is the risk signal,
// not USD strengthening. WeeklyChange convention: positive when USD
// rose, negative when USD fell — so -1% is the start of the yellow
// band, -2% the start of red.
func TestRegimeRow_USDJPYWatchesYenStrengthening(t *testing.T) {
	t.Parallel()
	mk := func(chg float64) rpc.RegimeUSDJPY {
		last := 158.0
		return rpc.RegimeUSDJPY{Status: rpc.RegimeStatusOK, Last: &last, WeeklyChange: &chg}
	}
	cases := []struct {
		chg  float64
		want regimeBand
		why  string
	}{
		{+0.5, bandGreen, "USD up 0.5%/wk is calm"},
		{+5.0, bandGreen, "USD strengthening alone is not the risk signal"},
		{-0.5, bandGreen, "yen up 0.5%/wk is calm"},
		{-1.0, bandYellow, "yen +1% triggers yellow"},
		{-1.9, bandYellow, "still yellow"},
		{-2.0, bandRed, "yen +2% triggers red"},
		{-5.0, bandRed, "stays red"},
	}
	for _, tc := range cases {
		got := rowUSDJPY(time.Now(), mk(tc.chg)).band
		if got != tc.want {
			t.Errorf("chg=%+.1f%% band=%v want %v (%s)", tc.chg, got, tc.want, tc.why)
		}
	}
}

// TestRegimeRow_HYGSPYUnrankedWhen52WMissing pins the renderer's
// honesty-floor: when HYG is below its 50dma but SPY's 52w high
// didn't land, the yellow-band trigger ("SPY within 3% of high") is
// unknown — so the row is unranked rather than guessed at.
func TestRegimeRow_HYGSPYUnrankedWhen52WMissing(t *testing.T) {
	t.Parallel()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	row := rowHYGSPY(time.Now(), rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50,
		SPYPrice: &spy, // SPY52WHigh nil
	})
	if row.band != bandUnranked {
		t.Errorf("HYG<50dma + 52w missing should leave row unranked, got %v", row.band)
	}
}

func TestRegimeRow_HYGSPYMissingPriceDoesNotRenderZeroOrPanic(t *testing.T) {
	t.Parallel()
	hyg50 := 80.0
	row := rowHYGSPY(time.Now(), rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYG50DMA: &hyg50,
	})
	if row.band != bandUnranked {
		t.Errorf("missing HYG price should leave row unranked, got %v", row.band)
	}
	if row.stateNote != "HYG price unavailable" {
		t.Errorf("missing HYG price should surface an honest state note, got %q", row.stateNote)
	}
	if strings.Contains(row.value, "0.00") {
		t.Errorf("missing HYG price must not render a fake zero: %q", row.value)
	}
}

func TestRegimeRow_HYGSPYNearHighIsRed(t *testing.T) {
	t.Parallel()
	hyg := 79.0
	hyg50 := 80.0
	spy := 737.0
	spy52 := 749.0
	row := rowHYGSPY(time.Now(), rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50,
		SPYPrice: &spy, SPY52WHigh: &spy52,
	})
	if row.band != bandRed {
		t.Errorf("HYG<50dma + SPY near highs should be red, got %v", row.band)
	}
	if !strings.Contains(row.reason, "near highs") {
		t.Errorf("red HYG/SPY row should explain the divergence, got %q", row.reason)
	}
}

func TestRegimeRow_VIXMissingLegDoesNotRenderZero(t *testing.T) {
	t.Parallel()
	vix3m := 21.36
	ratio := 0.86
	row := rowVIXTerm(time.Now(), rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK,
		VIX3M:  &vix3m,
		Ratio:  &ratio,
	})
	if strings.Contains(row.value, "0.00 /") {
		t.Errorf("missing VIX leg must not render a fake zero: %q", row.value)
	}
	if !strings.Contains(row.value, "— / 21.36") {
		t.Errorf("missing VIX leg should render as an em dash, got %q", row.value)
	}
}

func TestRegimeRow_USDJPYMissingLastDoesNotRenderZero(t *testing.T) {
	t.Parallel()
	weekly := -1.2
	row := rowUSDJPY(time.Now(), rpc.RegimeUSDJPY{
		Status:       rpc.RegimeStatusOK,
		WeeklyChange: &weekly,
	})
	if row.band != bandUnranked {
		t.Errorf("missing USD/JPY last should leave row unranked, got %v", row.band)
	}
	if row.stateNote != "FX tick unavailable" {
		t.Errorf("missing USD/JPY last should surface an honest state note, got %q", row.stateNote)
	}
	if strings.Contains(row.value, "0.0000") {
		t.Errorf("missing USD/JPY last must not render a fake zero: %q", row.value)
	}
}

func TestRegimeRow_NewStressSignalBands(t *testing.T) {
	t.Parallel()
	now := time.Now()

	vvix := 112.0
	if row := rowVolOfVol(now, rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, Last: &vvix}); row.band != bandRed {
		t.Errorf("VVIX 112 should be red, got %v", row.band)
	}

	hy := 3.8
	widen := 0.6
	if row := rowCreditSpreads(now, rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: &hy, HY20DChange: &widen}); row.band != bandYellow {
		t.Errorf("HY OAS widening +0.6pp should be yellow, got %v", row.band)
	}

	funding := 80.0
	if row := rowFundingStress(now, rpc.RegimeFundingStress{Status: rpc.RegimeStatusOK, SpreadBps: &funding}); row.band != bandRed {
		t.Errorf("funding spread 80bp should be red, got %v", row.band)
	}
}

// ----- per-scalar provenance (Quality envelope, v0.24.x+) -----

// TestRenderRegime_FirmLiveOmitsQualityTag pins the "no clutter for
// fresh data" rule: when every value on a row came from a firm-live
// gateway tick, the renderer must NOT add a quality annotation. The
// firm-live state IS the default — surfacing it would just add ink.
func TestRenderRegime_FirmLiveOmitsQualityTag(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	vix := 18.43
	vix3m := 21.36
	ratio := vix / vix3m
	row := rowVIXTerm(now, rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, VIX: &vix, VIX3M: &vix3m, Ratio: &ratio,
		VIXQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		VIX3MQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
	})
	if row.quality != "" {
		t.Errorf("firm-live row should have empty quality tag, got %q", row.quality)
	}
}

// TestRenderRegime_DerivedSPY52WHighShowsEstimate pins the
// methodology-honesty path: when SPY's 52-week high comes from the
// history-fallback (max(High) over 252 daily bars) rather than the
// live tick-165 (Misc Stats), the row carries a "· est" annotation.
// This is the case the user asked for: a renderer that says
// "this value is derived" rather than presenting it as a firm tick.
func TestRenderRegime_DerivedSPY52WHighShowsEstimate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	derivedAt := now.Add(-18 * time.Second) // history fetch finished 18s before snapshot
	hyg := 79.55
	hyg50 := 80.10
	spy := 737.34
	spy52 := 749.30
	row := rowHYGSPY(now, rpc.RegimeHYGSPYDivergence{
		Status:   rpc.RegimeStatusOK,
		HYGPrice: &hyg, HYG50DMA: &hyg50, SPYPrice: &spy, SPY52WHigh: &spy52,
		HYGQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		HYG50DMAQuality: &rpc.Quality{
			AsOf: derivedAt, FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
		},
		SPYQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm,
		},
		SPY52WHighQuality: &rpc.Quality{
			AsOf: derivedAt, FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
			Source: "SPY 252d max(High) fallback",
		},
	})
	if !strings.Contains(row.quality, "est") {
		t.Errorf("derived-fallback row should carry an 'est' tag, got %q", row.quality)
	}
	if !strings.Contains(row.quality, "18s") {
		t.Errorf("derived row 18s old should include age in tag, got %q", row.quality)
	}
}

// TestRenderRegime_GammaModelledAnnotation pins the gamma row's
// methodology disclosure: the BS-sweep zero-flip estimate carries a
// "modelled" tag because it's not a measurement — it's a model with
// documented caveats (sign convention, sticky IV during sweep).
func TestRenderRegime_GammaModelledAnnotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	flip := 5125.4
	gap := 1.5
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{ZeroGamma: &flip, GapPct: &gap, Method: "perfiliev-bs-sweep-v1", Quality: rankableRegimeGammaQuality()},
		},
		ZeroGammaQuality: &rpc.Quality{
			AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
			Source: "perfiliev-bs-sweep-v1",
		},
	})
	if !strings.Contains(row.quality, "modelled") {
		t.Errorf("gamma row should carry 'modelled' tag, got %q", row.quality)
	}
}

// TestRenderRegime_ExplainIncludesQualityBlocks pins the --explain
// path: every populated Quality envelope surfaces as a concise
// confidence/freshness line. Raw source strings stay behind
// --diagnostics so normal explain mode is not a source-mechanics wall.
func TestRenderRegime_ExplainIncludesQualityBlocks(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	fix := regimeFixture()
	fix.AsOf = now
	// Attach a sampling of Quality envelopes covering each kind.
	fix.VIXTermStructure.VIXQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessLive, Confidence: rpc.ConfidenceFirm, Source: "VIX tick",
	}
	fix.HYGSPYDivergence.SPY52WHighQuality = &rpc.Quality{
		AsOf: now.Add(-18 * time.Second), FreshnessClass: rpc.FreshnessDerived, Confidence: rpc.ConfidenceEstimate,
		Source: "SPY 252d max(High) fallback",
	}
	// Gamma fixture is in "computing" state — replace with Ready+modelled.
	flip := 5125.4
	gap := 1.5
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{ZeroGamma: &flip, GapPct: &gap, Method: "perfiliev-bs-sweep-v1", Quality: rankableRegimeGammaQuality()},
	}
	fix.GammaZero.ZeroGammaQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
		Source: "perfiliev-bs-sweep-v1",
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"firm", "live",
		"estimate", "derived",
		"modelled", "proxy",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing Quality marker %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"VIX tick", "SPY 252d max(High) fallback", "perfiliev-bs-sweep-v1"} {
		if strings.Contains(out, notWant) {
			t.Errorf("--explain should hide raw quality source %q:\n%s", notWant, out)
		}
	}

	stdout.Reset()
	if code := renderRegimeTextWithOptions(env, &stdout, fix, regimeRenderOptions{Explain: true, Diagnostics: true}); code != 0 {
		t.Fatalf("diagnostics code=%d", code)
	}
	diagnostics := stdout.String()
	for _, want := range []string{"VIX tick", "SPY 252d max(High) fallback", "perfiliev-bs-sweep-v1"} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("--diagnostics output missing quality source %q:\n%s", want, diagnostics)
		}
	}
}

// TestRenderRegime_NilQualityNoQualitySuffix pins the v0.29.0 UX
// rework: quality / stale-tick suffixes are no longer rendered on
// default rows (promoted to --explain instead). Nil-Quality rows still
// must not panic and must not invent quality tags out of thin air.
func TestRenderRegime_NilQualityNoQualitySuffix(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, regimeFixture()); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, fresh := range []string{"· est", "· modelled", "· frozen", "stale tick"} {
		if strings.Contains(out, fresh) {
			t.Errorf("default render should not show quality tag %q (promoted to --explain):\n%s", fresh, out)
		}
	}
}

// ----- v0.26.0: gamma row no-crossing branches + jargon cleanup -----

// TestRegimeRow_GammaPositiveSignBandsGreen pins the new no-crossing
// branch: when the swept profile never crosses zero AND the whole
// profile is positive, the row ranks green (dealer long-γ across the
// window, stabilizing regime). Previously this hid behind an
// "unavailable" glyph; now it surfaces as a real regime statement.
func TestRegimeRow_GammaPositiveSignBandsGreen(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "positive",
				GammaTotalAbs:  2.7e9,
				Quality:        rankableRegimeGammaQuality(),
				// ZeroGamma / GapPct both nil — no crossing in window
			},
		},
	})
	if row.band != bandGreen {
		t.Errorf("positive GammaSign should band green, got %v", row.band)
	}
	if !strings.Contains(row.reason, "long-γ") {
		t.Errorf("positive GammaSign reason should mention long-γ, got %q", row.reason)
	}
	if !strings.Contains(row.value, "737") {
		t.Errorf("value cell should still surface spot, got %q", row.value)
	}
}

// TestRegimeRow_GammaNegativeSignBandsRed pins the amplifying-regime
// branch: short-γ across the whole sweep is a red band, not unranked.
func TestRegimeRow_GammaNegativeSignBandsRed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "negative",
				GammaTotalAbs:  3.1e9,
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if row.band != bandRed {
		t.Errorf("negative GammaSign should band red, got %v", row.band)
	}
	if !strings.Contains(row.reason, "short-γ") {
		t.Errorf("negative GammaSign reason should mention short-γ, got %q", row.reason)
	}
}

// TestRegimeRow_GammaNoDataStaysUnranked pins the genuine "empty
// profile" path: with no crossing AND no signed signal, the row stays
// unranked (current behaviour for empty / pathological compute).
func TestRegimeRow_GammaNoDataStaysUnranked(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "no_data",
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if row.band != bandUnranked {
		t.Errorf("no_data sign should stay unranked, got %v", row.band)
	}
}

func TestRegimeRow_GammaRequiresExplicitRankableQuality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		quality *rpc.GammaSignalQuality
	}{
		{name: "nil_quality"},
		{name: "blocked_quality", quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "OI coverage blocked"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			row := rowGamma(now, rpc.RegimeGammaZero{
				Status: rpc.RegimeStatusOK,
				Envelope: rpc.GammaZeroSPXResult{
					Status: rpc.GammaZeroStatusReady,
					Result: &rpc.GammaZeroComputed{
						SpotUnderlying: 737.0,
						GammaSign:      "negative",
						GammaTotalAbs:  3.1e9,
						Quality:        tc.quality,
					},
				},
			})
			if row.band != bandUnranked {
				t.Fatalf("row band = %v, want unranked", row.band)
			}
			if !strings.Contains(row.value, "short-γ") {
				t.Fatalf("row should still display the gamma read, value=%q", row.value)
			}
			if row.reason == "" {
				t.Fatalf("row should explain why gamma did not vote")
			}
		})
	}
}

func TestRenderRegime_GammaContextOnlySaysNoVote(t *testing.T) {
	t.Parallel()
	fix := regimeFixture()
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.AsOf = &rpc.RegimeAsOfSummary{Label: "1d old"}
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:           rpc.GammaZeroScopeCombined,
			GammaTotalAbs:   34.9e9,
			RegimeAgreement: "agree:short-gamma",
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityContextOnly,
				RankabilityReason: "freshness: market is closed; cached gamma is context only",
				Freshness:         "closed_session_context",
				Session:           rpc.SessionClosed.String(),
			},
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "negative", Quality: rankableRegimeGammaQuality()},
				"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "negative", Quality: rankableRegimeGammaQuality()},
			},
		},
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeText(env, fix); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"no vote",
		"did not vote: after-hours cached gamma; not a fresh",
		"market-structure read",
		"short-γ (amplifying)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("regime render missing %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"closed-session context", "context only"} {
		if strings.Contains(out, notWant) {
			t.Errorf("regime render should not use vague gamma context wording %q:\n%s", notWant, out)
		}
	}
}

// TestRegimeRow_GammaUsesGammaZeroLabel pins the jargon cleanup: the
// value cell uses "γ-zero", not "flip". Same for the reason strings.
// "flip" stays in code identifiers but does not appear in user-facing
// row text.
func TestRegimeRow_GammaUsesGammaZeroLabel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	zg := 735.0
	gap := 0.5
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				ZeroGamma:      &zg,
				GapPct:         &gap,
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if strings.Contains(row.value, "flip") {
		t.Errorf("value cell must not contain 'flip' jargon: %q", row.value)
	}
	if strings.Contains(row.reason, "flip") {
		t.Errorf("reason must not contain 'flip' jargon: %q", row.reason)
	}
	if !strings.Contains(row.value, "γ-zero") {
		t.Errorf("value cell should label the crossing as 'γ-zero': %q", row.value)
	}
}

// TestQualityTag_ModelledSuppressesShortAge pins the modelled-age behaviour:
// a model output 37 s old reads as fresh, not stale. The "· modelled" tag
// stays unannotated until the model age crosses 20 min, at which point the
// tag adopts a stale-model warning form. The bar sits above the gamma
// compute's own wall clock because the age is measured from the input spot,
// so a lower bar would annotate every result the moment it was published.
func TestQualityTag_ModelledSuppressesShortAge(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	cases := []struct {
		name string
		age  time.Duration
		want string
	}{
		{"1s", 1 * time.Second, "· modelled"},
		{"37s", 37 * time.Second, "· modelled"},
		{"4m_59s", 4*time.Minute + 59*time.Second, "· modelled"},
		{"6m", 6 * time.Minute, "· modelled"},
		{"19m_59s", 19*time.Minute + 59*time.Second, "· modelled"},
		{"21m", 21 * time.Minute, "· modelled 21 min old"},
		{"45m", 45 * time.Minute, "· modelled 45 min old"},
		{"119m", 119 * time.Minute, "· modelled 119 min old"},
		{"overnight", 8*time.Hour + 45*time.Minute, "· modelled 8h old"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := &rpc.Quality{
				AsOf:           now.Add(-tc.age),
				FreshnessClass: rpc.FreshnessModelled,
				Confidence:     rpc.ConfidenceProxy,
			}
			got := qualityTag(now, q)
			if got != tc.want {
				t.Errorf("age=%s tag=%q, want %q", tc.age, got, tc.want)
			}
		})
	}
}

// TestRenderRegime_ExplainCarriesGammaZeroExplanation pins the
// renderer's --explain extension: the gamma row carries a plain-English
// paragraph explaining what the level means and how to read the bands,
// so a non-quant reader doesn't need to leave the terminal for the
// methodology spec.
func TestRenderRegime_ExplainCarriesGammaZeroExplanation(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, regimeFixture(), true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	// Three load-bearing phrases — the read must explain what γ-zero is,
	// what happens above it, and what happens below it.
	for _, want := range []string{"γ-zero", "stabilizing", "amplifying"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing γ-zero plain-English term %q:\n%s", want, out)
		}
	}
}

// TestRenderRegime_ExplainSurfacesDerivedIVDisclosure pins the
// "compute used N derived IVs" line that fires when any leg used the
// BS-IV Newton-Raphson fallback. The fallback runs when the gateway's
// model-computation engine is idle; the disclosure makes the quote/close
// inversion visible to a reader auditing the result.
func TestRenderRegime_ExplainSurfacesDerivedIVDisclosure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	fix := regimeFixture()
	fix.AsOf = now
	zg := 735.0
	gap := 0.27
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			SpotUnderlying: 737.0,
			ZeroGamma:      &zg,
			GapPct:         &gap,
			LegCount:       240,
			PricedLegCount: 900,
			DerivedIVLegs:  240,
			Method:         "perfiliev-bs-sweep-v1",
			Quality:        rankableRegimeGammaQuality(),
		},
	}
	fix.GammaZero.ZeroGammaQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
		Source: "perfiliev-bs-sweep-v1 · BS-IV from option quote/close fallback",
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{"240/900 priced legs", "derived IV", "quote/close"} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing derived-IV disclosure %q:\n%s", want, out)
		}
	}
}

func TestRenderRegime_ExplainSurfacesGammaSPXCacheFallback(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	fix := regimeFixture()
	fix.AsOf = now
	fix.GammaZero.Status = rpc.RegimeStatusOK
	fix.GammaZero.Envelope = rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope:           rpc.GammaZeroScopeCombined,
			GammaTotalAbs:   3.0e9,
			RegimeAgreement: "disagree",
			Quality:         rankableRegimeGammaQuality(),
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive", GammaTotalAbs: 1.0e9, Quality: rankableRegimeGammaQuality()},
				"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "negative", GammaTotalAbs: 2.0e9, Quality: rankableRegimeGammaQuality()},
			},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code:    "spx_cache_fallback:context canceled",
				Scope:   "SPX",
				Message: "SPX live refresh was canceled; using the last successful cached SPX slice.",
				Impact:  "SPX is included but may be stale; treat the combined gamma regime as degraded.",
				Action:  "Refresh during 09:30-16:15 ET and inspect the SPX per-index as_of before relying on the combined gamma row.",
			}},
		},
	}
	fix.GammaZero.ZeroGammaQuality = &rpc.Quality{
		AsOf: now, FreshnessClass: rpc.FreshnessModelled, Confidence: rpc.ConfidenceProxy,
		Source: "bs-gamma-profile-v3-stickymoneyness-0dte-split",
	}

	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	if code := renderRegimeTextTo(env, &stdout, fix, true); code != 0 {
		t.Fatalf("code=%d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"Data note:",
		"cached SPX slice",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--explain output missing gamma SPX fallback note %q:\n%s", want, out)
		}
	}
	for _, notWant := range []string{"SPX live refresh was canceled", "Action: Refresh during", "09:30-16:15 ET"} {
		if strings.Contains(out, notWant) {
			t.Errorf("--explain should hide diagnostic gamma fallback detail %q:\n%s", notWant, out)
		}
	}

	stdout.Reset()
	if code := renderRegimeTextWithOptions(env, &stdout, fix, regimeRenderOptions{Explain: true, Diagnostics: true}); code != 0 {
		t.Fatalf("diagnostics code=%d", code)
	}
	diagnostics := stdout.String()
	for _, want := range []string{"SPX live refresh was canceled", "Action: Refresh during", "09:30-16:15 ET"} {
		if !strings.Contains(diagnostics, want) {
			t.Errorf("--diagnostics output missing gamma fallback detail %q:\n%s", want, diagnostics)
		}
	}
}

// TestAppendRegimeLog_WritesJSONLEntry pins the JSONL append contract:
// one object per call, top-level keys {timestamp, regime}, trailing
// newline, valid JSON in isolation. Concurrent writers aren't in scope
// for v1 (the spec's calibration ritual is daily-cadence).
func TestAppendRegimeLog_WritesJSONLEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "regime-v1.jsonl")
	snap := rpc.RegimeSnapshotResult{
		AsOf:    time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		SpecDoc: "docs/docs/internals/regime-dashboard.md",
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
		},
	}
	if err := appendRegimeLog(path, snap); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := appendRegimeLog(path, snap); err != nil {
		t.Fatalf("second append: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("line count: want 2, got %d (raw: %q)", len(lines), string(data))
	}
	for i, line := range lines {
		var got struct {
			Timestamp time.Time                `json:"timestamp"`
			Regime    rpc.RegimeSnapshotResult `json:"regime"`
		}
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d: invalid JSON: %v\n%s", i, err, line)
			continue
		}
		if got.Regime.SpecDoc != "docs/docs/internals/regime-dashboard.md" {
			t.Errorf("line %d: regime envelope round-tripped wrong: got SpecDoc=%q",
				i, got.Regime.SpecDoc)
		}
		if got.Timestamp.IsZero() {
			t.Errorf("line %d: timestamp missing", i)
		}
	}
}

// TestAppendRegimeLog_CreatesFileIfMissing covers the cold-install
// case: the path doesn't exist yet; the first call creates it.
func TestAppendRegimeLog_CreatesFileIfMissing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fresh.jsonl")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("path should not exist before append: %v", err)
	}
	if err := appendRegimeLog(path, rpc.RegimeSnapshotResult{}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("path should exist after append: %v", err)
	}
}

// ----- B3: scope-aware gamma row label -----

// TestGammaRowLabel_ScopeAware pins the name column for the gamma row:
// SPY-only / SPX-only / combined runs each get their own label so a
// reader doesn't mis-read a combined run as SPY. A nil result (cold /
// computing / error state) defaults to the combined label since the
// regime daemon always asks for combined gamma — the row name must not
// silently flip between calls depending on whether the envelope has
// landed. A populated result with an empty Scope string (a legacy
// daemon that pre-dates the scope field) still falls back to
// "SPY γ-zero" to preserve old behaviour.
func TestGammaRowLabel_ScopeAware(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		scope  string
		want   string
		hasRes bool
	}{
		{"spy", rpc.GammaZeroScopeSPY, "SPY γ-zero", true},
		{"spx", rpc.GammaZeroScopeSPX, "SPX γ-zero", true},
		{"combined", rpc.GammaZeroScopeCombined, "γ-zero (SPY+SPX)", true},
		{"empty_scope_legacy", "", "SPY γ-zero", true},
		{"nil_result_defaults_combined", "", "γ-zero (SPY+SPX)", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := rpc.RegimeGammaZero{}
			if tc.hasRes {
				r.Envelope.Result = &rpc.GammaZeroComputed{Scope: tc.scope}
			}
			if got := gammaRowLabel(r); got != tc.want {
				t.Errorf("scope=%q hasRes=%v label=%q want %q", tc.scope, tc.hasRes, got, tc.want)
			}
		})
	}
}

// TestRowGamma_UsesScopeAwareLabel pins the integration: rowGamma reads
// the Scope from the envelope and threads it into the row's name field
// via gammaRowLabel.
func TestRowGamma_UsesScopeAwareLabel(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				Scope:          rpc.GammaZeroScopeCombined,
				SpotUnderlying: 737.0,
				GammaSign:      "positive",
				GammaTotalAbs:  2.7e9,
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if row.name != "γ-zero (SPY+SPX)" {
		t.Errorf("combined-scope row name=%q want %q", row.name, "γ-zero (SPY+SPX)")
	}
}

// ----- B4: conditional |GEX| rendering in the regime row -----

// TestRowGamma_OmitsMagnitudeWhenZero pins the conditional rendering:
// when GammaTotalAbs is zero (no-crossing degenerate case or v2 daemon
// without the aggregator), the row value omits the "|GEX| X.Xbn"
// segment entirely rather than painting a misleading "$0.0bn".
func TestRowGamma_OmitsMagnitudeWhenZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "positive",
				GammaTotalAbs:  0, // explicit zero
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if strings.Contains(row.value, "|GEX|") {
		t.Errorf("zero magnitude should omit |GEX| segment, got value=%q", row.value)
	}
	if !strings.Contains(row.value, "long-γ") {
		t.Errorf("regime classification should still surface, got value=%q", row.value)
	}
}

// TestRowGamma_KeepsMagnitudeWhenNonZero pins the positive path:
// non-zero GammaTotalAbs renders inline alongside the signed call so
// the magnitude co-primary stays visible in the regime row.
func TestRowGamma_KeepsMagnitudeWhenNonZero(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				SpotUnderlying: 737.0,
				GammaSign:      "positive",
				GammaTotalAbs:  2.7e9,
				Quality:        rankableRegimeGammaQuality(),
			},
		},
	})
	if !strings.Contains(row.value, "|GEX| 2.7bn") {
		t.Errorf("non-zero magnitude should render inline, got value=%q", row.value)
	}
}

// ----- U3: shortened gamma-row reason -----

// TestRowGamma_ShortReason pins the compressed reason strings: the
// long-form spec disclosure ("stabilizing regime, γ-zero is well below
// spot") has moved to --explain; the row carries the compact form
// "dealer long-γ · stabilizing" / "dealer short-γ · amplifying".
func TestRowGamma_ShortReason(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	cases := []struct {
		sign string
		want string
	}{
		{"positive", "dealer long-γ · stabilizing"},
		{"negative", "dealer short-γ · amplifying"},
	}
	for _, tc := range cases {
		row := rowGamma(now, rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					SpotUnderlying: 737.0,
					GammaSign:      tc.sign,
					GammaTotalAbs:  2.7e9,
					Quality:        rankableRegimeGammaQuality(),
				},
			},
		})
		if row.reason != tc.want {
			t.Errorf("sign=%q reason=%q want %q", tc.sign, row.reason, tc.want)
		}
		// The long-form disclosure must NOT leak into the row reason.
		if strings.Contains(row.reason, "well below spot") || strings.Contains(row.reason, "well above spot") {
			t.Errorf("long-form spec text should live under --explain, not the row reason: %q", row.reason)
		}
	}
}

// TestRegimeRow_GammaCombinedHidesPerIndexMechanics pins the combined
// default contract: there is no top-level SPY anchor, and the regime row
// should render the trader-facing gamma implication instead of SPY/SPX
// agreement mechanics.
func TestRegimeRow_GammaCombinedHidesPerIndexMechanics(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				Scope:           rpc.GammaZeroScopeCombined,
				GammaTotalAbs:   1.8e9,
				RegimeAgreement: "agree:long-gamma",
				Quality:         rankableRegimeGammaQuality(),
				PerIndex: map[string]*rpc.GammaZeroComputed{
					"SPY": {Scope: rpc.GammaZeroScopeSPY, SpotUnderlying: 743.73, GammaSign: "positive", Quality: rankableRegimeGammaQuality()},
					"SPX": {Scope: rpc.GammaZeroScopeSPX, SpotUnderlying: 5430.0, GammaSign: "positive", Quality: rankableRegimeGammaQuality()},
				},
			},
		},
	})
	if strings.Contains(row.value, "spot 743.73") {
		t.Errorf("combined-scope row must not invent a top-level SPY spot: %q", row.value)
	}
	if !strings.Contains(row.value, "long-γ (stabilizing)") {
		t.Errorf("combined-scope row should surface gamma implication: %q", row.value)
	}
	if strings.Contains(row.value, "SPY/SPX") || strings.Contains(row.reason, "SPY/SPX") {
		t.Errorf("combined-scope row should hide per-index mechanics: value=%q reason=%q", row.value, row.reason)
	}
	if !strings.Contains(row.value, "|GEX|") {
		t.Errorf("combined-scope row should still surface magnitude when non-zero: %q", row.value)
	}
	if row.band != bandGreen {
		t.Errorf("combined long-gamma agreement should be green, got %v", row.band)
	}
	if row.reason != "dealer gamma stabilizing" {
		t.Errorf("reason=%q want trader-facing gamma implication", row.reason)
	}
}

func TestRegimeRow_GammaCombinedDisagreementEscalatesDominantRed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	row := rowGamma(now, rpc.RegimeGammaZero{
		Status: rpc.RegimeStatusOK,
		Envelope: rpc.GammaZeroSPXResult{
			Status: rpc.GammaZeroStatusReady,
			Result: &rpc.GammaZeroComputed{
				Scope:           rpc.GammaZeroScopeCombined,
				GammaTotalAbs:   1.8e9,
				RegimeAgreement: "disagree",
				Quality:         rankableRegimeGammaQuality(),
				PerIndex: map[string]*rpc.GammaZeroComputed{
					"SPY": {Scope: rpc.GammaZeroScopeSPY, GammaSign: "positive", Quality: rankableRegimeGammaQuality()},
					"SPX": {Scope: rpc.GammaZeroScopeSPX, GammaSign: "negative", Quality: rankableRegimeGammaQuality()},
				},
			},
		},
	})
	if row.band != bandRed {
		t.Errorf("SPX-dominant combined disagreement should be red, got %v", row.band)
	}
	if row.reason != "dealer gamma amplifying" {
		t.Errorf("combined dominant-red reason=%q want trader-facing amplification", row.reason)
	}
	if strings.Contains(row.value+" "+row.reason, "SPY/SPX") || strings.Contains(row.value+" "+row.reason, "disagree") {
		t.Errorf("combined dominant-red row should not leak internal agreement mechanics: value=%q reason=%q", row.value, row.reason)
	}
}

// TestRegimeRow_GammaSingleScopeKeepsSpotPrefix pins the matching
// invariant: single-underlying envelopes (SPY-only or SPX-only) keep
// the "spot X.XX" prefix because the row's spot IS the regime anchor
// — the header line may show SPY for SPX-only and the reader needs
// the explicit anchor to interpret the band.
func TestRegimeRow_GammaSingleScopeKeepsSpotPrefix(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 13, 12, 0, 0, time.UTC)
	for _, scope := range []string{rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX, ""} {
		row := rowGamma(now, rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					Scope:          scope,
					SpotUnderlying: 5430.0,
					GammaSign:      "positive",
					GammaTotalAbs:  4.2e9,
					Quality:        rankableRegimeGammaQuality(),
				},
			},
		})
		if !strings.Contains(row.value, "spot 5430.00") {
			t.Errorf("scope=%q value cell should keep the spot prefix: %q", scope, row.value)
		}
	}
}

func TestRegimeEligibilityReasonTextDistinguishesNotDue(t *testing.T) {
	if got := regimeEligibilityReasonText([]string{"data_not_due"}); got != "new session data not due yet" {
		t.Fatalf("not-due reason=%q", got)
	}
	if got := regimeEligibilityReasonText([]string{"data_overdue"}); got != "data overdue" {
		t.Fatalf("overdue reason=%q", got)
	}
}
