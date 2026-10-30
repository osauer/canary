package daemon

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestHoldingsDenylistIsTheLatestPositionReportPerAccount(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	statements := []flexstmt.Statement{
		{AccountID: "a", Positions: []flexstmt.OpenPosition{
			{Symbol: "ZQXOLD", ReportDate: day("2026-01-01")},
		}},
		{AccountID: "a", Positions: []flexstmt.OpenPosition{
			{Symbol: "ZQXNEW", ReportDate: day("2026-02-02")},
			{Symbol: "ZQXOPT1 260918C00005000", UnderlyingSymbol: "ZQXOPT", ReportDate: day("2026-02-02")},
			{Symbol: "ZQXK B", ReportDate: day("2026-02-02")},
			{Symbol: "zqxlower", ReportDate: day("2026-02-02")},
		}},
		{AccountID: "b", Positions: []flexstmt.OpenPosition{
			{AccountID: "b", Symbol: "ZQXB", ReportDate: day("2026-01-15")},
		}},
	}
	got := string(holdingsDenylist(statements))
	want := "# as of 2026-02-02\nZQXB\nZQXK\nZQXNEW\nZQXOPT\nZQXOPT1\n"
	if got != want {
		t.Fatalf("holdings file:\n%s\nwant:\n%s", got, want)
	}
	if got := string(holdingsDenylist(nil)); got != noFlexPositionReport+"\n" {
		t.Fatalf("no statements: %q", got)
	}
}

func TestMoneyDenylistKeepsOnlyTheLiveBook(t *testing.T) {
	live := []byte(`{"state":{"account_id":"A","account_mode":"live","last_equity_base":618034.27,"last_equity_as_of":"2026-02-02T10:00:00Z"}}`)
	cash := []byte(`{"account_mode":"live","cash_sweep":{"currencies":[{"currency":"EUR","cash":314159.26,"settled_cash":298765.43},
		{"currency":"USD","cash":-271828.18,"trade_date_cash":-270000},{"currency":"GBP","state":"cash_unavailable"}]}}`)
	got, err := moneyDenylist(live, cash)
	if err != nil {
		t.Fatal(err)
	}
	want := "nlv|618034.27|2026-02-02\ncash|314159.26|\ncash|298765.43|\ncash|-271828.18|\ncash|-270000|\n"
	if string(got) != want {
		t.Fatalf("money file:\n%s\nwant:\n%s", got, want)
	}

	paper := []byte(`{"state":{"account_mode":"paper","last_equity_base":864197.53,"last_equity_as_of":"2026-02-02T10:00:00Z"}}`)
	paperCash := []byte(`{"account_mode":"paper","cash_sweep":{"currencies":[{"currency":"EUR","cash":123456.78}]}}`)
	if got, err := moneyDenylist(paper, paperCash); err != nil || len(got) != 0 {
		t.Fatalf("paper book: %q %v", got, err)
	}
	// An observation time without a value means the document changed shape:
	// the gate fails on "missing" rather than skip.
	noValue := []byte(`{"state":{"account_mode":"live","last_equity_as_of":"2026-02-02T10:00:00Z"}}`)
	if got, err := moneyDenylist(noValue, nil); err != nil || string(got) != "nlv|missing|2026-02-02\n" {
		t.Fatalf("live book without NLV: %q %v", got, err)
	}
	if got, err := moneyDenylist([]byte(`{"state":{"account_mode":"live"}}`), nil); err != nil || len(got) != 0 {
		t.Fatalf("live book never observed: %q %v", got, err)
	}
}

// TestAccountDataOutsideOpenerHelper is the outside process of
// TestOutsideSQLiteOpenerIsNamed: it holds a read-only SQLite connection on
// the database the parent names until its stdin closes.
func TestAccountDataOutsideOpenerHelper(t *testing.T) {
	path := os.Getenv("CANARY_TEST_OUTSIDE_SQLITE_DB")
	if path == "" {
		t.Skip("helper process only")
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestOutsideSQLiteOpenerIsNamed(t *testing.T) {
	path := filepath.Join(privateTestDir(t), "daemon.db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var logs bytes.Buffer
	s := &Server{coreStore: store, coreStorePath: path, logger: NewLogger(&logs, "warn")}

	// The daemon's own connection is never reported.
	s.checkOutsideSQLiteOpeners(t.Context())
	if logs.Len() != 0 {
		t.Fatalf("own connection reported: %s", logs.String())
	}

	helper := exec.Command(os.Args[0], "-test.run=^TestAccountDataOutsideOpenerHelper$")
	helper.Env = append(os.Environ(), "CANARY_TEST_OUTSIDE_SQLITE_DB="+path)
	stdin, err := helper.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = helper.Wait()
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "ready" {
		t.Fatalf("helper did not open the database: %q %v", line, err)
	}

	s.checkOutsideSQLiteOpeners(t.Context())
	want := fmt.Sprintf("another process (pid %d ", helper.Process.Pid)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("outside opener not named; logs: %s", logs.String())
	}
	// Once per process.
	before := logs.Len()
	s.checkOutsideSQLiteOpeners(t.Context())
	if logs.Len() != before {
		t.Fatalf("outside opener warned twice: %s", logs.String())
	}
}
