package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	edgecore "github.com/osauer/canary/v2/internal/edge"
	"github.com/osauer/canary/v2/internal/rpc"
)

func uncachedOrderReadAllocations(t *testing.T, s *Server) float64 {
	t.Helper()
	return testing.AllocsPerRun(1, func() {
		events, err := s.orderJournal.LoadEvents(0)
		if err != nil {
			t.Fatal(err)
		}
		views, byKey := buildOrderViews(events), buildOrderEventsByKey(events)
		inferDayOrderExpiry(views, byKey, s.orderNow())
		if len(views) != 1 || len(byKey) == 0 {
			t.Fatal("missing synthetic order evidence")
		}
	})
}

func TestOrderViewsWarmReadsAvoidHistoryReplay(t *testing.T) {
	s, _ := healthJournalFixture(t)
	cold := uncachedOrderReadAllocations(t, s)
	warm := testing.AllocsPerRun(3, func() {
		views, byKey, err := s.loadOrderViews()
		if err != nil || len(views) != 1 || !views[0].Open || len(byKey) == 0 {
			t.Fatalf("warm order view: rows=%d err=%v", len(views), err)
		}
	})
	if warm >= cold/10 {
		t.Fatalf("ordinary readers replay unchanged history: warm %.0f, full replay %.0f allocations", warm, cold)
	}
	t.Logf("ordinary order reads: warm %.0f, full replay %.0f allocations", warm, cold)
}

func TestOrderReadProjectionReturnsOwnedViewsAndEvents(t *testing.T) {
	s, ev := healthJournalFixture(t)
	ev.At = ev.At.Add(time.Second)
	ev.Trail = &rpc.OrderTrailSpec{OffsetType: "percent", TrailingPercent: new(1.), InitialStopPrice: 100}
	if err := s.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	views, events, err := s.loadOrderViews()
	if err != nil || len(views) != 1 || views[0].Trail == nil {
		t.Fatalf("trailing order: %+v err=%v", views, err)
	}
	key := orderViewKey(views[0])
	rows := events[key]
	if len(rows) == 0 || rows[len(rows)-1].Trail == nil {
		t.Fatal("missing trailing event")
	}
	views[0].Open = false
	*views[0].Trail.TrailingPercent = 99
	*rows[len(rows)-1].Trail.TrailingPercent = 99
	delete(events, key)
	again, byKey, err := s.loadOrderViews()
	if err != nil || len(again) != 1 || !again[0].Open || *again[0].Trail.TrailingPercent != 1 {
		t.Fatalf("caller mutated retained order view: %+v err=%v", again, err)
	}
	rows = byKey[key]
	if len(rows) == 0 || *rows[len(rows)-1].Trail.TrailingPercent != 1 {
		t.Fatal("caller mutated retained event map or trailing terms")
	}
	ev.At = ev.At.Add(time.Second)
	ev.Status, ev.Remaining, ev.Filled = "Filled", 0, ev.Quantity
	if err := s.orderJournal.Append(ev); err != nil {
		t.Fatal(err)
	}
	views, _, head, err := s.loadOrderViewsAtStableHead()
	current, headErr := s.orderJournal.AuthorityHead()
	if err != nil || headErr != nil || head != current.LastEventSeq || len(views) != 1 || views[0].Open {
		t.Fatalf("fenced read missed appended order evidence: head=%d err=%v headErr=%v", head, err, headErr)
	}
}

func TestEdgeHealthWarmReadsAvoidPublicationDecode(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "fixture"}}, coreStore: store}
	result := edgecore.Result{}
	for i := range 1000 {
		result.Changes = append(result.Changes, edgecore.Change{ID: fmt.Sprintf("synthetic-%d", i), Symbol: "SYNTH", Action: "open", Direction: "long", ExecutionVWAP: new(10.)})
	}
	publication := edgePublication{State: rpc.EdgeStateCurrent, Windows: map[string]edgecore.Result{"365d": result}}
	if err := s.saveEdgePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	cold := testing.AllocsPerRun(1, func() {
		if _, ok, err := s.loadEdgePublication(t.Context()); err != nil || !ok {
			t.Fatalf("publication: ok=%v err=%v", ok, err)
		}
	})
	warm := testing.AllocsPerRun(3, func() {
		if got := s.edgeSubsystemHealth(); got.Status != "ready" {
			t.Fatalf("health: %+v", got)
		}
	})
	if warm >= cold/10 {
		t.Fatalf("health decodes unchanged analytical results: warm %.0f, full decode %.0f allocations", warm, cold)
	}
	t.Logf("Edge health: warm %.0f, full decode %.0f allocations", warm, cold)
	publication.State = rpc.EdgeStateDegraded
	if err := s.saveEdgePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if got := s.edgeSubsystemHealth(); got.Status != "degraded" {
		t.Fatalf("changed publication reused ready state: %+v", got)
	}
	for _, tc := range []struct{ raw, status string }{
		{fmt.Sprintf(`{"version":%d,"state":"current","windows":{"365d":42}}`, edgePublicationVersion), "unavailable"},
		{fmt.Sprintf(`{"version":%d,"state":"current"}`, edgePublicationVersion-1), "computing"},
		{fmt.Sprintf(`{"version":%d,"state":"current"}`, edgePublicationVersion+1), "unavailable"},
	} {
		if err := s.replaceEdgeStateDocument(t.Context(), edgePublicationStateKind, []byte(tc.raw)); err != nil {
			t.Fatal(err)
		}
		if got := s.edgeSubsystemHealth(); got.Status != tc.status {
			t.Fatalf("invalid or unsupported publication reused prior health: %+v want %s", got, tc.status)
		}
	}
	publication.State = rpc.EdgeStateCurrent
	if err := s.saveEdgePublication(t.Context(), publication); err != nil {
		t.Fatal(err)
	}
	if got := s.edgeSubsystemHealth(); got.Status != "ready" {
		t.Fatalf("repaired publication: %+v", got)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if got := s.edgeSubsystemHealth(); got.Status != "unavailable" {
		t.Fatalf("closed storage reused ready Edge health: %+v", got)
	}
}
