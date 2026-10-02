package corestore

import (
	"errors"
	"testing"
	"time"
)

func TestExactEventReceiptDigestAndNoOpRevisionFence(t *testing.T) {
	s, _ := openTestStore(t)
	cas := StateDocumentCAS{ScopeKey: "settings", Kind: "preferences", JSON: []byte(`{"value":1}`)}
	doc, err := s.CompareAndSwapStateDocument(t.Context(), cas)
	if err != nil {
		t.Fatal(err)
	}
	input := EventInput{ScopeKey: "settings", EventKey: "receipt-a", Type: "preference", Action: "save", Origin: "agent", OccurredAt: time.Now().UTC(), PayloadJSON: []byte(`{"request":"a"}`)}
	cas.ExpectedRevision = doc.Revision
	if _, err = s.AppendEventsAtStateRevision(t.Context(), cas, []EventInput{input}); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetEvent(t.Context(), "settings", "receipt-a")
	if err != nil || !found || string(got.PayloadJSON) != string(input.PayloadJSON) {
		t.Fatalf("receipt %+v %v %v", got, found, err)
	}
	if _, found, err = s.GetEvent(t.Context(), "other-scope", "receipt-a"); err != nil || found {
		t.Fatal("receipt escaped scope")
	}
	current, _, _ := s.GetStateDocument(t.Context(), "settings", "preferences")
	if current.Revision != doc.Revision || string(current.JSON) != string(doc.JSON) {
		t.Fatal("no-op changed state")
	}
	cas.ExpectedRevision = 0
	input.EventKey = "stale"
	if _, err = s.AppendEventsAtStateRevision(t.Context(), cas, []EventInput{input}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("no-op stale fence", err)
	}
	if _, found, _ := s.GetEvent(t.Context(), "settings", "stale"); found {
		t.Fatal("failed no-op appended receipt")
	}
	// SQL injection here is a corruption witness, bypassing the typed writer.
	if _, err = s.db.Exec(`INSERT INTO event_log(scope_key,event_key,event_type,action_kind,origin,occurred_at,occurred_at_ms,recorded_at,payload_json,payload_sha256) SELECT scope_key,'corrupt-receipt',event_type,action_kind,origin,occurred_at,occurred_at_ms,recorded_at,payload_json,zeroblob(32) FROM event_log WHERE event_key='receipt-a'`); err != nil {
		t.Fatal(err)
	}
	if _, found, err = s.GetEvent(t.Context(), "settings", "corrupt-receipt"); err == nil || found {
		t.Fatal("corrupt receipt treated as usable or absent")
	}
}

func TestAcceptedStateDocumentWithholdsLatchedView(t *testing.T) {
	s, _ := openTestStore(t)
	_, err := s.CompareAndSwapStateDocument(t.Context(), StateDocumentCAS{ScopeKey: "settings", Kind: "preferences", JSON: []byte(`{"value":1}`)})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	if err = s.WithAcceptedStateDocument(t.Context(), "settings", "preferences", func(doc StateDocument, found bool) error {
		called = true
		if !found || string(doc.JSON) != `{"value":1}` {
			t.Fatal("accepted read lost verified document")
		}
		return nil
	}); err != nil || !called {
		t.Fatal("healthy document not accepted", err)
	}
	s.latchHealth("head_watermark")
	called = false
	if err = s.WithAcceptedStateDocument(t.Context(), "settings", "preferences", func(StateDocument, bool) error { called = true; return nil }); !errors.Is(err, ErrBlocked) || called {
		t.Fatal("latched document published", err, called)
	}
}

func TestAbsentStateRevisionNoOpReceiptDoesNotCreateDocument(t *testing.T) {
	s, _ := openTestStore(t)
	cas := StateDocumentCAS{ScopeKey: "settings", Kind: "preferences", ExpectedRevision: 0}
	input := EventInput{ScopeKey: "settings", EventKey: "initial-clear", Type: "preference", Action: "save", Origin: "agent", OccurredAt: time.Now().UTC(), PayloadJSON: []byte(`{"value":null}`)}
	if _, err := s.AppendEventsAtStateRevision(t.Context(), cas, []EventInput{input}); err != nil {
		t.Fatal("absent default receipt rejected", err)
	}
	if _, found, err := s.GetStateDocument(t.Context(), "settings", "preferences"); err != nil || found {
		t.Fatal("default no-op created a settings document", err)
	}
	cas.JSON = []byte(`{"value":1}`)
	if _, err := s.CompareAndSwapStateDocument(t.Context(), cas); err != nil {
		t.Fatal(err)
	}
	input.EventKey = "stale-absence"
	if _, err := s.AppendEventsAtStateRevision(t.Context(), cas, []EventInput{input}); !errors.Is(err, ErrRevisionConflict) {
		t.Fatal("absence fence replaced existing state", err)
	}
	if _, found, _ := s.GetEvent(t.Context(), "settings", "stale-absence"); found {
		t.Fatal("stale absence wrote a receipt")
	}
}
