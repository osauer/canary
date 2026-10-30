package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"path/filepath"
	"slices"
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

// A daily year-to-date Flex file repeats every earlier row. An unchanged row
// stays on the file that first reported it, so the refresh writes no current
// update for it; a changed row moves to the newest file, a row that changes
// back starts a new run, and metadata keeps the newest generation as the
// report's freshness receipt. Removing the first file moves its rows to the
// next file in the run.
func TestStatementProjectionKeepsUnchangedRowsOnTheirFirstFile(t *testing.T) {
	t.Parallel()
	account, ytd := "U", time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
	price := func(v float64) *float64 { return &v }
	daily := func(name string, to time.Time, changing float64, equity ...float64) statementProjectionFile {
		statement := flexstmt.Statement{
			AccountID: account, FromDate: ytd, ToDate: to, WhenGenerated: to.Add(30 * time.Hour),
			ManifestVersion: flexstmt.ManifestVersion,
			Trades: []flexstmt.Trade{
				{RecordID: "same", AccountID: account, ConID: 1, ExecutedAt: ytd.Add(24 * time.Hour), Price: price(10)},
				{RecordID: "changing", AccountID: account, ConID: 2, ExecutedAt: ytd.Add(48 * time.Hour), Price: price(changing)},
			},
		}
		for i, total := range equity {
			statement.Equity = append(statement.Equity, flexstmt.EquityRow{ReportDate: ytd.AddDate(0, 0, i), TotalBase: total})
		}
		return statementProjectionFile{name: name, size: 1, digest: sha256.Sum256([]byte(name)), statements: []flexstmt.Statement{statement}}
	}
	day := func(n int) time.Time { return time.Date(2026, time.October, n, 0, 0, 0, 0, time.UTC) }
	f27 := daily("flex-20261027.xml", day(27), 100, 1000)
	f28 := daily("flex-20261028.xml", day(28), 101, 1000, 1010)
	f29 := daily("flex-20261029.xml", day(29), 100, 1000, 1010)
	// The retained set reads name-descending, newest first.
	sources := func(files ...statementProjectionFile) (map[string]string, []corestore.StatementEquityDayRecord, []corestore.StatementRecord) {
		t.Helper()
		_, days, records, _, err := buildStatementProjection(files, day(30), "")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, row := range days {
			got["day "+row.Day] = row.StatementFileKey
		}
		for _, record := range records {
			if record.Kind == corestore.StatementRecordTrade {
				var trade flexstmt.Trade
				if err := json.Unmarshal(record.RawJSON, &trade); err != nil {
					t.Fatal(err)
				}
				got["trade "+trade.RecordID] = record.StatementFileKey
			}
		}
		return got, days, records
	}
	want := func(got, want map[string]string) {
		t.Helper()
		for key, file := range want {
			if got[key] != file {
				t.Errorf("%s source = %q, want %q", key, got[key], file)
			}
		}
	}
	got, _, _ := sources(f29, f28, f27)
	want(got, map[string]string{
		"trade same": f27.name, "day 2026-01-01": f27.name,
		"day 2026-01-02": f28.name,
		// 100 → 101 → 100: the return to 100 is new content on f29.
		"trade changing": f29.name,
	})
	got, _, _ = sources(f29, f28)
	want(got, map[string]string{"trade same": f28.name, "day 2026-01-01": f28.name})

	// A daily refresh that repeats every row leaves every current row except
	// the new report's metadata exactly as stored, so reconciling writes none.
	store := openTestCoreStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	t.Cleanup(func() { _ = store.Close() })
	scope := statementProjectionScopeForSelection(flexEvidenceSelection{})
	refresh := func(files ...statementProjectionFile) corestore.StatementProjectionSnapshot {
		t.Helper()
		inventory, days, records, versions, err := buildStatementProjection(files, day(30), "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.ReplaceStatementProjection(t.Context(), scope, inventory, days, records, versions); err != nil {
			t.Fatal(err)
		}
		snapshot, err := store.LoadStatementProjectionSnapshot(t.Context(), scope, statementProjectionMaxRows, statementProjectionMaxRows)
		if err != nil {
			t.Fatal(err)
		}
		return snapshot
	}
	repeat := daily("flex-20261030.xml", day(30), 101, 1000, 1010)
	before, after := refresh(f28), refresh(repeat, f28)
	if len(after.Records) != len(before.Records)+1 || len(after.EquityDays) != len(before.EquityDays) {
		t.Fatalf("records %d→%d days %d→%d, want one new metadata row", len(before.Records), len(after.Records), len(before.EquityDays), len(after.EquityDays))
	}
	for _, old := range before.Records {
		if old.Kind == corestore.StatementRecordMetadata {
			continue
		}
		if !slices.ContainsFunc(after.Records, func(r corestore.StatementRecord) bool {
			return r.ID == old.ID && r.StatementFileKey == old.StatementFileKey && r.GeneratedAt.Equal(old.GeneratedAt) && bytes.Equal(r.RawJSON, old.RawJSON)
		}) {
			t.Errorf("unchanged %s record moved off %s", old.Kind, old.StatementFileKey)
		}
	}
	for i, old := range before.EquityDays {
		if now := after.EquityDays[i]; now.ID != old.ID || now.StatementFileKey != old.StatementFileKey || !now.GeneratedAt.Equal(old.GeneratedAt) {
			t.Errorf("unchanged day %s moved %s → %s", old.Day, old.StatementFileKey, now.StatementFileKey)
		}
	}
	newest := filterStatementRecords(after.Records, corestore.StatementRecordMetadata)
	if !slices.ContainsFunc(newest, func(r corestore.StatementRecord) bool { return r.StatementFileKey == repeat.name }) {
		t.Errorf("the repeated report left no metadata receipt on %s", repeat.name)
	}
}

// Metadata for one re-served date range is the freshness receipt Edge
// acquisition reads: identical content still follows the newest generation.
func TestStatementProjectionMetadataFollowsNewestGeneration(t *testing.T) {
	t.Parallel()
	from, to := time.Date(2025, time.October, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC)
	file := func(name string, generated time.Time) statementProjectionFile {
		return statementProjectionFile{name: name, size: 1, digest: sha256.Sum256([]byte(name)), statements: []flexstmt.Statement{{
			AccountID: "U", FromDate: from, ToDate: to, WhenGenerated: generated, ManifestVersion: flexstmt.ManifestVersion,
		}}}
	}
	older, newer := file("flex-a.xml", to.AddDate(0, 0, 1)), file("flex-b.xml", to.AddDate(0, 1, 0))
	_, _, records, _, err := buildStatementProjection([]statementProjectionFile{newer, older}, to.AddDate(0, 1, 0), "")
	if err != nil {
		t.Fatal(err)
	}
	metadata := filterStatementRecords(records, corestore.StatementRecordMetadata)
	if len(metadata) != 1 || metadata[0].StatementFileKey != newer.name {
		t.Fatalf("metadata = %+v, want one row on %s", metadata, newer.name)
	}
}
