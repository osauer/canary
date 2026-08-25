package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFlexQueryRotationKeepsRetiredEvidenceOutOfActiveProjection(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flex-legacy.xml"), flexGenerationFixture(true), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := &Server{
		cfg:       &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "111"}},
		coreStore: store,
		now:       func() time.Time { return time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC) },
	}
	if err := srv.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldSelection := srv.flexEvidenceSelection()
	oldScope := statementProjectionScopeForSelection(oldSelection)
	legacyRecords, err := store.LoadStatementRecords(t.Context(), oldScope, []string{corestore.StatementRecordMetadata}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacyRecords) != 0 {
		t.Fatalf("unknown-provenance legacy evidence entered configured projection: %d record(s)", len(legacyRecords))
	}
	if _, err := srv.retainFlexStatementForQuery(t.Context(), flexGenerationFixture(true), oldSelection.ActiveQueryFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := srv.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldRecords, err := store.LoadStatementRecords(t.Context(), oldScope, []string{corestore.StatementRecordMetadata}, 10)
	if err != nil || len(oldRecords) != 1 {
		t.Fatalf("legacy active projection records=%d err=%v", len(oldRecords), err)
	}
	oldStatements, err := reportingStatementsFromMetadata(oldRecords)
	if err != nil {
		t.Fatal(err)
	}
	oldTradeStatus := ""
	for _, section := range flexstmt.QueryRequirementEvidence(oldStatements) {
		if section.Key == "trades" {
			oldTradeStatus = section.Status
		}
	}
	if oldTradeStatus != flexstmt.QueryRequirementObserved {
		t.Fatalf("retired fixture did not establish complete Trades evidence: %q", oldTradeStatus)
	}

	srv.cfg.Flex.QueryID = "222"
	newFingerprint := flexQueryFingerprint("222")
	if _, err := srv.retainFlexStatementForQuery(t.Context(), flexGenerationFixture(false), newFingerprint); err != nil {
		t.Fatal(err)
	}
	if err := srv.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	newSelection := srv.flexEvidenceSelection()
	if newSelection.ActiveQueryFingerprint != newFingerprint {
		t.Fatalf("query generation binding=%+v", newSelection)
	}
	newRecords, err := store.LoadStatementRecords(t.Context(), statementProjectionScopeForSelection(newSelection), []string{corestore.StatementRecordMetadata}, 10)
	if err != nil || len(newRecords) != 1 {
		t.Fatalf("replacement active projection records=%d err=%v", len(newRecords), err)
	}
	statements, err := reportingStatementsFromMetadata(newRecords)
	if err != nil {
		t.Fatal(err)
	}
	var tradeStatus string
	for _, section := range flexstmt.QueryRequirementEvidence(statements) {
		if section.Key == "trades" {
			tradeStatus = section.Status
		}
	}
	if tradeStatus != flexstmt.QueryRequirementUnproved {
		t.Fatalf("retired complete Trades evidence certified replacement: status=%q", tradeStatus)
	}
	srv.flexFetch.state = flexFetchStateV2{
		Version: flexFetchStateVersion, QueryFingerprint: oldSelection.ActiveQueryFingerprint,
		Stage: rpc.ReconReportStateActionRequired, LastReason: rpc.ReconReportReasonQueryInvalid,
		LastBrokerCode: "1012",
	}
	status := srv.flexFetchStatusAt(srv.now())
	if status.State == rpc.ReconReportStateActionRequired || status.BrokerCode != "" || status.Reason == rpc.ReconReportReasonQueryInvalid {
		t.Fatalf("retired query broker status certified replacement: %+v", status)
	}

	allStatements, problems, err := loadRetainedFlexStatements()
	if err != nil || len(problems) != 0 || len(allStatements) != 3 {
		t.Fatalf("immutable retained evidence statements=%d problems=%v err=%v", len(allStatements), problems, err)
	}
}

func flexGenerationFixture(withTrade bool) []byte {
	trade := ""
	to, generated := "20260825", "20260825;120000"
	if withTrade {
		to, generated = "20260824", "20260824;120000"
		trade = `<Trade accountId="U" assetCategory="STK" currency="USD" fxRateToBase="1" symbol="ACME" conid="123" underlyingConid="0" underlyingSymbol="" multiplier="1" tradeID="t1" ibOrderID="o1" ibExecID="e1" transactionID="x1" tradeDate="20260820" tradeTime="120000" buySell="BUY" quantity="1" tradePrice="10" proceeds="-10" IBCommission="0" IBCommissionCurrency="USD" taxes="0" openCloseIndicator="O" cost="10" fifoPnlRealized="0" mtmPnl="0" closePrice="10" netCash="-10" levelOfDetail="EXECUTION"/>`
	}
	return []byte(`<FlexQueryResponse><FlexStatements><FlexStatement accountId="U" fromDate="20260801" toDate="` + to + `" whenGenerated="` + generated + `"><Trades>` + trade + `</Trades><EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="` + to + `" total="100"/></EquitySummaryInBase></FlexStatement></FlexStatements></FlexQueryResponse>`)
}
