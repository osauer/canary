package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// damagedAuthority writes one large rebuildable document and one document
// the daemon cannot rebuild, closes the store, and overwrites the
// chosen document's stored JSON so its content hash no longer matches.
func damagedAuthority(t *testing.T, damage string) (path string, head AuthorityHead) {
	t.Helper()
	path = filepath.Join(privateTempDir(t), "daemon.db")
	s, err := Open(t.Context(), Options{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"cache": `{"pad":"` + strings.Repeat("CACHEMARK", 3000) + `"}`,
		"order": `{"pad":"` + strings.Repeat("ORDERMARK", 3000) + `"}`,
	}
	for kind, body := range docs {
		if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "test", Kind: kind, JSON: []byte(body)}); err != nil {
			t.Fatal(err)
		}
	}
	if head, err = s.AuthorityHead(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Free pages can hold an older copy, so every copy is damaged. Include
	// the JSON prefix: a bare marker can begin at an overflow page pointer's
	// low byte (79 is 'O') and accidentally damage the b-tree as well.
	prefix := `{"pad":"`
	marker := []byte(prefix + strings.ToUpper(damage) + "MARK")
	if !bytes.Contains(raw, marker) {
		t.Fatalf("marker for %s not found in the file", damage)
	}
	raw = bytes.ReplaceAll(raw, marker, []byte(prefix+strings.Repeat("X", len(damage)+len("MARK"))))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, head
}

func rebuildable(kinds ...string) func(string) bool {
	return func(kind string) bool { return slices.Contains(kinds, kind) }
}

func TestSalvageDropsOnlyRebuildableDocumentsAndKeepsTheHead(t *testing.T) {
	path, head := damagedAuthority(t, "cache")
	if _, err := Open(t.Context(), Options{Path: path}); !errors.Is(err, ErrDamaged) {
		t.Fatalf("open of a damaged authority: %v, want ErrDamaged", err)
	}
	preserve := filepath.Join(filepath.Dir(path), "damaged")
	result, err := Salvage(t.Context(), path, head, preserve, rebuildable("cache"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Dropped["cache"] != 1 || len(result.Dropped) != 1 {
		t.Fatalf("dropped %v, want one cache document", result.Dropped)
	}
	if _, err := os.Stat(filepath.Join(result.Preserved, "daemon.db")); err != nil {
		t.Fatalf("damaged file not kept: %v", err)
	}
	s, err := Open(t.Context(), Options{Path: path, MinimumHead: &head})
	if err != nil {
		t.Fatalf("open after salvage: %v", err)
	}
	defer s.Close()
	if _, ok, err := s.GetStateDocument(t.Context(), "test", "cache"); err != nil || ok {
		t.Fatalf("damaged cache document survived: ok=%v err=%v", ok, err)
	}
	if doc, ok, err := s.GetStateDocument(t.Context(), "test", "order"); err != nil || !ok || !strings.Contains(string(doc.JSON), "ORDERMARK") {
		t.Fatalf("intact document lost: ok=%v err=%v", ok, err)
	}
	got, err := s.AuthorityHead(t.Context())
	if err != nil || got != head {
		t.Fatalf("head after salvage = %+v err=%v, want %+v", got, err, head)
	}
}

func TestSalvageRefusesALossItCannotRebuildAndLeavesTheFile(t *testing.T) {
	path, head := damagedAuthority(t, "order")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Salvage(t.Context(), path, head, filepath.Join(filepath.Dir(path), "damaged"), rebuildable("cache"))
	if err == nil || !strings.Contains(err.Error(), "order") {
		t.Fatalf("salvage of a non-rebuildable loss: %v, want a refusal naming the kind", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("a refused salvage changed the authority file")
	}
	if _, err := os.Stat(path + ".salvage"); !os.IsNotExist(err) {
		t.Fatalf("refused salvage left its rebuild behind: %v", err)
	}
}

func TestSalvageRefusesARollback(t *testing.T) {
	path, head := damagedAuthority(t, "cache")
	ahead := head
	ahead.HeadGeneration++
	_, err := Salvage(context.Background(), path, ahead, filepath.Join(filepath.Dir(path), "damaged"), rebuildable("cache"))
	if !errors.Is(err, ErrRollback) {
		t.Fatalf("salvage below the watermark: %v, want ErrRollback", err)
	}
}

func TestSalvageRefusesStructuralDamageThatHidesAuthorityRows(t *testing.T) {
	s, path := openTestStore(t)
	if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "test", Kind: "holding_locks", JSON: []byte(`{"version":1,"locks":[{"symbol":"SYNTH"}]}`),
	}); err != nil {
		t.Fatal(err)
	}
	head, err := s.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var root, pageSize int64
	if err := s.db.QueryRowContext(t.Context(), `SELECT rootpage FROM sqlite_schema WHERE name='state_documents'`).Scan(&root); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(t.Context(), `PRAGMA page_size`).Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	page := raw[(root-1)*pageSize : root*pageSize]
	if page[0] != 0x0d || binary.BigEndian.Uint16(page[3:5]) != 1 {
		t.Fatal("expected a table leaf containing the one authority document")
	}
	// Damage the leaf's cell count so SELECT sees an empty table even though
	// the committed authority row and unchanged head remain in the file.
	// Hashing only copied rows cannot prove that this row was preserved.
	binary.BigEndian.PutUint16(page[3:5], 0)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if probe, err := Open(t.Context(), Options{Path: path, MinimumHead: &head}); !errors.Is(err, ErrDamaged) {
		if probe != nil {
			_ = probe.Close()
		}
		t.Fatalf("structurally damaged authority opened: %v", err)
	}
	result, err := Salvage(t.Context(), path, head, filepath.Join(filepath.Dir(path), "damaged"), rebuildable("cache"))
	if err == nil {
		t.Fatal("salvage accepted structural damage hiding a non-rebuildable authority row")
	}
	for _, kept := range []string{path, filepath.Join(result.Preserved, filepath.Base(path))} {
		got, readErr := os.ReadFile(kept)
		if readErr != nil || !bytes.Equal(got, raw) {
			t.Fatalf("damaged authority was not preserved at %s: %v", kept, readErr)
		}
	}
}

func TestSalvagePreservesEventSequenceAfterPruning(t *testing.T) {
	for _, legacyDuplicates := range []bool{false, true} {
		t.Run(fmt.Sprint(legacyDuplicates), func(t *testing.T) {
			s, path := openTestStore(t)
			event := func(key string) EventInput {
				return EventInput{ScopeKey: "test", EventKey: key, Type: "test", Action: "record", Origin: "test", OccurredAt: time.Now(), PayloadJSON: []byte(`{}`)}
			}
			if _, err := s.AppendEvents(t.Context(), []EventInput{event("retained"), event("pruned")}); err != nil {
				t.Fatal(err)
			}
			// Approved operational-history migrations can prune the highest
			// event while retaining its AUTOINCREMENT and authority-head floors.
			for _, query := range []string{`DROP TRIGGER event_log_no_delete`, `DELETE FROM event_log WHERE event_seq=2`, appendOnlyDeleteTrigger("event_log")} {
				if _, err := s.db.ExecContext(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			}
			if legacyDuplicates {
				// Earlier salvage appended the old sequence after the copy had
				// seeded a lower one, leaving duplicate names in sqlite_sequence.
				for _, query := range []string{`UPDATE sqlite_sequence SET seq=1 WHERE name='event_log'`, `INSERT INTO sqlite_sequence(name,seq) VALUES('event_log',2)`} {
					if _, err := s.db.ExecContext(t.Context(), query); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "test", Kind: "cache", JSON: []byte(`{"ok":true}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(t.Context(), `UPDATE state_documents SET document_json=CAST('{"ok":false}' AS BLOB) WHERE kind='cache'`); err != nil {
				t.Fatal(err)
			}
			head, err := s.AuthorityHead(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Salvage(t.Context(), path, head, filepath.Join(filepath.Dir(path), "damaged"), rebuildable("cache")); err != nil {
				t.Fatal(err)
			}
			repaired, err := Open(t.Context(), Options{Path: path, MinimumHead: &head})
			if err != nil {
				t.Fatal(err)
			}
			defer repaired.Close()
			receipts, err := repaired.AppendEvents(t.Context(), []EventInput{event("after-repair")})
			if err != nil {
				t.Fatal(err)
			}
			if receipts[0].EventSeq <= head.LastEventSeq {
				t.Errorf("new event sequence %d reused the pre-repair frontier %d", receipts[0].EventSeq, head.LastEventSeq)
			}
			got, err := repaired.LoadEvents(t.Context(), EventQuery{AfterEventSeq: head.LastEventSeq})
			if err != nil || len(got) != 1 || got[0].EventKey != "after-repair" {
				t.Fatalf("reader at pre-repair frontier missed the new event: %v, %v", got, err)
			}
		})
	}
}
