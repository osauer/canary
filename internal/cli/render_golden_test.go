package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/mandate"
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

// goldenZone is the local zone of every test in this package: a fixed zone
// with the offset and abbreviation the goldens were recorded in. The machine's
// zone would move the hours a renderer prints in UTC and, through the
// abbreviation's width, where lines wrap; CI runs in UTC.
var goldenZone = time.FixedZone("CEST", 2*60*60)

// goldenAt anchors every synthetic timestamp; goldenText writes the zone
// abbreviation as "TZ".
var goldenAt = time.Date(2026, 9, 5, 14, 0, 0, 0, goldenZone)

// TestMain pins time.Local before any test starts, so no goroutine reads it
// while it changes.
func TestMain(m *testing.M) {
	time.Local = goldenZone
	os.Exit(m.Run())
}

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
	// The offline status prints the daemon log path; pin it so the render
	// does not depend on the machine's home or state directory.
	t.Setenv("CANARY_LOG", "/synthetic/state/ibkr/ibkr-daemon.log")
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

// goldenText makes a render machine-independent and reviewable: timestamp zone
// abbreviations become "TZ" and each ESC byte is written as the text \x1b.
// Literal zone names in help text remain part of the output contract.
func goldenText(out string) string {
	if zone := goldenAt.Format("MST"); zone != "" {
		stamp := regexp.MustCompile(`(\b[0-9]{2}:[0-9]{2}(?::[0-9]{2})?) ` + regexp.QuoteMeta(zone) + `\b`)
		out = stamp.ReplaceAllString(out, "${1} TZ")
	}
	return strings.ReplaceAll(out, "\x1b", `\x1b`)
}

func TestGoldenTextPreservesLiteralTimezone(t *testing.T) {
	zone := goldenAt.Format("MST")
	input := "YYYY-MM-DD UTC day; local zone " + zone + "\n" +
		"Observed 5 Sep 14:00 " + zone + "\n" +
		"\x1b[1mUpdated 14:00:05 " + zone + "\x1b[0m\n"
	want := "YYYY-MM-DD UTC day; local zone " + zone + "\n" +
		"Observed 5 Sep 14:00 TZ\n" +
		`\x1b[1mUpdated 14:00:05 TZ\x1b[0m` + "\n"
	if got := goldenText(input); got != want {
		t.Fatalf("normalized output = %q, want %q", got, want)
	}
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
	diagnosticHealth := goldenHealthReady(readyTrading)
	diagnosticHealth.ServerVersion = 203
	diagnosticHealth.GatewayAPI = &rpc.GatewayAPIHealth{TradingProtocolMax: 203, ConfigReadProtocol: 219, ConfigState: "observed", DiagnosticProtocol: 219,
		Settings: &rpc.GatewayAPISettings{ReadOnly: new(false), VolumeInLots: new(false), RetainOrdersOnReconnect: new(true), DownloadOpenOrders: new(true),
			LocalhostOnly: new(true), LoopbackTrusted: new(true), CreateAPILog: new(true), MarketDataInAPILog: new(false), LoggingLevel: new("warning"), SocketPort: new(4002)},
		Precautions: &rpc.GatewayAPIPrecautions{Bypassed: []string{}},
		LockAndExit: &rpc.GatewayLockAndExit{Type: "restart", Time: "11:45", Period: "pm"}}
	diagnosticHealth.GatewaySetup = &rpc.GatewaySetupHealth{State: rpc.GatewaySetupAttention, AsOf: goldenAt.Add(-119 * time.Minute),
		Notes: []string{"Master API client ID is not 7: orders and fills from TWS or other apps stay unseen."}}
	return []renderGoldenCase{
		{name: "recon_comparable", argv: []string{"recon", "show"}, conn: goldenConn{"recon.snapshot": &rpc.ReconResult{AsOf: goldenAt, Status: rpc.ReconStatusActive, Equity: &rpc.ReconEquityCheck{StatementDate: goldenAt, StatementTotalBase: 100000, RuntimeEquityBase: new(98000.0), SameDay: true, Comparability: "comparable", DivergencePct: new(0.0)}}}},
		{name: "recon_basis_pending", argv: []string{"recon", "show"}, conn: goldenConn{"recon.snapshot": &rpc.ReconResult{AsOf: goldenAt, Status: rpc.ReconStatusActive, Equity: &rpc.ReconEquityCheck{StatementDate: goldenAt, StatementTotalBase: 100000, RuntimeEquityBase: new(98000.0), SameDay: true, Comparability: "runtime_basis_missing"}}}},
		{name: "update-trading-refusal", argv: []string{"update"}, render: func(env *Env) { renderTradingUpdateRefusal(env.Stdout) }},
		{name: "help", argv: []string{"help"}, render: renderUsage},
		{name: "help_add", argv: []string{"add", "--help"}},
		{name: "help_add_market", argv: []string{"add", "market", "--help"}},
		{name: "help_add_call_plan", argv: []string{"add", "call-plan", "--help"}},
		{name: "help_order", argv: []string{"order", "--help"}},
		{name: "help_order_preview", argv: []string{"order", "preview", "--help"}},
		{name: "help_orders_history", argv: []string{"orders", "history", "--help"}},
		{name: "help_proposals", argv: []string{"proposals", "--help"}},
		{name: "help_proposals_queue", argv: []string{"proposals", "queue", "--help"}},
		{name: "order_preview_ineligible", argv: []string{"order", "preview"}, render: func(env *Env) {
			r := goldenBondPreview()
			r.SubmitEligible = false
			r.TokenMinted = true
			r.WhatIf.Status = rpc.OrderWhatIfStatusUnavailable
			r.WhatIf.RequiredForSubmit = true
			renderOrderPreviewText(env, &r)
		}},
		{name: "stress_undated", argv: []string{"stress"}, render: func(env *Env) { r := goldenStress(); r.AsOf = time.Time{}; renderStress(env, r, false) }},
		{name: "status_gateway_diagnostics", argv: []string{"status", "--gateway-diagnostics"}, conn: goldenConn{rpc.MethodStatusHealth: diagnosticHealth}},
		{name: "portfolio_plan", argv: []string{"portfolio", "plan"}, conn: goldenConn{rpc.MethodPortfolioPlan: goldenPortfolioPlan()}},
		{name: "portfolio_plan_held", argv: []string{"portfolio", "plan"}, conn: goldenConn{rpc.MethodPortfolioPlan: rpc.PortfolioPlanResult{State: "held", Reason: "Current portfolio evidence is incomplete.", Blockers: []rpc.TradingBlocker{{Code: "portfolio_book_unavailable", Message: "A complete same-session account and portfolio are required."}}}}},
		{name: "add_plan", argv: []string{"add", "plan", "SYNA", "--currency", "USD", "--limit", "100", "--max"}, conn: goldenConn{rpc.MethodAddPlan: goldenStockAdd()}},
		{name: "add_fixed_stop", render: func(env *Env) { p := goldenStockAdd(); p.StopPrice = 95; renderAddPlan(env, p) }},
		{name: "add_pair_status", render: func(env *Env) {
			renderOrderStatusText(env, &rpc.OrderStatusResult{Found: true, Order: rpc.OrderView{OrderRef: "synthetic-entry", Symbol: "SYNA", Action: "BUY", Quantity: 10, Filled: 10, LifecycleStatus: rpc.OrderLifecycleFilled, AttachedStop: &rpc.AttachedStockStop{Price: 95}}, AttachedStop: &rpc.OrderView{OrderRef: "synthetic-stop", Remaining: 10, FixedStopPrice: 95, EntryState: "activation_pending"}}, "synthetic-entry")
		}},
		{name: "add_held", render: func(env *Env) {
			p := goldenStockAdd()
			p.Quantity, p.MaxQuantity, p.OrderUpperBound, p.MaximumKnown = 0, 0, 0, false
			p.After, p.StockPctAfter, p.UnderlyingPctAfter = p.Before, p.StockPctBefore, p.UnderlyingPctBefore
			p.Cash.Available, p.Cash.Spendable = 695, 95
			p.Allowances[0] = risk.StockAddAllowance{Code: "cash", Limit: 95, Remaining: 95, Shares: 0}
			p.Protection.UncoveredAfter = p.Protection.UncoveredBefore
			p.RiskChecks[0].AfterEvidence, p.RiskChecks[0].AfterStatus = "", ""
			p.Blockers = []risk.StockAddBlocker{{Kind: "capacity", Code: "add_no_capacity", Message: "The available allowance cannot fund one whole share."}}
			renderAddPlan(env, p)
		}},
		{name: "regime", argv: []string{"regime"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(fresh)}},
		{name: "regime_explain", argv: []string{"regime", "--explain"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(fresh)}},
		{name: "regime_stale", argv: []string{"regime"}, conn: goldenConn{rpc.MethodRegimeSnapshot: goldenRegime(stale)}},
		// runStress derives its result from account, positions, regime and
		// rulebook reads; the golden pins the renderer it hands the result to.
		{name: "stress", argv: []string{"stress"}, render: func(env *Env) { renderStress(env, goldenStress(), false) }},
		{name: "scorecard", argv: []string{"scorecard"}, conn: goldenConn{rpc.MethodScorecardRead: goldenScorecard()}},
		{name: "scorecard_backfilling", argv: []string{"scorecard"}, conn: goldenConn{rpc.MethodScorecardRead: goldenScorecardBackfilling()}},
		{name: "stress_details", argv: []string{"stress", "--details"}, render: func(env *Env) { renderStress(env, goldenStress(), true) }},
		{name: "mandate", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandate(mandate.StatusWithin)}},
		{name: "mandate_reduce", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateReducing()}},
		{name: "mandate_allocations", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateAllocations()}},
		{name: "mandate_owner_stop", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateOwnerStop()}},
		{name: "reporting_costs", argv: []string{"reporting", "costs"}, conn: goldenConn{rpc.MethodReportingCosts: goldenReportingCosts()}},
		{name: "reporting_costs_empty", argv: []string{"reporting", "costs", "--month", "2026-08"}, conn: goldenConn{rpc.MethodReportingCosts: goldenReportingCostsEmpty()}},
		{name: "mandate_register", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateRegister()}},
		{name: "mandate_unknown", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateUnknown()}},
		{name: "mandate_iv_fallback", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateIVFallback()}},
		{name: "mandate_unknown_held", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateHeld()}},
		{name: "mandate_no_room", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateNoRoom()}},
		{name: "mandate_exposure", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: goldenMandateExposure()}},
		{name: "mandate_inactive", argv: []string{"mandate"}, conn: goldenConn{rpc.MethodMandateRisk: rpc.MandateRiskResult{AsOf: goldenAt, Message: "the risk constitution carries no [mandate] table; the mandate budget is inactive", Gate: &rpc.MandateGateView{}}}},
		{name: "core", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCore()}},
		{name: "core_hold", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCoreHold()}},
		{name: "core_trend_off", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCoreTrendOff()}},
		{name: "core_ambiguous", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCoreAmbiguous()}},
		{name: "core_london", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCoreLondon()}},
		{name: "core_shadow", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: goldenCoreShadow()}},
		{name: "strategy_shadow", argv: []string{"strategy", "trend-fund"}, conn: goldenConn{rpc.MethodStrategyTarget: goldenStrategyShadow()}},
		{name: "strategy_hold", argv: []string{"strategy", "trend-fund"}, conn: goldenConn{rpc.MethodStrategyTarget: goldenStrategyHold()}},
		{name: "strategy_live", argv: []string{"strategy", "trend-fund"}, conn: goldenConn{rpc.MethodStrategyTarget: goldenStrategyLive()}},
		{name: "strategy_signal", argv: []string{"strategy", "trend-fund"}, conn: goldenConn{rpc.MethodStrategyTarget: goldenStrategySignal()}},
		{name: "strategy_replay", argv: []string{"strategy", "core", "--replay", "2025-01-01", "2026-09-30"}, conn: goldenConn{rpc.MethodStrategyReplay: goldenStrategyReplay()}},
		{name: "core_resolve", argv: []string{"core", "resolve"}, conn: goldenConn{rpc.MethodCoreResolve: goldenCoreResolve()}},
		{name: "core_resolve_pinned", argv: []string{"core", "resolve"}, conn: goldenConn{rpc.MethodCoreResolve: goldenCoreResolvePinned()}},
		{name: "core_resolve_none", argv: []string{"core", "resolve", "--isin", "IE00B5BMR087", "--currency", "EUR"}, conn: goldenConn{rpc.MethodCoreResolve: goldenCoreResolveNone()}},
		{name: "core_inactive", argv: []string{"core"}, conn: goldenConn{rpc.MethodCoreTarget: rpc.CoreTargetResult{AsOf: goldenAt, Status: rpc.CoreStatusInactive, ReadOnly: true, Message: "the risk constitution carries no [mandate.core] table; the core sleeve is inactive"}}},
		{name: "strategies_list", argv: []string{"strategies", "list"}, conn: goldenConn{rpc.MethodPositionsList: strategiesAuthorityCases()[6].res}},
		{name: "strategies_list_empty", argv: []string{"strategies", "list"}, conn: goldenConn{rpc.MethodPositionsList: strategiesAuthorityCases()[5].res}},
		{name: "strategies_list_unavailable", argv: []string{"strategies", "list"}, conn: goldenConn{rpc.MethodPositionsList: strategiesAuthorityCases()[0].res}},
		{name: "strategies_list_stale", argv: []string{"strategies", "list"}, conn: goldenConn{rpc.MethodPositionsList: strategiesAuthorityCases()[4].res}},
		{name: "status", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: goldenHealthReady(readyTrading), rpc.MethodAlertCandidates: goldenAlerts(3)}},
		{name: "status_attention", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: goldenHealthAttention(), rpc.MethodAlertCandidates: goldenAlerts(2)}},
		{name: "status_offline_no_gateway", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: statusOfflineCases()[0].res}, exit: 1},
		{name: "status_offline_refused_pinned", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: statusOfflineCases()[1].res}, exit: 1},
		{name: "status_offline_rejecting", argv: []string{"status"}, conn: goldenConn{rpc.MethodStatusHealth: statusOfflineCases()[4].res}, exit: 1},
		{name: "trading_status", argv: []string{"trading", "status"}, conn: goldenConn{rpc.MethodTradingStatus: readyTrading}},
		{name: "trading_status_blocked", argv: []string{"trading", "status"}, conn: goldenConn{rpc.MethodTradingStatus: blockedTrading}, exit: 1},
		{name: "settings_show", argv: []string{"settings", "show"}, conn: goldenConn{rpc.MethodSettingsGet: goldenSettings()}},
		{name: "chains_recorded", argv: []string{"chains", "recorded"}, conn: goldenConn{rpc.MethodChainsRecorded: goldenChainsRecorded()}},
		{name: "chains_recorded_off", argv: []string{"chains", "recorded"}, conn: goldenConn{rpc.MethodChainsRecorded: goldenChainsOff()}},
		{name: "chains_recorded_csv", argv: []string{"chains", "recorded", "--date", "2026-10-14", "--csv"}, conn: goldenChainsCSV()},
		{name: "technical", argv: []string{"technical", "SYNA,SYNB,SYNC"}, conn: goldenConn{rpc.MethodTechnical: goldenTechnical()}},
		{name: "proposals_list_empty", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: rpc.TradeProposalSnapshot{Revision: "rev-0000", PolicyID: "protection", PolicyVersion: 1, Proposals: []rpc.TradeProposal{}}}},
		{name: "proposals_list_one", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenProposals()}},
		{name: "proposals_list_mandate", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenMandateProposals()}},
		{name: "proposals_list_mandate_details", argv: []string{"proposals", "list", "--details"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenMandateProposals()}},
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
		{name: "proposals_reduce_preview", argv: []string{"proposals", "reduce", "SYNA", "--percent", "50"}, conn: goldenConn{rpc.MethodTradeProposalsReducePreview: goldenReducePreviewResult()}},
		{name: "proposals_reduce_submit_token", argv: []string{"proposals", "reduce", "--submit", "--preview-token", goldenReduceToken, "--request-ref", "desk-7f3a"},
			conn: goldenConn{rpc.MethodTradeProposalsReduceSubmit: goldenReduceSubmitResult()}},
		{name: "proposals_reduce_submit_refused", argv: []string{"proposals", "reduce", "--submit", "--preview-token", goldenReduceToken, "--request-ref", "desk-7f3a"},
			conn: goldenConn{rpc.MethodTradeProposalsReduceSubmit: goldenReduceSubmitRefused()}},
		{name: "proposals_reduce_portfolio_preview", argv: []string{"proposals", "reduce", "--portfolio", "--percent", "50"}, conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioPreview: goldenReducePortfolioPreview()}},
		{name: "proposals_reduce_portfolio_plan_proposals", argv: []string{"proposals", "reduce", "--portfolio", "--plan", "--proposal", "rr-SYNA", "--proposal", "rr-SYNC"}, conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioPreview: goldenReducePortfolioPlanProposals()}},
		{name: "proposals_reduce_portfolio_plan_longs", argv: []string{"proposals", "reduce", "--portfolio", "--mode", "longs", "--percent", "25", "--plan"}, conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioPreview: goldenReducePortfolioPlanLongs()}},
		{name: "proposals_reduce_portfolio_plan_hedge", argv: []string{"proposals", "reduce", "--portfolio", "--mode", "hedge", "--plan"}, conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioPreview: goldenReducePortfolioPlanHedge()}},
		{name: "proposals_reduce_portfolio_plan_hedge_none", argv: []string{"proposals", "reduce", "--portfolio", "--mode", "hedge", "--plan"}, conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioPreview: goldenReducePortfolioPlanHedgeNone()}},
		{name: "exercises_pending", argv: []string{"opportunities", "exercises"}, conn: goldenConn{rpc.MethodOpportunitiesExercises: goldenPendingExercises()}},
		{name: "exercises_empty", argv: []string{"opportunities", "exercises"}, conn: goldenConn{rpc.MethodOpportunitiesExercises: rpc.ExercisePendingResult{Requests: []rpc.ExercisePendingRequest{}}}},
		{name: "exercise_reconciliation", argv: []string{"opportunities", "reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete"}, render: func(env *Env) { renderExerciseReconciliation(env, goldenExerciseResolution(false)) }},
		{name: "exercise_reconciliation_replay", argv: []string{"opportunities", "reconcile-exercise", "synthetic-request", "revision-one", "--evidence", "synthetic-evidence", "--confirm-complete"}, render: func(env *Env) { renderExerciseReconciliation(env, goldenExerciseResolution(true)) }},
		{name: "help_opportunities_exercises", argv: []string{"opportunities", "exercises", "--help"}},
		{name: "help_opportunities_reconcile_exercise", argv: []string{"opportunities", "reconcile-exercise", "--help"}},
		{name: "locks_list", argv: []string{"locks"}, conn: goldenConn{rpc.MethodHoldingLocksList: rpc.HoldingLocks{Version: 1, Revision: 3, AsOf: goldenAt, Locks: []rpc.HoldingLock{
			{Symbol: "SYNA", Role: rpc.HoldingLockRoleCore, Contracts: []int{goldenReduceStockCon, goldenReduceOptionCon}, Held: new(true)},
			{ConID: goldenReduceOptionCon, Symbol: "SYNB", Role: rpc.HoldingLockRoleHedge},
			{ConID: 4242, Symbol: "SYNG", Role: rpc.HoldingLockRoleCore, Held: new(false)}}}}},
		{name: "proposals_list_index_loss", argv: []string{"proposals", "list"}, conn: goldenConn{rpc.MethodTradeProposalsSnapshot: goldenProposalsIndexLoss()}},
		{name: "locks_list_empty", argv: []string{"locks"}, conn: goldenConn{rpc.MethodHoldingLocksList: rpc.HoldingLocks{Version: 1, AsOf: goldenAt, Locks: []rpc.HoldingLock{}}}},
		{name: "proposals_reduce_portfolio_submit_tokens", argv: []string{"proposals", "reduce", "--portfolio", "--submit", "--preview-token", goldenReduceToken, "--preview-token", goldenReduceTokenB, "--request-ref", "desk-basket-7f3a"},
			conn: goldenConn{rpc.MethodTradeProposalsReducePortfolioSubmit: goldenReducePortfolioSubmit()}},
		{name: "brief", argv: []string{"brief"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(true)}},
		{name: "setups_evaluate_confirmed", argv: []string{"setups", "evaluate"}, render: func(env *Env) { renderSetupResult(env, goldenSetup()) }},
		{name: "brief_details", argv: []string{"brief", "--details"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(true)}},
		{name: "brief_rows", argv: []string{"brief"}, conn: goldenConn{rpc.MethodBriefSnapshot: goldenBrief(false)}},
		{name: "order_preview_bond", argv: []string{"order", "preview", "buy", "DE000SYN0000", "10000", "--type", "BOND", "--currency", "EUR"}, conn: goldenConn{rpc.MethodOrderPreview: goldenBondPreview()}},
		{name: "positions_bond_risk", argv: []string{"positions"}, render: func(env *Env) {
			bonds, book := goldenBondRisk()
			renderBondsTable(env, env.Stdout, bonds)
			renderBondRisk(env, env.Stdout, bonds, book)
		}},
		{name: "positions_bond_facts", argv: []string{"positions"}, render: func(env *Env) { renderBondFacts(env, env.Stdout, goldenBondFacts()) }},
		{name: "edge", argv: []string{"edge"}, conn: goldenConn{rpc.MethodEdgeSnapshot: goldenEdge(0, 3)}},
		{name: "edge_option_page", argv: []string{"edge", "--option-offset", "3", "--option-limit", "3"}, conn: goldenConn{rpc.MethodEdgeSnapshot: goldenEdge(3, 3)}},
		{name: "edge_replay", argv: []string{"edge", "replay", "--from", "2026-08-03"}, conn: goldenConn{rpc.MethodEdgeReplay: goldenEdgeReplay()}},
		{name: "edge_replay_incomplete", argv: []string{"edge", "replay", "--from", "2026-08-03"}, conn: goldenConn{rpc.MethodEdgeReplay: goldenEdgeReplayIncomplete()}},
		{name: "edge_replay_unreconciled", argv: []string{"edge", "replay", "--from", "2026-08-03"}, conn: goldenConn{rpc.MethodEdgeReplay: goldenEdgeReplayUnreconciled()}},
	}
}

// goldenEdge is a synthetic Edge review whose option lists are one page of
// rows [offset, offset+limit): completed positions closed by trade, expiry
// and assignment, realized episodes with one row lacking a P/L, and open
// positions. The counts and totals cover every row, as the daemon sends them.
// goldenEdgeReplay is a synthetic exit replay over two underlyings: the
// actual baseline, the four sets and one single rule on recorded prices, and
// set B on modelled prices.
func goldenEdgeReplay() rpc.EdgeReplayResult {
	day := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	run := func(id, prices string, pnl, diff float64, exits int, reasons []rpc.EdgeReplayReason, timing rpc.EdgeReplayTiming, peak, pct float64, cov rpc.EdgeReplayCoverage, syna, synb float64) rpc.EdgeReplayRun {
		return rpc.EdgeReplayRun{ID: id, Rules: rpc.EdgeReplaySetRules(id), Prices: prices, PNLBase: pnl, DiffVsActualBase: diff, Exits: exits, ExitsByReason: reasons,
			VsOwner: timing, PeakValueBase: peak, PeakDate: day("2026-08-20"), PeakNLVBase: new(200000.0), PeakPctNLV: new(pct), Coverage: cov,
			ByUnderlying: []rpc.EdgeReplayNameRow{{Underlying: "SYNA", PNLBase: syna}, {Underlying: "SYNB", PNLBase: synb}}}
	}
	recorded := rpc.EdgeReplayCoverage{StatementDays: 40, TradeCloseDays: 6, UntestableEvents: 2, UnitsKeptActual: 12}
	return rpc.EdgeReplayResult{
		SchemaVersion: rpc.EdgeReplaySchemaVersion, State: rpc.EdgeStateCurrent, From: day("2026-08-03"), To: day("2026-08-28"), StartStateDate: day("2026-07-31"),
		BaseCurrency: "EUR", StatementDays: 20, NotExecution: true,
		Parameters: rpc.EdgeReplayParameters{
			Rules: []rpc.EdgeReplayParameter{
				{Name: "loss_exit_pct", Value: 60}, {Name: "expiry_close_dte", Value: 7}, {Name: "profit_arm_gain_pct", Value: 50}, {Name: "profit_trail_pct", Value: 30},
				{Name: "locked_gain_pct", Value: 5}, {Name: "profit_trail_min_dte", Value: 14}, {Name: "min_trail_abs", Value: 0.1}, {Name: "recover_cost_gain_pct", Value: 100},
				{Name: "option_name_act_pct", Value: 15}, {Name: "stock_trail_range_multiple", Value: 2.5}, {Name: "stock_trail_atr_sessions", Value: 20},
			},
			HalfSpreadPct: new(0.2), HalfSpreadSamples: 12, HalfSpreadSource: "median of 12 measured option fills", CommissionPerContractBase: new(0.55), CommissionContracts: 400,
			PolicySource: "Rulebook rulebook-default v1 (compiled-default) · protection protection-mvp v3",
		},
		Scope: rpc.EdgeReplayScope{Contracts: 4, Underlyings: []string{"SYNA", "SYNB"}, BuyLots: 6, PreheldLots: 1, ContractsBought: 120, ExcludedHedgeContracts: 2, ExcludedHedgeExecutions: 5, ExcludedOther: map[string]int{"instrument_unavailable": 1}},
		Runs: []rpc.EdgeReplayRun{
			run("A", rpc.EdgeReplayPricesActual, 4200, 0, 5, []rpc.EdgeReplayReason{{Reason: "sold", Events: 4, Units: 100}, {Reason: "expiry", Events: 1, Units: 10}}, rpc.EdgeReplayTiming{Same: 130}, 30000, 15, rpc.EdgeReplayCoverage{}, 3000, 1200),
			run("B", rpc.EdgeReplayPricesRecorded, 1500, -2700, 3, []rpc.EdgeReplayReason{{Reason: "loss_exit", Events: 2, Units: 30}, {Reason: "expiry_close", Events: 1, Units: 10}}, rpc.EdgeReplayTiming{Earlier: 20, Later: 60, Same: 38, Untestable: 12}, 52000, 26, recorded, 900, 600),
			run("C", rpc.EdgeReplayPricesRecorded, 4800, 600, 7, []rpc.EdgeReplayReason{{Reason: "loss_exit", Events: 2, Units: 30}, {Reason: "trim_to_cap", Events: 4, Units: 25}, {Reason: "recover_cost", Events: 1, Units: 5}}, rpc.EdgeReplayTiming{Earlier: 45, Later: 40, Same: 33, Untestable: 12}, 30500, 15.2, recorded, 3500, 1300),
			run("D", rpc.EdgeReplayPricesRecorded, 2100, -2100, 4, []rpc.EdgeReplayReason{{Reason: "loss_exit", Events: 2, Units: 30}, {Reason: "stock_trail", Events: 2, Units: 20}}, rpc.EdgeReplayTiming{Earlier: 30, Later: 50, Same: 38, Untestable: 12}, 52000, 26, recorded, 1400, 700),
			run("loss_exit", rpc.EdgeReplayPricesRecorded, -800, -5000, 2, []rpc.EdgeReplayReason{{Reason: "loss_exit", Events: 2, Units: 30}}, rpc.EdgeReplayTiming{Earlier: 30, Later: 70, Same: 18, Untestable: 12}, 61000, 30.5, recorded, -500, -300),
			run("B", rpc.EdgeReplayPricesModelled, 2600, -1600, 4, []rpc.EdgeReplayReason{{Reason: "loss_exit", Events: 2, Units: 30}, {Reason: "profit_trail", Events: 2, Units: 15}}, rpc.EdgeReplayTiming{Earlier: 20, Later: 98, Same: 12}, 58000, 29, rpc.EdgeReplayCoverage{StatementDays: 40, TradeCloseDays: 6, ModelledDays: 18}, 1600, 1000),
		},
		Reconciliation: rpc.EdgeReplayReconciliation{
			ActualUnitsBase: 4200, ActualCashBase: 4200, FlowsBase: -3800, StartValueBase: 2000, EndValueBase: 10000, BrokerRealizedBase: new(3900.0),
			EdgeCycles: 3, EdgeCycleContracts: 3, ComparedContracts: 2, EdgeCyclesPNLBase: 2500, ActualOnCyclesBase: 2450, FXTranslationBase: -50, Reconciled: true,
			Status: rpc.EdgeReplayReconciledPartly, NotCompared: []rpc.EdgeReplayReconContract{{Underlying: "SYNB", Contract: "SYNB 2026-09-18 90 P", Reason: rpc.EdgeReplayNotComparedExercised}},
			ResidualContracts: []rpc.EdgeReplayReconContract{}, ReconciliationNotes: []string{},
		},
	}
}

// goldenEdgeReplayIncomplete is goldenEdgeReplay with four underlyings left
// out for want of daily history (one still arriving from the broker), and
// set D's stock trail measured partly from closes only.
func goldenEdgeReplayIncomplete() rpc.EdgeReplayResult {
	day := func(s string) time.Time { v, _ := time.Parse(time.DateOnly, s); return v }
	result := goldenEdgeReplay()
	result.Incomplete = true
	result.MissingHistory = []rpc.EdgeReplayMissingHistory{
		{Underlying: "SYNC", Reason: rpc.EdgeReplayHistoryGatewayOffline, Detail: "no retained history", NeededFrom: day("2026-08-03"), NeededTo: day("2026-08-28"), Contracts: 2, ActualPNLBase: -1800},
		{Underlying: "SYND", Reason: rpc.EdgeReplayHistoryGatewayOffline, Detail: "history from 2026-05-04 to 2026-08-14", NeededFrom: day("2026-08-05"), NeededTo: day("2026-08-28"), HistoryFrom: day("2026-05-04"), HistoryTo: day("2026-08-14"), Contracts: 1, ActualPNLBase: 650},
		{Underlying: "SYNE", Reason: rpc.EdgeReplayHistoryInstrumentUnavailable, Detail: "no security definition for contract", NeededFrom: day("2026-08-10"), NeededTo: day("2026-08-21"), Contracts: 1, ActualPNLBase: -120},
		{Underlying: "SYNF", Reason: rpc.EdgeReplayHistoryFetching, Detail: "the broker is still sending it; no retained history", NeededFrom: day("2026-08-12"), NeededTo: day("2026-08-28"), Contracts: 1, ActualPNLBase: 75},
	}
	result.Runs[3].Coverage.CloseOnlyRangeDays, result.Runs[3].Coverage.TrailUnmeasuredDays = 14, 3
	for i := 1; i <= 4; i++ {
		result.Runs[i].Coverage.ClosedSessionDays = 6
	}
	return result
}

// goldenEdgeReplayUnreconciled is goldenEdgeReplay with one compared
// contract whose cash flows differ from the broker's realized P/L.
func goldenEdgeReplayUnreconciled() rpc.EdgeReplayResult {
	result := goldenEdgeReplay()
	rec := &result.Reconciliation
	rec.Reconciled, rec.Status, rec.CycleCashResidual = false, rpc.EdgeReplayUnreconciled, -18
	rec.ResidualContracts = []rpc.EdgeReplayReconContract{{Underlying: "SYNA", Contract: "SYNA 2026-09-18 120 C", Reason: rpc.EdgeReplayResidualCashVsRealized}}
	return result
}

func goldenEdge(offset, limit int) rpc.EdgeResult {
	result := edgeCLIResult()
	at := result.AsOf
	money := func(v float64) *float64 { return &v }
	cycles := []rpc.EdgeOptionCycle{
		{ID: "option-cycle_a1", Symbol: "SYNA  260821C00115000", Direction: "short", OpenedAt: at.AddDate(0, -2, 0), ClosedAt: at.AddDate(0, 0, -3), ClosedBy: "assignment", ExecutionCount: 2, RealizedPNLBase: money(4890), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option-cycle_a2", Symbol: "SYNB  260918P00090000", Direction: "long", OpenedAt: at.AddDate(0, -1, 0), ClosedAt: at.AddDate(0, 0, -5), ClosedBy: "expiry", ExecutionCount: 3, RealizedPNLBase: money(-1250), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option-cycle_a3", Symbol: "SYNC  261016C00050000", Direction: "long", OpenedAt: at.AddDate(0, -1, 0), ClosedAt: at.AddDate(0, 0, -8), ClosedBy: "trade", ExecutionCount: 2, RealizedPNLBase: money(830), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option-cycle_a4", Symbol: "SYNA  260821P00100000", Direction: "long", OpenedAt: at.AddDate(0, -1, -5), ClosedAt: at.AddDate(0, 0, -10), ClosedBy: "exercise", ExecutionCount: 2, RealizedPNLBase: money(-310), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option-cycle_a5", Symbol: "SYND  260918C00020000", Direction: "short", OpenedAt: at.AddDate(0, -1, -9), ClosedAt: at.AddDate(0, 0, -12), ClosedBy: "trade", ExecutionCount: 2, RealizedPNLBase: money(120), PNLStatus: "complete", MissingEvidence: []string{}},
	}
	leg := func(symbol string, strike float64, putCall string) []rpc.EdgeOptionLegIdentity {
		return []rpc.EdgeOptionLegIdentity{{Symbol: symbol, Underlying: symbol[:4], Expiry: "2026-09-18", Strike: &strike, PutCall: putCall}}
	}
	episodes := []rpc.EdgeOptionEpisodeSummary{
		{ID: "option_e1", Grouping: "exact_order", Lifecycle: "closing", Underlying: "SYNA", ActivityFrom: at.AddDate(0, 0, -3), ActivityTo: at.AddDate(0, 0, -3), RealizedPNLBase: money(4890), PNLStatus: "complete", MissingEvidence: []string{}, Legs: leg("SYNA CALL", 115, "call")},
		{ID: "option_e2", Grouping: "exact_order", Lifecycle: "closing", Underlying: "SYNB", ActivityFrom: at.AddDate(0, 0, -5), ActivityTo: at.AddDate(0, 0, -5), RealizedPNLBase: money(-1250), PNLStatus: "complete", MissingEvidence: []string{}, Legs: leg("SYNB PUT", 90, "put")},
		{ID: "option_e3", Grouping: "exact_order", Lifecycle: "closing", Underlying: "SYNC", ActivityFrom: at.AddDate(0, 0, -8), ActivityTo: at.AddDate(0, 0, -8), RealizedPNLBase: money(830), PNLStatus: "complete", MissingEvidence: []string{}, Legs: leg("SYNC CALL", 50, "call")},
		{ID: "option_e4", Grouping: "unlinked_execution", Lifecycle: "closing", Underlying: "SYND", ActivityFrom: at.AddDate(0, 0, -12), ActivityTo: at.AddDate(0, 0, -12), RealizedPNLBase: money(120), PNLStatus: "complete", MissingEvidence: []string{}, Legs: leg("SYND CALL", 20, "call")},
		{ID: "option_e5", Grouping: "unlinked_execution", Lifecycle: "closing", Underlying: "SYNE", ActivityFrom: at.AddDate(0, 0, -14), ActivityTo: at.AddDate(0, 0, -14), PNLStatus: "unavailable", MissingEvidence: []string{"realized_pnl"}, Legs: leg("SYNE PUT", 30, "put")},
	}
	positions := []rpc.EdgeOptionOpenPositionSummary{
		{ID: "option_o1", Symbol: "SYNA CALL", Underlying: "SYNA", SnapshotDate: at, Expiry: "2026-12-18", Strike: money(120), PutCall: "call", OpenPNLBase: money(-640), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option_o2", Symbol: "SYNB PUT", Underlying: "SYNB", SnapshotDate: at, Expiry: "2026-12-18", Strike: money(85), PutCall: "put", OpenPNLBase: money(410), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option_o3", Symbol: "SYNC CALL", Underlying: "SYNC", SnapshotDate: at, Expiry: "2027-01-15", Strike: money(55), PutCall: "call", OpenPNLBase: money(-90), PNLStatus: "complete", MissingEvidence: []string{}},
		{ID: "option_o4", Symbol: "SYND PUT", Underlying: "SYND", SnapshotDate: at, Expiry: "2027-01-15", Strike: money(18), PutCall: "put", OpenPNLBase: money(35), PNLStatus: "complete", MissingEvidence: []string{}},
	}
	page := func(total int) (int, int, *int) {
		start, end := min(offset, total), min(total, offset+limit)
		if end < total {
			return start, end, &end
		}
		return start, end, nil
	}
	options := rpc.EdgeOptionReview{
		Coverage: rpc.EdgeOptionCoverage{ExecutionEpisodes: 5, ClosingEpisodes: 5},
		Cycles:   rpc.EdgeOptionCycles{CompletedCount: 5, CompletePNLCount: 5, OpenContractCount: 4, ExcludedContracts: 1, Reasons: map[string]int{"opened_before_window": 2, "lifecycle_or_transfer": 1}, KnownPNLBase: money(4280), Offset: offset},
		Realized: rpc.EdgeOptionRealizedReview{KnownPNLBase: money(4590), PositiveCount: 3, NegativeCount: 1, CompleteCount: 4, UnavailableCount: 1, TotalCount: 5, Offset: offset},
		Open:     rpc.EdgeOptionOpenReview{SnapshotDate: at, KnownPNLBase: money(-285), PositiveCount: 2, NegativeCount: 2, CompleteCount: 4, TotalCount: 4, Offset: offset},
	}
	start, end, next := page(len(cycles))
	options.Cycles.Cycles, options.Cycles.NextOffset, options.Cycles.Truncated = cycles[start:end], next, end-start < len(cycles)
	start, end, next = page(len(episodes))
	options.Realized.Episodes, options.Realized.NextOffset, options.Realized.Truncated = episodes[start:end], next, end-start < len(episodes)
	start, end, next = page(len(positions))
	options.Open.Positions, options.Open.NextOffset, options.Open.Truncated = positions[start:end], next, end-start < len(positions)
	result.Options = options
	return result
}

// goldenBondFacts is a synthetic held bill book with the console's facts
// (bond-risk.md, "Bill and bond facts"): a US bill bought through Canary
// with its gain split and no spread, a French bill against a German
// benchmark whose spread part runs from a later record, and a bill bought
// elsewhere whose quote is still on its way.
func goldenBondFacts() []rpc.PositionBond {
	quoteAt := time.Date(2026, 10, 12, 9, 30, 0, 0, time.UTC)
	usBill := &rpc.BondFacts{Currency: "USD", PriceConvention: rpc.BondPriceConventionPer100, Bid: new(99.40), Ask: new(99.42), Mark: new(99.40), Close: new(99.38),
		QuoteAsOf: quoteAt, QuoteFresh: true, PriceSource: rpc.BondPriceSourceBid, Price: new(99.40), YieldPct: new(4.08), YieldBasis: risk.BillYieldBasisAct365,
		YieldBasisNote: "simple annualised yield from price to par, actual/365", PriorCloseYieldPct: new(4.07), PriorCloseDate: "2026-10-09", YieldChangeBp: new(1.4),
		ValueChangeUp25Bp: new(-18.3), ValueChangeDown25Bp: new(18.3), DV01: new(0.73), ScenarioModel: rpc.BondScenarioModel,
		FaceValue: new(50000.0), FaceSource: rpc.BondFaceSourceQuantityUnit, PaysAtMaturity: &rpc.BondMaturityPayment{Amount: 50000, Currency: "USD", Date: "2026-12-04", Days: 54},
		Purchase: &rpc.BondPurchase{Date: "2026-10-02", PricePer100: 99.18, Lots: 1, YieldPct: new(4.79), Source: rpc.BondPurchaseSourceJournal},
		PnL: &rpc.BondPnLSplit{Currency: "USD", Scope: rpc.BondPnLScopeCleanPrice, Total: 110, Carry: new(58.2), Rates: new(51.8),
			SpreadUnavailable: "US Treasury bills are the USD benchmark; spread does not apply", TotalSource: rpc.BondPnLTotalSourceLots}}
	btf := &rpc.BondFacts{Currency: "EUR", PriceConvention: rpc.BondPriceConventionPer100, Bid: new(99.40), Ask: new(99.42), Mark: new(99.40), Close: new(99.41),
		QuoteAsOf: quoteAt, QuoteFresh: true, PriceSource: rpc.BondPriceSourceBid, Price: new(99.40), YieldPct: new(2.31), YieldBasis: risk.BillYieldBasisAct360,
		YieldBasisNote: "simple annualised yield from price to par, actual/360", PriorCloseYieldPct: new(2.29), PriorCloseDate: "2026-10-09", YieldChangeBp: new(2.3),
		ValueChangeUp25Bp: new(-25.8), ValueChangeDown25Bp: new(25.9), DV01: new(1.03), ScenarioModel: rpc.BondScenarioModel,
		FaceValue: new(40000.0), FaceSource: rpc.BondFaceSourceQuantityUnit, PaysAtMaturity: &rpc.BondMaturityPayment{Amount: 40000, Currency: "EUR", Date: "2027-01-13", Days: 94},
		Purchase: &rpc.BondPurchase{Date: "2026-09-22", PricePer100: 99.32, Lots: 1, YieldPct: new(2.17), Source: rpc.BondPurchaseSourceJournal},
		PnL: &rpc.BondPnLSplit{Currency: "EUR", Scope: rpc.BondPnLScopeCleanPrice, Total: 32, Carry: new(58.4), Rates: new(-18.6), Spread: new(-7.8), TotalSource: rpc.BondPnLTotalSourceLots,
			Benchmark: &rpc.BondBenchmark{ConID: 900012, ISIN: "DE000SYN0002", Issuer: "Synthetic Republic", Maturity: "2027-01-19", YieldPct: new(2.02), YieldBasis: risk.BillYieldBasisAct360,
				QuoteAsOf: quoteAt, SpreadBpNow: new(29.0), SpreadBpThen: new(21.0), SpreadSince: "2026-10-01", Source: "de_bubill"}}}
	elsewhere := &rpc.BondFacts{Currency: "GBP", PriceConvention: rpc.BondPriceConventionPer100, Mark: new(98.90), PriceSource: rpc.BondPriceSourceMark, Price: new(98.90),
		QuoteNote: "quote requested; the next read carries it", YieldPct: new(4.46), YieldBasis: risk.BillYieldBasisAct365, PriorCloseDate: "2026-10-09",
		YieldChangeUnavailable: "the broker sent no prior close", ValueChangeUp25Bp: new(-6.1), ValueChangeDown25Bp: new(6.1), DV01: new(0.24), ScenarioModel: rpc.BondScenarioModel,
		FaceValue: new(10000.0), FaceSource: rpc.BondFaceSourceQuantityUnit, PaysAtMaturity: &rpc.BondMaturityPayment{Amount: 10000, Currency: "GBP", Date: "2027-01-11", Days: 91},
		Purchase:                 &rpc.BondPurchase{PricePer100: 98.75, Source: rpc.BondPurchaseSourceBrokerCost},
		PnL:                      &rpc.BondPnLSplit{Currency: "GBP", Scope: rpc.BondPnLScopeCleanPrice, Total: 15, TotalSource: rpc.BondPnLTotalSourceBroker, SpreadUnavailable: "no benchmark instrument is defined for uk_tbill"},
		SincePurchaseUnavailable: "the purchase date is unknown: the line was not bought through Canary, so the gain cannot be split"}
	return []rpc.PositionBond{
		{ConID: 900010, Symbol: "SYNTH", Currency: "USD", Class: rpc.BondClassBill, CUSIP: "SYN797ZZ1", Maturity: "2026-12-04", Quantity: 50, Mark: 99.40, MarketValue: 49700, Facts: usBill},
		{ConID: 900011, Symbol: "SYNTHB", Currency: "EUR", Class: rpc.BondClassBill, ISIN: "FR000SYN0001", Maturity: "2027-01-13", Quantity: 40000, Mark: 99.40, MarketValue: 39760, Facts: btf},
		{ConID: 900013, Symbol: "SYNTHG", Currency: "GBP", Class: rpc.BondClassBill, ISIN: "GB00SYN00001", Maturity: "2027-01-11", Quantity: 10000, Mark: 98.90, MarketValue: 9890, Facts: elsewhere},
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
				EvidenceAsOf: time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC), Coupon: new(2.5), AccruedBound: 250,
				YieldPct: new(2.68), ModifiedDuration: new(8.6), RateShockLoss: new(812.4)}},
		Quote:    rpc.OrderQuoteSnapshot{Symbol: "SYNB", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, AsOf: goldenAt},
		Position: rpc.OrderPositionImpact{Before: 0, After: 10000, Effect: rpc.OrderPositionEffectOpen},
		Notional: 10092, NotionalCurrency: "EUR", NotionalBase: 10092, BaseCurrency: "EUR",
		WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true},
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

// goldenMandateAllocations is the within book with the core running at its full
// stage: its allocation of 6 points leaves yours 9, which the book is over.
func goldenMandateAllocations() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	v := *res.Verdict
	v.Allocations = []mandate.AllocationReport{
		{Sleeve: mandate.SleeveOwner, AllocationPct: 9, AllocationBase: 19230, UseBase: 23970, ContributionPct: 100, StandaloneBase: 23970, ExcessBase: 4740, Over: true, Legs: 3},
		{Sleeve: mandate.SleeveCore, TargetPct: 6, StagePct: 100, AllocationPct: 6, AllocationBase: 12820, ExcessBase: -12820, Note: "the core is live at scale 100 of its 6 allocation"},
		{Sleeve: mandate.SleeveOverlay, TargetPct: 4, Note: "no strategy registered in the overlay yet"},
	}
	res.Verdict = &v
	res.Gate = &rpc.MandateGateView{RefusesAdditions: true, Reason: "your sleeve carries 23,970 EUR a year, over its allocation 19,230 EUR (the budget less the machine's allocations of 12,820 EUR)"}
	return res
}

// goldenMandateOwnerStop is a verdict within the budget whose owner sleeve
// stop is engaged (owner_stop_pct 25): the sleeve 27% below its own
// high-water mark, the reduction cutting it to half its risk at the stop.
func goldenMandateOwnerStop() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	v := *res.Verdict
	v.Allocations = []mandate.AllocationReport{
		{Sleeve: mandate.SleeveOwner, AllocationPct: 15, AllocationBase: 32050, UseBase: 23970, ContributionPct: 100, StandaloneBase: 23970, ExcessBase: -8080, Legs: 3},
		{Sleeve: mandate.SleeveCore, TargetPct: 6, Note: "no [mandate.core]: the core is inactive"},
		{Sleeve: mandate.SleeveOverlay, TargetPct: 4, Note: "no strategy registered in the overlay yet"},
	}
	v.OwnerStop = &mandate.SleeveStop{Sleeve: mandate.SleeveOwner, StopPct: 25, ReleasePct: 20, Known: true, StartedAt: "2026-09-01", RiskBase: 175000, HWMRiskBase: 240000, HWMAsOf: goldenAt.Add(-72 * time.Hour),
		DrawdownBase: 65000, DrawdownPct: 27.08, Engaged: true, EngagedAt: goldenAt.Add(-30 * time.Minute), UseAtStopBase: 31200, TargetUseBase: 15600, UseBase: 23970, Over: true}
	res.Verdict = &v
	res.Policy.Sleeves = &rpc.MandateSleevesView{OwnerVolPct: new(7.0), CoreVolPct: new(6.0), OverlayVolPct: new(4.0), OwnerStopPct: new(25.0)}
	res.Gate = &rpc.MandateGateView{RefusesAdditions: true, Reason: "your sleeve is 27% below its high-water mark 240,000 EUR, past its 25% stop; the sleeve is reduce-only until its drawdown is back under 20%, 5 points under the stop"}
	res.Reduction = &rpc.MandateReductionView{State: rpc.MandateReductionReducing, Rows: 1, PreAuthorised: true, Pending: 1, VetoUntil: goldenAt.Add(28 * time.Minute),
		Reason: "the book is within the budget, but your sleeve is 27% below its high-water mark 240,000 EUR, past its 25% stop (risk 23,970 EUR a year, over its stop target 15,600 EUR); 1 cut sized to take the risk to 15,100 EUR a year, the sleeve's risk to its stop target 15,600 EUR",
		Orders: []rpc.MandateReductionOrder{{Rank: 1, Key: "mandate_reduction:SELL:STK:9001", Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYNA", SecType: "STK"}, Action: rpc.OrderActionSell, Quantity: 300,
			Phase: rpc.MandatePhaseStop, ContributionPct: 71.4, Automatic: rpc.TradeProposalAutomaticPending, SubmitAt: goldenAt.Add(28 * time.Minute)}}}
	return res
}

// goldenMandateRegister is a verdict with the strategy register: the core
// in its first form (no stage written, so shadow; the compiled spec stands)
// and a synthetic later entry on hold for a missing key and a rule this
// build does not carry (strategy-register.md).
func goldenMandateRegister() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	res.Register = []rpc.MandateStrategyView{
		{ID: "core", Sleeve: "core", Rule: "core-ibcf-v1", RuleCompiled: true, AllocationPct: 6, AllocationKey: "mandate.sleeves.core_vol_pct", State: "shadow", ScalePct: 25,
			Notes:          []string{"stage unwritten: shadow, as before the register", "spec_hash unwritten: the compiled rule's own spec stands"},
			DeclaredSeries: []string{"IBCF EUR daily closes, 253 sessions"}, ISIN: core.ISIN, Currency: core.Currency, Signal: core.TrendSMA10.ID},
		{ID: "s9", Sleeve: "s9", Rule: "synthetic-v1", SpecHash: "abababababababababababababababababababababababababababababababab", SpecCited: true, AllocationPct: 2,
			AllocationKey: "mandate.strategies.s9.risk_allocation_pct", Stage: "live", State: "hold",
			Reasons: []string{"mandate.strategies.s9.scale_pct unwritten", "rule synthetic-v1 is not compiled into this Canary"}},
	}
	return res
}

// goldenMandate is a synthetic mandate verdict: a two-name book with a
// short call, SYNA stopped and SYNB not, within the maximum loss or, with
// the quantities scaled up, a reduce on the loss measure with two gap
// breaches (spec TM-04.12).
func goldenMandate(status string) rpc.MandateRiskResult {
	scale := 1.0
	if status == mandate.StatusReduce {
		scale = 4
	}
	capital := mandate.Capital{Known: true, BaseCurrency: "EUR", StartedAt: "2026-09-01", EquityBase: 307000, EquityAsOf: goldenAt.Add(-2 * time.Minute),
		TaxReserveBase: 70000, RiskBase: 232000, FlowsSinceStart: -5000, HWMRiskBase: 235000, HWMAsOf: goldenAt.Add(-48 * time.Hour),
		BudgetBase: 230000, DrawdownBase: 5000, DrawdownPct: 2.13, Room: 0.929, BudgetAnnualSD: 32050}
	up := 6200 * scale
	// The within book is drawn gaining in the index stress and the reduce
	// book losing, so both words of the P&L render are pinned.
	indexLoss := -9870.0
	if scale > 1 {
		indexLoss = 39480
	}
	// The within book has read so since the morning, the reduce book since
	// an hour ago; the unknown variants reset it below.
	since := goldenAt.Add(-5 * time.Hour)
	if status == mandate.StatusReduce {
		since = goldenAt.Add(-62 * time.Minute)
	}
	verdict := &mandate.Verdict{Status: status, AsOf: goldenAt, BaseCurrency: "EUR", Capital: capital,
		Risk: &mandate.Risk{SD1DBase: 1510 * scale, SDAnnualBase: 23970 * scale, ES99Base: 4120 * scale, RatioToBudget: 0.748 * scale, FXContributionPct: 11.2},
		Loss: &mandate.LossMeasure{TotalBase: 20750 * scale, StopLossBase: 20750 * scale, RoomBase: 64101, RatioToRoom: 20750 * scale / 64101,
			Names: []mandate.LossName{{Underlying: "SYNA", TotalBase: 20750 * scale, StopLossBase: 20750 * scale}}, Unstopped: []string{"SYNB"}},
		Stresses:  &mandate.Stresses{GapShockPct: 25, GapCapBase: 22940, IndexShockPct: 10, IndexLossBase: indexLoss},
		Scenarios: 252, ScenariosFrom: "2025-09-05", ScenariosTo: "2026-09-04",
		Legs: []mandate.LegReport{
			{ID: "con:9001", Symbol: "SYNA", Underlying: "SYNA", Kind: mandate.KindStock, Quantity: 1000 * scale, ValueBase: 90000 * scale, ContributionPct: 71.4, LossBase: 20750 * scale},
			{ID: "con:9002", Symbol: "SYNB", Underlying: "SYNB", Kind: mandate.KindStock, Quantity: 500 * scale, ValueBase: 40000 * scale, ContributionPct: 33.1},
			{ID: "con:9004", Symbol: "SYNA 20261218 110 C", Underlying: "SYNA", Kind: mandate.KindOption, Quantity: -2 * scale, ValueBase: -540 * scale, ContributionPct: -4.5},
		},
		Underlyings: []mandate.UnderlyingReport{
			{Underlying: "SYNA", ValueBase: 89460 * scale, ContributionPct: 66.9, Beta: 1.12, GapDownLossBase: 22100 * scale, GapUpLossBase: &up, GapLossBase: 22100 * scale, GapCapBase: 22940, GapBreach: scale > 1},
			{Underlying: "SYNB", ValueBase: 40000 * scale, ContributionPct: 33.1, Beta: 0.64, GapDownLossBase: 10000 * scale, GapLossBase: 10000 * scale, GapCapBase: 22940, GapBreach: scale > 1},
		},
		NotModelled: []string{"SYNT (bill, no equity risk modelled)"},
		Allocations: []mandate.AllocationReport{
			{Sleeve: mandate.SleeveOwner, AllocationPct: 15, AllocationBase: 32050, UseBase: 23970 * scale, ContributionPct: 100, StandaloneBase: 23970 * scale, ExcessBase: 23970*scale - 32050, Over: scale > 1, Legs: 3},
			{Sleeve: mandate.SleeveCore, TargetPct: 6, Note: "the core runs in shadow: it serves its target and sends no orders, so it reserves nothing (stage unwritten: shadow, as before the register)"},
			{Sleeve: mandate.SleeveOverlay, TargetPct: 4, Note: "no strategy registered in the overlay yet"},
		}}
	if scale > 1 {
		verdict.Stresses.GapBreaches = []string{"SYNA", "SYNB"}
	}
	res := rpc.MandateRiskResult{AsOf: goldenAt, Active: true, Verdict: verdict, StatusSince: since,
		PolicyFingerprint: &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: "sha256:synthetic"},
		Policy:            &rpc.MandatePolicyView{MaxLossPct: 30, VolTargetPct: 15, TaxReserveBase: 70000, StartedAt: "2026-09-01", GapShockPct: 25, GapRoomPct: 33.3},
		Evidence: &rpc.MandateEvidence{EvaluatedAt: goldenAt, EquityAsOf: goldenAt.Add(-2 * time.Minute), PositionsAsOf: goldenAt.Add(-time.Minute),
			Series: []rpc.MandateSeriesEvidence{
				{Key: "fx:USD", Closes: 320, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · MIDPOINT"},
				{Key: "u:SPY", Closes: 320, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · TRADES"},
				{Key: "u:SYNA", Closes: 320, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · TRADES"},
				{Key: "u:SYNB", Closes: 318, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · TRADES"},
			}},
		Gate: &rpc.MandateGateView{}}
	if status != mandate.StatusWithin {
		res.Gate = &rpc.MandateGateView{RefusesAdditions: true, Reason: goldenMandateLossSentence + "; a gap in SYNA, SYNB would lose more than the gap cap 22,940 EUR"}
	}
	return res
}

// goldenMandateLossSentence is the reduce verdict's loss sentence, as the
// daemon states it for every gate refusal.
const goldenMandateLossSentence = "the loss measure 83,000 EUR (premium at risk 0 EUR, stop loss 83,000 EUR) is over the mandate's loss room 64,101 EUR (129%)"

// goldenMandateExposure is the reduce verdict with the exposure view, the
// options gate reading the premium limits and an active override of the
// volatility budget (owner decision 2026-10-23 18:22 CEST).
func goldenMandateExposure() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusReduce)
	nlv, beta := 300000.0, 1.12
	num := func(v float64) *float64 { return &v }
	res.Exposure = &rpc.MandateExposureView{AsOf: goldenAt.Add(-time.Minute), BaseCurrency: "EUR", NLVBase: &nlv, NamePremiumCapPct: 15, BookPremiumCapPct: 35, BookPremiumWatchPct: 25, BookPremiumSet: "calm",
		Book: rpc.MandateExposureRow{Legs: 4, Options: 2, DeltaBase: num(121300), DeltaBetaBase: num(126204), GammaBase: num(-310), ThetaBase: num(-42), PremiumAtRiskBase: num(48000), PremiumPctNLV: num(16), PremiumCapBase: num(105000), PremiumRoomBase: num(57000),
			Unknown: []string{"vega unknown for SYNB"}},
		Names: []rpc.MandateExposureRow{
			{Underlying: "SYNA", Legs: 2, Options: 1, ContributionPct: num(66.9), Beta: &beta, DeltaBase: num(81300), DeltaBetaBase: num(91056), GammaBase: num(-310), ThetaBase: num(-42), VegaBase: num(-180), PremiumAtRiskBase: num(48000), PremiumPctNLV: num(16), PremiumCapBase: num(45000), PremiumRoomBase: num(0), PremiumOver: true},
			{Underlying: "SYNB", Legs: 2, Options: 1, ContributionPct: num(33.1), Beta: num(0.64), DeltaBase: num(40000), DeltaBetaBase: num(25600), GammaBase: num(0), ThetaBase: num(0), PremiumAtRiskBase: num(0), PremiumPctNLV: num(0), PremiumCapBase: num(45000), PremiumRoomBase: num(45000),
				Unknown: []string{"SYNB 20261218 C 90: no vega from the broker's model"}},
		}}
	// The verdict's premium at risk is the exposure view's: 48,000 EUR of
	// premium beside the 83,000 EUR of stop loss.
	res.Verdict.Loss = &mandate.LossMeasure{TotalBase: 131000, PremiumBase: 48000, StopLossBase: 83000, RoomBase: 64101, RatioToRoom: 131000.0 / 64101,
		Names: []mandate.LossName{{Underlying: "SYNA", TotalBase: 131000, PremiumBase: 48000, StopLossBase: 83000}}, Unstopped: []string{"SYNB"}}
	loss := "the loss measure 131,000 EUR (premium at risk 48,000 EUR, stop loss 83,000 EUR) is over the mandate's loss room 64,101 EUR (204%)"
	res.Gate = &rpc.MandateGateView{Reason: "your override of mandate.vol_target_pct lifts the volatility budget until 2026-09-05 10:00 UTC (\"earnings week, adding by hand\"); the verdict reads: " + loss,
		Options:  &rpc.MandateOptionsGateView{Warning: loss + "; a gap in SYNA, SYNB would lose more than the gap cap 22,940 EUR"},
		Override: &rpc.MandateOverrideView{ID: "ov-20260905-080000-1", Control: "mandate.vol_target_pct", Reason: "earnings week, adding by hand", GrantedAt: goldenAt.Add(-2 * time.Hour), ExpiresAt: goldenAt.Add(22 * time.Hour)}}
	return res
}

// goldenMandateReducing is the reduce verdict with its forced reduction:
// two rows, pre-authorised, the daemon placing both after the veto window.
func goldenMandateReducing() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusReduce)
	res.Reduction = &rpc.MandateReductionView{State: rpc.MandateReductionReducing, Rows: 2, Pending: 2, VetoUntil: goldenAt.Add(30 * time.Minute), PreAuthorised: true,
		Reason: "the verdict reads reduce (the loss measure 83,000 EUR is 129% of the loss room 64,101 EUR); 2 cuts sized to take the loss measure to 60,100 EUR, inside the 60,896 EUR target",
		Orders: []rpc.MandateReductionOrder{
			{Rank: 1, Key: "mandate_reduction:0001", Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK"}, Action: rpc.OrderActionSell, Quantity: 3200,
				Phase: rpc.MandatePhaseGap, LossBase: 83000, ContributionPct: 71.4, Automatic: rpc.TradeProposalAutomaticPending, SubmitAt: goldenAt.Add(30 * time.Minute)},
			{Rank: 2, Key: "mandate_reduction:0002", Contract: rpc.ContractParams{Symbol: "SYNB", SecType: "STK"}, Action: rpc.OrderActionSell, Quantity: 1200,
				Phase: rpc.MandatePhasePremium, ContributionPct: 33.1, Automatic: rpc.TradeProposalAutomaticPending, SubmitAt: goldenAt.Add(30 * time.Minute)},
		}}
	return res
}

// goldenMandateNoRoom is the reduce at room 0: the loss from the high has
// passed the maximum, so the loss room, the budget and the gap caps are zero
// and the ratios are unbounded (null on the wire, read back as +Inf through
// goldenConn).
func goldenMandateNoRoom() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	v := res.Verdict
	v.Status = mandate.StatusReduce
	c := &v.Capital
	c.EquityBase, c.TaxReserveBase, c.RiskBase, c.DrawdownBase, c.DrawdownPct, c.Room, c.BudgetAnnualSD = 240000, 85000, 155000, 75000, 31.91, 0, 0
	v.Risk.RatioToBudget = math.Inf(1)
	v.Loss.RoomBase, v.Loss.RatioToRoom = 0, math.Inf(1)
	v.Stresses.GapCapBase, v.Stresses.GapBreaches = 0, []string{"SYNA", "SYNB"}
	for i := range v.Underlyings {
		v.Underlyings[i].GapCapBase, v.Underlyings[i].GapBreach = 0, true
	}
	res.Gate = &rpc.MandateGateView{RefusesAdditions: true, Reason: "the loss measure 20,750 EUR (premium at risk 0 EUR, stop loss 20,750 EUR) is over the mandate's loss room 0 EUR (the loss has reached the mandate's maximum, so no loss room is left); a gap in SYNA, SYNB would lose more than the gap cap 0 EUR"}
	return res
}

// goldenMandateHeld is the unknown verdict with a standing batch held: not
// pre-authorised, so the row would wait for the owner anyway.
func goldenMandateHeld() rpc.MandateRiskResult {
	res := goldenMandateUnknown()
	res.Reduction = &rpc.MandateReductionView{State: rpc.MandateReductionHeld, Rows: 1, Held: true,
		HoldReason: "the mandate verdict cannot be measured: daily history for SYNB is stale (last read 2026-09-02 06:10:00)",
		Orders: []rpc.MandateReductionOrder{{Rank: 1, Key: "mandate_reduction:0001", Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK"}, Action: rpc.OrderActionSell,
			Quantity: 400, Phase: rpc.MandatePhasePremium, LossBase: 20750, ContributionPct: 71.4, Held: true}}}
	return res
}

// goldenMandateProposals is the forced reduction as canary proposals lists
// it: an expiring call first, then the largest premium at risk held to the
// order cap, pre-authorised and waiting out the veto window.
func goldenMandateProposals() rpc.TradeProposalSnapshot {
	verdictAt := goldenAt.Add(-time.Minute)
	auto := &rpc.TradeProposalAutomatic{PreAuthorised: true, Bucket: rpc.TradeProposalBucketMandateReduction, State: rpc.TradeProposalAutomaticPending,
		CreatedAt: goldenAt, SubmitAt: goldenAt.Add(30 * time.Minute), VetoWindow: "30m0s"}
	sessions := 2
	mandateRow := func(rank int, phase string, contribution, cut, legLoss, lossCut float64) *rpc.TradeProposalMandate {
		m := &rpc.TradeProposalMandate{Rank: rank, BatchRows: 2, Phase: phase, ContributionPct: contribution, Units: 600,
			LossBase: 72000, LossRoomBase: 64000, LossTargetBase: 60800, LegLossBase: legLoss, LossCutBase: lossCut, LossAfterBase: 60500,
			RiskAnnualBase: 41200, BudgetAnnualSD: 36000, RiskCutAnnualBase: cut, RiskAfterAnnualBase: 34050,
			VerdictStatus: mandate.StatusReduce, VerdictAt: verdictAt, VerdictFingerprint: "a1b2c3d4e5f6", BaseCurrency: "EUR"}
		if phase == rpc.MandatePhaseExpiry {
			m.Units, m.SessionsToExpiry = 5, &sessions
		}
		return m
	}
	return rpc.TradeProposalSnapshot{
		Revision: "rev-0002", PolicyID: "protection", PolicyVersion: 3,
		Counts: rpc.TradeProposalCounts{Total: 2, Actionable: 2, MandateReduction: 2},
		MandateReduction: &rpc.TradeProposalMandateStatus{State: rpc.MandateReductionReducing, VerdictStatus: mandate.StatusReduce, VerdictAt: verdictAt,
			VerdictFingerprint: "a1b2c3d4e5f6", ConsecutiveOver: 2, LossBase: 72000, LossRoomBase: 64000, LossTargetBase: 60800, ProjectedLossBase: new(60500.0),
			RiskAnnualBase: 41200, BudgetAnnualSD: 36000, ProjectedRiskAnnualBase: new(34050.0),
			TargetReached: true, Rows: 2, BaseCurrency: "EUR",
			Reason: "the verdict reads reduce (the loss measure 72,000 EUR is 112% of the loss room 64,000 EUR); 2 cuts sized to take the loss measure to 60,500 EUR, inside the 60,800 EUR target"},
		Proposals: []rpc.TradeProposal{
			{Key: "mandate_reduction:0001", Bucket: rpc.TradeProposalBucketMandateReduction, Action: rpc.OrderActionSell, Quantity: 5, Symbol: "SYNB", SecType: "OPT",
				OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, PositionEffect: rpc.OrderPositionEffectClose, PositionQuantity: 5, NeverSkipVeto: true,
				Contract: rpc.ContractParams{Symbol: "SYNB", SecType: "OPT", Currency: "EUR", Expiry: "20260909", Strike: 75, Right: "C"},
				Reason:   "the loss measure 72,000 EUR is 112% of the mandate's loss room 64,000 EUR (reduce); in the money 2 sessions before expiry; exercised, the delivered shares would push the reduced book's volatility over its budget, so it goes first: sell 5 of 5 contracts; the batch takes the loss measure to 60,500 EUR, inside the 60,800 EUR target",
				Mandate:  mandateRow(1, rpc.MandatePhaseExpiry, 4.2, 700, 2500, 2500), Automatic: auto,
				Details: []string{"order 1 of 2 in the batch, sent in this order; this cut takes 2,500 EUR off the loss measure and 700 EUR a year off the volatility", "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop"}},
			{Key: "mandate_reduction:0002", Bucket: rpc.TradeProposalBucketMandateReduction, Action: rpc.OrderActionSell, Quantity: 200, Symbol: "SYNA", SecType: "STK",
				OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, PositionEffect: rpc.OrderPositionEffectReduce, PositionQuantity: 3000, NeverSkipVeto: true,
				Contract: rpc.ContractParams{Symbol: "SYNA", SecType: "STK", Currency: "EUR"},
				Reason:   "the loss measure 72,000 EUR is 112% of the mandate's loss room 64,000 EUR (reduce); the largest premium at risk left, SYNA in your sleeve (60,000 EUR of the loss measure): sell 200 of 3000 shares now (the batch asks for 600; the order caps hold one order to 200, and the next cycle measures the rest); the batch takes the loss measure to 60,500 EUR, inside the 60,800 EUR target",
				Mandate:  mandateRow(2, rpc.MandatePhasePremium, 88.1, 6450, 60000, 9000), Automatic: auto,
				Details: []string{"order 2 of 2 in the batch, sent in this order; this cut takes 9,000 EUR off the loss measure and 6,450 EUR a year off the volatility", "waits the full veto window even under a latched brake: a reduction to budget is a discretionary-scale action, not a stop"}},
		},
	}
}

// goldenMandateUnknown is a verdict the evidence cannot establish: one
// series stale, one option without IV.
// goldenMandateIVFallback is a measured verdict with one option priced on
// the volatility its mark implies (owner, 2026-10-28 08:51 CET).
func goldenMandateIVFallback() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	ratio := 0.012
	res.Verdict.IVFallback = &mandate.IVFallbackReport{IndexLossLowBase: 8100, IndexLossHighBase: 8700, UncertaintyBase: 300, RatioToRoom: &ratio, Tier: mandate.IVTierMeasured,
		Legs: []mandate.IVFallbackLeg{{Symbol: "SYNA 20261218 110 C", Source: mandate.IVSourceMark, AsOf: goldenAt.Add(-14 * time.Hour), DaysToExpiry: 51, IV: 0.34, BandPct: 8}}}
	return res
}

func goldenMandateUnknown() rpc.MandateRiskResult {
	res := goldenMandate(mandate.StatusWithin)
	v := res.Verdict
	v.Status = mandate.StatusUnknown
	res.StatusSince = goldenAt.Add(-50*time.Hour - 10*time.Minute)
	v.Reasons = []string{"daily history for SYNB is stale (last read 2026-09-02 06:10:00)", "SYNA 20261218 110 C: no implied volatility on the position row"}
	v.Risk, v.Stresses, v.Underlyings = nil, nil, nil
	v.Scenarios, v.ScenariosFrom, v.ScenariosTo = 0, "", ""
	for i := range v.Legs {
		v.Legs[i].ContributionPct, v.Legs[i].ValueBase = 0, 0
		v.Legs[i].Unknown = "not simulated"
	}
	v.Legs[2].Unknown = "no implied volatility on the position row"
	res.Evidence.Series[3].Stale = true
	res.Evidence.Series[3].LastClose = "2026-09-01"
	res.Gate = &rpc.MandateGateView{RefusesAdditions: true, Reason: "the mandate risk cannot be measured: daily history for SYNB is stale (last read 2026-09-02 06:10:00)"}
	return res
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
		GatewayAPI:       &rpc.GatewayAPIHealth{TradingProtocolMax: 203, ConfigReadProtocol: 219, ConfigState: "not_requested"},
		GatewaySetup:     &rpc.GatewaySetupHealth{State: rpc.GatewaySetupAsExpected, AsOf: goldenAt.Add(-119 * time.Minute)},
		ServerVersion:    176,
		BackgroundTasks:  []rpc.BackgroundTaskStatus{},
		Subsystems:       []rpc.SubsystemHealth{{Name: "regime", Status: "ready"}, {Name: "breadth", Status: "ready"}, {Name: "rulebook", Status: "ready"}},
		Members:          rpc.MembersHealth{Source: "fixture", AsOf: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), Count: 500, RefreshState: "healthy"},
		Trading:          trading,
		Executions:       &rpc.ExecutionVisibilityHealth{Visibility: rpc.ExecutionVisibilityAllClients, ProvenAt: goldenAt.Add(-25 * time.Minute), ClientID: 7, Setting: rpc.ExecutionVisibilitySetting},
		DataHealth:       &rpc.DataHealthResult{Summary: rpc.DataHealthSummary{State: "current", Label: "12 of 12 sources current"}},
		Strategies: []rpc.StrategyHealth{
			{ID: "core", Sleeve: "core", State: "live", Row: &rpc.StrategyHealthRow{Key: "core_rebalance:synthetic01", Action: "BUY", Quantity: 908, Symbol: "SYNX", Session: "2026-09-04", State: rpc.TradeProposalStateGenerated, Readiness: "opening_window"}},
			{ID: "trend-fund", Sleeve: "trend", State: "live", Why: "the 5-session window after 2026-08-31 ended with 2026-09-07; what is left waits for the next decision"},
		},
	}
}

func goldenHealthAttention() rpc.HealthResult {
	res := goldenHealthReady(rpc.TradingStatus{Mode: config.TradingModePaper, MCPTrading: rpc.TradingMCPDisabled, Blocked: true, Blockers: []rpc.TradingBlocker{{Code: "market_data_delayed", Message: "market data is delayed"}}})
	res.Verdict = rpc.HealthVerdict{State: "ATTENTION", Reason: "Historical data farm broken"}
	res.DataType = rpc.MarketDataDelayed
	res.DataFarms = []rpc.DataFarmHealth{{Name: "ushmds", Type: "hmds", Status: "broken", Code: 2105}}
	res.MarketDataAccess = []rpc.MarketDataAccessHealth{{RouteKey: "SYNB", Symbol: "SYNB", Code: 354, Reason: rpc.MarketDataAccessNotSubscribed, ObservedAt: goldenAt, RetryAt: goldenAt.Add(30 * time.Minute)}}
	res.BackendLink = &rpc.BackendLinkHealth{Losses: 2, LossesInMaintenanceWindow: 1, LastOutageSeconds: 30, LongestOutageSeconds: 90}
	res.Executions = &rpc.ExecutionVisibilityHealth{Visibility: rpc.ExecutionVisibilityUnproven, ClientID: 7, Setting: rpc.ExecutionVisibilitySetting,
		Action: "No fills from TWS or other API clients have been observed for this account and API client. To include them, check that TWS Global Configuration → API → Settings → Master API client ID is Canary's client id (7); the owner decides that setting."}
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
	st.Chains.Recorder.Enabled = rpc.SettingsBool{Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Chains.Recorder.Symbols = rpc.SettingsStringList{Value: []string{"SPX", "SPY", "QQQ"}, Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Trading.Freeze = rpc.SettingsBool{Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime}
	st.Trading.TWSOrderControl = rpc.TWSOrderControlSettings{
		Enabled:       rpc.SettingsBool{Access: rpc.SettingsAccessWrite, Source: rpc.SettingsSourceRuntime},
		PaperProvenAt: rpc.SettingsString{Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceRuntime},
		Status:        rpc.SettingsString{Value: rpc.TWSOrderControlStatusOff, Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved},
	}
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

// goldenProposalsIndexLoss is a list priced for the index fall: the book's
// loss now and with every actionable row filled, each row's own figure, and
// a trailing stop that has none because it fills only when reached.
func goldenProposalsIndexLoss() rpc.TradeProposalSnapshot {
	snap := goldenProposals()
	snap.BaseCurrency = "USD"
	snap.Counts = rpc.TradeProposalCounts{Total: 2, Actionable: 2, TrailingStop: 1, RiskReduction: 1}
	snap.IndexLoss = &rpc.IndexLoss{IndexShockPct: 10, IndexLossBeforeBase: new(8300.0), IndexLossAfterBase: new(7050.0)}
	snap.Proposals[0].IndexLoss = &rpc.IndexLoss{IndexShockPct: 10, IndexLossUnknown: "a stop or trail fills only when the market reaches it; the loss is measured for orders that fill now"}
	snap.Proposals = append(snap.Proposals, rpc.TradeProposal{
		Key: "rr-SYNC", Bucket: rpc.TradeProposalBucketRiskReduction, Action: rpc.OrderActionSell, Quantity: 10, Symbol: "SYNC", SecType: "STK",
		OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: rpc.ContractParams{Symbol: "SYNC", SecType: "STK", Currency: "USD", ConID: 5151},
		Reason: "issuer over its worst-case loss limit", PositionQuantity: 40,
		IndexLoss: &rpc.IndexLoss{IndexShockPct: 10, IndexLossBeforeBase: new(8300.0), IndexLossAfterBase: new(7050.0)},
	})
	return snap
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

// goldenBondRisk is a synthetic held bond book (bond-risk.md, phase 1): a
// measured government note, a measured corporate, and a line the sums miss.
func goldenBondRisk() ([]rpc.PositionBond, *risk.BondBook) {
	bonds := []rpc.PositionBond{
		{ConID: 900001, Symbol: "SYNT", Currency: "USD", Class: rpc.BondClassBond, CUSIP: "SYN000001", Maturity: "2033-09-30", DaysToMaturity: new(2550),
			Coupon: new(5.0), Quantity: 10, Mark: 99.14, MarketValue: 9914, EvidenceIssuer: "United States Treasury", IssuerClass: rpc.BondIssuerGovernment,
			EvidenceSource: "treasurydirect", YieldPct: new(5.15), ModifiedDuration: new(5.8), MarketValueBase: new(8850.0), DV01Base: new(5.13), RateShockLossBase: new(498.2)},
		{ConID: 900002, Symbol: "SYNC", Currency: "EUR", Class: rpc.BondClassBond, ISIN: "XS000SYN0000", Maturity: "2035-07-01", DaysToMaturity: new(3189),
			Coupon: new(3.647), Quantity: 5000, Mark: 94.8, MarketValue: 4740, EvidenceIssuer: "Synthetic Energy International", IssuerClass: rpc.BondIssuerInvestmentGrade,
			EvidenceSource: "ecb_eligible_assets", YieldPct: new(4.38), ModifiedDuration: new(7.1), MarketValueBase: new(4740.0), DV01Base: new(3.37), RateShockLossBase: new(323.9)},
		{ConID: 900003, Symbol: "SYNL", Currency: "EUR", Class: rpc.BondClassBond, ISIN: "DE000SYN0001", Quantity: 1000, Mark: 101, MarketValue: 1010,
			MarketValueBase: new(1010.0), RiskUnmeasured: "inflation-linked: IBKR prices it on a factored principal"},
	}
	book := risk.SummarizeBondBook([]risk.BondBookLine{
		{Symbol: "SYNT", Currency: "USD", Issuer: "United States Treasury", IssuerClass: rpc.BondIssuerGovernment, MarketValueBase: new(8850.0), RateShockLossBase: new(498.2)},
		{Symbol: "SYNC", Currency: "EUR", Issuer: "Synthetic Energy International", IssuerClass: rpc.BondIssuerInvestmentGrade, MarketValueBase: new(4740.0), RateShockLossBase: new(323.9)},
		{Symbol: "SYNL", Currency: "EUR", Issuer: "Federal Republic of Germany", IssuerClass: rpc.BondIssuerGovernment, MarketValueBase: new(1010.0), Unmeasured: bonds[2].RiskUnmeasured},
	}, new(240000.0), "EUR")
	return bonds, book
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
