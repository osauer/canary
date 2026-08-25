package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestBuildStatementProjectionKeepsLosingTypedVersions(t *testing.T) {
	t.Parallel()
	oldPrice, newPrice := 100.0, 101.0
	oldGenerated := time.Date(2026, time.August, 23, 12, 0, 0, 0, time.UTC)
	newGenerated := oldGenerated.Add(time.Hour)
	statement := func(generated time.Time, price *float64) flexstmt.Statement {
		return flexstmt.Statement{
			AccountID:     "U",
			ToDate:        generated,
			WhenGenerated: generated,
			Trades: []flexstmt.Trade{{
				RecordID: "trade-stable", AccountID: "U", ConID: 123,
				ExecutedAt: oldGenerated.Add(-24 * time.Hour), Price: price,
			}},
		}
	}
	files := []statementProjectionFile{
		{name: "old.xml", digest: sha256.Sum256([]byte("old")), statements: []flexstmt.Statement{statement(oldGenerated, &oldPrice)}},
		{name: "new.xml", digest: sha256.Sum256([]byte("new")), statements: []flexstmt.Statement{statement(newGenerated, &newPrice)}},
	}
	_, _, current, versions, err := buildStatementProjection(files, newGenerated, "")
	if err != nil {
		t.Fatal(err)
	}
	currentTrades, versionTrades := filterStatementRecords(current, "trade"), filterStatementRecords(versions, "trade")
	if len(currentTrades) != 1 || len(versionTrades) != 2 {
		t.Fatalf("current trades=%d version trades=%d want 1/2", len(currentTrades), len(versionTrades))
	}
	var winner flexstmt.Trade
	if err := json.Unmarshal(currentTrades[0].RawJSON, &winner); err != nil {
		t.Fatal(err)
	}
	if winner.Price == nil || *winner.Price != newPrice || currentTrades[0].StatementFileKey != "new.xml" {
		t.Fatalf("current typed winner=%+v source=%q", winner, currentTrades[0].StatementFileKey)
	}
}

func TestProjectedEmptyOpenPositionsSnapshotRetractsOlderAnchor(t *testing.T) {
	t.Parallel()
	account := "U"
	conID := int64(123)
	one, zero, ten := 1.0, 0.0, 10.0
	buyAt := time.Date(2026, time.January, 5, 15, 0, 0, 0, time.UTC)
	sellAt := time.Date(2026, time.January, 20, 15, 0, 0, 0, time.UTC)
	oldTo := time.Date(2026, time.January, 10, 0, 0, 0, 0, time.UTC)
	newTo := time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC)
	coverage := []flexstmt.SectionCoverage{{Key: "open_positions", Present: true}}
	trade := func(id, side string, at time.Time) flexstmt.Trade {
		return flexstmt.Trade{
			RecordID: id, AccountID: account, ConID: conID, Symbol: "ACME", AssetClass: "STK", Currency: "USD",
			ExecutedAt: at, Side: side, Quantity: &ten, Price: &ten, Multiplier: &one,
			Commission: &zero, CommissionCurrency: "USD", Taxes: &zero, FXRateToBase: &one, LevelOfDetail: "EXECUTION",
		}
	}
	old := flexstmt.Statement{
		AccountID: account, FromDate: buyAt, ToDate: oldTo, WhenGenerated: oldTo.Add(12 * time.Hour),
		ManifestVersion: flexstmt.ManifestVersion, Coverage: coverage,
		Trades:    []flexstmt.Trade{trade("buy", "BUY", buyAt)},
		Positions: []flexstmt.OpenPosition{{RecordID: "position", AccountID: account, ConID: conID, ReportDate: oldTo, Quantity: &ten}},
	}
	current := flexstmt.Statement{
		AccountID: account, FromDate: buyAt, ToDate: newTo, WhenGenerated: newTo.Add(12 * time.Hour),
		ManifestVersion: flexstmt.ManifestVersion, Coverage: coverage,
		Trades:    []flexstmt.Trade{trade("buy", "BUY", buyAt), trade("sell", "SELL", sellAt)},
		Positions: []flexstmt.OpenPosition{},
	}
	files := []statementProjectionFile{
		{name: "old.xml", size: 1, digest: sha256.Sum256([]byte("old")), statements: []flexstmt.Statement{old}},
		{name: "new.xml", size: 1, digest: sha256.Sum256([]byte("new")), statements: []flexstmt.Statement{current}},
	}
	_, days, records, _, err := buildStatementProjection(files, newTo, flexQueryFingerprint("123"))
	if err != nil {
		t.Fatal(err)
	}
	statements, err := edgeStatementsFromProjection(corestore.StatementProjectionSnapshot{Records: records, EquityDays: days}, brokerStateScope{Account: account})
	if err != nil {
		t.Fatal(err)
	}
	result, err := edgecore.Analyze(edgecore.Input{AsOf: newTo.AddDate(0, 0, 1), WindowDays: 90, BaseCurrency: "USD", Statements: statements})
	if err != nil {
		t.Fatal(err)
	}
	if result.Coverage.ReasonCounts[edgecore.ReasonPositionPathUnbalanced] != 0 {
		t.Fatalf("new empty position snapshot left path unbalanced: %+v", result.Coverage.ReasonCounts)
	}
	if len(result.Changes) != 2 || result.Changes[1].Action != edgecore.ActionExit {
		t.Fatalf("position replay did not reach the broker-proven zero anchor: %+v", result.Changes)
	}
}

func filterStatementRecords(records []corestore.StatementRecord, kind string) []corestore.StatementRecord {
	out := []corestore.StatementRecord{}
	for _, record := range records {
		if record.Kind == kind {
			out = append(out, record)
		}
	}
	return out
}
