package corestore

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func TestSetupCoverageKeepsTheNewestThirtySessions(t *testing.T) {
	s, _ := openTestStore(t)
	if _, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "fixture", Kind: "unrelated", JSON: json.RawMessage(`{"kept":true}`)}); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.UTC)
	var want []string
	for i := range SetupCoverageSessions + 5 {
		session := day.AddDate(0, 0, i).Format(time.DateOnly)
		if _, err := s.SaveSetupCoverage(t.Context(), session, []byte(`{"version":1}`)); err != nil {
			t.Fatal(err)
		}
		want = append(want, session)
	}
	// Rewriting a retained session replaces it in place.
	if saved, err := s.SaveSetupCoverage(t.Context(), want[len(want)-1], []byte(`{"version":1,"rewritten":true}`)); err != nil || saved.Revision != 2 {
		t.Fatal(saved, err)
	}
	slices.Reverse(want)
	want = want[:SetupCoverageSessions]
	got, err := s.ListSetupCoverageSessions(t.Context())
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("sessions = %v, %v", got, err)
	}
	if _, ok, err := s.LoadSetupCoverage(t.Context(), day.Format(time.DateOnly)); err != nil || ok {
		t.Fatal("oldest session was not bounded", err)
	}
	doc, ok, err := s.LoadSetupCoverage(t.Context(), want[0])
	if err != nil || !ok || string(doc.JSON) != `{"version":1,"rewritten":true}` {
		t.Fatal(string(doc.JSON), ok, err)
	}
	if _, ok, err := s.GetStateDocument(t.Context(), "fixture", "unrelated"); err != nil || !ok {
		t.Fatal("coverage retention touched other authority", err)
	}
	for _, bad := range []string{"2026-9-30", "2026-02-30", "latest", "../2026-09-30"} {
		if _, err := s.SaveSetupCoverage(t.Context(), bad, []byte(`{}`)); err == nil {
			t.Fatalf("session %q accepted", bad)
		}
	}
	if _, err := s.SaveSetupCoverage(t.Context(), want[0], make([]byte, 1<<20+1)); err == nil {
		t.Fatal("oversized coverage accepted")
	}
}
