package corestore

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func openObservedStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), Options{Path: filepath.Join(privateTempDir(t), "daemon.db"), CommitObserver: func(AuthorityHead) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A commit counter that went back is a lost tail of commits, which the next
// commit would otherwise reuse so that the watermark agrees again: the store
// says so before anything is written on top, with both figures.
func TestAuthorityHeadThatWentBackLatches(t *testing.T) {
	ctx := context.Background()
	s := openObservedStore(t)
	first, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "daemon", Kind: "one", JSON: []byte(`{"n":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	// A trigger refuses any decrease through SQL, so only pages going back can
	// lower the counter: stand in for that with a commit the store saw land.
	s.writeMu.Lock()
	s.lastObservedHead.HeadGeneration += 1
	s.writeMu.Unlock()
	_, err = s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "daemon", Kind: "one", ExpectedRevision: first.Revision, JSON: []byte(`{"n":2}`)})
	if !errors.Is(err, ErrHeadWentBack) {
		t.Fatalf("a head that went back was written over: %v", err)
	}
	if h := s.Health(); h.Ready || h.Code != "lost_write" {
		t.Fatalf("health = %+v", h)
	}
	doc, _, err := s.GetStateDocument(ctx, "daemon", "one")
	if err != nil || doc.Revision != first.Revision {
		t.Fatalf("document after the refusal = %+v %v", doc, err)
	}
}

func TestAuthorityHeadMovingForwardDoesNotLatch(t *testing.T) {
	ctx := context.Background()
	s := openObservedStore(t)
	rev := int64(0)
	for range 3 {
		saved, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "daemon", Kind: "one", ExpectedRevision: rev, JSON: []byte(`{}`)})
		if err != nil {
			t.Fatal(err)
		}
		rev = saved.Revision
	}
	if h := s.Health(); !h.Ready {
		t.Fatalf("health = %+v", h)
	}
}

// The log has to say which document lost its write: on 2026-11-06 it named
// only the revisions, and the document had to be found from the file.
func TestLostWriteNamesTheDocument(t *testing.T) {
	ctx := context.Background()
	s := openObservedStore(t)
	first, err := s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "daemon", Kind: "one", JSON: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.CompareAndSwapStateDocument(ctx, StateDocumentCAS{ScopeKey: "daemon", Kind: "one", ExpectedRevision: first.Revision + 1, JSON: []byte(`{}`)})
	var conflict *RevisionConflictError
	if !errors.As(err, &conflict) || !conflict.LostWrite() || !strings.Contains(err.Error(), "daemon/one") {
		t.Fatalf("err = %v", err)
	}
}
