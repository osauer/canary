package corestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	// Free pages can hold an older copy, so every copy is damaged.
	marker := []byte(strings.ToUpper(damage) + "MARK")
	if !bytes.Contains(raw, marker) {
		t.Fatalf("marker for %s not found in the file", damage)
	}
	raw = bytes.ReplaceAll(raw, marker, []byte(strings.Repeat("X", len(marker))))
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
