package daemon

import (
	"crypto/sha256"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestEdgeAcceptanceBrokerFixtureThroughSQLiteAndRPC(t *testing.T) {
	t.Parallel()
	const account = "UEDGEFIXTURE"
	now := time.Date(2026, time.August, 24, 23, 0, 0, 0, time.UTC)
	store, projected, evidenceFingerprint := projectEdgeAcceptanceFixture(t, "edge-acceptance-365.xml", account, now)

	if missing := missingEdgeManifestSections(projected); len(missing) != 0 {
		t.Fatalf("fixture has proven missing query requirements: %v", missing)
	}
	core, err := edgecore.Analyze(edgecore.Input{
		AsOf: now, WindowDays: 365, BaseCurrency: "USD", Statements: projected,
		Bars: edgeAcceptanceBars(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if core.Account == nil || core.Account.StartingEquityBase != 100_000 || core.Account.EndingEquityBase != 112_500 || core.Account.ExternalFlowsBase != 10_000 || core.Account.ProfitLossBase != 2_500 {
		t.Fatalf("account hand calculation mismatch: %+v", core.Account)
	}

	apexAdd := acceptanceChange(t, core.Changes, "APEX", edgecore.ActionAdd)
	for _, want := range []struct {
		sessions int
		close    float64
		impact   float64
		notional float64
		pct      float64
	}{
		{sessions: 1, close: 104, impact: -21, notional: 2_100, pct: -1},
		{sessions: 5, close: 100, impact: -101, notional: 2_100, pct: -4.809523809523809},
		{sessions: 20, close: 95, impact: -201, notional: 2_100, pct: -9.571428571428571},
	} {
		score := acceptanceScore(t, apexAdd, want.sessions)
		if score.HorizonClose == nil || *score.HorizonClose != want.close || score.DecisionImpactBase == nil || *score.DecisionImpactBase != want.impact || score.DecisionNotionalBase == nil || *score.DecisionNotionalBase != want.notional || score.DecisionImpactPct == nil || math.Abs(*score.DecisionImpactPct-want.pct) > 1e-12 {
			t.Fatalf("APEX add %d-session hand calculation mismatch: %+v", want.sessions, score)
		}
	}
	if score := acceptanceScore(t, acceptanceChange(t, core.Changes, "APEX", edgecore.ActionOpen), 20); score.Reason != edgecore.ReasonInterveningChange || score.DecisionImpactBase != nil {
		t.Fatalf("intervening add did not suppress the earlier open: %+v", score)
	}
	option := acceptanceOption(core.Options, "exact_order")
	if option == nil || option.ActualPNLBase == nil || *option.ActualPNLBase != 90 || option.LegCount != 2 {
		t.Fatalf("exact-linked broker-actual option result mismatch: %+v", core.Options)
	}

	port := 4001
	srv := &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}},
		coreStore: store, now: func() time.Time { return now },
	}
	scope := srv.currentBrokerStateScope()
	publication := edgePublication{
		ScopeFingerprint: edgeScopeFingerprint(scope), EvidenceFingerprint: evidenceFingerprint,
		State: rpc.EdgeStateCurrent, Windows: map[string]edgecore.Result{"365d": core},
		LastFullRevalidation: now, UpdatedAt: now,
	}
	if err := srv.saveEdgePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	result, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{"window":"365d","horizon_sessions":20,"limit":3}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || result.Reason != "" || result.Account == nil || result.Account.ProfitLossBase != 2_500 {
		t.Fatalf("published acceptance result: %+v", result)
	}
	if got, want := result.Headline, "Across 3 clean adds, observed 20-session Decision price impact totaled -453.00 USD; median -151.00 USD."; got != want {
		t.Fatalf("headline=%q want %q", got, want)
	}
	if len(result.Findings) != 3 || result.Findings[0].Symbol != "GAMMA" || result.Findings[0].Action != edgecore.ActionAdd || result.Findings[1].DecisionImpactBase <= 0 {
		t.Fatalf("decision-useful finding rank mismatch: %+v", result.Findings)
	}
	if result.Coverage.ScoredByHorizon[20] != 7 || result.Coverage.TradeChanges != 17 || result.Coverage.EligibleChanges != 15 {
		t.Fatalf("acceptance coverage mismatch: %+v", result.Coverage)
	}

	detail, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{"window":"365d","horizon_sessions":20,"change_id":"` + result.Findings[0].ChangeID + `"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if detail.Change == nil || detail.Change.Symbol != "GAMMA" || detail.Change.PositionBefore != 30 || detail.Change.PositionAfter != 45 || detail.Change.ExecutionVWAP == nil || *detail.Change.ExecutionVWAP != 55 || detail.Change.DirectCostsBase == nil || *detail.Change.DirectCostsBase != 1 {
		t.Fatalf("opaque change detail did not explain the ranked finding: %+v", detail.Change)
	}
	detailScore := acceptanceRPCScore(t, detail.Change.Scores, 20)
	if detailScore.HorizonClose == nil || *detailScore.HorizonClose != 45 || detailScore.HorizonFX == nil || *detailScore.HorizonFX != 1 || detailScore.DecisionNotionalBase == nil || *detailScore.DecisionNotionalBase != 825 || detailScore.DecisionImpactBase == nil || *detailScore.DecisionImpactBase != -151 {
		t.Fatalf("opaque change calculation trail mismatch: %+v", detailScore)
	}
}

func TestEdgeAcceptanceZeroTradeFixtureIsHonestAndUseful(t *testing.T) {
	t.Parallel()
	const account = "UEDGEZERO"
	now := time.Date(2026, time.August, 24, 23, 0, 0, 0, time.UTC)
	store, projected, evidenceFingerprint := projectEdgeAcceptanceFixture(t, "edge-zero-trades-365.xml", account, now)
	core, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 365, BaseCurrency: "USD", Statements: projected})
	if err != nil {
		t.Fatal(err)
	}
	if core.Account == nil || core.Account.ProfitLossBase != 500 || core.Coverage.TradeChanges != 0 || !slices.Contains(core.Coverage.PresentSections, "trades") || slices.Contains(core.Coverage.MissingSections, "trades") {
		t.Fatalf("zero-trade broker evidence was not preserved: %+v", core)
	}
	state, reason := edgePublicationStatus(map[string]edgecore.Result{"365d": core}, 0)
	if state != rpc.EdgeStateCurrent || reason != "no_trade_changes" {
		t.Fatalf("proven zero-trade state=%s/%s", state, reason)
	}
	port := 4001
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: account, Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}}, coreStore: store, now: func() time.Time { return now }}
	scope := srv.currentBrokerStateScope()
	if err := srv.saveEdgePublication(t.Context(), edgePublication{
		ScopeFingerprint: edgeScopeFingerprint(scope), EvidenceFingerprint: evidenceFingerprint,
		State: state, Reason: reason, Windows: map[string]edgecore.Result{"365d": core}, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	result, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{"window":"365d"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != rpc.EdgeStateCurrent || result.Reason != "no_trade_changes" || result.Account == nil || result.Account.ProfitLossBase != 500 || len(result.Findings) != 0 || !strings.Contains(result.Headline, "No stock or ETF position changes") || !strings.Contains(result.Headline, "account P/L remains separate") {
		t.Fatalf("zero-trade public explanation is hollow: %+v", result)
	}
}

func projectEdgeAcceptanceFixture(t *testing.T, name, account string, now time.Time) (*corestore.Store, []flexstmt.Statement, string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	statements, err := flexstmt.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 || statements[0].AccountID != account {
		t.Fatalf("fixture account scope mismatch")
	}
	digest := sha256.Sum256(raw)
	queryFingerprint := flexQueryFingerprint("424242")
	files, days, records, versions, err := buildStatementProjection([]statementProjectionFile{{name: name, size: int64(len(raw)), digest: digest, data: raw, statements: statements}}, now, queryFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	projectionScope := statementProjectionScopeForSelection(flexEvidenceSelection{ActiveQueryFingerprint: queryFingerprint})
	if err := store.ReplaceStatementProjection(t.Context(), projectionScope, files, days, records, versions); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.LoadStatementProjectionSnapshot(t.Context(), projectionScope, statementProjectionMaxRows*25, statementProjectionMaxRows)
	if err != nil {
		t.Fatal(err)
	}
	projected, err := edgeStatementsFromProjection(snapshot, brokerStateScope{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	scope := brokerStateScope{Account: account, Mode: rpc.AccountModeLive}
	return store, projected, edgeProjectionFingerprintSnapshot(snapshot, scope, edgeScopeFingerprint(scope))
}

func edgeAcceptanceBars() map[int64][]edgecore.DailyBar {
	out := map[int64][]edgecore.DailyBar{}
	add := func(conid int64, start string, count int, first, fifth, twentieth float64) {
		day, err := time.Parse(time.DateOnly, start)
		if err != nil {
			panic(err)
		}
		for i := range count {
			close := first
			switch i {
			case 4:
				close = fifth
			case 19:
				close = twentieth
			}
			out[conid] = append(out[conid], edgecore.DailyBar{ConID: conid, Day: day.AddDate(0, 0, i), Close: close})
		}
	}
	add(1001, "2025-09-03", 1, 101, 0, 0)
	add(1001, "2025-09-11", 20, 104, 100, 95)
	add(1001, "2026-03-03", 20, 109, 105, 100)
	add(1001, "2026-08-04", 10, 114, 112, 0)
	add(1002, "2025-10-02", 1, 198, 0, 0)
	add(1002, "2025-10-09", 20, 189, 185, 180)
	add(1002, "2026-04-07", 20, 209, 202, 195)
	add(1002, "2026-08-06", 10, 219, 218, 0)
	add(1003, "2025-11-04", 1, 49, 0, 0)
	add(1003, "2025-11-11", 20, 54, 50, 45)
	add(1003, "2026-08-08", 10, 59, 58, 0)
	add(1004, "2026-05-02", 1, 79, 0, 0)
	add(1004, "2026-05-12", 20, 74, 72, 70)
	add(1004, "2026-08-11", 10, 69, 68, 0)
	return out
}

func acceptanceChange(t *testing.T, changes []edgecore.Change, symbol, action string) edgecore.Change {
	t.Helper()
	for _, change := range changes {
		if change.Symbol == symbol && change.Action == action {
			return change
		}
	}
	t.Fatalf("missing %s %s change", symbol, action)
	return edgecore.Change{}
}

func acceptanceScore(t *testing.T, change edgecore.Change, sessions int) edgecore.HorizonScore {
	t.Helper()
	for _, score := range change.Scores {
		if score.Sessions == sessions {
			return score
		}
	}
	t.Fatalf("missing %d-session score", sessions)
	return edgecore.HorizonScore{}
}

func acceptanceRPCScore(t *testing.T, scores []rpc.EdgeHorizonScore, sessions int) rpc.EdgeHorizonScore {
	t.Helper()
	for _, score := range scores {
		if score.Sessions == sessions {
			return score
		}
	}
	t.Fatalf("missing %d-session RPC score", sessions)
	return rpc.EdgeHorizonScore{}
}

func acceptanceOption(options []edgecore.OptionResult, grouping string) *edgecore.OptionResult {
	for i := range options {
		if options[i].Grouping == grouping {
			return &options[i]
		}
	}
	return nil
}

func TestEdgeAcceptanceFixtureContainsNoPrivateOrInstructionalText(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "edge-acceptance-365.xml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"DU", "http://", "https://", "ignore previous", "system prompt"} {
		if strings.Contains(strings.ToLower(string(raw)), strings.ToLower(forbidden)) {
			t.Fatalf("acceptance fixture contains forbidden text %q", forbidden)
		}
	}
}
