package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestEdgeFlexRangesCoverOneInclusiveYearWithoutOverlap(t *testing.T) {
	anchor := time.Date(2026, time.August, 24, 18, 45, 0, 0, time.FixedZone("test", 2*60*60))
	ranges := edgeFlexRanges(anchor)
	if len(ranges) != 4 {
		t.Fatalf("range count=%d want 4", len(ranges))
	}
	wantSizes := []int{92, 91, 91, 91}
	total := 0
	for i, item := range ranges {
		days := int(item.To.Sub(item.From).Hours()/24) + 1
		if days != wantSizes[i] {
			t.Fatalf("range %d days=%d want %d", i, days, wantSizes[i])
		}
		total += days
		if i > 0 && !ranges[i-1].To.AddDate(0, 0, 1).Equal(item.From) {
			t.Fatalf("range %d is not contiguous", i)
		}
	}
	if total != 365 || !ranges[0].From.Equal(time.Date(2025, time.August, 25, 0, 0, 0, 0, time.UTC)) || !ranges[3].To.Equal(time.Date(2026, time.August, 24, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected yearly coverage: %+v", ranges)
	}
}

func TestEdgeFlexAcquisitionResumesDurablyOnePacedChunkAtATime(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	var fetched []edgeFlexRange
	projectionCalls := 0
	var scheduled []time.Duration
	newServer := func() *Server {
		srv := &Server{coreStore: store, now: func() time.Time { return now }}
		srv.edgeFlexFetchRangeFn = func(_ context.Context, from, to time.Time) (flexFetchOutcome, error) {
			fetched = append(fetched, edgeFlexRange{From: from, To: to})
			return flexFetchOutcome{Path: "/private/raw/account-statement.xml", CoverageFrom: from, CoverageTo: to, WhenGenerated: now}, nil
		}
		srv.flexProjectionFn = func(context.Context) error { projectionCalls++; return nil }
		srv.edgeScheduleRebuildFn = func(delay time.Duration) { scheduled = append(scheduled, delay) }
		return srv
	}

	srv := newServer()
	for i := range 2 {
		progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
		if err != nil {
			t.Fatal(err)
		}
		if !progress.Pending {
			t.Fatalf("chunk %d unexpectedly completed acquisition", i)
		}
		now = now.Add(edgeFlexPace)
	}
	// A fresh Server proves progress lives in SQLite, not worker memory.
	srv = newServer()
	for i := 2; i < 4; i++ {
		progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
		if err != nil {
			t.Fatal(err)
		}
		if (i < 3) != progress.Pending {
			t.Fatalf("chunk %d pending=%v", i, progress.Pending)
		}
		if i == 3 && !progress.LastFullRevalidation.Equal(now) {
			t.Fatalf("last full revalidation=%s want %s", progress.LastFullRevalidation, now)
		}
		now = now.Add(edgeFlexPace)
	}

	want := edgeFlexRanges(latestCompletedFlexDate(time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)))
	if len(fetched) != len(want) || projectionCalls != 4 {
		t.Fatalf("fetched=%d projection calls=%d", len(fetched), projectionCalls)
	}
	for i := range want {
		if fetched[i] != want[i] {
			t.Fatalf("range %d=%+v want %+v", i, fetched[i], want[i])
		}
	}
	if len(scheduled) != 3 {
		t.Fatalf("scheduled rebuilds=%v want three paced continuations", scheduled)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, edgeFlexAcquisitionStateKind)
	if err != nil || !ok {
		t.Fatalf("load acquisition document: ok=%v err=%v", ok, err)
	}
	if string(doc.JSON) == "" || strings.Contains(string(doc.JSON), "/private/raw") {
		t.Fatalf("acquisition state exposed a retained evidence path: %s", doc.JSON)
	}
}

func TestEdgeFlexAcquisitionDoesNotAdvanceWithoutReturnedRangeProof(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	projectionCalls := 0
	srv := &Server{coreStore: store, now: func() time.Time { return now }}
	srv.edgeFlexFetchRangeFn = func(_ context.Context, from, to time.Time) (flexFetchOutcome, error) {
		return flexFetchOutcome{CoverageFrom: from.AddDate(0, 0, 1), CoverageTo: to, WhenGenerated: now}, nil
	}
	srv.flexProjectionFn = func(context.Context) error { projectionCalls++; return nil }
	srv.edgeScheduleRebuildFn = func(time.Duration) {}

	progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Pending || progress.State != rpc.EdgeStateBackfilling || progress.Reason != rpc.ReconReportReasonReportNotReady {
		t.Fatalf("short-range progress=%+v", progress)
	}
	if projectionCalls != 0 {
		t.Fatalf("short-range response refreshed projection %d times", projectionCalls)
	}
	state, err := srv.loadEdgeFlexAcquisition(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.NextChunk != 0 || state.LastFullRevalidation != (time.Time{}) || state.NextAttempt.IsZero() {
		t.Fatalf("short-range response advanced acquisition: %+v", state)
	}
}

func TestEdgeFlexAcquisitionRestartsWhenCanonicalQueryChanges(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	queryID := "private-query-one"
	var fetched []edgeFlexRange
	srv := &Server{
		cfg:       &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: queryID}},
		coreStore: store,
		now:       func() time.Time { return now },
	}
	srv.edgeFlexFetchRangeFn = func(_ context.Context, from, to time.Time) (flexFetchOutcome, error) {
		fetched = append(fetched, edgeFlexRange{From: from, To: to})
		return flexFetchOutcome{CoverageFrom: from, CoverageTo: to, WhenGenerated: now}, nil
	}
	srv.flexProjectionFn = func(context.Context) error { return nil }
	srv.edgeScheduleRebuildFn = func(time.Duration) {}

	for range edgeFlexChunkCount {
		if _, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test"); err != nil {
			t.Fatal(err)
		}
		now = now.Add(edgeFlexPace)
	}
	if len(fetched) != edgeFlexChunkCount {
		t.Fatalf("initial fetch count=%d want %d", len(fetched), edgeFlexChunkCount)
	}

	srv.cfg.Flex.QueryID = "private-query-two"
	progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Pending || len(fetched) != edgeFlexChunkCount+1 {
		t.Fatalf("query replacement did not restart backfill: progress=%+v fetched=%d", progress, len(fetched))
	}
	wantFirst := edgeFlexRanges(latestCompletedFlexDate(now))[0]
	if fetched[len(fetched)-1] != wantFirst {
		t.Fatalf("replacement first range=%+v want %+v", fetched[len(fetched)-1], wantFirst)
	}
	doc, ok, err := store.GetStateDocument(t.Context(), daemonStateScope, edgeFlexAcquisitionStateKind)
	if err != nil || !ok {
		t.Fatalf("load acquisition document: ok=%v err=%v", ok, err)
	}
	stateJSON := string(doc.JSON)
	if strings.Contains(stateJSON, "private-query-one") || strings.Contains(stateJSON, "private-query-two") {
		t.Fatalf("acquisition state exposed a Flex query id: %s", stateJSON)
	}
	if !strings.Contains(stateJSON, "query_") {
		t.Fatalf("acquisition state omitted opaque query binding: %s", stateJSON)
	}
}

func TestEdgeSnapshotReadMakesNoFlexOrMarketDataRequest(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	port := 4001
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: "UEDGE1", Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "private-query"}},
		coreStore: store,
		now:       func() time.Time { return now },
	}
	var flexCalls, marketCalls int
	srv.edgeFlexFetchRangeFn = func(context.Context, time.Time, time.Time) (flexFetchOutcome, error) {
		flexCalls++
		return flexFetchOutcome{}, nil
	}
	srv.edgeFetchBarsFn = func(context.Context, ibkrlib.Contract, int) ([]ibkrlib.HistoricalBar, error) {
		marketCalls++
		return nil, nil
	}
	result90, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 90, BaseCurrency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	result365, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 365, BaseCurrency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	scope := srv.currentBrokerStateScope()
	if err := srv.saveEdgePublication(t.Context(), edgePublication{
		ScopeFingerprint: edgeScopeFingerprint(scope), State: rpc.EdgeStateCurrent,
		Windows: map[string]edgecore.Result{"90d": result90, "365d": result365}, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != rpc.EdgeStateCurrent || got.Window != "365d" || got.HorizonSessions != 20 || !got.NotExecution || got.Fingerprint == "" {
		t.Fatalf("unexpected Edge result: %+v", got)
	}
	if flexCalls != 0 || marketCalls != 0 {
		t.Fatalf("read triggered broker work: Flex=%d HMDS=%d", flexCalls, marketCalls)
	}
}

func TestEdgeSnapshotMarksChangedProjectionDegradedWithoutBrokerRequest(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	port := 4001
	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		cfg:       &config.Resolved{Gateway: config.Gateway{Account: "UEDGE1", Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "private-query"}},
		coreStore: store,
		now:       func() time.Time { return now },
	}
	scope := srv.currentBrokerStateScope()
	scopeFingerprint := edgeScopeFingerprint(scope)
	oldEvidence, err := srv.edgeProjectionFingerprint(t.Context(), scope, scopeFingerprint)
	if err != nil {
		t.Fatal(err)
	}
	result90, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 90, BaseCurrency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	result365, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 365, BaseCurrency: "USD"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{
		ScopeFingerprint: scopeFingerprint, EvidenceFingerprint: oldEvidence,
		State: rpc.EdgeStateCurrent, Windows: map[string]edgecore.Result{"90d": result90, "365d": result365}, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	file := corestore.StatementFileRecord{FileKey: "retained.xml", SHA256: sha256.Sum256([]byte("retained")), Status: "valid"}
	record := corestore.StatementRecord{
		Kind: corestore.StatementRecordTrade, RecordKey: "rec_opaque", AccountKey: "UEDGE1",
		EffectiveAt: now, StatementFileKey: file.FileKey, StatementFileSHA256: file.SHA256,
		GeneratedAt: now, RawJSON: []byte(`{"price":100}`),
	}
	projectionScope := srv.activeStatementProjectionScope()
	if err := store.ReplaceStatementProjection(t.Context(), projectionScope, []corestore.StatementFileRecord{file}, nil, []corestore.StatementRecord{record}, nil); err != nil {
		t.Fatal(err)
	}
	var flexCalls, marketCalls int
	srv.edgeFlexFetchRangeFn = func(context.Context, time.Time, time.Time) (flexFetchOutcome, error) {
		flexCalls++
		return flexFetchOutcome{}, nil
	}
	srv.edgeFetchBarsFn = func(context.Context, ibkrlib.Contract, int) ([]ibkrlib.HistoricalBar, error) {
		marketCalls++
		return nil, nil
	}

	got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != rpc.EdgeStateDegraded || got.Reason != "evidence_changed" || got.Fingerprint == "" {
		t.Fatalf("changed-evidence result=%+v", got)
	}
	if flexCalls != 0 || marketCalls != 0 {
		t.Fatalf("degraded read triggered broker work: Flex=%d HMDS=%d", flexCalls, marketCalls)
	}
}

func TestEdgeSnapshotRejectsUnboundedParametersBeforeReadingAuthority(t *testing.T) {
	srv := &Server{}
	for name, raw := range map[string]string{
		"window":     `{"window":"all"}`,
		"horizon":    `{"horizon_sessions":10}`,
		"limit low":  `{"limit":-1}`,
		"limit high": `{"limit":21}`,
		"change":     `{"change_id":"broker-order-123"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(raw)}); err == nil {
				t.Fatal("invalid Edge parameter was accepted")
			}
		})
	}
}

func TestEdgeSnapshotRejectsPublicationFromAnotherAccountScope(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	port := 4001
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: "UEDGE1", Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "private-query"}}, coreStore: store}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{ScopeFingerprint: edgeScopeFingerprint(srv.currentBrokerStateScope()), State: rpc.EdgeStateCurrent}); err != nil {
		t.Fatal(err)
	}
	srv.cfg.Gateway.Account = "UEDGE2"
	got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != rpc.EdgeStateUnavailable || got.Reason != "account_scope_changed" || got.Fingerprint != "" || got.Account != nil {
		t.Fatalf("cross-scope publication escaped: %+v", got)
	}
}

func TestEdgeSnapshotKeepsFlexSetupVisibleAfterUnscopedPublication(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	port := 4001
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: "UEDGE1", Port: &port}}, coreStore: store}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{State: rpc.EdgeStateActionRequired, Reason: "flex_configuration_required"}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got.State != rpc.EdgeStateActionRequired || got.Setup == nil || len(got.Setup.Steps) != 3 {
		t.Fatalf("Flex setup disappeared: %+v", got)
	}
}

func TestEdgeSnapshotCarriesExactMissingQueryRequirements(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	port := 4001
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: "UEDGE1", Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "private-query"}}, coreStore: store}
	missing := []string{"trades.ibOrderID", "open_positions.markPrice"}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{ScopeFingerprint: edgeScopeFingerprint(srv.currentBrokerStateScope()), State: rpc.EdgeStateActionRequired, Reason: "flex_query_incomplete", MissingRequirements: missing}); err != nil {
		t.Fatal(err)
	}
	got, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if got.Setup == nil || !slices.Equal(got.Setup.MissingRequirements, missing) {
		t.Fatalf("missing requirements were lost: %+v", got.Setup)
	}
}

func TestEdgeProjectedEvidenceUsesTheSQLiteRestatementWinner(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	generated := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	from, to := generated.AddDate(0, 0, -30), generated.AddDate(0, 0, -1)
	statement := func(price float64) flexstmt.Statement {
		return flexstmt.Statement{
			AccountID: "UEDGE1", FromDate: from, ToDate: to, WhenGenerated: generated, ManifestVersion: flexstmt.ManifestVersion,
			Trades: []flexstmt.Trade{{RecordID: "same-record", AccountID: "UEDGE1", ConID: 123, ExecutedAt: from.Add(time.Hour), Price: new(price)}},
		}
	}
	lowDigest, highDigest := [sha256.Size]byte{1}, [sha256.Size]byte{2}
	files := []statementProjectionFile{
		{name: "lexically-larger-payload.xml", size: 1, digest: lowDigest, statements: []flexstmt.Statement{statement(999)}},
		{name: "authoritative-digest-winner.xml", size: 1, digest: highDigest, statements: []flexstmt.Statement{statement(1)}},
	}
	fileRows, days, records, versions, err := buildStatementProjection(files, generated, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceStatementProjection(t.Context(), statementProjectionScope, fileRows, days, records, versions); err != nil {
		t.Fatal(err)
	}
	srv := &Server{coreStore: store}
	scope := brokerStateScope{Account: "UEDGE1", Mode: rpc.AccountModeLive}
	evidence, err := srv.loadEdgeProjectionEvidence(t.Context(), scope, edgeScopeFingerprint(scope))
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Statements) != 1 || len(evidence.Statements[0].Trades) != 1 || evidence.Statements[0].Trades[0].Price == nil || *evidence.Statements[0].Trades[0].Price != 1 {
		t.Fatalf("Edge did not consume the current SQLite winner: %+v", evidence.Statements)
	}
	if !strings.HasPrefix(evidence.Fingerprint, "evidence_") {
		t.Fatalf("projection fingerprint=%q", evidence.Fingerprint)
	}
}

func TestEdgeStatementsForScopeExcludesSiblingAccounts(t *testing.T) {
	statements := []flexstmt.Statement{{AccountID: "UEDGE1"}, {AccountID: "uedge2"}, {AccountID: " UEDGE1 "}}
	got := edgeStatementsForScope(statements, brokerStateScope{Account: "uedge1", Mode: rpc.AccountModeLive})
	if len(got) != 2 {
		t.Fatalf("scoped statements=%+v", got)
	}
}

func TestInferEdgeBaseCurrencyUsesBrokerConversionEvidence(t *testing.T) {
	one := 1.0
	pointNine := .9
	if got := inferEdgeBaseCurrency([]flexstmt.Statement{{FXRates: []flexstmt.FXRate{{ToCurrency: "EUR", Rate: &pointNine}}}}); got != "EUR" {
		t.Fatalf("conversion target base=%q", got)
	}
	if got := inferEdgeBaseCurrency([]flexstmt.Statement{{Trades: []flexstmt.Trade{{Currency: "USD", FXRateToBase: &one}}}}); got != "USD" {
		t.Fatalf("identity-rate base=%q", got)
	}
	if got := inferEdgeBaseCurrency([]flexstmt.Statement{{Trades: []flexstmt.Trade{{Currency: "USD", FXRateToBase: &one}, {Currency: "CAD", FXRateToBase: &one}}}}); got != "" {
		t.Fatalf("ambiguous identity-rate base=%q", got)
	}
}

func TestEdgePublicationSeparatesUnprovedTradesFromProvenZeroTrades(t *testing.T) {
	t.Parallel()
	windows := map[string]edgecore.Result{"365d": {Coverage: edgecore.Coverage{PresentSections: []string{"equity"}}}}
	if state, reason := edgePublicationStatus(windows, 0); state != rpc.EdgeStateInsufficient || reason != "trade_history_unproved" {
		t.Fatalf("unproved trade history=%s/%s", state, reason)
	}
	windows["365d"] = edgecore.Result{Coverage: edgecore.Coverage{PresentSections: []string{"trades"}}}
	if state, reason := edgePublicationStatus(windows, 0); state != rpc.EdgeStateCurrent || reason != "no_trade_changes" {
		t.Fatalf("proven empty trade history=%s/%s", state, reason)
	}
}

func TestEdgeHeadlineUsesMostObservedActionAndExplainsEmptyEvidence(t *testing.T) {
	t.Parallel()
	totalOpen, medianOpen := 1_000.0, 1_000.0
	totalAdds, medianAdds := -300.0, -100.0
	result := &rpc.EdgeResult{
		Window: "365d", HorizonSessions: 20, Account: &rpc.EdgeAccountResult{BaseCurrency: "USD"},
		Findings: []rpc.EdgeFinding{{ChangeID: "change_one"}},
		ActionRollups: []rpc.EdgeActionRollup{
			{Action: edgecore.ActionOpen, Horizons: []rpc.EdgeHorizonRollup{{Sessions: 20, SampleCount: 1, TotalBase: &totalOpen, MedianBase: &medianOpen}}},
			{Action: edgecore.ActionAdd, Horizons: []rpc.EdgeHorizonRollup{{Sessions: 20, SampleCount: 3, TotalBase: &totalAdds, MedianBase: &medianAdds}}},
		},
	}
	if got := edgeHeadline(result); !strings.Contains(got, "Observed drag: across 3 clean adds") || !strings.Contains(got, "totaled -300.00 USD") || !strings.Contains(got, "median -100.00 USD") {
		t.Fatalf("headline=%q", got)
	}
	totalAdds, medianAdds = 300, 100
	if got := edgeHeadline(result); !strings.HasPrefix(got, "Observed strength:") {
		t.Fatalf("positive pattern headline=%q", got)
	}
	medianAdds = -100
	if got := edgeHeadline(result); !strings.HasPrefix(got, "Mixed observed pattern:") {
		t.Fatalf("mixed pattern headline=%q", got)
	}
	result.Findings = nil
	result.ActionRollups = nil
	result.Coverage.MissingSections = []string{"trades"}
	if got := edgeHeadline(result); !strings.Contains(got, "waiting for broker-confirmed trade history") || !strings.Contains(got, "retries automatically") {
		t.Fatalf("unproved headline=%q", got)
	}
	result.Coverage.MissingSections = nil
	if got := edgeHeadline(result); !strings.Contains(got, "No stock or ETF position changes") || !strings.Contains(got, "account P/L remains separate") {
		t.Fatalf("zero-trade headline=%q", got)
	}
}

func TestEdgeSubsystemHealthProjectsSnapshotStateWithoutBrokerWork(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "private-query"}}, coreStore: store}
	if got := srv.edgeSubsystemHealth(); got.Name != "edge" || got.Status != "computing" {
		t.Fatalf("cold Edge health = %+v", got)
	}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{State: rpc.EdgeStateCurrent}); err != nil {
		t.Fatal(err)
	}
	if got := srv.edgeSubsystemHealth(); got.Status != "ready" {
		t.Fatalf("current Edge health = %+v", got)
	}
	srv.edgeBusy.Store(true)
	if got := srv.edgeSubsystemHealth(); got.Status != "computing" {
		t.Fatalf("refreshing Edge health = %+v", got)
	}
}
