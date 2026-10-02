package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFinancingAcceptanceProjectionRPCAndEquityInvariant(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../flexstmt/testdata/lending-synthetic.xml")
	if err != nil {
		t.Fatal(err)
	}
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	port := 7497
	srv := &Server{cfg: &config.Resolved{Gateway: config.Gateway{Account: "DUSYNTHFIN", Port: &port}, Flex: config.Flex{Enabled: true, QueryID: "424242"}}, coreStore: store, now: func() time.Time { return now }}
	project := func(inputs ...[]byte) corestore.StatementProjectionSnapshot {
		t.Helper()
		inputFiles := []statementProjectionFile{}
		for i, data := range inputs {
			statements, parseErr := flexstmt.Parse(data)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			inputFiles = append(inputFiles, statementProjectionFile{name: string(rune('a'+i)) + ".xml", size: int64(len(data)), digest: sha256.Sum256(data), statements: statements})
		}
		files, days, records, versions, buildErr := buildStatementProjection(inputFiles, now, flexQueryFingerprint(srv.cfg.Flex.QueryID))
		if buildErr != nil {
			t.Fatal(buildErr)
		}
		if err := store.ReplaceStatementProjection(t.Context(), srv.activeStatementProjectionScope(), files, days, records, versions); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.LoadStatementProjectionSnapshot(t.Context(), srv.activeStatementProjectionScope(), statementProjectionMaxRows*25, statementProjectionMaxRows)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	snapshot := project(raw, raw) // Repeated import cannot increase earned fees.
	metadata := filterStatementRecords(snapshot.Records, corestore.StatementRecordMetadata)
	var payload statementMetadataProjectionPayload
	if len(metadata) != 1 || json.Unmarshal(metadata[0].RawJSON, &payload) != nil || payload.Financing == nil || len(payload.Financing.Fees) != 2 {
		t.Fatal("typed fee snapshots were not retained")
	}
	scope := srv.currentBrokerStateScope()
	statements, err := edgeStatementsFromProjection(snapshot, scope)
	if err != nil {
		t.Fatal(err)
	}
	core, err := edgecore.Analyze(edgecore.Input{AsOf: now, WindowDays: 365, BaseCurrency: "EUR", Statements: statements})
	if err != nil {
		t.Fatal(err)
	}
	if core.Account == nil || core.Account.ProfitLossBase != 1500 || core.Account.ExternalFlowsBase != 0 {
		t.Fatalf("collateral/fee affected equity: %+v", core.Account)
	}
	if err := srv.saveEdgePublication(t.Context(), edgePublication{ScopeFingerprint: edgeScopeFingerprint(scope), EvidenceFingerprint: edgeProjectionFingerprintSnapshot(snapshot, scope, edgeScopeFingerprint(scope)), State: rpc.EdgeStateCurrent, Windows: map[string]edgecore.Result{"365d": core}, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	edge, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	if edge.Account == nil || edge.Account.ProfitLossBase != 1500 || edge.Account.Financing == nil || edge.Account.Financing.EarnedBase == nil || math.Abs(*edge.Account.Financing.EarnedBase-3.78) > 1e-12 {
		t.Fatalf("Edge lending attribution lost: %+v", edge.Account)
	}
	read := func(params rpc.FinancingFeesParams) (*rpc.FinancingFeesResult, error) {
		data, _ := json.Marshal(params)
		return srv.handleFinancingFees(t.Context(), &rpc.Request{Params: data})
	}
	params := rpc.FinancingFeesParams{Limit: 1, Fingerprint: edge.Account.Financing.Fingerprint}
	first, err := read(params)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Fees) != 1 || first.NextCursor == "" || first.Summary.FeeCount != 2 || first.Summary.PNLReconciliation != "unproved" {
		t.Fatalf("bounded page lost full total: %+v", first)
	}
	params.Cursor = first.NextCursor
	second, err := read(params)
	if err != nil || len(second.Fees) != 1 || second.Fees[0].ID == first.Fees[0].ID || second.NextCursor != "" || *second.Summary.EarnedBase != *first.Summary.EarnedBase {
		t.Fatal("paging changed the total or repeated a row", err)
	}
	params.ConID = 900901
	if _, err := read(params); err == nil {
		t.Fatal("account cursor accepted for different contract filter")
	}
	params.Cursor = ""
	params.ConID = 900902
	filtered, err := read(params)
	if err != nil || len(filtered.Fees) != 0 || filtered.FilteredCount != 0 || filtered.Summary.FeeCount != 2 {
		t.Fatal("contract filtering altered account summary", err)
	}
	positions := &rpc.PositionsResult{Authority: &rpc.AccountDataAuthority{Scope: accountDataScope(scope), Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent}, Stocks: []rpc.PositionView{{ConID: 900901, SecType: "STOCK", Currency: "USD", Quantity: 100}}}
	positions.ByUnderlying = groupByUnderlying(positions.Stocks, nil, "EUR", new(100000.0))
	srv.attachLendingAnnotations(t.Context(), positions, scope)
	if positions.Stocks[0].Lending == nil || positions.Stocks[0].Lending.Quantity != 60 || positions.ByUnderlying[0].Stock.Lending == nil {
		t.Fatal("dated exact-contract annotation missing")
	}
	positions.Stocks[0].Lending = nil
	positions.Stocks[0].Quantity = 20
	positions.ByUnderlying[0].Stock.Lending = nil
	srv.attachLendingAnnotations(t.Context(), positions, scope)
	if positions.Stocks[0].Lending != nil {
		t.Fatal("quantity mismatch was clamped or attached")
	}
	changed := strings.Replace(string(raw), "20261002;020000", "20261002;030000", 1)
	changed = strings.Replace(changed, `netLendFee="2.10"`, `netLendFee="-1.10"`, 1)
	project(raw, []byte(changed))
	if _, err := read(rpc.FinancingFeesParams{Cursor: first.NextCursor}); err == nil {
		t.Fatal("restatement accepted an old cursor")
	}
	if _, err := read(rpc.FinancingFeesParams{Fingerprint: first.Summary.Fingerprint}); err == nil {
		t.Fatal("restatement accepted old Edge fingerprint")
	}
	updated, err := read(rpc.FinancingFeesParams{})
	if err != nil || updated.Summary.EarnedBase == nil || math.Abs(*updated.Summary.EarnedBase-.9) > 1e-12 || updated.Summary.FeeCount != 2 {
		t.Fatal("signed restatement did not replace prior fee", err)
	}
	oldEdge, err := srv.handleEdgeSnapshot(t.Context(), &rpc.Request{Params: []byte(`{}`)})
	if err != nil || oldEdge.Account == nil || oldEdge.Account.ProfitLossBase != 1500 || oldEdge.Account.Financing != nil {
		t.Fatal("old equity publication mixed new financing generation", err)
	}
	updatedFirst, err := read(rpc.FinancingFeesParams{Limit: 1})
	if err != nil || updatedFirst.NextCursor == "" {
		t.Fatal("updated statement could not be paged", err)
	}
	srv.cfg.Flex.QueryID = "424243"
	changedQuery, err := read(rpc.FinancingFeesParams{})
	if err != nil || changedQuery.Summary.State != rpc.FinancingUnavailable || len(changedQuery.Fees) != 0 {
		t.Fatal("old query evidence reused", err)
	}
	project(raw, []byte(changed)) // Identical contents under a different query.
	if _, err := read(rpc.FinancingFeesParams{Fingerprint: updatedFirst.Summary.Fingerprint}); err == nil {
		t.Fatal("new query accepted a previous generation fingerprint")
	}
	if _, err := read(rpc.FinancingFeesParams{Cursor: updatedFirst.NextCursor}); err == nil {
		t.Fatal("new query accepted a previous generation cursor")
	}
	srv.cfg.Flex.QueryID = "424242"
	srv.cfg.Gateway.Account = "DUSYNTHOTHER"
	changedAccount, err := read(rpc.FinancingFeesParams{})
	if err != nil || changedAccount.Summary.State != rpc.FinancingUnavailable || len(changedAccount.Fees) != 0 {
		t.Fatal("other account fees exposed", err)
	}
}
