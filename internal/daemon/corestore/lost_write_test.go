package corestore

import (
	"errors"
	"testing"
)

// A stored revision older than the one this process committed is a lost
// write: the store latches fail-closed. A stored revision newer than the
// caller expected is an ordinary race and leaves the store Ready.
func TestLostWriteLatchesTheStoreAndOrdinaryConflictDoesNot(t *testing.T) {
	f := openRecoveryFixture(t)
	save := func(expected int64) (StateDocument, error) {
		return f.store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "test", Kind: "victim", ExpectedRevision: expected, JSON: []byte(`{}`)})
	}
	for expected := range int64(2) {
		if _, err := save(expected); err != nil {
			t.Fatal(err)
		}
	}

	// Another writer moved the document on: the caller is behind, not the store.
	if _, err := f.store.db.ExecContext(t.Context(), `UPDATE state_documents SET revision=3 WHERE scope_key='test' AND kind='victim'`); err != nil {
		t.Fatal(err)
	}
	_, err := save(2)
	if conflict, ok := errors.AsType[*RevisionConflictError](err); !ok || conflict.LostWrite() {
		t.Fatalf("newer stored revision: err=%v, want an ordinary conflict", err)
	}
	if !f.store.Health().Ready {
		t.Fatalf("an ordinary conflict latched the store: %+v", f.store.Health())
	}

	// The committed revision 3 goes back to 2 underneath the store.
	if _, err := f.store.db.ExecContext(t.Context(), `UPDATE state_documents SET revision=2 WHERE scope_key='test' AND kind='victim'`); err != nil {
		t.Fatal(err)
	}
	_, err = save(3)
	if conflict, ok := errors.AsType[*RevisionConflictError](err); !ok || !conflict.LostWrite() {
		t.Fatalf("older stored revision: err=%v, want a lost write", err)
	}
	if got := f.store.Health(); got.Ready || got.Code != "lost_write" {
		t.Fatalf("health after a lost write = %+v, want latched lost_write", got)
	}
	if seen := f.recorder.transitions(t, 1); seen[0].health.Code != "lost_write" {
		t.Fatalf("health observer saw %+v", seen[0])
	}
	if err := f.mutate(t, "after"); err == nil {
		t.Fatal("a mutation was accepted after a lost write")
	}
}
