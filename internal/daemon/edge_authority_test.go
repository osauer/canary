package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
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

func TestEdgeFlexSchemaChangeRestartsOnlyCompletedAcquisition(t *testing.T) {
	completed := edgeFlexAcquisition{SchemaFingerprint: "flex_schema_old", LastFullRevalidation: time.Now()}
	if !edgeFlexSchemaChanged(completed, "flex_schema_new") {
		t.Fatal("completed acquisition ignored a new observed schema")
	}
	inProgress := completed
	inProgress.AnchorDate = time.Now()
	if edgeFlexSchemaChanged(inProgress, "flex_schema_new") {
		t.Fatal("in-progress acquisition would restart on its own chunk evidence")
	}
	if edgeFlexSchemaChanged(completed, "") || edgeFlexSchemaChanged(completed, completed.SchemaFingerprint) {
		t.Fatal("empty or unchanged schema triggered a restart")
	}
}

func TestEdgeBarRefreshPlanRequiresContractSpecificFullSeed(t *testing.T) {
	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-time.Hour)
	proved := now.Add(-24 * time.Hour)
	cases := map[string]struct {
		globalFullDue bool
		series        edgeBarSeries
		wantLookback  int
		wantFetch     bool
	}{
		"new contract despite recent global refresh": {
			series: edgeBarSeries{FetchedAt: recent}, wantLookback: edgeFullLookbackDays, wantFetch: true,
		},
		"recent contract proof skips duplicate refresh": {
			series: edgeBarSeries{FetchedAt: recent, FullRevalidatedAt: proved}, wantFetch: false,
		},
		"stale daily refresh keeps proven contract bounded": {
			series: edgeBarSeries{FetchedAt: now.Add(-edgeDailyRefreshAfter), FullRevalidatedAt: proved}, wantLookback: edgeDailyLookbackDays, wantFetch: true,
		},
		"expired contract proof forces full refresh": {
			series: edgeBarSeries{FetchedAt: recent, FullRevalidatedAt: now.Add(-edgeFullRevalidateAfter)}, wantLookback: edgeFullLookbackDays, wantFetch: true,
		},
		"global refresh forces full refresh": {
			globalFullDue: true, series: edgeBarSeries{FetchedAt: recent, FullRevalidatedAt: proved}, wantLookback: edgeFullLookbackDays, wantFetch: true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			lookback, fetch := edgeBarRefreshPlan(now, tc.globalFullDue, tc.series)
			if lookback != tc.wantLookback || fetch != tc.wantFetch {
				t.Fatalf("plan=(lookback=%d fetch=%v) want (%d %v)", lookback, fetch, tc.wantLookback, tc.wantFetch)
			}
		})
	}
}

func TestFetchEdgeMarketBarsUsesTheFixedSymbolAndNormalizesDailyEvidence(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	srv.edgeFetchMarketBarsFn = func(_ context.Context, symbol string, lookback int) ([]ibkrlib.HistoricalBar, error) {
		if symbol != "VIX" || lookback != edgeFullLookbackDays {
			t.Fatalf("fetch=%s/%d", symbol, lookback)
		}
		return []ibkrlib.HistoricalBar{{Date: "2026-08-24", Close: 14.5}, {Date: "bad", Close: 20}, {Date: "2026-08-25", Close: 0}}, nil
	}
	bars, err := srv.fetchEdgeMarketBars(t.Context(), "VIX", edgeFullLookbackDays)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 || bars[0].ConID != 0 || bars[0].Day.Format("2006-01-02") != "2026-08-24" || bars[0].Close != 14.5 {
		t.Fatalf("market bars=%+v", bars)
	}
}

func TestEdgeBarCacheMigratesV1WithoutInventingContractProof(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	legacy := edgeBarCache{
		Version: 1, LastFullRevalidation: now.Add(-time.Hour),
		Contracts: map[string]edgeBarSeries{"1": {ConID: 1, FetchedAt: now.Add(-time.Hour)}},
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{coreStore: store}
	if err := srv.replaceEdgeStateDocument(t.Context(), edgeBarCacheStateKind, raw); err != nil {
		t.Fatal(err)
	}

	cache, err := srv.loadEdgeBarCache(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	series := cache.Contracts["1"]
	if cache.Version != edgeBarCacheVersion || !series.FullRevalidatedAt.IsZero() || cache.MarketContext == nil {
		t.Fatalf("migrated cache invented contract proof: %+v", cache)
	}
	lookback, fetch := edgeBarRefreshPlan(now, false, series)
	if !fetch || lookback != edgeFullLookbackDays {
		t.Fatalf("migrated series plan=(lookback=%d fetch=%v)", lookback, fetch)
	}
}

func TestEdgePublicationRegeneratesOldSchemaButRejectsFutureSchema(t *testing.T) {
	t.Parallel()
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := &Server{coreStore: store}
	write := func(version int) {
		raw, marshalErr := json.Marshal(edgePublication{Version: version})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if replaceErr := srv.replaceEdgeStateDocument(t.Context(), edgePublicationStateKind, raw); replaceErr != nil {
			t.Fatal(replaceErr)
		}
	}
	write(edgePublicationVersion - 1)
	if _, ok, loadErr := srv.loadEdgePublication(t.Context()); loadErr != nil || ok {
		t.Fatalf("old publication load ok=%v err=%v", ok, loadErr)
	}
	write(edgePublicationVersion + 1)
	if _, _, loadErr := srv.loadEdgePublication(t.Context()); loadErr == nil {
		t.Fatal("future publication schema was accepted")
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

func TestEdgeFlexAcquisitionMigratesV1AndRestartsWhenSameQuerySchemaChanged(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		cfg:       &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "private-query"}},
		coreStore: store,
		now:       func() time.Time { return now },
	}
	queryFingerprint := flexQueryFingerprint(srv.cfg.Flex.QueryID)
	digest := sha256.Sum256([]byte("same-query-current-schema"))
	file := corestore.StatementFileRecord{FileKey: "current.xml", SHA256: digest, Status: statementProjectionStatus}
	payload, err := json.Marshal(statementMetadataProjectionPayload{
		Version: statementProjectionVersion, QueryFingerprint: queryFingerprint,
		FromDate: now.AddDate(0, 0, -34), ToDate: now.AddDate(0, 0, -1), ManifestVersion: flexstmt.ManifestVersion,
		Coverage: []flexstmt.SectionCoverage{{Key: "equity", Present: true, RowCount: 1, ObservedFields: []string{"reportDate", "total"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	record := corestore.StatementRecord{
		Kind: corestore.StatementRecordMetadata, RecordKey: "statement:current", AccountKey: "UEDGE1",
		EffectiveAt: now.AddDate(0, 0, -1), StatementFileKey: file.FileKey, StatementFileSHA256: digest,
		GeneratedAt: now.Add(-time.Hour), RawJSON: payload,
	}
	if err := store.ReplaceStatementProjection(t.Context(), srv.activeStatementProjectionScope(), []corestore.StatementFileRecord{file}, nil, []corestore.StatementRecord{record}, nil); err != nil {
		t.Fatal(err)
	}
	currentSchema, err := srv.latestReportingSchemaFingerprint(t.Context())
	if err != nil || currentSchema == "" {
		t.Fatalf("current schema=%q err=%v", currentSchema, err)
	}
	legacy := edgeFlexAcquisition{
		Version: 1, ScopeFingerprint: "scope_test", QueryFingerprint: queryFingerprint,
		LastFullRevalidation: now.Add(-time.Hour),
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.replaceEdgeStateDocument(t.Context(), edgeFlexAcquisitionStateKind, raw); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	srv.edgeFlexFetchRangeFn = func(_ context.Context, from, to time.Time) (flexFetchOutcome, error) {
		fetches++
		return flexFetchOutcome{CoverageFrom: from, CoverageTo: to, WhenGenerated: now}, nil
	}
	srv.flexProjectionFn = func(context.Context) error { return nil }
	srv.edgeScheduleRebuildFn = func(time.Duration) {}

	progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
	if err != nil {
		t.Fatal(err)
	}
	if !progress.Pending || fetches != 1 {
		t.Fatalf("same-ID schema change did not restart: progress=%+v fetches=%d", progress, fetches)
	}
	state, err := srv.loadEdgeFlexAcquisition(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != edgeFlexAcquisitionVersion || state.SchemaFingerprint != currentSchema || state.NextChunk != 1 || state.AnchorDate.IsZero() {
		t.Fatalf("migrated acquisition state=%+v", state)
	}
}

func TestEdgeFlexAcquisitionAdoptsRecentCompleteReceiptWhenSchemaBasisChanges(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	now := time.Date(2026, time.August, 27, 12, 0, 0, 0, time.UTC)
	srv := &Server{
		cfg:       &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "private-query"}},
		coreStore: store,
		now:       func() time.Time { return now },
	}
	queryFingerprint := flexQueryFingerprint(srv.cfg.Flex.QueryID)
	anchor := latestCompletedFlexDate(now)
	ranges := edgeFlexRanges(anchor)
	files := make([]corestore.StatementFileRecord, 0, len(ranges)+1)
	records := make([]corestore.StatementRecord, 0, len(ranges)+1)
	dailyGenerated := now.Add(-10 * time.Minute)
	dailyDigest := sha256.Sum256([]byte("daily-schema-evidence"))
	dailyRaw, err := json.Marshal(statementMetadataProjectionPayload{
		Version: statementProjectionVersion, QueryFingerprint: queryFingerprint,
		FromDate: anchor.AddDate(0, 0, -(edgeDailyLookbackDays - 1)), ToDate: anchor,
		ManifestVersion: flexstmt.ManifestVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, corestore.StatementFileRecord{FileKey: "daily.xml", SHA256: dailyDigest, Status: statementProjectionStatus})
	records = append(records, corestore.StatementRecord{
		Kind: corestore.StatementRecordMetadata, RecordKey: "statement:daily", AccountKey: "UEDGE1",
		EffectiveAt: anchor, StatementFileKey: "daily.xml", StatementFileSHA256: dailyDigest,
		GeneratedAt: dailyGenerated, RawJSON: dailyRaw,
	})
	var newest time.Time
	for i, item := range ranges {
		generated := now.Add(time.Duration(i-len(ranges)) * time.Minute)
		newest = generated
		digest := sha256.Sum256([]byte(fmt.Sprintf("receipt-%d", i)))
		fileKey := fmt.Sprintf("receipt-%d.xml", i)
		files = append(files, corestore.StatementFileRecord{FileKey: fileKey, SHA256: digest, Status: statementProjectionStatus})
		raw, marshalErr := json.Marshal(statementMetadataProjectionPayload{
			Version: statementProjectionVersion, QueryFingerprint: queryFingerprint,
			FromDate: item.From, ToDate: item.To, ManifestVersion: flexstmt.ManifestVersion,
		})
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		records = append(records, corestore.StatementRecord{
			Kind: corestore.StatementRecordMetadata, RecordKey: fmt.Sprintf("statement:%d", i), AccountKey: "UEDGE1",
			EffectiveAt: item.To, StatementFileKey: fileKey, StatementFileSHA256: digest,
			GeneratedAt: generated, RawJSON: raw,
		})
	}
	if err := store.ReplaceStatementProjection(t.Context(), srv.activeStatementProjectionScope(), files, nil, records, nil); err != nil {
		t.Fatal(err)
	}
	receiptAt, proved, err := srv.recentCompletedEdgeFlexReceipt(t.Context(), queryFingerprint, dailyGenerated, now)
	if err != nil || !proved || !receiptAt.Equal(newest) {
		t.Fatalf("recent receipt=(%s %v) err=%v", receiptAt, proved, err)
	}
	if _, proved, err := srv.recentCompletedEdgeFlexReceipt(t.Context(), queryFingerprint, now, now); err != nil || proved {
		t.Fatalf("pre-schema receipt proved=%v err=%v", proved, err)
	}
	interrupted := edgeFlexAcquisition{
		Version: edgeFlexAcquisitionVersion, ScopeFingerprint: "scope_test", QueryFingerprint: queryFingerprint,
		SchemaFingerprint: "flex_schema_old_selection", AnchorDate: anchor,
		NextAttempt: now.Add(edgeFlexRetryAfter), LastReason: rpc.ReconReportReasonReportNotReady,
	}
	raw, err := json.Marshal(interrupted)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.replaceEdgeStateDocument(t.Context(), edgeFlexAcquisitionStateKind, raw); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	srv.edgeFlexFetchRangeFn = func(context.Context, time.Time, time.Time) (flexFetchOutcome, error) {
		fetches++
		return flexFetchOutcome{}, nil
	}

	progress, err := srv.advanceEdgeFlexAcquisition(t.Context(), "scope_test")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Pending || fetches != 0 || !progress.LastFullRevalidation.Equal(newest) {
		t.Fatalf("receipt migration progress=%+v fetches=%d", progress, fetches)
	}
	state, err := srv.loadEdgeFlexAcquisition(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if state.SchemaBasis != edgeFlexSchemaBasis || state.SchemaFingerprint == "" || !state.AnchorDate.IsZero() || state.LastReason != "" || !state.LastFullRevalidation.Equal(newest) {
		t.Fatalf("receipt-migrated state=%+v", state)
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
	totalOpen, medianOpen := 50.0, 5.0
	totalAdds, medianAdds := -300.0, -100.0
	result := &rpc.EdgeResult{
		Window: "365d", HorizonSessions: 20, Account: &rpc.EdgeAccountResult{BaseCurrency: "USD", StartingEquityBase: 100_000},
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 8, ScoredChanges: 8, CoveragePct: 100, LargestActionSample: 5, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true},
		AutomaticHorizon: true,
		Findings:         []rpc.EdgeFinding{{ChangeID: "change_one"}},
		ActionRollups: []rpc.EdgeActionRollup{
			{Action: edgecore.ActionOpen, Horizons: []rpc.EdgeHorizonRollup{{Sessions: 20, SampleCount: 5, TotalBase: &totalOpen, MedianBase: &medianOpen}}},
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
	if got := edgeHeadline(result); !strings.Contains(got, "completed one-year broker report returned no Trades section") || !strings.Contains(got, "verify Trades at execution detail") || strings.Contains(got, "waiting") {
		t.Fatalf("unproved headline=%q", got)
	}
	result.Coverage.MissingSections = nil
	if got := edgeHeadline(result); !strings.Contains(got, "No stock or ETF position changes") || !strings.Contains(got, "account P/L remains separate") {
		t.Fatalf("zero-trade headline=%q", got)
	}
}

func TestAutomaticEdgeHorizonChoosesLongestAdequatelyCoveredLens(t *testing.T) {
	t.Parallel()
	total, median := 100.0, 25.0
	result := edgecore.Result{
		Coverage: edgecore.Coverage{EligibleChanges: 10, ScoredByHorizon: map[int]int{1: 8, 5: 4, 20: 2}},
		Rollups: []edgecore.ActionRollup{{Action: edgecore.ActionAdd, Horizons: []edgecore.HorizonRollup{
			{Sessions: 1, SampleCount: 4, TotalBase: &total, MedianBase: &median},
			{Sessions: 5, SampleCount: 3, TotalBase: &total, MedianBase: &median},
			{Sessions: 20, SampleCount: 2, TotalBase: &total, MedianBase: &median},
		}}},
	}
	if got := selectAutomaticEdgeHorizon(result); got != 5 {
		t.Fatalf("automatic horizon=%d want 5", got)
	}
	selection := edgeHorizonSelection(result, 5, true)
	if !selection.Adequate || selection.CoveragePct != 40 || selection.LargestActionSample != 3 {
		t.Fatalf("selection=%+v", selection)
	}

	result.Coverage.ScoredByHorizon = map[int]int{1: 3, 5: 2, 20: 1}
	result.Rollups[0].Horizons[0].SampleCount = 2
	result.Rollups[0].Horizons[1].SampleCount = 2
	result.Rollups[0].Horizons[2].SampleCount = 1
	if got := selectAutomaticEdgeHorizon(result); got != 1 {
		t.Fatalf("best available horizon=%d want 1", got)
	}
}

func TestEdgeHeadlineRefusesTinyOrUnderSampledPatterns(t *testing.T) {
	t.Parallel()
	total, median := 1_000.0, 1_000.0
	result := &rpc.EdgeResult{
		Window: "365d", HorizonSessions: 20, AutomaticHorizon: true,
		Account:          &rpc.EdgeAccountResult{BaseCurrency: "USD", StartingEquityBase: 100_000},
		HorizonSelection: rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "best_available", EligibleChanges: 1, ScoredChanges: 1, CoveragePct: 100, LargestActionSample: 1, MinimumSample: 3, MinimumCoveragePct: 25},
		ActionRollups:    []rpc.EdgeActionRollup{{Action: edgecore.ActionAdd, Horizons: []rpc.EdgeHorizonRollup{{Sessions: 20, SampleCount: 1, TotalBase: &total, MedianBase: &median}}}},
		Coverage:         rpc.EdgeCoverage{TradeChanges: 1, EligibleChanges: 1, ScoredByHorizon: map[int]int{20: 1}},
	}
	if got := edgeHeadline(result); strings.Contains(got, "Observed strength") || !strings.Contains(got, "at least 3 is required") {
		t.Fatalf("under-sampled headline=%q", got)
	}

	total, median = 50, 10
	result.HorizonSelection = rpc.EdgeHorizonSelection{Mode: "automatic", Reason: "longest_adequately_covered", EligibleChanges: 3, ScoredChanges: 3, CoveragePct: 100, LargestActionSample: 3, MinimumSample: 3, MinimumCoveragePct: 25, Adequate: true}
	result.ActionRollups[0].Horizons[0] = rpc.EdgeHorizonRollup{Sessions: 20, SampleCount: 3, TotalBase: &total, MedianBase: &median}
	if got := edgeHeadline(result); strings.Contains(got, "Observed strength") || !strings.Contains(got, "account-materiality gates") {
		t.Fatalf("immaterial headline=%q", got)
	}
}

func TestPopulateEdgeResultDisclosesOptionTruncation(t *testing.T) {
	t.Parallel()
	in := edgecore.Result{Coverage: edgecore.Coverage{ScoredByHorizon: map[int]int{}}, Method: edgecore.Method{}}
	for i := range rpc.MaxEdgeOptionResults + 5 {
		value := float64(i)
		in.Options = append(in.Options, edgecore.OptionResult{ID: fmt.Sprintf("option_%02d", i), Grouping: "contract", Symbol: "SYN", LegCount: 1, ActualPNLBase: &value, ActualOnly: true})
	}
	out := edgeStateOnlyResult(rpc.EdgeStateCurrent, "", "365d", 20, true)
	populateRPCEdgeResult(out, in, 20, 3, true)
	if len(out.Options) != rpc.MaxEdgeOptionResults || out.OptionsTotalCount != rpc.MaxEdgeOptionResults+5 || !out.OptionsTruncated {
		t.Fatalf("option disclosure=%d/%d truncated=%v", len(out.Options), out.OptionsTotalCount, out.OptionsTruncated)
	}
}

func TestPopulateEdgeResultNamesEveryUnavailableMarketBenchmark(t *testing.T) {
	t.Parallel()
	total, median := 200.0, 50.0
	in := edgecore.Result{
		Account:  &edgecore.AccountResult{StartingEquityBase: 100_000},
		Coverage: edgecore.Coverage{EligibleChanges: 3, ScoredByHorizon: map[int]int{20: 3}},
		Rollups:  []edgecore.ActionRollup{{Action: edgecore.ActionAdd, Horizons: []edgecore.HorizonRollup{{Sessions: 20, SampleCount: 3, TotalBase: &total, MedianBase: &median}}}},
	}
	out := edgeStateOnlyResult(rpc.EdgeStateDegraded, "context_test", "365d", 20, true)
	populateRPCEdgeResult(out, in, 20, 3, true)
	if !slices.Equal(out.MarketContextMissing, []string{"spy", "qqq", "dia", "vix"}) || len(out.MarketContext) != 0 {
		t.Fatalf("market context disclosure=%+v missing=%v", out.MarketContext, out.MarketContextMissing)
	}
}

func TestUnprovedTradeHistoryCarriesSetupWithoutHidingAccountEvidence(t *testing.T) {
	t.Parallel()
	result := edgeStateOnlyResult(rpc.EdgeStateInsufficient, "trade_history_unproved", "365d", 20, true)
	if result.Setup == nil || len(result.Setup.Steps) != 3 || len(result.Setup.Sections) == 0 {
		t.Fatalf("unproved trade setup=%+v", result.Setup)
	}
	if err := rpc.ValidateEdgeResult(*result); err != nil {
		t.Fatalf("unproved trade setup rejected: %v", err)
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
