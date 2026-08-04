package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// writeFlexFixture drops one raw statement into the retained-statements
// dir, the same way the fetcher would.
func writeFlexFixture(t *testing.T, name, whenGenerated, from, to, body string) {
	writeFlexFixtureForAccount(t, name, "U1234567", whenGenerated, from, to, body)
}

func writeFlexFixtureForAccount(t *testing.T, name, account, whenGenerated, from, to, body string) {
	t.Helper()
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	doc := fmt.Sprintf(`<FlexQueryResponse queryName="recon" type="AF">
 <FlexStatements count="1">
  <FlexStatement accountId="%s" fromDate="%s" toDate="%s" whenGenerated="%s">
%s
  </FlexStatement>
 </FlexStatements>
</FlexQueryResponse>`, account, from, to, whenGenerated, body)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
}

func cashLine(id, typ string, amount float64, date string) string {
	return fmt.Sprintf(`   <CashTransactions><CashTransaction transactionID=%q type=%q currency="EUR" fxRateToBase="1" amount="%f" dateTime="%s;120000" settleDate=%q description="FIXTURE" /></CashTransactions>`, id, typ, amount, date, date)
}

func equityRow(date string, total float64) string {
	return fmt.Sprintf(`   <EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate=%q total="%f" /></EquitySummaryInBase>`, date, total)
}

// newReconTestServer builds a server with an active policy (recon keys
// approved), an isolated state dir, and no gateway.
func newReconTestServer(t *testing.T) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, validRiskPolicyTOML)
	return s
}

func newReconV3TestServer(t *testing.T) *Server {
	t.Helper()
	s := newRiskPolicyTestServer(t, validRiskPolicyV3TOML())
	return s
}

func TestReconNudgeAccessorUsesOpaqueBrokerBackedContentIdentity(t *testing.T) {
	now := time.Now().UTC()
	s := newV4NudgeTestServer(t, now)
	day := now.Format("20060102")
	writeFlexFixture(t, "flex-nudge-accessor.xml", now.Format("20060102")+";120000", day, day,
		strings.Replace(cashLine("private-line-id", "Deposits/Withdrawals", 123, day), "FIXTURE", "hostile broker prose", 1))
	report, snapshot := s.buildReconReportWithSnapshot()
	if report.Status != rpc.ReconStatusActive || len(report.Confirmed) != 1 || snapshot == nil {
		t.Fatalf("report=%+v snapshot=%+v", report, snapshot)
	}
	facts := snapshot.NudgeConfirmedFlows
	if facts.PolicyVersion != 4 || !strings.HasPrefix(facts.ReportIdentity, "sha256:") || len(facts.ConfirmedRows) != 1 || !strings.HasPrefix(facts.ConfirmedRows[0], "sha256:") {
		t.Fatalf("nudge facts=%+v", facts)
	}
	raw, _ := json.Marshal(facts)
	for _, forbidden := range []string{"private-line-id", report.ReportID, "hostile broker prose", "123"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("nudge accessor leaked %q: %s", forbidden, raw)
		}
	}
}

func declare(t *testing.T, s *Server, typ string, amount float64, effectiveAt string) {
	t.Helper()
	var eff time.Time
	if effectiveAt != "" {
		var err error
		if eff, err = time.Parse("2006-01-02", effectiveAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.riskCapital.ApplyCapitalEventForPolicy(rpc.CapitalEventParams{Type: typ, AmountBase: amount, EffectiveAt: eff}, rpc.OrderOriginHumanTTY,
		s.riskPolicies.snapshot().policy); err != nil {
		t.Fatal(err)
	}
}

func recentGenerated() string { return time.Now().UTC().Format("20060102") + ";060000" }

func seedReconRuntime(s *Server, genesis time.Time) {
	s.riskCapital.mu.Lock()
	defer s.riskCapital.mu.Unlock()
	s.riskCapital.state.GenesisAt = genesis
	s.riskCapital.state.Seeded = true
}

func TestReconMatchAndCategories(t *testing.T) {
	s := newReconTestServer(t)
	gen := recentGenerated()

	// Matched deposit; missing withdrawal; amount mismatch; date mismatch;
	// ledger-only event; unknown line type.
	writeFlexFixture(t, "flex-20260710-000001.xml", gen, "20260706", "20260712",
		cashLine("m1", "Deposits/Withdrawals", 20000, "20260707")+"\n"+
			cashLine("w1", "Deposits/Withdrawals", -9250, "20260708")+"\n"+
			cashLine("a1", "Deposits/Withdrawals", 5000, "20260709")+"\n"+
			cashLine("d1", "Deposits/Withdrawals", 3000, "20260701")+"\n"+
			cashLine("u1", "Some Future Line Type", 12.34, "20260710"))
	declare(t, s, "deposit", 20000, "2026-07-07")  // matches m1
	declare(t, s, "deposit", 5300, "2026-07-09")   // amount mismatch vs a1 (300 > tol)
	declare(t, s, "deposit", 3000, "2026-07-08")   // 5 business days from d1 → date mismatch
	declare(t, s, "withdrawal", 700, "2026-06-15") // ledger-only

	rep := s.buildReconReport()
	if rep.Status != rpc.ReconStatusActive {
		t.Fatalf("status = %s (%s)", rep.Status, rep.Message)
	}
	want := map[string]int{
		"matched":                  1,
		rpc.ReconMissingFromLedger: 1, // w1
		rpc.ReconAmountMismatch:    1, // a1
		rpc.ReconDateMismatch:      1, // d1
		rpc.ReconLedgerOnly:        1,
		rpc.ReconUncategorized:     1, // u1
	}
	for cat, n := range want {
		if rep.Counts[cat] != n {
			t.Fatalf("count[%s] = %d, want %d (all: %v)", cat, rep.Counts[cat], n, rep.Counts)
		}
	}
	if rep.Unresolved != 5 {
		t.Fatalf("unresolved = %d, want 5", rep.Unresolved)
	}
	if !strings.HasPrefix(rep.ReportID, "recon-") {
		t.Fatalf("report id = %q", rep.ReportID)
	}
	// Deterministic: same inputs, same id.
	if again := s.buildReconReport(); again.ReportID != rep.ReportID {
		t.Fatalf("report id not deterministic: %s vs %s", again.ReportID, rep.ReportID)
	}
}

func TestReconFiltersSiblingAccountsBeforeMerge(t *testing.T) {
	s := newReconV3TestServer(t)
	generated := recentGenerated()
	writeFlexFixtureForAccount(t, "flex-selected.xml", "U1234567", generated, "20260708", "20260708",
		cashLine("selected-flow", "Deposits/Withdrawals", 100, "20260708")+"\n"+equityRow("20260708", 1000))
	writeFlexFixtureForAccount(t, "flex-sibling.xml", "SIBLING-SECRET", generated, "20260708", "20260708",
		cashLine("sibling-flow", "Deposits/Withdrawals", 900, "20260708")+"\n"+equityRow("20260708", 9000))

	report := s.buildReconReport()
	if report.Status != rpc.ReconStatusActive || report.Counts[reconSkippedSiblingStatementsCount] != 1 {
		t.Fatalf("account-scoped report status=%s counts=%v", report.Status, report.Counts)
	}
	if report.StatementCumFlowsBase == nil || *report.StatementCumFlowsBase != 100 || len(report.Confirmed) != 1 || report.Confirmed[0].LineID != "cash-selected-flow" {
		t.Fatalf("sibling statement participated in reconciliation: %+v", report)
	}
	if report.Equity == nil || report.Equity.StatementTotalBase != 1000 {
		t.Fatalf("sibling equity won the selected account's day: %+v", report.Equity)
	}

	backtest := s.buildReconBacktest()
	if backtest.Status != rpc.ReconStatusActive || backtest.FlowCounts[reconSkippedSiblingStatementsCount] != 1 || len(backtest.Flows) != 1 || backtest.Flows[0].LineID != "cash-selected-flow" || backtest.EquityDays != 1 {
		t.Fatalf("account-scoped backtest = %+v", backtest)
	}
	raw, err := json.Marshal(struct {
		Report   *rpc.ReconResult
		Backtest *rpc.ReconBacktestResult
	}{report, backtest})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SIBLING-SECRET", "sibling-flow", "9000"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("account-scoped result leaked sibling detail %q: %s", forbidden, raw)
		}
	}
}

func TestMergeRetainedStatementsKeysEquityWinnerByAccountAndDay(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	generated := recentGenerated()
	writeFlexFixtureForAccount(t, "flex-account-a.xml", "U1234567", generated, "20260708", "20260708", equityRow("20260708", 1000))
	writeFlexFixtureForAccount(t, "flex-account-b.xml", "U7654321", generated, "20260708", "20260708", equityRow("20260708", 2000))
	statements, problems, err := loadRetainedFlexStatements()
	if err != nil || len(problems) != 0 {
		t.Fatalf("load retained statements: problems=%v err=%v", problems, err)
	}
	merged := mergeRetainedStatements(statements)
	if len(merged.equityByDay) != 2 {
		t.Fatalf("same-day account equity rows collapsed: %+v", merged.equityByDay)
	}
}

func TestReconRefusesUnresolvedAccountScope(t *testing.T) {
	s := newReconTestServer(t)
	s.endpoint.Account = ""
	s.endpoint.Port = 0
	writeFlexFixture(t, "flex-unscoped.xml", recentGenerated(), "20260708", "20260708", equityRow("20260708", 1000))

	report := s.buildReconReport()
	if report.Status != rpc.ReconStatusUnavailable || report.Fetch.Reason != rpc.ReconReportReasonAuthorityUnavailable || !strings.Contains(report.Message, "one selected broker account") {
		t.Fatalf("unresolved account report = %+v", report)
	}
	backtest := s.buildReconBacktest()
	if backtest.Status != rpc.ReconStatusUnavailable || !strings.Contains(backtest.Message, "one selected broker account") {
		t.Fatalf("unresolved account backtest = %+v", backtest)
	}
}

func TestReconKeepsRetainedFileAndParserDetailsDaemonLocal(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-valid.xml", recentGenerated(), "20260708", "20260708", equityRow("20260708", 1000))
	dir, err := flexStatementsDirPath()
	if err != nil {
		t.Fatal(err)
	}
	const privateName = "private-host-path-and-account.xml"
	if err := os.WriteFile(filepath.Join(dir, privateName), []byte(`<not-flex private="broker prose">`), 0o600); err != nil {
		t.Fatal(err)
	}

	report := s.buildReconReport()
	if report.Status != rpc.ReconStatusDegraded {
		t.Fatalf("status=%s health=%+v", report.Status, report.InputHealth)
	}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{privateName, "not-flex", "broker prose", "XML syntax error"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("reconciliation response leaked private parser detail %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(string(raw), rpc.ReconReportReasonResponseInvalid) {
		t.Fatalf("reconciliation response lacks typed failure code: %s", raw)
	}
}

func TestReconMissingCashAmountIsUncategorized(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-missing-amount.xml", recentGenerated(), "20260706", "20260712",
		`   <CashTransactions><CashTransaction transactionID="missing-amount" type="Deposits/Withdrawals" currency="EUR" fxRateToBase="1" dateTime="20260708;120000" settleDate="20260708" description="FIXTURE" /></CashTransactions>`)
	seedReconRuntime(s, time.Date(2026, 7, 9, 15, 0, 0, 0, time.UTC))

	rep := s.buildReconReport()
	if rep.Counts[rpc.ReconUncategorized] != 1 || rep.Unresolved != 1 {
		t.Fatalf("counts = %v unresolved = %d, want one unresolved uncategorized exception", rep.Counts, rep.Unresolved)
	}
	if len(rep.Exceptions) != 1 || rep.Exceptions[0].AmountBase != nil || rep.Exceptions[0].Note != "flow line without a usable base amount" {
		t.Fatalf("exception = %+v, want missing-base flow exception", rep.Exceptions)
	}
	if len(rep.Baseline) != 0 || rep.Counts[rpc.ReconBaseline] != 0 {
		t.Fatalf("amountless pre-genesis flow was baselined: baseline=%+v counts=%v", rep.Baseline, rep.Counts)
	}
}

// Two declared events qualifying for one statement line is ambiguous —
// never a best-effort pick (never-false-match).
func TestReconAmbiguityNeverAutoResolves(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("amb1", "Deposits/Withdrawals", 10000, "20260708"))
	declare(t, s, "deposit", 10000, "2026-07-08")
	declare(t, s, "deposit", 10001, "2026-07-09")

	rep := s.buildReconReport()
	if rep.Counts[rpc.ReconAmbiguous] != 1 {
		t.Fatalf("counts = %v, want one ambiguous", rep.Counts)
	}
	if rep.Counts["matched"] != 0 {
		t.Fatalf("ambiguous line must not also match (counts %v)", rep.Counts)
	}
}

// Restatement: the same line id in a newer file supersedes the older copy.
func TestReconRestatementSupersedesByLineID(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-20260709-000001.xml", recentGenerated(), "20260706", "20260709",
		cashLine("r1", "Deposits/Withdrawals", 9999, "20260708"))
	writeFlexFixture(t, "flex-20260710-000002.xml", recentGenerated(), "20260706", "20260710",
		cashLine("r1", "Deposits/Withdrawals", 10000, "20260708"))
	declare(t, s, "deposit", 10000, "2026-07-08")

	rep := s.buildReconReport()
	if rep.Counts["matched"] != 1 || rep.Unresolved != 0 {
		t.Fatalf("restated line must match once cleanly: counts %v unresolved %d", rep.Counts, rep.Unresolved)
	}
}

func TestReconV3ConfirmedReplacesMissingFromLedger(t *testing.T) {
	build := func(t *testing.T, v3 bool) *rpc.ReconResult {
		t.Helper()
		var s *Server
		if v3 {
			s = newReconV3TestServer(t)
		} else {
			s = newReconTestServer(t)
		}
		writeFlexFixture(t, "flex-confirmed.xml", recentGenerated(), "20260706", "20260712",
			cashLine("confirmed-one", "Deposits/Withdrawals", 1000, "20260708"))
		seedReconRuntime(s, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
		return s.buildReconReport()
	}
	v2 := build(t, false)
	if v2.Counts[rpc.ReconMissingFromLedger] != 1 || v2.Unresolved != 1 || len(v2.Confirmed) != 0 || v2.StatementCumFlowsBase != nil {
		t.Fatalf("v2 report = %+v", v2)
	}
	v3 := build(t, true)
	if v3.Counts[rpc.ReconConfirmed] != 1 || v3.Unresolved != 0 || len(v3.Exceptions) != 0 || len(v3.Confirmed) != 1 {
		t.Fatalf("v3 report counts=%v unresolved=%d exceptions=%+v confirmed=%+v", v3.Counts, v3.Unresolved, v3.Exceptions, v3.Confirmed)
	}
	if v3.Confirmed[0].Category != rpc.ReconConfirmed || v3.Confirmed[0].AmountBase == nil || *v3.Confirmed[0].AmountBase != 1000 || v3.StatementCumFlowsBase == nil || *v3.StatementCumFlowsBase != 1000 {
		t.Fatalf("v3 confirmed/report sum = %+v / %v", v3.Confirmed[0], v3.StatementCumFlowsBase)
	}
}

func TestReconV3StatementAuthorityAndBridgeBoundary(t *testing.T) {
	s := newReconV3TestServer(t)
	seedReconRuntime(s, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	writeFlexFixture(t, "flex-bridge.xml", recentGenerated(), "20260701", "20260710",
		cashLine("matched", "Deposits/Withdrawals", 1000, "20260705"))
	declare(t, s, "deposit", 1005, "2026-07-05")   // statement value wins within tolerance
	declare(t, s, "withdrawal", 200, "2026-07-10") // coverage boundary: ledger_only, excluded
	declare(t, s, "deposit", 300, "2026-07-11")    // after coverage: bridge, counted
	declare(t, s, "deposit", 400, "2026-06-30")    // pre-genesis ledger event stays loud

	rep := s.buildReconReport()
	if rep.StatementCumFlowsBase == nil || *rep.StatementCumFlowsBase != 1300 {
		t.Fatalf("statement-authoritative flows = %v, want 1300", rep.StatementCumFlowsBase)
	}
	if rep.Counts["matched"] != 1 || rep.Counts[rpc.ReconLedgerOnly] != 2 || rep.Unresolved != 2 {
		t.Fatalf("counts=%v unresolved=%d", rep.Counts, rep.Unresolved)
	}
	for _, ex := range rep.Exceptions {
		if ex.EventAt.Format("2006-01-02") == "2026-07-11" {
			t.Fatalf("bridge declaration became an exception: %+v", ex)
		}
	}
}

func TestReconV3ConfirmedRestatementChangesReportID(t *testing.T) {
	s := newReconV3TestServer(t)
	seedReconRuntime(s, time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC))
	generated := recentGenerated()
	writeFlexFixture(t, "flex-confirmed-restatement.xml", generated, "20260701", "20260710",
		cashLine("same-line", "Deposits/Withdrawals", 1000, "20260705"))
	before := s.buildReconReport()
	writeFlexFixture(t, "flex-confirmed-restatement.xml", generated, "20260701", "20260710",
		cashLine("same-line", "Deposits/Withdrawals", 1200, "20260705"))
	after := s.buildReconReport()
	if before.ReportID == after.ReportID {
		t.Fatalf("confirmed restatement reused report id %s", before.ReportID)
	}
	if after.StatementCumFlowsBase == nil || *after.StatementCumFlowsBase != 1200 {
		t.Fatalf("restated statement flows = %v", after.StatementCumFlowsBase)
	}
}

func TestReconEquityCheckSameDayOnly(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-equity.xml", recentGenerated(), "20260710", "20260710", equityRow("20260710", 100))

	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.LastEquityBase = 150
	s.riskCapital.state.LastEquityAsOf = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	s.riskCapital.mu.Unlock()

	rep := s.buildReconReport()
	if rep.Equity == nil {
		t.Fatal("equity check is nil")
	}
	if rep.Equity.SameDay || rep.Equity.DivergencePct != nil {
		t.Fatalf("without same-day sample: %+v, want unknown divergence", rep.Equity)
	}
	if rep.Equity.RuntimeEquityBase == nil || *rep.Equity.RuntimeEquityBase != 150 {
		t.Fatalf("runtime context = %v, want latest observation 150", rep.Equity.RuntimeEquityBase)
	}

	s.riskCapital.mu.Lock()
	s.riskCapital.state.DailyEquity = map[string]float64{"2026-07-10": 110}
	s.riskCapital.mu.Unlock()
	rep = s.buildReconReport()
	if rep.Equity == nil || !rep.Equity.SameDay || rep.Equity.DivergencePct == nil {
		t.Fatalf("with same-day sample: %+v", rep.Equity)
	}
	if *rep.Equity.RuntimeEquityBase != 110 || math.Abs(*rep.Equity.DivergencePct-10) > 1e-9 {
		t.Fatalf("same-day equity = %v divergence = %v, want 110 and 10%%", rep.Equity.RuntimeEquityBase, rep.Equity.DivergencePct)
	}
	if !rep.Equity.RuntimeAsOf.IsZero() {
		t.Fatalf("same-day runtime as-of = %s, want zero (day-key sample)", rep.Equity.RuntimeAsOf)
	}
}

func TestReconEquityCheckUsesNewestAvailableSameDayPair(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-equity-pairs.xml", recentGenerated(), "20260710", "20260711",
		equityRow("20260710", 100)+"\n"+equityRow("20260711", 200))
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.LastEquityBase = 250
	s.riskCapital.state.LastEquityAsOf = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	s.riskCapital.state.DailyEquity = map[string]float64{"2026-07-10": 101}
	s.riskCapital.mu.Unlock()
	rep := s.buildReconReport()
	if rep.Equity == nil || !rep.Equity.SameDay || rep.Equity.StatementDate.Format("2006-01-02") != "2026-07-10" ||
		rep.Equity.DivergencePct == nil || math.Abs(*rep.Equity.DivergencePct-1) > 1e-9 {
		t.Fatalf("equity pair = %+v", rep.Equity)
	}
}

func TestReconPreGenesisBaseline(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-genesis.xml", recentGenerated(), "20260701", "20260705",
		cashLine("before", "Deposits/Withdrawals", -500, "20260701")+"\n"+
			cashLine("after", "Deposits/Withdrawals", -600, "20260704"))

	unseeded := s.buildReconReport()
	if len(unseeded.Baseline) != 0 || unseeded.Counts[rpc.ReconMissingFromLedger] != 2 || unseeded.Unresolved != 2 {
		t.Fatalf("unseeded report baselined flows: baseline=%+v counts=%v unresolved=%d", unseeded.Baseline, unseeded.Counts, unseeded.Unresolved)
	}

	genesis := time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC)
	s.riskCapital.mu.Lock()
	s.riskCapital.state.GenesisAt = genesis
	s.riskCapital.mu.Unlock()
	staleGenesis := s.buildReconReport()
	if len(staleGenesis.Baseline) != 0 || staleGenesis.Counts[rpc.ReconMissingFromLedger] != 2 {
		t.Fatalf("unseeded state with genesis baselined flows: baseline=%+v counts=%v", staleGenesis.Baseline, staleGenesis.Counts)
	}
	s.riskCapital.mu.Lock()
	s.riskCapital.state.GenesisAt = time.Time{}
	s.riskCapital.state.Seeded = true
	s.riskCapital.mu.Unlock()
	seededWithoutGenesis := s.buildReconReport()
	if len(seededWithoutGenesis.Baseline) != 0 || seededWithoutGenesis.Counts[rpc.ReconMissingFromLedger] != 2 {
		t.Fatalf("seeded state without genesis baselined flows: baseline=%+v counts=%v", seededWithoutGenesis.Baseline, seededWithoutGenesis.Counts)
	}
	seedReconRuntime(s, genesis)
	seeded := s.buildReconReport()
	if seeded.ReportID == unseeded.ReportID {
		t.Fatalf("baseline classification did not change report id: %s", seeded.ReportID)
	}
	if !seeded.GenesisAt.Equal(genesis) || seeded.Counts[rpc.ReconBaseline] != 1 || seeded.Counts[rpc.ReconMissingFromLedger] != 1 || seeded.Unresolved != 1 {
		t.Fatalf("seeded report: genesis=%s counts=%v unresolved=%d", seeded.GenesisAt, seeded.Counts, seeded.Unresolved)
	}
	if len(seeded.Baseline) != 1 || len(seeded.Exceptions) != 1 || seeded.Exceptions[0].LineID != "cash-after" {
		t.Fatalf("baseline=%+v exceptions=%+v", seeded.Baseline, seeded.Exceptions)
	}
	before := seeded.Baseline[0]
	if before.LineID != "cash-before" || before.Category != rpc.ReconBaseline || !before.PreGenesis || before.AmountBase == nil || *before.AmountBase != -500 ||
		before.Note != "embedded in the seeded baseline (pre-genesis); no ledger event belongs here" {
		t.Fatalf("baseline row = %+v", before)
	}

	declare(t, s, "withdrawal", 600, "2026-07-04")
	clean := s.buildReconReport()
	if clean.Unresolved != 0 || len(clean.Baseline) != 1 || clean.Counts[rpc.ReconBaseline] != 1 || clean.Counts["matched"] != 1 {
		t.Fatalf("resolved report: baseline=%+v counts=%v unresolved=%d", clean.Baseline, clean.Counts, clean.Unresolved)
	}
	if _, err := s.handleRiskPolicyCapitalEvent(context.Background(), rawParams(t, rpc.CapitalEventParams{
		Type: "reconcile", Report: clean.ReportID, Origin: rpc.OrderOriginHumanTTY,
	})); err != nil {
		t.Fatalf("sign-off with baseline rows: %v", err)
	}
}

func TestReconPreGenesisRestatementChangesReportID(t *testing.T) {
	s := newReconTestServer(t)
	generated := recentGenerated()
	writeFlexFixture(t, "flex-restated.xml", generated, "20260701", "20260705",
		cashLine("pre-one", "Deposits/Withdrawals", 500, "20260701"))
	seedReconRuntime(s, time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC))

	before := s.buildReconReport()
	writeFlexFixture(t, "flex-restated.xml", generated, "20260701", "20260705",
		cashLine("pre-one", "Deposits/Withdrawals", 500, "20260701")+"\n"+
			cashLine("pre-two", "Deposits/Withdrawals", -300, "20260702"))
	after := s.buildReconReport()
	if len(before.Baseline) != 1 || len(after.Baseline) != 2 {
		t.Fatalf("baseline rows before=%+v after=%+v", before.Baseline, after.Baseline)
	}
	if after.ReportID == before.ReportID {
		t.Fatalf("new backdated baseline line did not change report id: %s", after.ReportID)
	}
}

func TestReconPreGenesisLedgerEventStaysLoud(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-ledger-only.xml", recentGenerated(), "20260701", "20260705",
		cashLine("pre", "Deposits/Withdrawals", 700, "20260701")+"\n"+equityRow("20260705", 100000))
	seedReconRuntime(s, time.Date(2026, 7, 3, 15, 0, 0, 0, time.UTC))
	declare(t, s, "deposit", 700, "2026-07-01")

	rep := s.buildReconReport()
	if len(rep.Baseline) != 1 || rep.Counts[rpc.ReconBaseline] != 1 || rep.Counts[rpc.ReconLedgerOnly] != 1 || rep.Unresolved != 1 {
		t.Fatalf("pre-genesis ledger event was not loud: baseline=%+v counts=%v unresolved=%d", rep.Baseline, rep.Counts, rep.Unresolved)
	}
	if len(rep.Exceptions) != 1 || rep.Exceptions[0].Category != rpc.ReconLedgerOnly || rep.Exceptions[0].EventAt.Format("2006-01-02") != "2026-07-01" {
		t.Fatalf("ledger-only exception = %+v", rep.Exceptions)
	}
}

func TestReconDismissResolvesAndChangesReportID(t *testing.T) {
	s := newReconTestServer(t)
	ctx := context.Background()
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("x1", "Deposits/Withdrawals", -500, "20260708"))

	before := s.buildReconReport()
	if before.Unresolved != 1 {
		t.Fatalf("unresolved = %d, want 1", before.Unresolved)
	}

	// Agent origins cannot dismiss.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "r", Origin: rpc.OrderOriginAgent})); err == nil {
		t.Fatal("agent dismiss must be rejected")
	}
	// Unknown lines cannot be dismissed.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-nope", Reason: "r", Origin: rpc.OrderOriginHumanTTY})); err == nil {
		t.Fatal("dismissing a non-exception must fail")
	}
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "bank fee reversal, not a flow", Origin: rpc.OrderOriginHumanTTY})); err != nil {
		t.Fatal(err)
	}

	after := s.buildReconReport()
	if after.Unresolved != 0 {
		t.Fatalf("unresolved after dismiss = %d, want 0", after.Unresolved)
	}
	if len(after.Exceptions) != 1 || !after.Exceptions[0].Dismissed || after.Exceptions[0].DismissReason == "" {
		t.Fatalf("exception = %+v, want dismissed with reason", after.Exceptions[0])
	}
	if after.ReportID == before.ReportID {
		t.Fatal("report id must change when the exception set changes")
	}
	// Double dismissal is refused.
	if _, err := s.handleReconDismiss(ctx, rawParams(t, rpc.ReconDismissParams{LineID: "cash-x1", Reason: "again", Origin: rpc.OrderOriginHumanTTY})); err == nil {
		t.Fatal("double dismiss must fail")
	}
}

func TestReconDismissRefusesAccountChangeBeforeGovernanceWrite(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-scope-race.xml", recentGenerated(), "20260706", "20260712",
		cashLine("scope-race", "Deposits/Withdrawals", -500, "20260708"))
	before := s.buildReconReport()
	if before.Unresolved != 1 || len(before.Exceptions) != 1 {
		t.Fatalf("fixture report = %+v", before)
	}

	switched := false
	s.nudgeScanCheckpoint = func(stage string) {
		if stage != "recon_complete" || switched {
			return
		}
		switched = true
		s.mu.Lock()
		s.endpoint.Account = "U-CHANGED-DURING-REVIEW"
		s.mu.Unlock()
	}
	_, err := s.handleReconDismiss(context.Background(), rawParams(t, rpc.ReconDismissParams{
		LineID: before.Exceptions[0].LineID, Reason: "reviewed", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err == nil || !strings.Contains(err.Error(), "selected broker account changed") {
		t.Fatalf("scope-changing dismissal error = %v", err)
	}
	path, pathErr := defaultTradingStatePath(riskPolicyJournalFile)
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	if raw, readErr := os.ReadFile(path); readErr == nil && strings.Contains(string(raw), "recon_dismiss") {
		t.Fatalf("scope-changing dismissal reached the governance journal: %s", raw)
	} else if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
}

func TestReconUnapprovedAndUnavailable(t *testing.T) {
	// Policy without [recon] keys: unapproved, and reconcile is refused.
	minimal := `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1
`
	s := newRiskPolicyTestServer(t, minimal)
	rep := s.buildReconReport()
	if rep.Status != rpc.ReconStatusUnapproved {
		t.Fatalf("status = %s, want unapproved", rep.Status)
	}
	if _, err := s.reconcileReportGate("recon-whatever"); err == nil || !strings.Contains(err.Error(), "unapproved") {
		t.Fatalf("gate err = %v, want unapproved refusal", err)
	}

	// Approved keys but no statements: unavailable, reconcile refused.
	s2 := newReconTestServer(t)
	rep2 := s2.buildReconReport()
	if rep2.Status != rpc.ReconStatusUnavailable {
		t.Fatalf("status = %s, want unavailable", rep2.Status)
	}
	if _, err := s2.reconcileReportGate("recon-whatever"); err == nil || !strings.Contains(err.Error(), "no recon report") {
		t.Fatalf("gate err = %v, want unavailable refusal", err)
	}
	if _, err := s2.handleRiskPolicyCapitalEvent(context.Background(), rawParams(t, rpc.CapitalEventParams{
		Type: "reconcile", Origin: rpc.OrderOriginHumanTTY,
	})); err == nil || !strings.Contains(err.Error(), "unavailable to sign off") || strings.Contains(err.Error(), "requires --report") {
		t.Fatalf("defaulted unavailable report err = %v, want unavailable-to-sign-off refusal", err)
	}
}

func TestReconcileReportGate(t *testing.T) {
	s := newReconTestServer(t)
	ctx := context.Background()
	writeFlexFixture(t, "flex-20260710-000001.xml", recentGenerated(), "20260706", "20260712",
		cashLine("g1", "Deposits/Withdrawals", 1000, "20260708"))
	declare(t, s, "deposit", 1000, "2026-07-08")

	rep := s.buildReconReport()
	if rep.Unresolved != 0 {
		t.Fatalf("fixture not clean: %+v", rep.Counts)
	}

	// An empty resolved id means there is no current report to sign off.
	if _, err := s.reconcileReportGate(""); err == nil || !strings.Contains(err.Error(), "unavailable to sign off") {
		t.Fatalf("err = %v, want unavailable-to-sign-off refusal", err)
	}
	// Superseded/wrong ids are refused, and the error names the current one.
	if _, err := s.reconcileReportGate("recon-stale123"); err == nil || !strings.Contains(err.Error(), rep.ReportID) {
		t.Fatalf("err = %v, want supersession pointing at %s", err, rep.ReportID)
	}
	// The real id passes end to end through the handler.
	if _, err := s.handleRiskPolicyCapitalEvent(ctx, rawParams(t, rpc.CapitalEventParams{
		Type: "reconcile", Report: rep.ReportID, Origin: rpc.OrderOriginHumanTTY,
	})); err != nil {
		t.Fatal(err)
	}

	// Unresolved exceptions block sign-off.
	writeFlexFixture(t, "flex-20260711-000002.xml", recentGenerated(), "20260706", "20260712",
		cashLine("g2", "Deposits/Withdrawals", -400, "20260709"))
	rep2 := s.buildReconReport()
	if _, err := s.reconcileReportGate(rep2.ReportID); err == nil || !strings.Contains(err.Error(), "unresolved exception") {
		t.Fatalf("err = %v, want unresolved refusal", err)
	}
}

func TestReconcileEventDefaultsToCurrentReportAndRecordsIt(t *testing.T) {
	s := newReconTestServer(t)
	writeFlexFixture(t, "flex-audit.xml", recentGenerated(), "20260706", "20260712", equityRow("20260712", 250000))
	rep := s.buildReconReport()
	if rep.Unresolved != 0 || rep.ReportID == "" {
		t.Fatalf("fixture report = %+v", rep)
	}
	if _, err := s.handleRiskPolicyCapitalEvent(context.Background(), rawParams(t, rpc.CapitalEventParams{
		Type: "reconcile", Origin: rpc.OrderOriginHumanTTY,
	})); err != nil {
		t.Fatal(err)
	}
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got capitalEventV1
	for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
		var ev capitalEventV1
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Type == "reconcile" {
			got = ev
		}
	}
	if got.ReportID != rep.ReportID || got.CoverageTo.IsZero() || !got.CoverageTo.Equal(rep.CoverageTo) {
		t.Fatalf("journaled reconcile = %+v, want report %s coverage %s", got, rep.ReportID, rep.CoverageTo)
	}
}

// A report whose newest statement is older than recon.max_report_age_days
// cannot back a sign-off — stale truth is not truth.
func TestReconcileReportGateRejectsStaleStatements(t *testing.T) {
	s := newReconTestServer(t)
	old := time.Now().UTC().AddDate(0, 0, -10).Format("20060102") + ";060000"
	writeFlexFixture(t, "flex-old.xml", old, "20260620", "20260626",
		cashLine("s1", "Deposits/Withdrawals", 1000, "20260624"))
	declare(t, s, "deposit", 1000, "2026-06-24")

	rep := s.buildReconReport()
	if rep.Unresolved != 0 {
		t.Fatalf("fixture not clean: %+v", rep.Counts)
	}
	if _, err := s.reconcileReportGate(rep.ReportID); err == nil || !strings.Contains(err.Error(), "max_report_age_days") {
		t.Fatalf("err = %v, want staleness refusal", err)
	}
}

func TestBusinessDaysApart(t *testing.T) {
	d := func(s string) time.Time {
		v, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, tc := range []struct {
		a, b string
		want int
	}{
		{"2026-07-08", "2026-07-08", 0}, // same day
		{"2026-07-08", "2026-07-09", 1}, // Wed→Thu
		{"2026-07-10", "2026-07-13", 1}, // Fri→Mon skips the weekend
		{"2026-07-08", "2026-07-15", 5}, // Wed→Wed
		{"2026-07-01", "2026-07-08", 5}, // symmetric order
	} {
		if got := businessDaysApart(d(tc.a), d(tc.b)); got != tc.want {
			t.Fatalf("businessDaysApart(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

// Flex request construction failures are typed locally and never include
// the token-bearing form body or the rejected endpoint.
func TestFlexRequestFailureIsTypedAndRedacted(t *testing.T) {
	const secret = "SECRET-FLEX-TOKEN"
	_, err := flexRawCall(t.Context(), http.DefaultClient, "://invalid/"+secret, url.Values{"t": {secret}, "q": {"1"}})
	if err == nil {
		t.Fatal("invalid endpoint unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("typed Flex failure leaks credentials: %q", err)
	}
	reason, retryable := flexFailureStatus(err)
	if reason != rpc.ReconReportReasonResponseInvalid || !retryable {
		t.Fatalf("failure = %s retryable=%v, want response_invalid retry", reason, retryable)
	}
}
