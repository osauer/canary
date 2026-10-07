package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// updateGolden rewrites testdata/golden from the current renderer output:
//
//	go test ./internal/cli -run TestRenderGolden -update
//
// Review the resulting diff like code: every changed byte is a change to what
// a person reads in the terminal.
var updateGolden = flag.Bool("update", false, "rewrite internal/cli/testdata/golden from the current renderer output")

const goldenDir = "testdata/golden"

// goldenAt anchors every synthetic timestamp. It is built in the local zone so
// renderers that print .Local() wall-clock time show the same hours in every
// zone; goldenText then writes the zone abbreviation as "TZ".
var goldenAt = time.Date(2026, 9, 5, 14, 0, 0, 0, time.Local)

// goldenConn answers each daemon method with one fixed synthetic result,
// round-tripped through JSON as the daemon socket delivers it. A method
// without a result fails, so a renderer that grows a new read shows up here.
type goldenConn map[string]any

func (c goldenConn) Call(_ context.Context, method string, _ any, out any) error {
	result, ok := c[method]
	if !ok {
		return fmt.Errorf("golden: no synthetic result for %s", method)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (goldenConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return errors.New("golden: unexpected stream")
}

// renderGoldenCase is one human-readable command output pinned byte for byte.
type renderGoldenCase struct {
	name string
	// argv is the command line after `canary`; Run receives argv[0] and the
	// rest, exactly as cmd/canary dispatches it.
	argv []string
	// conn answers the daemon reads the command makes.
	conn goldenConn
	// exit is the command's expected exit code.
	exit int
	// render, when set, replaces Run with a direct call to the renderer the
	// command uses. Only for commands whose fetch derives the result from
	// several reads rather than serving one typed result.
	render func(env *Env)
	// stdin is what the command reads from standard input.
	stdin string
}

// TestRenderGolden pins the human-readable output of the desk commands at 80
// columns with colour off and on, so a silent loss of layout, labels or colour
// fails the gate instead of shipping. Every value is synthetic.
func TestRenderGolden(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	cases := renderGoldenCases()
	files := map[string]bool{}
	for _, tc := range cases {
		files[tc.name+".txt"] = true
		t.Run(tc.name, func(t *testing.T) {
			var doc strings.Builder
			for _, color := range []bool{false, true} {
				out := renderGolden(t, tc, color)
				mode := "colour off"
				if color {
					mode = `colour on, ESC written as \x1b`
				} else if strings.Contains(out, "\x1b") {
					t.Fatalf("colour-off output carries an escape sequence:\n%s", out)
				}
				fmt.Fprintf(&doc, "# canary %s · COLUMNS=80 · %s\n", strings.Join(tc.argv, " "), mode)
				doc.WriteString(goldenText(out))
			}
			compareGolden(t, filepath.Join(goldenDir, tc.name+".txt"), doc.String())
		})
	}
	entries, err := os.ReadDir(goldenDir)
	if err != nil {
		if *updateGolden {
			return
		}
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !files[entry.Name()] {
			t.Errorf("%s has no case in TestRenderGolden; delete the file or restore the case", filepath.Join(goldenDir, entry.Name()))
		}
	}
}

func renderGolden(t *testing.T, tc renderGoldenCase, color bool) string {
	t.Helper()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: tc.conn, Color: color}
	if tc.stdin != "" {
		env.Stdin = strings.NewReader(tc.stdin)
	}
	if tc.render != nil {
		tc.render(env)
	} else if code := Run(t.Context(), env, tc.argv[0], tc.argv[1:]); code != tc.exit {
		t.Fatalf("exit %d, want %d; stderr: %s", code, tc.exit, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	return stdout.String()
}

// goldenText makes a render machine-independent and reviewable: the local zone
// abbreviation becomes "TZ" and each ESC byte is written as the text \x1b.
func goldenText(out string) string {
	if zone := goldenAt.Format("MST"); zone != "" {
		out = strings.ReplaceAll(out, " "+zone, " TZ")
	}
	return strings.ReplaceAll(out, "\x1b", `\x1b`)
}

func compareGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; create it with: go test ./internal/cli -run TestRenderGolden -update", err)
	}
	want := string(raw)
	if got == want {
		return
	}
	wantLines, gotLines := strings.Split(want, "\n"), strings.Split(got, "\n")
	line := func(lines []string, i int) string {
		if i < len(lines) {
			return lines[i]
		}
		return "<end of output>"
	}
	for i := range max(len(wantLines), len(gotLines)) {
		if w, g := line(wantLines, i), line(gotLines, i); w != g {
			t.Fatalf("%s differs at line %d\nwant: %q\n got: %q\nif the change is intended: go test ./internal/cli -run TestRenderGolden -update", path, i+1, w, g)
		}
	}
}

func renderGoldenCases() []renderGoldenCase {
	fresh := rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityFresh, LastSuccessAt: new(goldenAt), LastSuccessAgeSeconds: new(int64(60))}
	stale := rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityStale, LastSuccessAt: new(goldenAt.Add(-2 * time.Hour)), LastSuccessAgeSeconds: new(int64(7200)), FailureCode: rpc.RegimeAuthorityFailureRefreshTimeout}
	readyTrading := rpc.TradingStatus{Mode: config.TradingModePaper, Endpoint: "127.0.0.1:4002", Account: "DU0000000", AccountOrigin: "config", ClientID: 7, ClientIDOrigin: "config", MCPTrading: rpc.TradingMCPDisabled, CanPreview: true, CanWrite: true, OpenOrders: 2, LastOrderEvent: "order 1 Submitted", TradingControlGeneration: 3,
		OrderLimits: &risk.OrderLimitsInForce{Complete: true, BaseCurrency: "EUR", CapBase: 12000, CapBound: risk.OrderCapBoundPctNLV, Summary: "12,000 EUR (5% of NLV 240,000 EUR; [order_limits])",
			MaxOptionContracts: 5, MaxBondMaturityYears: 30}}
	blockedTrading := rpc.TradingStatus{Mode: config.TradingModeLive, Endpoint: "127.0.0.1:4001", Account: "DU0000000", AccountOrigin: "config", ClientID: 7, ClientIDOrigin: "config", MCPTrading: rpc.TradingMCPDisabled, CanPreview: true, LiveOverride: rpc.TradingLiveOverrideBlocked, Blocked: true, Freeze: true, TradingControlGeneration: 4,
		Blockers:      []rpc.TradingBlocker{{Code: "live_override_missing", Message: "live trading needs the live override", Action: "set the live override in config.toml"}},
		WriteBlockers: []rpc.TradingBlocker{{Code: "trading_frozen", Message: "trading.freeze is on; only cancels pass"}},
	}
	return []renderGoldenCase{
		{name: "regime", argv: []string{"regime"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(fresh)}},
		{name: "regime_explain", argv: []string{"regime", "--explain"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(fresh)}},
		{name: "regime_stale", argv: []string{"regime"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(stale)}},
		// runStress derives its result from account, positions, regime and
		// rulebook reads; the golden pins the renderer it hands the result to.
		{name: "stress", argv: []string{"stress"}, render: func(env *Env) { renderStress(env, goldenStress(), false) }},
		{name: "stress_details", argv: []string{"stress", "--details"}, render: func(env *Env) { renderStress(env, goldenStress(), true) }},
		{name: "status", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: goldenHealthReady(readyTrading), rpc.MethodAlertCandidates: goldenAlerts(3)}},
		{name: "status_attention", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: goldenHealthAttention(), rpc.MethodAlertCandidates: goldenAlerts(2)}},
		{name: "trading_status", argv: []string{"trading", "status"}, conn: goldenConn{rpc.MethodTradingStatus: readyTrading}},
		{name: "trading_status_blocked", argv: []string{"trading", "status"}, conn: goldenConn{rpc.MethodTradingStatus: blockedTrading}, exit: 1},
		{name: "settings_show", argv: []string{"settings", "show"}, conn: goldenConn{rpc.MethodSettingsGet: goldenSettings()}},
		{name: "technical", argv: []string{"technical", "SYNA,SYNB,SYNC"}, conn: goldenConn{rpc.MethodTechnical: goldenTechnical()}},
		{name: "proposals_list_empty", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: rpc.TradeProposalSnapshot{Revision: "rev-0000", PolicyID: "protection", PolicyVersion: 1, Proposals: []rpc.TradeProposal{}}}},
		{name: "proposals_list_one", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenProposals()}},
		{name: "proposals_list_sweep_borrowed", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenSweepBorrowed()}},
		{name: "proposals_list_sweep_borrowed_details", argv: []string{"proposals", "list", "--details"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenSweepBorrowed()}},
		{name: "proposals_list_leveling", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenLevelingProposals()}},
		{name: "proposals_list_leveling_details", argv: []string{"proposals", "list", "--details"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenLevelingProposals()}},
		{name: "proposals_list_leveling_bundle", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenLevelingBundleProposals()}},
		{name: "proposals_list_leveling_bundle_details", argv: []string{"proposals", "list", "--details"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenLevelingBundleProposals()}},
		{name: "proposals_prepare_bundle", argv: []string{"proposals", "prepare-bundle", goldenBundleID, goldenBundleRevision}, conn: goldenConn{rpc.MethodTradeProposalsPrepareBundle: goldenBundlePrepared()}},
		{name: "proposals_submit_bundle_partly_sent", argv: []string{"proposals", "submit-bundle", "--stdin"}, stdin: goldenBundleSubmitInput,
			conn: goldenConn{rpc.MethodTradeProposalsSubmitBundle: goldenBundlePartlySent()}},
		{name: "proposals_bundle_status", argv: []string{"proposals", "bundle-status", "--bundle-ref-stdin"}, stdin: goldenBundleRef,
			conn: goldenConn{rpc.MethodTradeProposalsPreparedBundleStatus: goldenBundleStatus()}},
		{name: "brief", argv: []string{"brief"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(true)}},
		{name: "setups_evaluate_confirmed", argv: []string{"setups", "evaluate"}, render: func(env *Env) { renderSetupResult(env, goldenSetup()) }},
		{name: "brief_details", argv: []string{"brief", "--details"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(true)}},
		{name: "brief_rows", argv: []string{"brief"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(false)}},
		{name: "order_preview_bond", argv: []string{"order", "preview", "buy", "DE000SYN0000", "10000", "--type", "BOND", "--currency", "EUR"}, conn: goldenConn{rpc.MethodOrderPreview: goldenBondPreview()}},
	}
}

// goldenBondPreview is a synthetic buy of a government bond named by ISIN
// (internal-docs/design/bond-orders.md), with its issuer evidence and its
// accrued interest bound.
func goldenBondPreview() rpc.OrderPreviewResult {
	bid, ask := 98.40, 98.44
	return rpc.OrderPreviewResult{
		PreviewToken: "tok-synthetic", PreviewTokenID: "ptk-0001", PreviewTokenScope: rpc.OrderTokenScopePlace, PreviewTokenExpiresAt: time.Date(2026, 9, 5, 12, 2, 0, 0, time.UTC),
		TokenMinted: true, SubmitEligible: true, Mode: config.TradingModePaper, Account: "DU0000000", Endpoint: "127.0.0.1:4002", ClientID: 7,
		Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Contract: rpc.ContractParams{ConID: 900001, Symbol: "SYNB", SecType: "BOND", Exchange: "SMART", Currency: "EUR"},
			Quantity: 10000, OrderType: rpc.OrderTypeLMT, LimitPrice: 98.42, TIF: rpc.OrderTIFDay, Strategy: rpc.OrderStrategyPatientLimit, OrderRef: "canary-synthetic",
			Bond: &rpc.OrderBondTerms{Instrument: rpc.OrderBondInstrumentByIdentifier, ISIN: "DE000SYN0000", QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1,
				PriceConvention: rpc.BondPriceConventionPer100, MinTick: 0.0001, MinSize: 1, SizeIncrement: 1, FaceValue: 10000, Maturity: "2036-02-15",
				MaturitySource: "ecb_eligible_assets", IssuerClass: rpc.BondIssuerGovernment, Issuer: "Synthetic Republic", EvidenceSource: "ecb_eligible_assets",
				EvidenceAsOf: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), Coupon: new(2.5), AccruedBound: 250}},
		Quote:    rpc.OrderQuoteSnapshot{Symbol: "SYNB", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, AsOf: goldenAt},
		Position: rpc.OrderPositionImpact{Before: 0, After: 10000, Effect: rpc.OrderPositionEffectOpen},
		Notional: 10092, NotionalCurrency: "EUR", NotionalBase: 10092, BaseCurrency: "EUR",
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, RequiredForSubmit: true},
	}
}

func goldenRegime(authority rpc.RegimeAuthorityHealth) rpc.RegimeSnapshotResult {
	thresholds := func(green, yellow, red string) *rpc.RegimeThresholds {
		return &rpc.RegimeThresholds{Green: green, Yellow: yellow, Red: red}
	}
	daily := func(date, source string) *rpc.RegimeAsOfSummary {
		return &rpc.RegimeAsOfSummary{Label: "close D-1", Date: date, Source: source}
	}
	return rpc.RegimeSnapshotResult{
		AsOf:            goldenAt,
		AuthorityHealth: &authority,
		Lifecycle: rpc.LifecycleState{Stage: "early_warning", Severity: "watch", Readiness: "ready",
			Governors: []rpc.GovernorAction{{Action: "downgrade", From: "confirmed_stress", To: "early_warning", Reason: "pending_backtest_no_tape_cosign"}}},
		Summary:   rpc.RegimeSummary{Evidence: "1 red, 1 amber, 5 green of 8 clusters", PunchLine: "One stress signal is unconfirmed; the other clusters read constructive."},
		Posture:   rpc.RegimePosture{Label: "Watch: one unconfirmed stress signal", Tone: rpc.RegimeToneWatch},
		Composite: rpc.RegimeComposite{Verdict: "Watch: one unconfirmed stress signal", GreenCount: 5, YellowCount: 1, RedCount: 1, RankedCount: 7, UnrankedCount: 1},
		VIXTermStructure: rpc.RegimeVIXTerm{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "VIX below VIX3M (contango)", AsOf: &rpc.RegimeAsOfSummary{Label: "live", Time: goldenAt, Source: "broker"}, Thresholds: thresholds("below 0.95", "0.95 to 1.05", "above 1.05")},
			Status:              rpc.RegimeStatusOK, VIX: new(18.0), VIX3M: new(20.0), Ratio: new(0.9),
		},
		VolOfVol: rpc.RegimeVolOfVol{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "yellow", BandReason: "VVIX up 10% in five sessions", AsOf: daily("2026-09-04", "Cboe"), Thresholds: &rpc.RegimeThresholds{Green: "5d change below 5%", Yellow: "5d change 5% to 15%", Red: "5d change above 15%", PendingBacktest: true}},
			Status:              rpc.RegimeStatusOK, Last: new(100.0), Change5D: new(10.0), AsOfDate: "2026-09-04",
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "HYG above its 50-day average", Thresholds: thresholds("HYG above 50-day average", "HYG below average, SPY near high", "HYG below average, SPY falling")},
			Status:              rpc.RegimeStatusOK, HYGPrice: new(80.0), HYG50DMA: new(79.0), SPYPrice: new(500.0), SPYChangePct: new(0.5),
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "high-yield spreads stable", AsOf: daily("2026-09-03", "FRED")},
			Status:              rpc.RegimeStatusOK, HYOAS: new(3.0), IGOAS: new(1.0), HYIGSpread: new(2.0), HY20DChange: new(0.1), AsOfDate: "2026-09-03",
		},
		FundingStress: rpc.RegimeFundingStress{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "funding spread normal", AsOf: daily("2026-09-03", "FRED")},
			Status:              rpc.RegimeStatusOK, SpreadBps: new(20.0), Change5Bps: new(2.0), AsOfDate: "2026-09-03",
		},
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "red", BandReason: "yen up 3% in a week", Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}, Eligibility: &rpc.RegimeEligibility{Reasons: []string{"no second cluster confirms"}}},
			Status:              rpc.RegimeStatusOK, Symbol: "USD.JPY", Last: new(145.0), WeeklyChange: new(-3.0),
		},
		GammaZero: rpc.RegimeGammaZero{Status: rpc.RegimeStatusComputing},
		Breadth: rpc.RegimeBreadth{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "most members above their 50-day average"},
			Status:              rpc.RegimeStatusOK, PctAbove50DMA: 60, PctAbove200DMA: new(65.0),
			Envelope: rpc.BreadthSPXResult{MemberCount: 500, Coverage50: 500, Coverage200: 500, CoverageHighsLows: 500},
		},
		WarningDetails: []rpc.RegimeWarning{{Code: "gamma_computing", Message: "Dealer gamma model is still computing."}},
		SourceHealth:   []rpc.SourceHealth{{Source: "broker", Status: "ok", AsOf: goldenAt, Notes: []string{"live quotes"}}},
	}
}

func goldenStress() rpc.StressResult {
	return rpc.StressResult{
		AsOf:               goldenAt,
		Action:             "watch",
		Severity:           risk.SeverityWatch,
		Summary:            "One finding needs review; market stress is unconfirmed.",
		InputHealth:        "complete",
		MarketConfirmation: "unconfirmed",
		PortfolioFit:       "relevant",
		Rows: []rpc.StressRow{
			{Title: "Portfolio stress", Severity: risk.SeverityWatch, Evidence: "1 act and 1 watch finding across 4 checks."},
			{Title: "Margin headroom", Direction: risk.DirectionDefensive, Severity: risk.SeverityWatch, Evidence: "Cushion is 40% of net liquidation.", Guidance: "Compare the cushion with the margin rule before adding exposure."},
			{Title: "Concentration", Direction: risk.DirectionDefensive, Severity: risk.SeverityAct, Evidence: "SYNA is 30% of net liquidation.", Guidance: "Review the position size against the concentration rule."},
			{Title: "Daily loss", Direction: risk.DirectionDefensive, Severity: risk.SeverityObserve, Evidence: "Down 0.5% on the day.", Guidance: "No action."},
			{Title: "Market events", Direction: risk.DirectionDataQuality, Severity: risk.SeverityWatch, Evidence: "Earnings calendar unavailable.", Guidance: "Retry once the calendar source recovers."},
		},
		MarketIndicators: []rpc.StressMarketIndicator{{Name: "VIX/VIX3M", Status: "green", Reading: "0.90", AsOf: "live", Comment: "contango"}},
		Warnings:         []string{"market events: calendar source unavailable"},
		SourceHealth:     []rpc.SourceHealth{{Source: "account", Status: "ok", AsOf: goldenAt}},
		NotExecution:     "Read-only assessment; it places no orders.",
	}
}

func goldenHealthReady(trading rpc.TradingStatus) rpc.HealthResult {
	return rpc.HealthResult{
		Verdict:          rpc.HealthVerdict{State: "READY"},
		DaemonVersion:    "v0.0.0-golden",
		UptimeSeconds:    7200,
		Account:          "DU0000000",
		ConnectedAccount: "DU0000000",
		AccountMode:      rpc.AccountModePaper,
		GatewayHost:      "127.0.0.1",
		GatewayPort:      4002,
		PortOrigin:       "configured",
		ClientID:         7,
		Connected:        true,
		GatewayPhase:     rpc.GatewayPhaseReady,
		DataType:         rpc.MarketDataLive,
		ServerVersion:    176,
		BackgroundTasks:  []rpc.BackgroundTaskStatus{},
		Subsystems:       []rpc.SubsystemHealth{{Name: "regime", Status: "ready"}, {Name: "breadth", Status: "ready"}, {Name: "rulebook", Status: "ready"}},
		Members:          rpc.MembersHealth{Source: "fixture", AsOf: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Count: 500, RefreshState: "healthy"},
		Trading:          trading,
		DataHealth:       &rpc.DataHealthResult{Summary: rpc.DataHealthSummary{State: "current", Label: "12 of 12 sources current"}},
	}
}

func goldenHealthAttention() rpc.HealthResult {
	res := goldenHealthReady(rpc.TradingStatus{Mode: config.TradingModePaper, MCPTrading: rpc.TradingMCPDisabled, Blocked: true, Blockers: []rpc.TradingBlocker{{Code: "market_data_delayed", Message: "market data is delayed"}}})
	res.Verdict = rpc.HealthVerdict{State: "ATTENTION", Reason: "Historical data farm broken"}
	res.DataType = rpc.MarketDataDelayed
	res.DataFarms = []rpc.DataFarmHealth{{Name: "ushmds", Type: "hmds", Status: "broken", Code: 2105}}
	res.MarketDataAccess = []rpc.MarketDataAccessHealth{{RouteKey: "SYNB", Symbol: "SYNB", Code: 354, Reason: rpc.MarketDataAccessNotSubscribed, ObservedAt: goldenAt, RetryAt: goldenAt.Add(30 * time.Minute)}}
	res.BackendLink = &rpc.BackendLinkHealth{Losses: 2, LossesInMaintenanceWindow: 1, LastOutageSeconds: 30, LongestOutageSeconds: 90}
	res.AnswerPath = []rpc.ConnectionAnswerPath{{Lane: "history", State: rpc.AnswerPathStalled, StalledSince: goldenAt.Add(-10 * time.Minute), RedialDue: goldenAt.Add(5 * time.Minute)}}
	res.BackgroundTasks = []rpc.BackgroundTaskStatus{{Name: "gamma-zero"}}
	res.Subsystems = []rpc.SubsystemHealth{{Name: "regime", Status: "ready"}, {Name: "gamma", Status: "degraded", Message: "model inputs partial"}, {Name: "breadth", Status: "warming", Progress: 40}}
	res.DataQuality = []rpc.DataQualityHealth{{Surface: "regime", Status: "degraded", DegradedClusters: []string{"gamma"}, Summary: "degraded: gamma"}}
	res.DataHealth = &rpc.DataHealthResult{
		Summary:  rpc.DataHealthSummary{State: "limited", Label: "10 of 12 sources current"},
		Concerns: []rpc.DataHealthConcern{{SourceID: "hmds", State: "unavailable", Label: "Historical data farm broken"}, {SourceID: "quotes", State: "limited", Label: "Delayed quotes for 1 symbol"}},
	}
	return res
}

func goldenAlerts(covered int) rpc.AlertCandidateSnapshot {
	expected := []rpc.AlertSource{rpc.AlertSourceStress, rpc.AlertSourceRegime, rpc.AlertSourceRulebook}
	freshness := rpc.AlertCoverageCurrent
	if covered < len(expected) {
		freshness = rpc.AlertCoverageStale
	}
	return rpc.AlertCandidateSnapshot{Coverage: rpc.AlertCoverage{Freshness: freshness, ExpectedSources: expected, CoveredSources: expected[:covered]}, Candidates: []rpc.AlertCandidate{}}
}

func goldenSettings() rpc.PlatformSettings {
	var st rpc.PlatformSettings
	st.Display.DateFormat = rpc.SettingsString{Value: rpc.DisplayDateFormatEU, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Features.StockProtection.Enabled = rpc.SettingsBool{Value: true, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Features.Rulebook.Enabled = rpc.SettingsBool{Value: true, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceConfig}
	st.Features.Rulebook.EarningsOverrides = rpc.SettingsStringMap{Value: map[string]string{"SYNA": "2026-10-20"}, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Trading.Freeze = rpc.SettingsBool{Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Trading.Mode = rpc.SettingsString{Value: config.TradingModePaper, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceConfig}
	st.Trading.Endpoint = rpc.SettingsString{Value: "127.0.0.1:4002", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved}
	st.Trading.Account = rpc.SettingsString{Value: "DU0000000", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved}
	st.Trading.MCPTrading = rpc.SettingsString{Value: rpc.TradingMCPDisabled, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceConfig}
	limitReason := "risk-policy.toml [order_limits]: 12,000 EUR (5% of NLV 240,000 EUR; [order_limits])"
	st.Trading.Limits.MaxNotional = rpc.SettingsFloat{Value: 12000, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Reason: limitReason}
	st.Trading.Limits.MaxOptionContracts = rpc.SettingsInt{Value: 5, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Reason: limitReason}
	st.Trading.Limits.AllowStockShort = rpc.SettingsBool{Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Reason: limitReason}
	st.Trading.Limits.AllowOptionSellToOpen = rpc.SettingsBool{Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Reason: limitReason}
	st.Trading.Limits.MaxBondMaturityYears = rpc.SettingsInt{Value: 30, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourcePolicy, Reason: limitReason}
	st.MarketData.Quality = rpc.PlatformMarketDataQuality{Status: "live", Summary: "all quotes live", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved}
	st.Build.Channel = rpc.SettingsString{Value: "release", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceBuild}
	return st
}

func goldenTechnical() rpc.TechnicalResult {
	return rpc.TechnicalResult{Benchmark: "SPY", LookbackDays: 420, AsOf: goldenAt, Rows: []rpc.TechnicalRow{
		{Symbol: "SYNA", Price: new(100.0), SMA50: new(95.0), SMA200: new(90.0), PctAbove200DMA: new(0.11), RS63D: new(0.05), RS126D: new(0.1), ATRPct: new(0.02), AvgVolume20D: new(int64(1_000_000)), AvgDollarVolume20D: new(100_000_000.0), TrendState: "uptrend", DataQuality: "ok"},
		{Symbol: "SYNB", Price: new(50.0), SMA50: new(55.0), RS63D: new(-0.05), RS126D: new(-0.1), ATRPct: new(0.04), AvgVolume20D: new(int64(20_000)), AvgDollarVolume20D: new(1_000_000.0), TrendState: "downtrend", DataQuality: "partial"},
		{Symbol: "SYNC", Error: "no historical data"},
	}}
}

func goldenProposals() rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{
		Revision:      "rev-0001",
		PolicyID:      "protection",
		PolicyVersion: 1,
		Counts:        rpc.TradeProposalCounts{Total: 1, Actionable: 1, TrailingStop: 1},
		Proposals: []rpc.TradeProposal{{
			Key: "ts-SYNA", Bucket: rpc.TradeProposalBucketTrailingStop, Action: rpc.OrderActionSell, Quantity: 100, Symbol: "SYNA", SecType: "STK",
			OrderType: rpc.OrderTypeTRAIL, Trail: &rpc.OrderTrailSpec{OffsetType: "percent", TrailingPercent: new(8.0), InitialStopPrice: 92},
			TIF: rpc.OrderTIFGTC, Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK", Currency: "USD"},
			Reason:           "long stock without a protective stop",
			PositionQuantity: 100, PositionMarketValue: 10000, MarketValuePctNLV: new(10.0),
			PositionDayChangeMoney: new(-200.0), PositionDayChangeCurrency: "USD", PositionDayChangePct: new(-2.0),
			TrailSizing:        &rpc.TradeProposalTrailSizing{Method: "atr", SelectedBy: "atr", PolicyMinPct: 5, PolicyMaxPct: 12, ChosenPct: 8},
			ExecutionSemantics: &rpc.TradeProposalExecutionSemantics{ReferenceSide: "bid", ReferencePrice: new(100.0), TriggerMethodLabel: "last", PriceGuarantee: "stop_price_is_not_execution_price"},
			StopRisk:           &rpc.TradeProposalStopRisk{EstimatedLoss: new(800.0), Currency: "USD", EstimatedLossPctNLV: new(0.8), DistancePct: new(8.0)},
			Details:            []string{"protects 100 sh of SYNA"},
		}},
	}
}

// goldenSweepBorrowed is a sweep whose EUR buy holds because USD is
// borrowed (no_buy_while_borrowed, 2026-10-05 21:24 CEST).
func goldenSweepBorrowed() rpc.TradeProposalSnapshot {
	msg := "USD is borrowed: −20,000 USD; bill buys wait until it is repaid"
	blocker := rpc.TradingBlocker{Code: rpc.CashSweepBlockerCurrencyBorrowed, Message: msg,
		Action: "Repay the USD debit by converting another currency or depositing USD; the sweep never converts. Bill buys resume on the next cycle once no currency is below −1 of its own unit. Selling bills to cover cash is still allowed."}
	return rpc.TradeProposalSnapshot{
		Revision: "rev-0002", PolicyID: "protection", PolicyVersion: 2, Proposals: []rpc.TradeProposal{},
		CashSweep: &rpc.TradeProposalCashSweepStatus{Mode: rpc.CashSweepModeActive, BaseCurrency: "EUR", TaxReviewed: true, TaxReviewedAt: "2026-10-01",
			Borrowing: &rpc.CashSweepBorrowing{State: rpc.CashSweepBorrowingBorrowed, NoBuyWhileBorrowed: new(true), HoldsBuys: true, ToleranceUnits: 1,
				Borrowed: []rpc.CashSweepBorrowedCurrency{{Currency: "USD", Cash: -20000, Borrowed: 20000, BorrowedBase: new(18000.0)}},
				Message:  msg, Action: blocker.Action},
			Currencies: []rpc.TradeProposalCashSweepCurrency{
				{Currency: "EUR", State: rpc.CashSweepStateHold, Reason: msg, Instruments: []string{"de_bubill"}, KeepCash: 5000, Cash: new(60000.0), Committed: new(0.0), Free: new(40000.0), Blockers: []rpc.TradingBlocker{blocker}},
				{Currency: "USD", State: rpc.CashSweepStateHold, Reason: "no bill is held to sell", Instruments: []string{"us_tbill"}, KeepCash: 5000, Cash: new(-20000.0), Committed: new(0.0)},
			}},
	}
}

// goldenLevelingProposals is a synthetic book (none of it is an account's):
// EUR cash +60,000, USD cash −20,000 at an illustrative EUR.USD of 1.17,
// illustrative statement rates (USD loan 5.1%, EUR cash 1.4%), leveling on
// with the values policy ensure writes and an order cap in force of 50,000
// EUR. The text is the planner's own output for that book.
func goldenLevelingProposals() rpc.TradeProposalSnapshot {
	rate := 1 / 1.17
	return rpc.TradeProposalSnapshot{
		Revision: "rev-0002", PolicyID: "protection", PolicyVersion: 2,
		Counts: rpc.TradeProposalCounts{Total: 1, Actionable: 1, CurrencyLeveling: 1},
		CurrencyLeveling: &rpc.TradeProposalCurrencyLevelingStatus{
			BaseCurrency: "EUR", BalanceSource: rpc.CurrencyLevelingBalanceTradeDate,
			TriggerBase: new(10000.0), CushionBase: new(250.0), MaxSlippageBP: new(2.0), PaybackDays: new(30), OrderCapBase: new(50000.0), Rows: 1,
			RatesSource: rpc.CurrencyLevelingRatesBrokerStatements, RatesThrough: "2026-10-05",
			Currencies: []rpc.TradeProposalCurrencyLevelingCurrency{
				{Currency: "EUR", State: rpc.CurrencyLevelingStatePays, Role: rpc.CurrencyLevelingRolePayer, Reason: "EUR pays about 17,219 EUR of the USD loan, earning 1.40% a year",
					Cash: new(60000.0), CashBase: new(60000.0), ExchangeRate: new(1.0), CashRate: new(0.014), CashRateThrough: "2026-10-05", SpendableBase: new(59750.0), PaysBase: 17219},
				{Currency: "USD", State: rpc.CurrencyLevelingStateConvert, Role: rpc.CurrencyLevelingRoleLoan, Cash: new(-20000.0), CashBase: new(-20000 * rate), ExchangeRate: new(rate),
					LoanRate: new(0.051), LoanRateThrough: "2026-10-05",
					Reason: "USD is borrowed: −20,000 USD (−17,094 EUR), beyond the 10,000 EUR band, costing 5.10% a year; a conversion follows"},
			},
			Bundles: []rpc.TradeProposalCurrencyLevelingBundle{{ID: "currency_leveling-bundle:a97d3c8b4a62526b", Currency: "USD", Keys: []string{"currency_leveling:ddbbabdee052f853"},
				Revision: "0000000000000000000000000000000a", SavingBase: 52.36, CostBase: 5.15, PaybackDays: 30}},
		},
		Proposals: []rpc.TradeProposal{{
			Key: "currency_leveling:ddbbabdee052f853", Bucket: rpc.TradeProposalBucketCurrencyLeveling, Symbol: "EUR.USD", SecType: "CASH",
			Action: rpc.OrderActionSell, Quantity: 17219, MaxQuantity: 17219, PositionEffect: rpc.OrderPositionEffectReduce, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
			Contract: rpc.ContractParams{ConID: 12087792, Symbol: "EUR", SecType: "CASH", Exchange: "IDEALPRO", Currency: "USD", LocalSymbol: "EUR.USD", MinTick: 0.00005},
			Reason:   "Convert 17,219 EUR into about 20,146 USD: USD cash is −20,000 USD, a margin loan beyond the 10,000 EUR band; this brings it to about +146 USD",
			Details: []string{
				"USD trade-date cash −20,000 USD (−17,094 EUR at the ledger rate 0.854701); the repayment lands it between 0 and +293 USD (cushion 250 EUR), never above",
				"EUR trade-date cash +60,000 EUR funds it; this conversion may spend at most 17,347 EUR of it and leaves it about +42,781 EUR",
				"SELL 17,219 EUR.USD on IDEALPRO (contract 12087792), sized at 1.17000 from the ledger rates; the preview reads a live bid and ask and sends a limit at most 2 bp from the mid, which can bring in no more than this conversion's share of the target",
				"the USD loan costs 5.10% a year and EUR cash earns 1.40%, from the broker's statements to 2026-10-05; within 30 days this conversion saves about 52 EUR and costs at most 5 EUR",
				"the loan's conversions together are held to the order cap in force, 50,000 EUR; FX spot settles in two days and the margin interest stops on the settled balance",
				"you approve each conversion; leveling is never pre-authorised",
			},
			NeverSkipVeto: true,
			CurrencyLeveling: &rpc.TradeProposalCurrencyLeveling{BundleID: "currency_leveling-bundle:a97d3c8b4a62526b", Leg: 1, Legs: 1,
				Currency: "USD", FundingCurrency: "EUR", Pair: "EUR.USD", PairSymbol: "EUR", PairCurrency: "USD",
				BalanceSource: rpc.CurrencyLevelingBalanceTradeDate, Cash: -20000, Target: 292.5, Received: 20146.23, FundingCash: 60000, Allotment: 17347.49, Spent: 17219, ExchangeRate: rate, PlanningPrice: 1.17,
				ValueBase: 17219, TriggerBase: 10000, CushionBase: 250, MaxSlippageBP: 2, OrderCapBase: 50000,
				LoanRate: 0.051, LoanRateThrough: "2026-10-05", FundingRate: 0.014, FundingRateThrough: "2026-10-05", PaybackDays: 30, SavingBase: 52.36, CostBase: 5.15},
		}},
	}
}

// goldenLevelingBundleProposals is the same loan beside CHF cash +6,000 at an
// illustrative 0%: CHF pays first, EUR the rest, one approval for both. The
// text is the planner's own output for that book.
func goldenLevelingBundleProposals() rpc.TradeProposalSnapshot {
	rate := 1 / 1.17
	const bundle = "currency_leveling-bundle:d3dea6a8de0b612f"
	leg := func(key, symbol, action string, qty, conID int, contract rpc.ContractParams, reason string, details []string, b rpc.TradeProposalCurrencyLeveling) rpc.TradeProposal {
		b.BundleID, b.Legs, b.Currency, b.BalanceSource, b.Cash, b.ExchangeRate = bundle, 2, "USD", rpc.CurrencyLevelingBalanceTradeDate, -20000, rate
		b.TriggerBase, b.CushionBase, b.MaxSlippageBP, b.OrderCapBase, b.LoanRate, b.LoanRateThrough, b.FundingRateThrough, b.PaybackDays = 10000, 250, 2, 50000, 0.051, "2026-10-05", "2026-10-05", 30
		contract.ConID, contract.SecType, contract.Exchange, contract.MinTick = conID, "CASH", "IDEALPRO", 0.00005
		return rpc.TradeProposal{Key: key, Bucket: rpc.TradeProposalBucketCurrencyLeveling, Symbol: symbol, SecType: "CASH", Action: action, Quantity: qty, MaxQuantity: qty,
			PositionEffect: rpc.OrderPositionEffectReduce, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, Details: details, NeverSkipVeto: true, CurrencyLeveling: &b}
	}
	common := func(own ...string) []string {
		return append([]string{"USD trade-date cash −20,000 USD (−17,094 EUR at the ledger rate 0.854701); the repayment lands it between 0 and +293 USD (cushion 250 EUR), never above"}, own...)
	}
	tail := []string{
		"the loan's conversions together are held to the order cap in force, 50,000 EUR; FX spot settles in two days and the margin interest stops on the settled balance",
	}
	chf := leg("currency_leveling:4d80cd1444744be4", "USD.CHF", rpc.OrderActionBuy, 7192, 12087792, rpc.ContractParams{Symbol: "USD", Currency: "CHF", LocalSymbol: "USD.CHF"},
		"Convert 5,745 CHF into about 7,192 USD (1 of 2 for the USD loan): USD cash is −20,000 USD, a margin loan beyond the 10,000 EUR band; the 2 bring it to about +119 USD",
		append(append(common(
			"CHF trade-date cash +6,000 CHF funds it; this conversion may spend at most 5,766 CHF of it and leaves it about +255 CHF",
			"BUY 7,192 USD.CHF on IDEALPRO (contract 12087792), sized at 0.79879 from the ledger rates; the preview reads a live bid and ask and sends a limit at most 2 bp from the mid, which can bring in no more than this conversion's share of the target",
			"the USD loan costs 5.10% a year and CHF cash earns 0.00%, from the broker's statements to 2026-10-05; within 30 days this conversion saves about 26 EUR and costs at most 3 EUR"), tail...),
			"conversion 1 of 2 repaying the USD loan, cheapest currency first; the 2 are approved and sent together", "you approve the repayment as a whole; leveling is never pre-authorised"),
		rpc.TradeProposalCurrencyLeveling{Leg: 1, FundingCurrency: "CHF", Pair: "USD.CHF", PairSymbol: "USD", PairCurrency: "CHF", Target: -12728.7, Received: 7192, FundingCash: 6000,
			Allotment: 5766.36, Spent: 5744.87, PlanningPrice: 0.79879, ValueBase: 6147.01, SavingBase: 25.77, CostBase: 2.94})
	eur := leg("currency_leveling:14b769cd67fdded3", "EUR.USD", rpc.OrderActionSell, 11049, 12087793, rpc.ContractParams{Symbol: "EUR", Currency: "USD", LocalSymbol: "EUR.USD"},
		"Convert 11,049 EUR into about 12,927 USD (2 of 2 for the USD loan): USD cash is −20,000 USD, a margin loan beyond the 10,000 EUR band; the 2 bring it to about +119 USD",
		append(append(common(
			"EUR trade-date cash +60,000 EUR funds it; this conversion may spend at most 11,131 EUR of it and leaves it about +48,951 EUR",
			"SELL 11,049 EUR.USD on IDEALPRO (contract 12087793), sized at 1.17000 from the ledger rates; the preview reads a live bid and ask and sends a limit at most 2 bp from the mid, which can bring in no more than this conversion's share of the target",
			"the USD loan costs 5.10% a year and EUR cash earns 1.40%, from the broker's statements to 2026-10-05; within 30 days this conversion saves about 34 EUR and costs at most 4 EUR"), tail...),
			"conversion 2 of 2 repaying the USD loan, cheapest currency first; the 2 are approved and sent together", "you approve the repayment as a whole; leveling is never pre-authorised"),
		rpc.TradeProposalCurrencyLeveling{Leg: 2, FundingCurrency: "EUR", Pair: "EUR.USD", PairSymbol: "EUR", PairCurrency: "USD", Target: -6978.8, Received: 12927.33, FundingCash: 60000,
			Allotment: 11131.45, Spent: 11049, PlanningPrice: 1.17, ValueBase: 11049, FundingRate: 0.014, SavingBase: 33.6, CostBase: 3.92})
	return rpc.TradeProposalSnapshot{
		Revision: "rev-0003", PolicyID: "protection", PolicyVersion: 2,
		Counts: rpc.TradeProposalCounts{Total: 2, Actionable: 2, CurrencyLeveling: 2},
		CurrencyLeveling: &rpc.TradeProposalCurrencyLevelingStatus{
			BaseCurrency: "EUR", BalanceSource: rpc.CurrencyLevelingBalanceTradeDate,
			TriggerBase: new(10000.0), CushionBase: new(250.0), MaxSlippageBP: new(2.0), PaybackDays: new(30), OrderCapBase: new(50000.0), Rows: 2,
			RatesSource: rpc.CurrencyLevelingRatesBrokerStatements, RatesThrough: "2026-10-05",
			Currencies: []rpc.TradeProposalCurrencyLevelingCurrency{
				{Currency: "CHF", State: rpc.CurrencyLevelingStatePays, Role: rpc.CurrencyLevelingRolePayer, Reason: "CHF pays about 6,147 EUR of the USD loan, earning 0.00% a year",
					Cash: new(6000.0), CashBase: new(6420.0), ExchangeRate: new(1.07), CashRate: new(0.0), CashRateThrough: "2026-10-05", SpendableBase: new(6170.0), PaysBase: 6147.01},
				{Currency: "EUR", State: rpc.CurrencyLevelingStatePays, Role: rpc.CurrencyLevelingRolePayer, Reason: "EUR pays about 11,049 EUR of the USD loan, earning 1.40% a year",
					Cash: new(60000.0), CashBase: new(60000.0), ExchangeRate: new(1.0), CashRate: new(0.014), CashRateThrough: "2026-10-05", SpendableBase: new(59750.0), PaysBase: 11049},
				{Currency: "USD", State: rpc.CurrencyLevelingStateConvert, Role: rpc.CurrencyLevelingRoleLoan, Cash: new(-20000.0), CashBase: new(-20000 * rate), ExchangeRate: new(rate),
					LoanRate: new(0.051), LoanRateThrough: "2026-10-05",
					Reason: "USD is borrowed: −20,000 USD (−17,094 EUR), beyond the 10,000 EUR band, costing 5.10% a year; 2 conversions follow, approved together"},
			},
			Bundles: []rpc.TradeProposalCurrencyLevelingBundle{{ID: bundle, Currency: "USD", Keys: []string{chf.Key, eur.Key},
				Revision: "0000000000000000000000000000000b", SavingBase: 59.37, CostBase: 6.86, PaybackDays: 30}},
		},
		// The engine serves rows by score and key; the section prints them in
		// the bundle's send order.
		Proposals: []rpc.TradeProposal{eur, chf},
	}
}

// The leveling repayment of goldenLevelingBundleProposals as the bundle
// commands see it: prepared with live quotes, then partly sent. Every value
// is synthetic; the reference and the action id stand in for private ones.
const (
	goldenBundleID       = "currency_leveling-bundle:d3dea6a8de0b612f"
	goldenBundleRevision = "0000000000000000000000000000000b"
	goldenBundleRef      = "canarypb1.U1lOVEhFVElDLVJFRi0wMQ.U1lOVEhFVElDLVJFRi0wMg"
	goldenBundleCHF      = "currency_leveling:4d80cd1444744be4"
	goldenBundleEUR      = "currency_leveling:14b769cd67fdded3"
)

var goldenBundleSubmitInput = `{"bundle_ref":"` + goldenBundleRef + `","bundle_id":"` + goldenBundleID + `","revision":"` + goldenBundleRevision +
	`","terms_digest":"sha256:` + strings.Repeat("ab", 32) + `","confirmation":{"desk_action_id":"desk-action-synthetic","credential":"credential-synthetic","envelope":"envelope-synthetic"}}`

func goldenBundleTerms() rpc.LevelingBundleTerms {
	quoteAt := goldenAt.Add(2 * time.Minute)
	return rpc.LevelingBundleTerms{Kind: rpc.LevelingBundleTermsKind, Version: rpc.LevelingBundleTermsVersion,
		AccountID: "DU0000000", AccountMode: "live", Endpoint: "127.0.0.1:4001", ClientID: 7,
		BundleID: goldenBundleID, Revision: goldenBundleRevision, PreparationID: "U1lOVEhFVElDLVBSRVAtMQ", ExpiresAt: goldenAt.Add(12 * time.Minute),
		BaseCurrency: "EUR", Currency: "USD", Cash: -20000, CashBase: -17094.02, LoanRate: 0.051, LoanRateThrough: "2026-10-05",
		TriggerBase: 10000, CushionBase: 250, Cushion: 292.5, LandsAtLeast: 117.12, SavingBase: 59.37, CostBase: 6.86, PaybackDays: 30, OrderCapBase: 50000,
		Legs: []rpc.LevelingBundleTermsLeg{
			{Leg: 1, Key: goldenBundleCHF, Revision: "sha256:row-chf", PreparationID: "U1lOVEhFVElDLVBSRVAtMg", DraftFingerprint: "fingerprint-chf", PreviewTokenID: "token-chf",
				OrderRef: "canary-20260905-140204-00000001", Pair: "USD.CHF", ConID: 12087792, Exchange: "IDEALPRO", Action: rpc.OrderActionBuy, Quantity: 7192,
				OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: 0.7989, Bid: 0.7987, Ask: 0.7988, QuoteAt: quoteAt, MaxSlippageBP: 2,
				Currency: "USD", Target: -12728.7, FundingCurrency: "CHF", Allotment: 5766.36, KeepsAtLeast: 254.31, FundingRateThrough: "2026-10-05",
				Pays: rpc.LevelingBundleAmount{Amount: 5745.69, Currency: "CHF", Bound: rpc.LevelingBundleBoundAtMost}, Receives: rpc.LevelingBundleAmount{Amount: 7192, Currency: "USD", Bound: rpc.LevelingBundleBoundExact},
				SavingBase: 25.77, CostBase: 2.94},
			{Leg: 2, Key: goldenBundleEUR, Revision: "sha256:row-eur", PreparationID: "U1lOVEhFVElDLVBSRVAtMw", DraftFingerprint: "fingerprint-eur", PreviewTokenID: "token-eur",
				OrderRef: "canary-20260905-140204-00000002", Pair: "EUR.USD", ConID: 12087793, Exchange: "IDEALPRO", Action: rpc.OrderActionSell, Quantity: 11049,
				OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, LimitPrice: 1.1698, Bid: 1.16995, Ask: 1.17005, QuoteAt: quoteAt, MaxSlippageBP: 2,
				Currency: "USD", Target: -6978.8, FundingCurrency: "EUR", Allotment: 11131.45, KeepsAtLeast: 48951, FundingRate: 0.014, FundingRateThrough: "2026-10-05",
				Pays: rpc.LevelingBundleAmount{Amount: 11049, Currency: "EUR", Bound: rpc.LevelingBundleBoundExact}, Receives: rpc.LevelingBundleAmount{Amount: 12925.12, Currency: "USD", Bound: rpc.LevelingBundleBoundAtLeast},
				SavingBase: 33.6, CostBase: 3.92},
		}}
}

func goldenBundlePrepared() rpc.TradeProposalPrepareBundleResult {
	terms := goldenBundleTerms()
	raw, err := json.Marshal(terms)
	if err != nil {
		panic(err)
	}
	return rpc.TradeProposalPrepareBundleResult{Accepted: true, BundleID: goldenBundleID, Revision: goldenBundleRevision, BundleRef: goldenBundleRef,
		ExpiresAt: terms.ExpiresAt, PreparationID: terms.PreparationID, Terms: string(raw), TermsDigest: "sha256:" + strings.Repeat("ab", 32),
		Legs: []rpc.TradeProposalPreviewResult{{Accepted: true}, {Accepted: true}}, AsOf: goldenAt.Add(2 * time.Minute)}
}

func goldenBundlePartlySent() rpc.TradeProposalSubmitBundleResult {
	return rpc.TradeProposalSubmitBundleResult{BundleID: goldenBundleID, Outcome: rpc.BundleOutcomePartlySent, Sent: 1, AsOf: goldenAt.Add(3 * time.Minute),
		Legs: []rpc.TradeProposalSubmitResult{
			{Leg: 1, Key: goldenBundleCHF, Outcome: rpc.BundleOutcomeSent, Accepted: true, OrderRef: "canary-20260905-140204-00000001",
				Proposal: rpc.TradeProposal{Key: goldenBundleCHF, Action: rpc.OrderActionBuy, Quantity: 7192, Symbol: "USD.CHF"}},
			{Leg: 2, Key: goldenBundleEUR, Outcome: rpc.BundleOutcomeRefused,
				Proposal: rpc.TradeProposal{Key: goldenBundleEUR, Action: rpc.OrderActionSell, Quantity: 11049, Symbol: "EUR.USD"},
				Blockers: []rpc.TradingBlocker{{Code: "submit_refused", Message: "Canary refused this conversion while sending it, so it did not reach the broker: trading is frozen"}}},
		},
		Blockers: []rpc.TradingBlocker{{Code: "bundle_partly_sent", Message: "conversion 2 of 2 was refused while sending and did not reach the broker; the one before it was sent, and the rest were not sent",
			Action: "Inspect each conversion's receipt; nothing resends the rest, and the next cycle plans what remains once the ledger shows the fills."}}}
}

func goldenBundleStatus() rpc.TradeProposalPreparedBundleStatusResult {
	return rpc.TradeProposalPreparedBundleStatusResult{BundleID: goldenBundleID, Revision: goldenBundleRevision, PreparationID: "U1lOVEhFVElDLVBSRVAtMQ",
		TermsDigest: "sha256:" + strings.Repeat("ab", 32), ExpiresAt: goldenAt.Add(12 * time.Minute), Outcome: rpc.BundleOutcomePartlySent, Sent: 1,
		SubmittedAt: goldenAt.Add(3 * time.Minute), AsOf: goldenAt.Add(9 * time.Minute),
		Legs: []rpc.TradeProposalBundleLeg{
			{Leg: 1, Key: goldenBundleCHF, Outcome: rpc.BundleOutcomeSent, OrderRef: "canary-20260905-140204-00000001",
				Preparation: &rpc.TradeProposalPreparation{ID: "U1lOVEhFVElDLVBSRVAtMg", State: "consumed", Consumed: new(true)},
				Order:       &rpc.OrderStatusResult{Found: true, Order: rpc.OrderView{OrderRef: "canary-20260905-140204-00000001", LifecycleStatus: rpc.OrderLifecycleSubmitted}}},
			{Leg: 2, Key: goldenBundleEUR, Outcome: rpc.BundleOutcomeRefused, OrderRef: "canary-20260905-140204-00000002",
				Preparation: &rpc.TradeProposalPreparation{ID: "U1lOVEhFVElDLVBSRVAtMw", State: "prepared", Consumed: new(false)}},
		}}
}

func goldenBrief(narrative bool) rpc.BriefResult {
	res := rpc.BriefResult{AsOf: goldenAt, BriefFingerprint: "sha256:0000000000000000000000000000"}
	review, ready := &res.Review, &res.Ready
	review.SessionPnL.Status, review.SessionPnL.EquityBase, review.SessionPnL.DailyPnLBase, review.SessionPnL.BaseCurrency = rpc.BriefStatusOK, new(100000.0), new(-500.0), "USD"
	review.LastSession.Status, review.LastSession.SessionDate, review.LastSession.DailyPnLBase, review.LastSession.BaseCurrency = rpc.BriefStatusOK, "2026-09-04", new(250.0), "USD"
	review.Edge.Status, review.Edge.State, review.Edge.Headline = rpc.BriefStatusOK, "no_decision", "No decision to review yet"
	review.Attribution.Status, review.Attribution.Rows = rpc.BriefStatusOK, []rpc.BriefMover{{Symbol: "SYNA", DailyPnLBase: -400}, {Symbol: "SYNB", DailyPnLBase: -100}}
	review.Rules.Status, review.Rules.Pass, review.Rules.Watch, review.Rules.Act = rpc.BriefStatusAttention, 18, 1, 1
	review.Proposals.Status, review.Proposals.Offered = rpc.BriefStatusOK, 1
	review.Overrides.Status = rpc.BriefStatusOK
	review.CapitalEvents.Status = rpc.BriefStatusOK
	review.Reconcile.Status, review.Reconcile.Detail = rpc.BriefStatusDegraded, "reconciliation source not configured"
	review.AutoExtend.Status = rpc.BriefStatusOK
	review.WorkingOrders.Status, review.WorkingOrders.Count = rpc.BriefStatusOK, new(2)
	ready.Regime.Status, ready.Regime.Stage, ready.Regime.Verdict = rpc.BriefStatusOK, "early_warning", "Watch: one unconfirmed stress signal"
	ready.Breadth.Status, ready.Breadth.PctAbove50DMA, ready.Breadth.PctAbove200DMA = rpc.BriefStatusOK, new(60.0), new(65.0)
	ready.Breadth.MemberCount, ready.Breadth.Coverage50, ready.Breadth.Coverage200, ready.Breadth.CoverageHighsLows = 500, 500, 500, 500
	ready.Gamma.Status, ready.Gamma.Detail = rpc.BriefStatusUnavailable, "dealer gamma model still computing"
	ready.Stress.Status, ready.Stress.Action, ready.Stress.Severity, ready.Stress.Summary = rpc.BriefStatusAttention, "watch", "watch", "One finding needs review."
	ready.Session.Status, ready.Session.Market, ready.Session.State = rpc.BriefStatusOK, "US", "open"
	ready.Capital.Status, ready.Capital.Tier, ready.Capital.ConsumedPct = rpc.BriefStatusOK, "normal", new(10.0)
	ready.Latch.Status = rpc.BriefStatusOK
	ready.PremiumAtRisk.Status, ready.PremiumAtRisk.AmountBase, ready.PremiumAtRisk.BaseCurrency, ready.PremiumAtRisk.PctOfRiskCapital = rpc.BriefStatusOK, new(1000.0), "USD", new(2.0)
	ready.HedgeCost.Status, ready.HedgeCost.AmountBase, ready.HedgeCost.BaseCurrency = rpc.BriefStatusOK, new(10.0), "USD"
	ready.Proposals.Status = rpc.BriefStatusOK
	ready.PolicyDrift.Status = rpc.BriefStatusOK
	if !narrative {
		return res
	}
	run := func(text, role string) rpc.BriefRun { return rpc.BriefRun{Text: text, Role: role} }
	res.Narrative = &rpc.BriefNarrative{
		Overview: &rpc.BriefOverview{
			Assessment: []rpc.BriefRun{run("Markets read constructive with ", ""), run("one unconfirmed stress signal", rpc.BriefRunRoleWatch), run("; the book needs one review before the open.", "")},
			Attention: []rpc.BriefParagraph{
				{Runs: []rpc.BriefRun{run("SYNA concentration is ", ""), run("30.0%", rpc.BriefRunRoleAct), run(" of net liquidation, above the rule's act level; review the position size before adding exposure.", "")}},
				{Runs: []rpc.BriefRun{run("Portfolio stress: ", ""), run("watch", rpc.BriefRunRoleWatch)}},
			},
			Context:  []rpc.BriefParagraph{{Runs: []rpc.BriefRun{run("Breadth ", ""), run("60.0%", rpc.BriefRunRoleFigure), run(" above the 50-day average.", "")}}},
			Coverage: []rpc.BriefParagraph{{Runs: []rpc.BriefRun{run("Dealer gamma unavailable; the model is still computing.", "")}}},
		},
		Lead: []rpc.BriefRun{run("Session P&L ", ""), {Text: "-$500.00", Role: rpc.BriefRunRoleFigure, AccountSensitive: true}, run(" on the day; one finding needs review.", "")},
		Review: []rpc.BriefParagraph{
			{Runs: []rpc.BriefRun{run("SYNA ", ""), run("-$400.00", rpc.BriefRunRoleFigure), run(" and SYNB ", ""), run("-$100.00", rpc.BriefRunRoleFigure), run(" led the move.", "")}},
			{Runs: []rpc.BriefRun{run("Policy adherence: 18 pass, ", ""), run("1 watch", rpc.BriefRunRoleWatch), run(", ", ""), run("1 act", rpc.BriefRunRoleAct), run(".", "")}},
		},
		Ready: []rpc.BriefParagraph{{Runs: []rpc.BriefRun{run("Regime early warning; breadth ", ""), run("60.0%", rpc.BriefRunRoleFigure), run(" above the 50-day average; US session open.", "")}}},
		Coda:  []rpc.BriefRun{run("Read-only; nothing here places an order.", "")},
	}
	return res
}

// goldenSetup is a confirmed volume turn with a known average trade. Clocks are
// UTC so the RFC 3339 stamps read the same in every zone.
func goldenSetup() rpc.SetupResult {
	spike := time.Date(2026, 9, 30, 14, 15, 0, 0, time.UTC)
	confirmed, valid := spike.Add(5*time.Minute), spike.Add(11*time.Minute)
	return rpc.SetupResult{Version: 1, Spec: rpc.SetupSpec{Version: 1, Template: "volume_turn_v1", Revision: "synthetic-3", SpikeMultiple: 3, ResponseBars: 2, BaselineSessions: 20},
		Contract: rpc.ContractParams{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD"}, EvaluatedAt: confirmed.Add(20 * time.Second), EvidenceKind: "current_observation",
		State: "confirmed", Reasons: []string{"volume_spike_price_rising"}, SpikeAt: &spike, FirstConfirmedAt: &confirmed, ConfirmationType: "rising", ValidUntil: &valid, BaselineSessions: 20,
		Features: rpc.SetupFeatures{SpikeMultiple: new(3.4), TradeSize: new(74.2), TradeSizeUsual: new(128.6), TradeSizeBars: 12}}
}
