package daemon

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestProjectionRefreshReusesVerifiedStatementBytes(t *testing.T) {
	s, scope, _ := flexCashSourceTestServer(t)
	raw := syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000")
	raw = []byte(strings.Replace(string(raw), "</FlexStatement>", "<!--"+strings.Repeat("x", 2<<20)+"--></FlexStatement>", 1))
	acceptSyntheticFlexCash(t, s, raw)
	// Simulate startup with accepted SQLite evidence and an empty parse cache.
	// One refresh must warm itself; no FX/cash read should be required first.
	s.retainedFlex = retainedFlexCache{}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			if err := s.refreshStatementProjection(t.Context()); err != nil {
				b.Fatal(err)
			}
		}
	})
	t.Logf("unchanged projection refresh: %d bytes/op", result.AllocedBytesPerOp())
	if result.AllocedBytesPerOp() >= int64(len(raw)/4) {
		t.Fatal("refresh copies unchanged XML")
	}
}

func TestReportingMetadataSkipsUnusedPositionAndFinancingArrays(t *testing.T) {
	now := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	item := statementMetadataProjectionPayload{Version: statementProjectionVersion, FromDate: now, ToDate: now, ManifestVersion: flexstmt.ManifestVersion, Financing: &flexstmt.FinancingStatement{}}
	for range 8000 {
		item.PositionSnapshot = append(item.PositionSnapshot, flexstmt.OpenPosition{})
		item.Financing.Loans = append(item.Financing.Loans, flexstmt.LendingLoan{})
	}
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	records := []corestore.StatementRecord{{Kind: corestore.StatementRecordMetadata, RawJSON: raw, GeneratedAt: now, AccountKey: "SYNTHETIC"}}
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			rows, err := reportingStatementsFromMetadata(records)
			if err != nil || len(rows) != 1 || !rows[0].ToDate.Equal(now) || rows[0].ManifestVersion != flexstmt.ManifestVersion {
				b.Fatalf("reporting header lost: %v", err)
			}
		}
	})
	t.Logf("reporting header: %d bytes/op for %d input bytes", result.AllocedBytesPerOp(), len(raw))
	if result.AllocedBytesPerOp() > int64(len(raw)/4) {
		t.Fatal("reporting health decodes unused trading arrays")
	}
	for _, bad := range []string{`{`, `{"version":0}`, `{"version":8,"coverage":"invalid"}`} {
		records[0].RawJSON = []byte(bad)
		if _, err := reportingStatementsFromMetadata(records); err == nil {
			t.Fatal("invalid reporting metadata accepted")
		}
	}
}

func syntheticRetainedHealthReport(t *testing.T) (*Server, rpc.DataHealthResult) {
	t.Helper()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rows := []rpc.DataSourceHealth{}
	for i := range 64 {
		row := rpc.DataSourceHealth{ID: fmt.Sprintf("synthetic:%02d", i), Name: fmt.Sprintf("Source %02d", i), State: "unavailable", Required: true, ProblemIDs: []string{fmt.Sprint(i)}}
		for j := range 64 {
			row.History = append(row.History, rpc.DataHealthTransition{At: now.Add(-time.Duration(j) * time.Minute), State: "unavailable", Reason: strings.Repeat("x", 64)})
		}
		rows = append(rows, row)
	}
	full, err := finalizeDataHealth(rows, "current", now, rpc.DataHealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	full.Sources, full.Offset, full.NextOffset, full.Complete = rows, 0, nil, true
	s := &Server{now: func() time.Time { return now }}
	s.dataHealth.latest = full
	s.dataHealth.reports = map[string]rpc.DataHealthResult{full.Revision: full}
	return s, full
}

func TestRetainedHealthPageDoesNotReencodeWholeReport(t *testing.T) {
	s, full := syntheticRetainedHealthReport(t)
	result := testing.Benchmark(func(b *testing.B) {
		for b.Loop() {
			page, err := s.handleDataHealth(rpc.DataHealthParams{Limit: 1})
			if err != nil || page.Revision != full.Revision || len(page.Sources) != 1 {
				b.Fatal("invalid retained page", err)
			}
		}
	})
	t.Logf("retained one-row health page: %d bytes/op", result.AllocedBytesPerOp())
	if result.AllocedBytesPerOp() > 100<<10 {
		t.Fatal("cached page rebuilds full catalogue")
	}
}

func TestRetainedHealthPagesPreserveReportContract(t *testing.T) {
	s, full := syntheticRetainedHealthReport(t)
	for _, p := range []rpc.DataHealthParams{{}, {Limit: 1}, {Offset: 9, Limit: 12, Revision: full.Revision}, {Offset: 64}, {Offset: 65}, {Offset: -1}, {Limit: 65}, {Revision: "unknown"}} {
		want, werr := finalizeDataHealth(slices.Clone(full.Sources), full.ScopeState, full.AsOf, p)
		got, gerr := s.handleDataHealth(p)
		if (werr != nil) != (gerr != nil) {
			t.Fatalf("params %+v error differs: %v / %v", p, werr, gerr)
		}
		if werr != nil {
			continue
		}
		a, _ := json.Marshal(want)
		b, _ := json.Marshal(got)
		if string(a) != string(b) {
			t.Fatalf("params %+v changed page contract", p)
		}
		if len(got.Sources) > 0 {
			got.Sources[0].ID = "caller mutation"
		}
		if len(got.Concerns) > 0 {
			got.Concerns[0].Label = "caller mutation"
		}
	}
}
