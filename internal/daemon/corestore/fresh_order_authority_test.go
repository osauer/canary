package corestore

import (
	"errors"
	"testing"
)

func TestInitializeFreshOrderAuthorityIsAtomicAndAllowsUnrelatedState(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{
		ScopeKey: "daemon", Kind: "settings_v1", JSON: []byte(`{"version":1}`),
	}); err != nil {
		t.Fatalf("seed unrelated state: %v", err)
	}

	if err := store.InitializeFreshOrderAuthority(t.Context()); err != nil {
		t.Fatalf("InitializeFreshOrderAuthority: %v", err)
	}
	for table, wantRows := range map[string]int{
		"legacy_imports":          1,
		"order_id_floors":         1,
		"order_events":            0,
		"consumed_preview_tokens": 0,
		"broker_scopes":           0,
	} {
		if got := countRows(t, store, table); got != wantRows {
			t.Fatalf("%s rows = %d, want %d", table, got, wantRows)
		}
	}
	floor, err := store.GlobalOrderIDFloor(t.Context())
	if err != nil || floor != 0 {
		t.Fatalf("global floor = %d, error = %v", floor, err)
	}
	if err := store.InitializeFreshOrderAuthority(t.Context()); !errors.Is(err, ErrFreshAuthorityConflict) {
		t.Fatalf("second initialization error = %v, want ErrFreshAuthorityConflict", err)
	}
}
