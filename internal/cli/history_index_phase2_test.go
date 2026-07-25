package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// phase2FakeConn answers stress.history / recon.equity with canned
// results and records the decoded params (historyFakeConn pattern).
type phase2FakeConn struct {
	method       string
	stressParams rpc.StressHistoryParams
	equityParams rpc.ReconEquityParams
	stress       rpc.StressHistoryResult
	equity       rpc.ReconEquityResult
}

func (c *phase2FakeConn) Call(_ context.Context, method string, params, out any) error {
	c.method = method
	raw, _ := json.Marshal(params)
	var result any
	switch method {
	case rpc.MethodStressHistory:
		_ = json.Unmarshal(raw, &c.stressParams)
		result = c.stress
	case rpc.MethodReconEquity:
		_ = json.Unmarshal(raw, &c.equityParams)
		result = c.equity
	default:
		result = struct{}{}
	}
	buf, _ := json.Marshal(result)
	return json.Unmarshal(buf, out)
}

func (*phase2FakeConn) Stream(context.Context, string, any, func(json.RawMessage) error) error {
	return nil
}

func stressHistoryFixture() rpc.StressHistoryResult {
	relevant := true
	return rpc.StressHistoryResult{
		AsOf:  time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Since: time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Entries: []rpc.StressHistoryEntry{
			{
				At: time.Date(2026, 7, 20, 13, 5, 0, 0, time.UTC), Severity: "act", Action: "defend",
				MarketStage: "confirmed_stress", Summary: "stress confirmed against held risk",
				PortfolioAlertRelevant: &relevant,
			},
			{
				At: time.Date(2026, 7, 19, 9, 30, 0, 0, time.UTC), Severity: "watch", Action: "watch",
				MarketStage: "early_warning", Summary: "market pressure building",
			},
		},
		Count: 2, TotalCount: 44, Limit: 2, Truncated: true,
		Index: rpc.HistoryIndexHealth{
			LastIngestAt:  time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
			IngestedBytes: 2048, JournalBytes: 2048,
		},
	}
}

func TestRunStressHistoryForwardsParamsAndRendersTable(t *testing.T) {
	t.Parallel()
	conn := &phase2FakeConn{stress: stressHistoryFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "stress", []string{
		"history", "--since", "2026-07-14", "--until", "2026-07-20", "--severity", "act", "--action", "defend", "--limit", "2",
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if conn.method != rpc.MethodStressHistory {
		t.Fatalf("method = %q, want %q", conn.method, rpc.MethodStressHistory)
	}
	p := conn.stressParams
	if p.Since != "2026-07-14" || p.Until != "2026-07-20" || p.Severity != "act" || p.Action != "defend" || p.Limit != 2 {
		t.Fatalf("params = %+v, want flags forwarded", p)
	}
	out := stdout.String()
	for _, want := range []string{
		"Stress history  2026-07-14 → 2026-07-20 UTC  2 of 44 rows (truncated; raise --limit)",
		"AT (UTC)",
		"SEV",
		"ACTION",
		"STAGE",
		"2026-07-20 13:05  act",
		"defend",
		"confirmed_stress",
		"stress confirmed against held risk",
		"index: through 2026-07-20 14:02Z · journal fully ingested",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunStressHistoryJSONPassThrough(t *testing.T) {
	t.Parallel()
	conn := &phase2FakeConn{stress: stressHistoryFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "stress", []string{"history", "--json"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var res rpc.StressHistoryResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not the result envelope: %v\n%s", err, stdout.String())
	}
	if res.TotalCount != 44 || len(res.Entries) != 2 || !res.Truncated {
		t.Fatalf("envelope did not pass through verbatim: %+v", res)
	}
	if res.Entries[0].PortfolioAlertRelevant == nil || !*res.Entries[0].PortfolioAlertRelevant {
		t.Fatalf("portfolio_alert_relevant lost in pass-through: %+v", res.Entries[0])
	}
}

func TestRunStressHistoryRejectsTrailingArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := runStressHistory(context.Background(), env, []string{"history", "extra"}); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ibkr stress history") {
		t.Fatalf("stderr missing usage: %s", stderr.String())
	}
}

func reconEquityFixture() rpc.ReconEquityResult {
	return rpc.ReconEquityResult{
		AsOf:  time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Since: time.Date(2026, 4, 21, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 7, 20, 14, 30, 0, 0, time.UTC),
		Days: []rpc.EquityDayEntry{
			{Day: "2026-07-10", AccountID: "U1", EquityBase: 260500.00, SourceStmt: "flex-20260714-063000.xml"},
			{Day: "2026-07-09", AccountID: "U1", EquityBase: 259999.99, SourceStmt: "flex-20260714-063000.xml"},
			{Day: "2026-07-08", AccountID: "U1", EquityBase: 261234.56, SourceStmt: "flex-20260713-063000.xml"},
		},
		Count: 3, TotalCount: 63, Limit: 3, Truncated: true,
		Events: []rpc.CapitalEventEntry{
			{At: time.Date(2026, 7, 9, 16, 0, 0, 0, time.UTC), Type: "withdrawal", AmountBase: 500, Origin: "human-tty", Note: "monthly"},
		},
		Index: rpc.HistoryIndexHealth{
			LastIngestAt:  time.Date(2026, 7, 20, 14, 2, 0, 0, time.UTC),
			IngestedBytes: 994, JournalBytes: 994,
		},
		Statements: rpc.HistoryIndexHealth{
			LastIngestAt:  time.Date(2026, 7, 20, 6, 40, 0, 0, time.UTC),
			IngestedBytes: 50000, JournalBytes: 61000,
		},
	}
}

func TestRunReconEquityForwardsParamsAndRendersTable(t *testing.T) {
	t.Parallel()
	conn := &phase2FakeConn{equity: reconEquityFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	code := Run(context.Background(), env, "recon", []string{
		"equity", "--since", "2026-04-21", "--until", "2026-07-20", "--limit", "3",
	})
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	if conn.method != rpc.MethodReconEquity {
		t.Fatalf("method = %q, want %q", conn.method, rpc.MethodReconEquity)
	}
	p := conn.equityParams
	if p.Since != "2026-04-21" || p.Until != "2026-07-20" || p.Limit != 3 {
		t.Fatalf("params = %+v, want flags forwarded", p)
	}
	out := stdout.String()
	for _, want := range []string{
		"Statement equity  2026-04-21 → 2026-07-20 UTC  3 of 63 days (truncated; raise --limit)",
		"DAY",
		"EQUITY(BASE)",
		"SOURCE",
		"2026-07-10       260500.00",
		"2026-07-09       259999.99",
		"flex-20260714-063000.xml",
		"2026-07-09 16:00  withdrawal 500.00  human-tty  monthly",
		"capital journal index: through 2026-07-20 14:02Z · fully ingested",
		"statements index catching up: 11000 bytes behind",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// Newest-first timeline: the 2026-07-09 16:00 withdrawal happened
	// before that day's EOD equity stamp, so it renders below the
	// 2026-07-09 day row and above the 2026-07-08 row.
	dayNine := strings.Index(out, "2026-07-09       259999.99")
	event := strings.Index(out, "withdrawal 500.00")
	dayEight := strings.Index(out, "2026-07-08       261234.56")
	if !(dayNine < event && event < dayEight) {
		t.Fatalf("capital event not interleaved at its timeline position:\n%s", out)
	}
}

func TestRunReconEquityJSONPassThrough(t *testing.T) {
	t.Parallel()
	conn := &phase2FakeConn{equity: reconEquityFixture()}
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr, Conn: conn}
	if code := Run(context.Background(), env, "recon", []string{"equity", "--json"}); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var res rpc.ReconEquityResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("stdout is not the result envelope: %v\n%s", err, stdout.String())
	}
	if res.TotalCount != 63 || len(res.Days) != 3 || len(res.Events) != 1 || res.Statements.JournalBytes != 61000 {
		t.Fatalf("envelope did not pass through verbatim: %+v", res)
	}
}

func TestRunReconEquityRejectsBadFlagAndTrailingArgs(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &stderr}
	if code := Run(context.Background(), env, "recon", []string{"equity", "--nope"}); code != 2 {
		t.Fatalf("bad flag exit=%d, want 2", code)
	}
	stderr.Reset()
	if code := runReconEquity(context.Background(), env, []string{"extra"}); code != 1 {
		t.Fatalf("trailing arg exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ibkr recon equity") {
		t.Fatalf("stderr missing usage: %s", stderr.String())
	}
}

// TestPhase2CatalogGuards pins the catalog rows: both new subverbs are
// read-only.
func TestPhase2CatalogGuards(t *testing.T) {
	t.Parallel()
	specs := map[string]CommandSpec{}
	for _, spec := range Catalog() {
		specs[spec.Name] = spec
	}
	stressSpec, ok := specs["stress"]
	if !ok {
		t.Fatal("stress catalog row missing")
	}
	foundHistory := false
	for _, sub := range stressSpec.Subcommands {
		if sub.Name == "history" {
			foundHistory = true
			if sub.Guard != GuardReadOnly {
				t.Fatalf("stress history guard = %v, want GuardReadOnly", sub.Guard)
			}
		}
	}
	if !foundHistory {
		t.Fatalf("stress catalog subcommands = %+v, want history present", stressSpec.Subcommands)
	}
	reconSpec, ok := specs["recon"]
	if !ok {
		t.Fatal("recon catalog row missing")
	}
	foundEquity := false
	for _, sub := range reconSpec.Subcommands {
		if sub.Name == "equity" {
			foundEquity = true
			if sub.Guard != GuardReadOnly {
				t.Fatalf("recon equity guard = %v, want GuardReadOnly", sub.Guard)
			}
		}
	}
	if !foundEquity {
		t.Fatalf("recon catalog subcommands = %+v, want equity present", reconSpec.Subcommands)
	}
}
