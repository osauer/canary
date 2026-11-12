package corestore

import (
	"bufio"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// An older image of the log's index put back under the open store must not
// make it read an older snapshot than it committed. With the index in a
// shared -shm file it did: the next mutation read a commit counter three
// commits back and latched lost_write, although every commit was intact in
// the log. With the index in the store's own memory there is no file to
// overwrite, and a file planted at that path is never read.
//
// The test holds one descriptor on the -shm for its whole life and closes it
// only after the store: closing any descriptor of that file drops every POSIX
// lock this process holds on it, SQLite's included. WriteAt never truncates,
// so the mapped region stays valid.
func TestStaleIndexImageCannotRollTheStoreBack(t *testing.T) {
	f := openRecoveryFixture(t)
	if err := f.mutate(t, "first"); err != nil {
		t.Fatal(err)
	}
	shmPath := f.store.path + "-shm"
	shm, err := os.OpenFile(shmPath, os.O_RDWR, 0)
	switch {
	case err == nil:
		t.Errorf("%s exists while the store is open; the log's index is shared with any opener", shmPath)
		defer func() {
			_ = f.store.Close()
			_ = shm.Close()
		}()
	case errors.Is(err, os.ErrNotExist):
		shm = nil
	default:
		t.Fatal(err)
	}
	var older []byte
	if shm != nil {
		if older, err = io.ReadAll(shm); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 3 {
		if err := f.mutate(t, fmt.Sprintf("later-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	newest := f.head(t)

	if shm != nil {
		if _, err := shm.WriteAt(older, 0); err != nil {
			t.Fatal(err)
		}
	} else {
		// A first-region-sized file at the index path, as a rebuilt image
		// would be. The store must not notice it.
		if err := os.WriteFile(shmPath, make([]byte, 32<<10), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Remove(shmPath) })
	}

	if err := f.mutate(t, "after"); err != nil {
		t.Errorf("mutation after the index image was put back: %v", err)
	}
	if got := f.store.Health(); !got.Ready {
		t.Fatalf("store latched after the index image was put back: %+v", got)
	}
	if got := f.head(t); got.HeadGeneration != newest.HeadGeneration+1 {
		t.Fatalf("head after one more mutation = %d, want %d", got.HeadGeneration, newest.HeadGeneration+1)
	}
}

// TestCorestoreOutsideOpenerHelper is the outside process of the tests below.
// With CANARY_TEST_CORESTORE_OUTSIDE set it tries a read-only and a
// read-write SQLite open of the named database and prints one line per try.
// With CANARY_TEST_CORESTORE_CRASH set it opens a Store there, commits the
// given number of times, prints the head and exits without closing.
func TestCorestoreOutsideOpenerHelper(t *testing.T) {
	if path := os.Getenv("CANARY_TEST_CORESTORE_OUTSIDE"); path != "" {
		for _, dsn := range []string{
			"file:" + path + "?mode=ro&_pragma=busy_timeout(100)",
			"file:" + path + "?_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)",
		} {
			fmt.Println(helperLine, outsideOpenResult(dsn))
		}
		return
	}
	if path := os.Getenv("CANARY_TEST_CORESTORE_CRASH"); path != "" {
		commits, _ := strconv.Atoi(os.Getenv("CANARY_TEST_CORESTORE_COMMITS"))
		store, err := Open(t.Context(), Options{Path: path, CommitObserver: func(AuthorityHead) error { return nil }})
		if err != nil {
			fmt.Println(helperLine, "open:", err)
			os.Exit(2)
		}
		for i := range commits {
			if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "crash", Kind: fmt.Sprintf("k%d", i), JSON: []byte(`{}`)}); err != nil {
				fmt.Println(helperLine, "commit:", err)
				os.Exit(2)
			}
		}
		head, err := store.AuthorityHead(t.Context())
		if err != nil {
			fmt.Println(helperLine, "head:", err)
			os.Exit(2)
		}
		fmt.Println(helperLine, head.HeadGeneration)
		os.Exit(0) // no Close: the log is left as a killed daemon leaves it
	}
	if path := os.Getenv("CANARY_TEST_CORESTORE_OLD_BUILD"); path != "" {
		// An earlier build's connection: WAL entered in normal locking mode,
		// so the index lives in daemon.db-shm.
		db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(1000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate")
		if err != nil {
			fmt.Println(helperLine, "open:", err)
			os.Exit(2)
		}
		db.SetMaxOpenConns(1)
		var generation int64
		if err := db.QueryRow(`UPDATE store_meta SET head_generation=head_generation+1 WHERE singleton=1 RETURNING head_generation`).Scan(&generation); err != nil {
			fmt.Println(helperLine, "commit:", err)
			os.Exit(2)
		}
		fmt.Println(helperLine, generation)
		if os.Getenv("CANARY_TEST_CORESTORE_HOLD") != "" {
			_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		}
		os.Exit(0) // no Close: the earlier build stopped without folding its log
	}
	t.Skip("helper process only")
}

func outsideOpenResult(dsn string) string {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "open: " + err.Error()
	}
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return "refused: " + err.Error()
	}
	return "opened"
}

// helperLine marks the helper's own output among whatever the test binary
// prints around it.
const helperLine = "corestore-helper:"

func helperLines(out []byte) []string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		if line, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), helperLine+" "); ok {
			lines = append(lines, line)
		}
	}
	return lines
}

func runHelper(t *testing.T, env ...string) []string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCorestoreOutsideOpenerHelper$")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	return helperLines(out)
}

// createdAndClosed makes a fresh authority at a private path and closes it.
func createdAndClosed(t *testing.T) string {
	t.Helper()
	path := filepath.Join(privateTempDir(t), "daemon.db")
	store, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// The first start after an upgrade meets the earlier build's files: a log
// with commits the main file lacks and the index file beside it. The store
// reads every commit and removes the index file, which it no longer uses.
func TestFirstOpenAfterAnEarlierBuildReadsItsLogAndDropsItsIndexFile(t *testing.T) {
	path := createdAndClosed(t)
	lines := runHelper(t, "CANARY_TEST_CORESTORE_OLD_BUILD="+path)
	if len(lines) != 1 {
		t.Fatalf("helper printed %q", lines)
	}
	committed, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		t.Fatalf("helper head %q: %v", lines[0], err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err != nil {
			t.Fatalf("the earlier build left no %s: %v", suffix, err)
		}
	}
	store, err := Open(t.Context(), Options{Path: path, MinimumHead: &AuthorityHead{HeadGeneration: committed}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if head, err := store.AuthorityHead(t.Context()); err != nil || head.HeadGeneration != committed {
		t.Fatalf("head = %+v %v, want generation %d", head, err, committed)
	}
	if _, err := os.Lstat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the earlier build's index file is still there: %v", err)
	}
}

// An index file another process still holds means that process has the
// authority open: the store refuses to open and names it, and leaves the
// file alone.
func TestOpenRefusesWhileAnotherProcessHoldsTheIndexFile(t *testing.T) {
	path := createdAndClosed(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCorestoreOutsideOpenerHelper$")
	cmd.Env = append(os.Environ(), "CANARY_TEST_CORESTORE_OLD_BUILD="+path, "CANARY_TEST_CORESTORE_HOLD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("helper did not commit: %v", err)
		}
		if strings.HasPrefix(line, helperLine) {
			break
		}
	}
	_, err = Open(t.Context(), Options{Path: path, BusyTimeout: 100 * time.Millisecond})
	if want := fmt.Sprintf("another process (pid %d) has the authority open", cmd.Process.Pid); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("open beside a held index file: %v, want %q", err, want)
	}
	if _, err := os.Lstat(path + "-shm"); err != nil {
		t.Fatalf("the held index file was removed: %v", err)
	}
}

// Another process cannot open the live store at all, read-only or
// read-write. The store stays ready with its head intact, and the refused
// opener leaves no index file behind.
func TestOutsideOpenerIsRefusedAndTheStoreKeepsItsHead(t *testing.T) {
	f := openRecoveryFixture(t)
	for i := range 3 {
		if err := f.mutate(t, fmt.Sprintf("k%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	before := f.head(t)

	results := runHelper(t, "CANARY_TEST_CORESTORE_OUTSIDE="+f.store.path)
	if len(results) != 2 {
		t.Fatalf("helper printed %q, want two results", results)
	}
	for _, got := range results {
		if !strings.HasPrefix(got, "refused: ") || !strings.Contains(got, "locked") {
			t.Errorf("outside open: %s, want refused as locked", got)
		}
	}
	if _, err := os.Lstat(f.store.path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an index file exists after the refused open: %v", err)
	}
	if got := f.store.Health(); !got.Ready {
		t.Fatalf("store latched after an outside open: %+v", got)
	}
	if got := f.head(t); got != before {
		t.Fatalf("head after an outside open = %+v, want %+v", got, before)
	}
	if err := f.mutate(t, "after"); err != nil {
		t.Fatal(err)
	}
	if got := f.head(t); got.HeadGeneration != before.HeadGeneration+1 {
		t.Fatalf("head after the next mutation = %d, want %d", got.HeadGeneration, before.HeadGeneration+1)
	}
}

// A daemon that dies without closing leaves a log and no index. The next
// open rebuilds the index from the log and reads every commit.
func TestStoreLeftWithoutCloseReopensAtItsNewestHead(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "daemon.db")
	lines := runHelper(t, "CANARY_TEST_CORESTORE_CRASH="+path, "CANARY_TEST_CORESTORE_COMMITS=5")
	if len(lines) != 1 {
		t.Fatalf("helper printed %q", lines)
	}
	committed, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil {
		t.Fatalf("helper head %q: %v", lines[0], err)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("the killed store left no log to replay: %v", err)
	}
	store, err := Open(t.Context(), Options{Path: path, MinimumHead: &AuthorityHead{HeadGeneration: committed}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	head, err := store.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head.HeadGeneration != committed {
		t.Fatalf("reopened head = %d, want %d", head.HeadGeneration, committed)
	}
	if _, err := os.Lstat(path + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an index file exists after the reopen: %v", err)
	}
}

// rawConn runs fn on the store's driver connection, through the pool.
func (s *Store) rawConn(ctx context.Context, fn func(any) error) error {
	c, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Raw(fn)
}

// requireStoreStillOwnsItsFile checks what must hold after the pool replaced
// the store's connection: the store is ready, its log check reports nothing,
// the next mutation lands one past the newest head, no index file exists, and
// an outside opener is still refused.
func requireStoreStillOwnsItsFile(t *testing.T, f *recoveryFixture, newest AuthorityHead) {
	t.Helper()
	if reason, err := f.store.LogChanged(t.Context()); err != nil || reason != "" {
		t.Fatalf("log check after the store's own reopen = %q %v, want nothing", reason, err)
	}
	if err := f.mutate(t, "after"); err != nil {
		t.Fatalf("mutation after the store's own reopen: %v", err)
	}
	if got := f.store.Health(); !got.Ready {
		t.Fatalf("store latched after its own reopen: %+v", got)
	}
	if got := f.head(t); got.HeadGeneration != newest.HeadGeneration+1 {
		t.Fatalf("head after the next mutation = %d, want %d", got.HeadGeneration, newest.HeadGeneration+1)
	}
	if reason, err := f.store.LogChanged(t.Context()); err != nil || reason != "" {
		t.Fatalf("log check after the next mutation = %q %v, want nothing", reason, err)
	}
	if err := requireIndexInMemory(f.store.path); err != nil {
		t.Fatal(err)
	}
	for _, got := range runHelper(t, "CANARY_TEST_CORESTORE_OUTSIDE="+f.store.path) {
		if !strings.HasPrefix(got, "refused: ") || !strings.Contains(got, "locked") {
			t.Fatalf("outside open after the store's own reopen: %s, want refused as locked", got)
		}
	}
}

func driverConnOf(t *testing.T, s *Store) any {
	t.Helper()
	var dc any
	if err := s.rawConn(t.Context(), func(c any) error { dc = c; return nil }); err != nil {
		t.Fatal(err)
	}
	return dc
}

// A read whose context ends mid-statement interrupts the connection; the
// driver then reports it unusable and the pool opens another on the next
// call. That is the store's own reopen, not an outside opener.
func TestCancelledReadReopensTheConnectionAndTheStoreKeepsItsFile(t *testing.T) {
	f := openRecoveryFixture(t)
	if err := f.mutate(t, "before"); err != nil {
		t.Fatal(err)
	}
	before := driverConnOf(t, f.store)
	newest := f.head(t)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	var n int64
	if err := f.store.db.QueryRowContext(ctx, `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c`).Scan(&n); err == nil {
		t.Fatal("an endless read finished")
	}
	if driverConnOf(t, f.store) == before {
		t.Fatal("the pool kept the interrupted connection; this test no longer reaches a reopen")
	}
	requireStoreStillOwnsItsFile(t, f, newest)
}

// A connection the driver reports bad is closed at once. The last close of
// an exclusive connection folds and deletes the log, and the next call opens
// a new connection with a new log.
func TestBadConnectionReopensAndTheStoreKeepsItsFile(t *testing.T) {
	f := openRecoveryFixture(t)
	if err := f.mutate(t, "before"); err != nil {
		t.Fatal(err)
	}
	newest := f.head(t)
	if _, err := os.Stat(f.store.path + "-wal"); err != nil {
		t.Fatal(err)
	}
	if err := f.store.rawConn(t.Context(), func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("raw: %v", err)
	}
	if _, err := os.Stat(f.store.path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the closed connection left its log: %v", err)
	}
	requireStoreStillOwnsItsFile(t, f, newest)
	// The old log is gone, so a log that exists now is a new file. Comparing
	// inodes would not show that: a file system may give the new log the
	// number the deleted one just freed.
	if _, err := os.Stat(f.store.path + "-wal"); err != nil {
		t.Fatalf("the reopened connection has no log (%v); this test no longer reaches a new log", err)
	}
}

// Between the pool's close and its reopen the store holds no lock. A process
// that opens the file in that gap holds it, and the store's next write fails
// closed instead of sharing the file.
func TestOpenerInTheReopenGapMakesTheNextWriteFailClosed(t *testing.T) {
	rec := &healthRecorder{t: t}
	path := filepath.Join(privateTempDir(t), "daemon.db")
	store, err := Open(t.Context(), Options{Path: path, BusyTimeout: 100 * time.Millisecond, CommitObserver: func(AuthorityHead) error { return nil }, HealthObserver: rec.observe})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	rec.store.Store(store)
	f := &recoveryFixture{store: store, recorder: rec}
	if err := f.mutate(t, "before"); err != nil {
		t.Fatal(err)
	}
	if err := store.rawConn(t.Context(), func(any) error { return driver.ErrBadConn }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("raw: %v", err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCorestoreOutsideOpenerHelper$")
	cmd.Env = append(os.Environ(), "CANARY_TEST_CORESTORE_OLD_BUILD="+path, "CANARY_TEST_CORESTORE_HOLD=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	reader := bufio.NewReader(stdout)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("the opener did not get in during the gap: %v", err)
		}
		if after, ok := strings.CutPrefix(line, helperLine); ok {
			if _, err := strconv.ParseInt(strings.TrimSpace(after), 10, 64); err != nil {
				t.Fatalf("the opener did not get in during the gap: %s", line)
			}
			break
		}
	}
	if err := f.mutate(t, "after"); err == nil {
		t.Fatal("a write was accepted while another process held the file")
	}
	if got := store.Health(); got.Ready || got.Code != "busy" {
		t.Fatalf("health after a write beside an outside opener = %+v, want latched busy", got)
	}
}

// One connection serves the store: a reader waits while a write transaction
// is open, and never sees what that transaction has not committed. A
// connection pinned outside the pool would run the reader inside the
// transaction instead.
func TestReaderWaitsForAnOpenWriteTransaction(t *testing.T) {
	f := openRecoveryFixture(t)
	before := f.head(t)
	tx, err := f.store.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(t.Context(), `UPDATE store_meta SET head_generation=head_generation+1000 WHERE singleton=1`); err != nil {
		t.Fatal(err)
	}
	read := make(chan AuthorityHead, 1)
	go func() {
		head, _ := f.store.AuthorityHead(context.Background())
		read <- head
	}()
	select {
	case head := <-read:
		t.Fatalf("a reader ran during an open write transaction and read generation %d", head.HeadGeneration)
	case <-time.After(200 * time.Millisecond):
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if head := <-read; head != before {
		t.Fatalf("reader after the rollback read %+v, want %+v", head, before)
	}
}

// The proof that the index is private names what it found: an index file,
// or a path it could not inspect.
func TestIndexProofNamesWhatItFound(t *testing.T) {
	dir := privateTempDir(t)
	path := filepath.Join(dir, "daemon.db")
	if err := requireIndexInMemory(path); err != nil {
		t.Fatalf("no index file: %v", err)
	}
	if err := os.WriteFile(path+"-shm", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireIndexInMemory(path); err == nil || !strings.HasSuffix(err.Error(), "daemon.db-shm exists") {
		t.Fatalf("index file present: %v", err)
	}
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	if err := requireIndexInMemory(filepath.Join(locked, "daemon.db")); err == nil || !errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("uninspectable path: %v", err)
	}
}
