package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/flexstmt"
)

func TestRetainedFlexWarmReadsDoNotReparseUnchangedXML(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var body strings.Builder
	body.WriteString("<CashTransactions>")
	for i := range 1000 {
		fmt.Fprintf(&body, `<CashTransaction transactionID="fixture-%d" type="Dividends" currency="EUR" fxRateToBase="1" amount="1" dateTime="20260102;120000"/>`, i)
	}
	body.WriteString("</CashTransactions>")
	writeFlexFixture(t, "flex-fixture.xml", "20260103;120000", "20260101", "20260102", body.String())
	s := &Server{}
	read := func() []flexstmt.Statement {
		t.Helper()
		statements, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil)
		if err != nil || len(problems) != 0 || len(statements) != 1 || len(statements[0].Cash) != 1000 {
			t.Fatalf("retained statements=%d problems=%v err=%v", len(statements), problems, err)
		}
		return statements
	}
	want := read()
	cold := testing.AllocsPerRun(1, func() {
		got, problems, err := loadRetainedFlexStatementsContextSelected(t.Context(), nil, s.flexEvidenceSelection())
		if err != nil || len(problems) != 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("uncached evidence differs: problems=%v err=%v", problems, err)
		}
	})
	warm := testing.AllocsPerRun(3, func() {
		if got := read(); !reflect.DeepEqual(got, want) {
			t.Fatal("warm read changed statement evidence")
		}
	})
	// Repeated health/reconciliation reads must not allocate a new XML object
	// graph. Relative to a real parse, this leaves ample room for file checks
	// while catching the original five-pass XML decode on every heartbeat.
	if warm >= cold/10 {
		t.Fatalf("unchanged statements still allocate like a full parse: warm %.0f, uncached %.0f allocations", warm, cold)
	}
	t.Logf("unchanged statements: warm %.0f, uncached %.0f allocations", warm, cold)
}

func TestRetainedFlexCacheChecksContentAndCurrentFileSet(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{}
	write := func(name string, amount float64) {
		t.Helper()
		writeFlexFixture(t, name, "20260103;120000", "20260101", "20260102", cashLine("fixture", "Dividends", amount, "20260102"))
	}
	read := func(want int, amount float64) {
		t.Helper()
		got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil)
		if err != nil || len(problems) != 0 || len(got) != want {
			t.Fatalf("read: statements=%d problems=%v err=%v", len(got), problems, err)
		}
		if want > 0 && (len(got[0].Cash) != 1 || got[0].Cash[0].AmountBase == nil || *got[0].Cash[0].AmountBase != amount) {
			t.Fatalf("statement correction not observed: %+v", got[0].Cash)
		}
		// Filtering/sorting the returned outer slice cannot edit retained input.
		if want > 0 {
			got[0] = flexstmt.Statement{}
		}
	}
	write("flex-a.xml", 10)
	read(1, 10)
	read(1, 10)
	dir, _ := flexStatementsDirPath()
	path := filepath.Join(dir, "flex-a.xml")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	write("flex-a.xml", 20)
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("fixture must preserve size and modification time: %v", err)
	}
	read(1, 20)
	write("flex-b.xml", 30)
	read(2, 30)
	if err := os.Remove(filepath.Join(dir, "flex-b.xml")); err != nil {
		t.Fatal(err)
	}
	read(1, 20)
	if len(s.retainedFlex.rows) != 1 {
		t.Fatal("removed statement retained in memory")
	}
	if err := os.WriteFile(path, []byte("malformed XML"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil)
	if err != nil || len(got) != 0 || len(problems) != 1 || len(s.retainedFlex.rows) != 0 {
		t.Fatalf("corrupt replacement reused healthy evidence: statements=%d problems=%v err=%v", len(got), problems, err)
	}
	write("flex-a.xml", 40)
	read(1, 40)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing.xml"), path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err == nil {
		t.Fatal("symlink replacement reused healthy evidence")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	read(0, 0)
}

func TestRetainedFlexCacheQueryRotationAndConcurrentReads(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	s := &Server{cfg: &config.Resolved{Flex: config.Flex{QueryID: "fixture-one"}}}
	for _, query := range []string{"fixture-one", "fixture-two"} {
		writeFlexFixtureForAccount(t, "flex-"+flexQueryFingerprint(query)+"-fixture.xml", query, "20260103;120000", "20260101", "20260102", "")
	}
	for _, query := range []string{"fixture-one", "fixture-two"} {
		s.cfg.Flex.QueryID = query
		var wg sync.WaitGroup
		for range 12 {
			wg.Go(func() {
				got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil)
				if err != nil || len(problems) != 0 || len(got) != 1 || got[0].AccountID != query {
					t.Errorf("wrong query evidence: statements=%v problems=%v err=%v", got, problems, err)
				}
			})
		}
		wg.Wait()
		if len(s.retainedFlex.rows) != 1 {
			t.Fatal("query rotation retained the retired parsed view")
		}
	}
}

func TestRetainedFlexCacheWaitCanBeCancelled(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	writeFlexFixture(t, "flex-fixture.xml", "20260103;120000", "20260101", "20260102", "")
	s := &Server{}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	go func() {
		defer close(done)
		_, _, _ = s.loadActiveRetainedFlexStatementsContext(t.Context(), func(stage string) error {
			if stage == "retained_statements_start" {
				close(entered)
				<-release
			}
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result := make(chan error, 1)
	go func() { _, _, err := s.loadActiveRetainedFlexStatementsContext(ctx, nil); result <- err }()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled waiter: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter blocked behind statement parsing")
	}
	unblock()
	<-done
	if got, _, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err != nil || len(got) != 1 {
		t.Fatalf("cancelled waiter poisoned the next read: %v", err)
	}
}

func TestRetainedFlexReadersShareParsingWithoutSharingAcceptance(t *testing.T) {
	s, scope, now := flexCashSourceTestServer(t)
	raw := syntheticFlexCashSourceXML(scope.Account, "20260930", "20260930", "20261001;003000", "11000")
	// Real statements carry large sections irrelevant to a given reader. All
	// readers must avoid decoding these again once their exact bytes are known.
	extra := "<Unused>" + strings.Repeat(`<Row value="fixture"/>`, 1000) + "</Unused>"
	raw = []byte(strings.Replace(string(raw), "</FlexStatement>", extra+"</FlexStatement>", 1))
	path := acceptSyntheticFlexCash(t, s, raw)
	read := func() {
		t.Helper()
		if got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err != nil || len(problems) != 0 || len(got) != 1 {
			t.Fatalf("reconciliation reader: problems=%v err=%v", problems, err)
		}
		if got, err := s.fxStatements(t.Context()); err != nil || len(got) != 1 {
			t.Fatalf("FX reader: %v", err)
		}
		if got, err := s.flexSettledCashBaseline(scope, now); err != nil || got.Currencies["EUR"].EndingSettledCash == nil || *got.Currencies["EUR"].EndingSettledCash != 11000 {
			t.Fatalf("cash reader: %v", err)
		}
	}
	read()
	cold := testing.AllocsPerRun(1, func() {
		if _, err := flexstmt.ParseContext(t.Context(), raw); err != nil {
			t.Fatal(err)
		}
	})
	warm := testing.AllocsPerRun(3, read)
	if warm >= cold/10 {
		t.Fatalf("recurring readers still parse accepted XML: warm %.0f, full parse %.0f allocations", warm, cold)
	}
	t.Logf("all recurring readers: warm %.0f, full parse %.0f allocations", warm, cold)
	// Reconciliation may read a replacement before ingestion accepts it. Even
	// if that populated the shared parse cache, neither cash nor FX may use it.
	changed := []byte(strings.Replace(string(raw), `endingSettledCash="11000"`, `endingSettledCash="12000"`, 1))
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err != nil || len(problems) != 0 || len(got) != 1 {
		t.Fatalf("changed raw evidence: problems=%v err=%v", problems, err)
	}
	if _, err := s.fxStatements(t.Context()); !errors.Is(err, errFXInventoryChanged) {
		t.Fatalf("unaccepted cached bytes certified FX: %v", err)
	}
	if _, err := s.flexSettledCashBaseline(scope, now); err == nil {
		t.Fatal("unaccepted cached bytes certified settled cash")
	}
}

func TestRetainedFlexCancelledScanKeepsVerifiedParses(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := range 2 {
		writeFlexFixture(t, fmt.Sprintf("flex-%d.xml", i), "20260103;120000", "20260101", "20260102", cashLine(fmt.Sprint(i), "Dividends", float64(i), "20260102"))
	}
	s := &Server{}
	if _, _, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	visited := 0
	_, _, err := s.loadActiveRetainedFlexStatementsContext(ctx, func(stage string) error {
		if stage == "retained_statement_file" {
			visited++
			if visited == 2 {
				cancel()
			}
		}
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || len(s.retainedFlex.rows) != 2 {
		t.Fatalf("cancelled partial scan discarded verified parses: rows=%d err=%v", len(s.retainedFlex.rows), err)
	}
}

func TestRetainedFlexColdCancelledScanPreservesCompletedFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	for i := range 2 {
		writeFlexFixture(t, fmt.Sprintf("flex-%d.xml", i), "20260103;120000", "20260101", "20260102", cashLine(fmt.Sprint(i), "Dividends", float64(i), "20260102"))
	}
	s := &Server{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	visited := 0
	_, _, err := s.loadActiveRetainedFlexStatementsContext(ctx, func(stage string) error {
		if stage == "retained_statement_file" {
			visited++
			if visited == 2 {
				cancel()
			}
		}
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) || len(s.retainedFlex.rows) != 1 {
		t.Fatalf("cold cancellation discarded a completed parse: rows=%d err=%v", len(s.retainedFlex.rows), err)
	}
	if got, problems, err := s.loadActiveRetainedFlexStatementsContext(t.Context(), nil); err != nil || len(problems) != 0 || len(got) != 2 || len(s.retainedFlex.rows) != 2 {
		t.Fatalf("next scan could not finish: rows=%d problems=%v err=%v", len(got), problems, err)
	}
}
