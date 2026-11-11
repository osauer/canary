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

// TestAccountDataOutsideOpenerHelper is the outside process of the tests
// below: it holds a read-only SQLite connection on the database the parent
// names until its stdin closes, or prints why it could not open it.
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
		fmt.Println("refused:", err)
		return
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

// startOutsideOpener runs the helper against path and returns its first line.
func startOutsideOpener(t *testing.T, path string) (*exec.Cmd, string) {
	t.Helper()
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
	if err != nil {
		t.Fatalf("helper printed nothing: %v", err)
	}
	return helper, strings.TrimSpace(line)
}

func openWatchedStore(t *testing.T) (*Server, *corestore.Store, string, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(privateTestDir(t), "daemon.db")
	store, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	var logs bytes.Buffer
	s := &Server{coreStore: store, coreStorePath: path, logger: NewLogger(&logs, "warn")}
	s.checkOutsideSQLiteOpeners(t.Context())
	if logs.Len() != 0 || !store.Health().Ready {
		t.Fatalf("own connection reported: %s %+v", logs.String(), store.Health())
	}
	return s, store, path, &logs
}

// An outside SQLite opener is refused while the daemon holds the store, and
// there is nothing for the watch to report.
func TestOutsideSQLiteOpenerIsRefused(t *testing.T) {
	s, store, path, logs := openWatchedStore(t)
	if _, line := startOutsideOpener(t, path); !strings.HasPrefix(line, "refused:") || !strings.Contains(line, "locked") {
		t.Fatalf("outside opener: %q, want refused as locked", line)
	}
	s.checkOutsideSQLiteOpeners(t.Context())
	if logs.Len() != 0 || !store.Health().Ready {
		t.Fatalf("refused opener reported: %s %+v", logs.String(), store.Health())
	}
}

// Commits, a full checkpoint and an online backup keep the log the same file
// and create no index file, so the watch stays quiet, and the store still
// holds its lock afterwards.
func TestNormalWorkKeepsTheWatchQuietAndTheLockHeld(t *testing.T) {
	s, store, path, logs := openWatchedStore(t)
	ctx := t.Context()
	write := func(kind string) {
		t.Helper()
		if _, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: "watch", Kind: kind, JSON: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 5 {
		write(fmt.Sprintf("k%d", i))
	}
	s.checkOutsideSQLiteOpeners(ctx)
	if result, err := store.Checkpoint(ctx); err != nil || result.Busy != 0 {
		t.Fatalf("checkpoint = %+v %v", result, err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() != 0 {
		t.Fatalf("log after a full checkpoint: %v %v", info, err)
	}
	if _, err := store.Backup(ctx, filepath.Join(privateTestDir(t), "backup.db")); err != nil {
		t.Fatal(err)
	}
	write("after")
	s.checkOutsideSQLiteOpeners(ctx)
	if logs.Len() != 0 || !store.Health().Ready {
		t.Fatalf("normal work reported: %s %+v", logs.String(), store.Health())
	}
	if _, line := startOutsideOpener(t, path); !strings.HasPrefix(line, "refused:") {
		t.Fatalf("outside opener after normal work: %q, want refused", line)
	}
}

// Closing a descriptor of daemon.db in the daemon drops its exclusive lock.
// An opener that then gets in creates daemon.db-shm: the watch latches the
// store fail-closed, once, and names the process.
func TestOpenerAfterTheLockIsLostLatchesAndIsNamed(t *testing.T) {
	s, store, path, logs := openWatchedStore(t)
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	helper, line := startOutsideOpener(t, path)
	if line != "ready" {
		t.Fatalf("helper did not open the database after the lock was dropped: %q", line)
	}
	s.checkOutsideSQLiteOpeners(t.Context())
	if got := store.Health(); got.Ready || got.Code != "outside_opener" {
		t.Fatalf("health after an outside open = %+v, want latched outside_opener", got)
	}
	want := fmt.Sprintf("another process (pid %d ", helper.Process.Pid)
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("outside opener not named; logs: %s", logs.String())
	}
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

// A log replaced under the daemon, as an opener that checkpoints and removes
// it leaves it, latches the store fail-closed and names the cause.
func TestReplacedLogLatchesTheStore(t *testing.T) {
	s, store, path, _ := openWatchedStore(t)
	if err := os.Rename(path+"-wal", path+"-wal.moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	s.checkOutsideSQLiteOpeners(t.Context())
	if got := store.Health(); got.Ready || got.Code != "outside_opener" {
		t.Fatalf("health after the log was replaced = %+v, want latched outside_opener", got)
	}
}
