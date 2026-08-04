package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The breadth subsystem row used to take its status from the PRIMARY
// connector's connected bool. Breadth's fan-out rides a second connection,
// and on 2026-08-03 that one died while the primary stayed up: the row read
// "ready" for the seven hours breadth published nothing.
func TestBreadthSubsystemHealthReadsItsOwnConnection(t *testing.T) {
	t.Parallel()
	attempt := time.Now().Add(-90 * time.Second)

	t.Run("no bulk dial attempted yet stays ready", func(t *testing.T) {
		// Start-up shape: postConnectSetup has not dialled the bulk lane, so
		// a nil connector is expected rather than a fault. Reporting degraded
		// here would light the row on every daemon boot.
		srv := newTestServer(t)
		if got := srv.breadthSubsystemHealth("ready"); got.Status != "ready" {
			t.Errorf("status = %q, want ready (message %q)", got.Status, got.Message)
		}
	})

	t.Run("dialled once and now dead reads degraded", func(t *testing.T) {
		srv := newTestServer(t)
		srv.lastBreadthConnectAttemptAt = attempt
		got := srv.breadthSubsystemHealth("ready")
		if got.Status != "degraded" {
			t.Fatalf("status = %q, want degraded", got.Status)
		}
		if got.LastError != "breadth_bulk_connector_down" {
			t.Errorf("LastError = %q, want breadth_bulk_connector_down", got.LastError)
		}
		if !got.LastErrorAt.Equal(attempt) {
			t.Errorf("LastErrorAt = %s, want the recorded dial attempt %s", got.LastErrorAt, attempt)
		}
		if got.Message == "" {
			t.Error("degraded row must say which connection is down")
		}
	})

	t.Run("failed redials are counted in the message", func(t *testing.T) {
		srv := newTestServer(t)
		srv.lastBreadthConnectAttemptAt = attempt
		srv.breadthConnectFailStreak = 4
		if got := srv.breadthSubsystemHealth("ready").Message; !strings.Contains(got, "4 redial attempts failed") {
			t.Errorf("message = %q, want the failed-redial count", got)
		}
	})

	t.Run("a down primary still wins", func(t *testing.T) {
		// The bulk lane cannot outlive the primary, so splitting hairs about
		// which of the two is deader would only make the row harder to read.
		srv := newTestServer(t)
		srv.lastBreadthConnectAttemptAt = attempt
		if got := srv.breadthSubsystemHealth("unavailable"); got.Status != "unavailable" {
			t.Errorf("status = %q, want unavailable", got.Status)
		}
	})
}

// alertShadowDataHealthFacts validates LastErrorAt against the health
// snapshot's as_of for EVERY subsystem row, before it filters down to the
// three roots it actually reports on. A row stamped with a next-retry time
// rather than an observation time therefore fails the whole data-health
// alert source closed, taking push delivery with it.
func TestSubsystemRowsNeverStampAFutureLastErrorAt(t *testing.T) {
	t.Parallel()
	srv := newTestServer(t)
	srv.lastBreadthConnectAttemptAt = time.Now().Add(-time.Minute)

	res := srv.statusHealthSnapshot()
	after := time.Now()
	for _, sub := range res.Subsystems {
		if sub.LastErrorAt.After(after) {
			t.Errorf("subsystem %q stamped LastErrorAt %s in the future", sub.Name, sub.LastErrorAt)
		}
	}
}

// The engine keeps serving its last good snapshot when a refresh cannot
// converge, so a lane that stops producing serves plausible numbers
// indefinitely. Stale dates them. State deliberately stays "ready": seven
// consumers gate the whole market read on it, and a stale close is still
// evidence.
func TestBreadthResultDatesAStaleSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Now()

	for _, tc := range []struct {
		name       string
		sessionKey string
		wantStale  bool
	}{
		{"latest completed session is current", spx.CompletedSessionKey(now), false},
		{"an older session is stale", "2026-07-20", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t)
			store := spx.NewStore(t.TempDir())
			if err := store.SaveSnapshot(spx.Snapshot{
				AsOf:           now.Add(-time.Hour),
				SessionKey:     tc.sessionKey,
				Method:         spx.MethodConstituentFanout,
				MemberCount:    503,
				Coverage:       480,
				PctAbove50DMA:  61.2,
				PctAbove200DMA: 55.0,
			}); err != nil {
				t.Fatalf("SaveSnapshot: %v", err)
			}
			srv.breadth = spx.New(store, &spx.FakeBarFetcher{}, spx.Options{})

			res, err := srv.buildBreadthSPX(&rpc.Request{}, false)
			if err != nil {
				t.Fatalf("buildBreadthSPX: %v", err)
			}
			if res.Stale != tc.wantStale {
				t.Errorf("Stale = %v, want %v (session_key %q)", res.Stale, tc.wantStale, res.SessionKey)
			}
			if res.State != rpc.BreadthStateReady {
				t.Errorf("State = %q, want ready — Stale must not gate the market read", res.State)
			}
			if res.SessionKey != tc.sessionKey {
				t.Errorf("SessionKey = %q, want %q", res.SessionKey, tc.sessionKey)
			}
		})
	}
}

// The brief is what the paired app renders, and it marked breadth OK on
// State == ready — which a stale snapshot satisfies. Every row now names its
// session, and an overdue one degrades. The ordinary post-close window does
// not: the newer session's fan-out takes ~75 min, and degrading through it
// would light the row on the phone after every close.
func TestBriefBreadthRowNamesItsSessionAndDegradesOnlyWhenOverdue(t *testing.T) {
	t.Parallel()
	// 2026-08-03 Mon and 2026-08-04 Tue are both regular US-equity sessions.
	// Tuesday's breadth refresh is due at its 16:00 ET close plus the 35 min
	// settlement pad — 20:35 UTC — and is allowed 90 min to publish.
	const monday, tuesday = "2026-08-03", "2026-08-04"
	insideWindow := time.Date(2026, 8, 4, 21, 0, 0, 0, time.UTC)
	pastWindow := time.Date(2026, 8, 4, 23, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name       string
		now        time.Time
		breadth    rpc.BreadthSPXResult
		wantStatus string
		wantDetail string
	}{
		{
			name:       "current session reads ok and says which one",
			now:        insideWindow,
			breadth:    rpc.BreadthSPXResult{State: rpc.BreadthStateReady, SessionKey: tuesday, PctAbove50DMA: 61.2},
			wantStatus: rpc.BriefStatusOK,
			wantDetail: tuesday,
		},
		{
			name:       "stale inside the publication window is not a fault",
			now:        insideWindow,
			breadth:    rpc.BreadthSPXResult{State: rpc.BreadthStateReady, SessionKey: monday, Stale: true, Refreshing: true, PctAbove50DMA: 61.2},
			wantStatus: rpc.BriefStatusOK,
			wantDetail: "still computing",
		},
		{
			name:       "stale past the window is overdue",
			now:        pastWindow,
			breadth:    rpc.BreadthSPXResult{State: rpc.BreadthStateReady, SessionKey: monday, Stale: true, PctAbove50DMA: 61.2},
			wantStatus: rpc.BriefStatusDegraded,
			wantDetail: "overdue",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			market, _ := composeBriefMarket(tc.now, nil, nil, nil, &tc.breadth, nil, nil, nil, nil, nil, nil, nil, false)
			row := market.Breadth
			if row.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q (detail %q)", row.Status, tc.wantStatus, row.Detail)
			}
			if !strings.Contains(row.Detail, tc.wantDetail) {
				t.Errorf("Detail = %q, want it to mention %q", row.Detail, tc.wantDetail)
			}
			if !strings.Contains(row.Detail, tc.breadth.SessionKey) {
				t.Errorf("Detail = %q, want it to name session %q", row.Detail, tc.breadth.SessionKey)
			}
			// A stale close is still a real close. Losing the numbers would
			// trade one wrong reading for no reading.
			if row.PctAbove50DMA == nil || *row.PctAbove50DMA != tc.breadth.PctAbove50DMA {
				t.Errorf("PctAbove50DMA = %v, want the served reading %v", row.PctAbove50DMA, tc.breadth.PctAbove50DMA)
			}
		})
	}
}
