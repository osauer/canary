package daemon

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
)

func syntheticFlexCashSourceXML(account, from, to, generated, amount string) []byte {
	cash := ""
	if amount != "" {
		cash = fmt.Sprintf(`<CashReport><CashReportCurrency accountId="%s" currency="EUR" fromDate="%s" toDate="%s" reportDate="%s" endingCash="12000" endingSettledCash="%s"/><CashReportCurrency accountId="%s" currency="USD" fromDate="%s" toDate="%s" reportDate="%s" endingCash="0" endingSettledCash="0"/></CashReport>`, account, from, to, to, amount, account, from, to, to)
	}
	return fmt.Appendf(nil, `<FlexQueryResponse><FlexStatements><FlexStatement accountId="%s" fromDate="%s" toDate="%s" whenGenerated="%s">%s<EquitySummaryInBase><EquitySummaryByReportDateInBase reportDate="%s" total="25000"/></EquitySummaryInBase></FlexStatement></FlexStatements></FlexQueryResponse>`, account, from, to, generated, cash, to)
}

func flexCashSourceTestServer(t *testing.T) (*Server, brokerStateScope, time.Time) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	srv := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "12345"}, Gateway: config.Gateway{Account: "DU-SYNTHETIC", Port: new(7497)}}, coreStore: store, now: func() time.Time { return now }}
	return srv, srv.currentBrokerStateScope(), now
}

func acceptSyntheticFlexCash(t *testing.T, s *Server, raw []byte) string {
	t.Helper()
	out, err := s.retainFlexStatementForQuery(t.Context(), raw, s.configuredFlexQueryFingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	return out.Path
}

func TestFlexCashBaselineAcceptedQueryScopedEvidence(t *testing.T) {
	s, scope, now := flexCashSourceTestServer(t)
	acceptSyntheticFlexCash(t, s, syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000"))
	baseline, err := s.flexSettledCashBaseline(scope, now)
	if err != nil || baseline.Currencies["EUR"].EndingSettledCash == nil || *baseline.Currencies["EUR"].EndingSettledCash != 11000 || baseline.Currencies["USD"].EndingSettledCash == nil || *baseline.Currencies["USD"].EndingSettledCash != 0 {
		t.Fatalf("baseline %+v error %v", baseline, err)
	}
	expected := time.Date(2026, 9, 30, 0, 0, 0, 0, flexReportingLocation())
	if !baseline.ActivityFrom.Equal(expected) || !baseline.AcceptedAt.Equal(now) || baseline.QueryFingerprint != s.configuredFlexQueryFingerprint() || baseline.ReportFingerprint == "" || !sameBrokerScope(baseline.Scope, scope) {
		t.Fatalf("unbound source identity: %+v", baseline)
	}
	// Date eligibility does not imply absence of pending debit obligations:
	// preserve both ending balances for the caller's independent proof.
	if *baseline.Currencies["EUR"].EndingCash == *baseline.Currencies["EUR"].EndingSettledCash {
		t.Fatal("source erased unsettled balances")
	}
	again, err := s.flexSettledCashBaseline(scope, now.Add(time.Minute))
	if err != nil || again.ReportFingerprint != baseline.ReportFingerprint {
		t.Fatal("read receipt churned report identity")
	}
}

func TestFlexCashBaselineAcceptedPeriodDatesWithoutRowReportDate(t *testing.T) {
	s, scope, now := flexCashSourceTestServer(t)
	raw := syntheticFlexCashSourceXML(scope.Account, "20260918", "20260930", "20261001;003000", "11000")
	raw = []byte(strings.ReplaceAll(string(raw), ` toDate="20260930" reportDate="20260930"`, ` toDate="20260930"`))
	acceptSyntheticFlexCash(t, s, raw)
	baseline, err := s.flexSettledCashBaseline(scope, now)
	if err != nil {
		t.Fatalf("exact accepted period-date export rejected: %v", err)
	}
	if baseline.Currencies["EUR"].EndingSettledCash == nil || *baseline.Currencies["EUR"].EndingSettledCash != 11000 || baseline.Currencies["USD"].EndingSettledCash == nil || *baseline.Currencies["USD"].EndingSettledCash != 0 {
		t.Fatal("native cash or explicit zero lost")
	}
	if !baseline.ReportDate.Equal(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)) || baseline.QueryFingerprint != s.configuredFlexQueryFingerprint() || !sameBrokerScope(baseline.Scope, scope) {
		t.Fatal("period-date baseline lost exact report/query/account binding")
	}
}

func TestFlexCashBaselineRejectsWrongAuthorityAndIncompleteSource(t *testing.T) {
	for _, name := range []string{"wrong query", "wrong account", "wrong mode", "stale date", "missing section", "modified accepted bytes", "unaccepted new file", "duplicate currency"} {
		t.Run(name, func(t *testing.T) {
			s, scope, now := flexCashSourceTestServer(t)
			account, date, amount := scope.Account, "20260930", "11000"
			if name == "wrong account" {
				account = "DU-OTHER-SYNTHETIC"
			}
			if name == "stale date" {
				date = "20260929"
			}
			if name == "missing section" {
				amount = ""
			}
			raw := syntheticFlexCashSourceXML(account, date, date, "20261001;003000", amount)
			if name == "duplicate currency" {
				raw = []byte(strings.Replace(string(raw), "</CashReport>", fmt.Sprintf(`<CashReportCurrency accountId="%s" currency="EUR" fromDate="%s" toDate="%s" reportDate="%s" endingCash="1" endingSettledCash="1"/></CashReport>`, account, date, date, date), 1))
			}
			path := acceptSyntheticFlexCash(t, s, raw)
			switch name {
			case "wrong query":
				s.cfg.Flex.QueryID = "54321"
			case "wrong mode":
				scope.Mode = "live"
			case "modified accepted bytes":
				if err := os.WriteFile(path, []byte(strings.Replace(string(raw), `endingSettledCash="11000"`, `endingSettledCash="11900"`, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unaccepted new file":
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), "flex-"+s.configuredFlexQueryFingerprint()+"-synthetic-new.xml"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.flexSettledCashBaseline(scope, now); err == nil {
				t.Fatal("unsafe historical source accepted")
			}
		})
	}
}

func TestFlexCashBaselineConflictingSameEpochReportsHold(t *testing.T) {
	s, scope, now := flexCashSourceTestServer(t)
	acceptSyntheticFlexCash(t, s, syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000"))
	acceptSyntheticFlexCash(t, s, syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11900"))
	if _, err := s.flexSettledCashBaseline(scope, now); err == nil {
		t.Fatal("conflicting same generation chose an arbitrary balance")
	}
}

func TestFlexCashBaselinePrefersCurrentDailyShapeAndRejectsInvalidRows(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU-SYNTHETIC", Mode: "paper"}
	parse := func(raw []byte) flexCashSourceStatement {
		t.Helper()
		sts, err := flexstmt.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return flexCashSourceStatement{Statement: sts[0], Digest: sha256.Sum256(raw), AcceptedAt: now}
	}
	daily := parse(syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000"))
	backfill := parse(syntheticFlexCashSourceXML(scope.Account, "20260101", "20260930", "20261001;010000", "11900"))
	base, err := selectFlexCashBaseline(flexQueryFingerprint("12345"), scope, []flexCashSourceStatement{backfill, daily}, now)
	if err != nil || *base.Currencies["EUR"].EndingSettledCash != 11000 {
		t.Fatal("backfill displaced current daily baseline")
	}
	for _, change := range []func(*flexCashSourceStatement){
		func(i *flexCashSourceStatement) { i.Statement.WhenGenerated = now.Add(36 * time.Hour) },
		func(i *flexCashSourceStatement) {
			i.Statement.CashBalances[0].ReportDate = i.Statement.ToDate.AddDate(0, 0, -1)
		},
		func(i *flexCashSourceStatement) { i.AcceptedAt = now.Add(time.Minute) },
		func(i *flexCashSourceStatement) { i.Statement.CashBalances[0].EndingSettledCash = nil },
	} {
		changed := parse(syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000"))
		change(&changed)
		if _, err := selectFlexCashBaseline(flexQueryFingerprint("12345"), scope, []flexCashSourceStatement{changed}, now); err == nil {
			t.Fatal("invalid baseline selected")
		}
	}
}
