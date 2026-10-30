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

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

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

// TestDaemonWritesNoAccountDataOutsideItsStore: the account-data gate asks the
// running daemon (scripts/account-data); the daemon never copies holdings or
// money figures to other files on the user's disk.
func TestDaemonWritesNoAccountDataOutsideItsStore(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	s, scope, _ := flexCashSourceTestServer(t)
	s.productionStateDatabase = true
	raw := syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000")
	raw = bytes.Replace(raw, []byte("<EquitySummaryInBase>"), fmt.Appendf(nil,
		`<OpenPositions><OpenPosition accountId="%s" assetCategory="STK" currency="USD" fxRateToBase="0.92" symbol="ZQXHELD" conid="123" reportDate="20260930" position="10" multiplier="1" markPrice="110" costBasisPrice="100" costBasisMoney="1000" fifoPnlUnrealized="100" side="Long" levelOfDetail="SUMMARY" /></OpenPositions><EquitySummaryInBase>`,
		scope.Account), 1)
	acceptSyntheticFlexCash(t, s, raw)
	var written []string
	_ = filepath.WalkDir(cacheHome, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			written = append(written, path)
		}
		return nil
	})
	if len(written) != 0 {
		t.Fatalf("the daemon wrote account data outside its store: %v", written)
	}
}

// The money file builds 7bffcc3d to 4e9f834e wrote goes at the next start;
// the gate's holdings cache at the same place stays.
func TestRetiredMoneyFileIsRemovedAtStart(t *testing.T) {
	cacheHome := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	dir := filepath.Join(cacheHome, "ibkr")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"money-denylist", "holdings-denylist"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var logs bytes.Buffer
	(&Server{logger: NewLogger(&logs, "info")}).removeRetiredMoneyFile()
	if _, err := os.Stat(filepath.Join(dir, "money-denylist")); !os.IsNotExist(err) {
		t.Fatalf("money file kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "holdings-denylist")); err != nil {
		t.Fatalf("gate cache removed: %v", err)
	}
}
