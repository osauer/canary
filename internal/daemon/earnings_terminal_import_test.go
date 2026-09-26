package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

var terminalImportBase = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

const terminalImportConID = 900001

// terminalImportAAA is a synthetic stock holding the fixtures classify.
var terminalImportAAA = risk.NameInput{Symbol: "AAA", StockConID: terminalImportConID, StockSecType: "STK"}

// writeTerminalImport writes a private import file reviewed at reviewedAt,
// classifying the synthetic AAA contract, or raw bytes when raw is non-nil.
func writeTerminalImport(t *testing.T, path string, reviewedAt time.Time, raw []byte) {
	t.Helper()
	if raw == nil {
		verified := reviewedAt.Add(-time.Hour)
		doc := map[string]any{
			"version":     1,
			"reviewed_at": reviewedAt,
			"contracts": []map[string]any{{
				"contract":         map[string]any{"con_id": terminalImportConID, "symbol": "AAA", "sec_type": "STK"},
				"issuer":           "Synthetic Issuer AAA",
				"cik":              "0000900001",
				"classification":   earningsTerminalClassEquityCancelled,
				"effective_date":   verified.Add(-72 * time.Hour).Format(time.DateOnly),
				"verified_at":      verified,
				"revalidate_after": verified.Add(180 * 24 * time.Hour),
				"evidence": []map[string]string{
					{"kind": "sec_filing", "url": "https://www.sec.gov/Archives/edgar/data/900001/000090000126000001/form15.htm"},
					{"kind": "finra_uniform_practice_advisory", "url": "https://www.finra.org/sites/default/files/2026-09/upa-synthetic-aaa.pdf"},
				},
			}},
		}
		var err error
		if raw, err = json.Marshal(doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A malformed or rolled-back import no longer fails startup (owner decision
// 2026-09-26): the committed revision stays in force, rules 6-8 keep reading
// it, and the import error is reported.
func TestTerminalEvidenceBadImportKeepsTheCommittedRevision(t *testing.T) {
	for name, bad := range map[string]func(t *testing.T, path string){
		"malformed JSON": func(t *testing.T, path string) { writeTerminalImport(t, path, time.Time{}, []byte("{ not json")) },
		"rollback": func(t *testing.T, path string) {
			writeTerminalImport(t, path, terminalImportBase.Add(-24*time.Hour), nil)
		},
		"missing file": func(t *testing.T, path string) { _ = os.Remove(path) },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openMarketTestCoreStore(t)
			path := filepath.Join(t.TempDir(), "terminal-evidence.json")
			writeTerminalImport(t, path, terminalImportBase, nil)
			first := newEarningsTerminalStore(path)
			if err := first.UseCoreStore(ctx, store, terminalImportBase.Add(time.Hour)); err != nil {
				t.Fatalf("valid import: %v", err)
			}
			committed := first.revision
			if _, found := first.terminalEarningsFor(terminalImportAAA, terminalImportBase.Add(time.Hour)); !found || committed == 0 {
				t.Fatalf("valid import was not committed: revision %d", committed)
			}

			bad(t, path)
			restarted := newEarningsTerminalStore(path)
			if err := restarted.UseCoreStore(ctx, store, terminalImportBase.Add(2*time.Hour)); err != nil {
				t.Fatalf("a bad import stopped startup: %v", err)
			}
			if restarted.revision != committed {
				t.Fatalf("revision = %d, want the committed %d kept", restarted.revision, committed)
			}
			match, found := restarted.terminalEarningsFor(terminalImportAAA, terminalImportBase.Add(2*time.Hour))
			if !found || match.Status != rpc.EarningsStatusTerminalNonReporting || match.Info.AuthorityRevision != committed {
				t.Fatalf("committed evidence not served after a bad import: found %v, %+v", found, match)
			}
			doc, ok, err := store.GetStateDocument(ctx, earningsAuthorityScope, earningsTerminalStateKind)
			if err != nil || !ok || doc.Revision != committed {
				t.Fatalf("daemon.db revision = %d (ok %v, err %v), want %d untouched", doc.Revision, ok, err, committed)
			}
			st := restarted.importStatus()
			if st.Status != rpc.TerminalEvidenceStatusImportError || st.ImportError == "" || st.AuthorityRevision != committed ||
				st.Contracts != 1 || !strings.Contains(st.Message, "stays in force") {
				t.Fatalf("import status = %+v", st)
			}
		})
	}
}

// With no revision committed before, a malformed import starts with an
// explicit empty authority: no holding is treated as terminal, so rules 6-8
// assess every holding normally rather than passing on the evidence's
// absence, and the import error is reported.
func TestTerminalEvidenceMalformedImportWithoutARevisionStartsEmpty(t *testing.T) {
	ctx := context.Background()
	store := openMarketTestCoreStore(t)
	path := filepath.Join(t.TempDir(), "terminal-evidence.json")
	writeTerminalImport(t, path, time.Time{}, []byte(`{"version": 1, "reviewed_at": "2026-09-26T08:00:00Z", "contracts": [{"contract": {"con_id": 900001}}]}`))
	terminal := newEarningsTerminalStore(path)
	if err := terminal.UseCoreStore(ctx, store, terminalImportBase); err != nil {
		t.Fatalf("a malformed import stopped startup: %v", err)
	}
	if terminal.revision == 0 || len(terminal.byConID) != 0 {
		t.Fatalf("authority = revision %d with %d contract(s), want an explicit empty revision", terminal.revision, len(terminal.byConID))
	}
	if _, found := terminal.terminalEarningsFor(terminalImportAAA, terminalImportBase); found {
		t.Fatal("a holding named only in the malformed file was treated as terminal")
	}
	s := &Server{earningsTerminal: terminal}
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "AAA", ConID: terminalImportConID, SecType: "STK"}}}
	if got := s.analysisPositions(pos, terminalImportBase); got != pos || len(got.Stocks) != 1 {
		t.Fatalf("analysis dropped the holding without terminal evidence: %+v", got)
	}
	st := terminal.importStatus()
	if st.Status != rpc.TerminalEvidenceStatusImportError || !strings.Contains(st.ImportError, "configured earnings terminal evidence") ||
		st.Contracts != 0 || !strings.Contains(st.Message, "none is in force") {
		t.Fatalf("import status = %+v", st)
	}
}

// The real daemon start: a malformed configured import reaches the socket
// and reports the error on status and the rules policy surface.
func TestDaemonStartsWithAMalformedTerminalEvidenceImport(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	evidence := filepath.Join(dir, "terminal-evidence.json")
	writeTerminalImport(t, evidence, time.Time{}, []byte("{ not json"))
	tlsFalse := false
	cfg := &config.Resolved{
		Gateway:  config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99), TLS: &tlsFalse},
		Rulebook: config.Rulebook{TerminalEvidenceFile: evidence},
	}
	cfg.Daemon.SetIdleTimeout(0)
	srv := New(Options{
		Config: cfg, SocketPath: filepath.Join(dir, "ibkrd.sock"), Version: "test",
		Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: filepath.Join(dir, "daemon.db"),
	})
	srv.orderJournal = newOrderJournalStore(filepath.Join(dir, "order-journal.jsonl"))
	serving := make(chan struct{}, 1)
	srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
	srv.attempterFactory = func(_ discover.Endpoint) connectAttempter { return &fakeAttempter{blockUntilCtxDone: true} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- srv.Start(ctx) }()
	select {
	case <-serving:
	case err := <-started:
		t.Fatalf("daemon did not start with a malformed terminal-evidence import: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon socket was not served")
	}

	var sub *rpc.SubsystemHealth
	for _, s := range srv.subsystemHealth(false, nil) {
		if s.Name == "terminal_evidence" {
			sub = &s
		}
	}
	if sub == nil || sub.Status != "degraded" || sub.LastError == "" || !strings.Contains(sub.Message, "none is in force") {
		t.Fatalf("status subsystem = %+v, want terminal_evidence degraded with the import error", sub)
	}
	rules := srv.rulebookUnavailableResult("test")
	if rules.TerminalEvidence == nil || rules.TerminalEvidence.Status != rpc.TerminalEvidenceStatusImportError || rules.TerminalEvidence.ImportError == "" {
		t.Fatalf("rules terminal_evidence = %+v", rules.TerminalEvidence)
	}
	cancel()
	select {
	case err := <-started:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	srv.Stop()
}

// A damaged daemon.db still stops startup, and the error names the recovery
// steps in the storage doc.
func TestDaemonStartIntegrityFailureNamesTheRecoveryDoc(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	db := filepath.Join(dir, "daemon.db")
	if err := os.WriteFile(db, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99)}}
	srv := New(Options{Config: cfg, SocketPath: filepath.Join(dir, "ibkrd.sock"), Version: "test",
		Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: db})
	err := srv.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), storageRecoveryDoc) {
		t.Fatalf("Start = %v, want a stop that names %s", err, storageRecoveryDoc)
	}
	srv.Stop()
}
