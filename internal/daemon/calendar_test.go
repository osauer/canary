package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// A scheduler must use the daemon's exchange facts, including DST, early closes
// and coverage exhaustion. A weekday extrapolation would create false sessions.
func TestCalendarSchedulingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		market, at, state, open, close string
		isOpen                         bool
	}{
		{"us", "2026-11-27T12:00:00-05:00", "early_close", "2026-11-27T14:30:00Z", "2026-11-27T18:00:00Z", true},
		{"us", "2026-11-27T13:00:00-05:00", "early_close", "2026-11-27T14:30:00Z", "2026-11-27T18:00:00Z", false},
		{"us", "2026-03-09T09:30:00-04:00", "regular", "2026-03-09T13:30:00Z", "2026-03-09T20:00:00Z", true},
		{"us-options", "2026-03-09T16:10:00-04:00", "regular", "2026-03-09T13:30:00Z", "2026-03-09T20:15:00Z", true},
		{"us", "2026-09-07T12:00:00-04:00", "holiday", "", "", false},
		{"us", "2029-01-02T12:00:00-05:00", "unknown", "", "", false},
	} {
		t.Run(tc.market+"/"+tc.at, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, tc.at)
			if err != nil {
				t.Fatal(err)
			}
			params, _ := json.Marshal(rpc.MarketCalendarParams{Market: tc.market, At: at, Days: 1})
			res, err := (&Server{}).handleMarketCalendar(&rpc.Request{Params: params})
			if err != nil {
				t.Fatal(err)
			}
			s := res.Session
			if s.State != tc.state || s.IsOpen != tc.isOpen || len(res.Sessions) != 1 || res.Timezone != "America/New_York" || res.SourceURL == "" || res.CoverageEnd == "" {
				t.Fatalf("lost scheduling authority: %+v", res)
			}
			if tc.open == "" {
				if !s.Open.IsZero() || !s.Close.IsZero() || (s.State == "unknown" && (s.NextOpen != nil || s.NextClose != nil)) {
					t.Fatal("invented trading time without calendar coverage")
				}
			} else if s.Open.UTC().Format(time.RFC3339) != tc.open || s.Close.UTC().Format(time.RFC3339) != tc.close {
				t.Fatalf("wrong session instant: open=%s close=%s", s.Open, s.Close)
			}
		})
	}
	for _, raw := range []string{`{"market":"unrecognized"}`, `{"date":"2026-99-01"}`, `{"at":"not-an-instant"}`} {
		if _, err := (&Server{}).handleMarketCalendar(&rpc.Request{Params: json.RawMessage(raw)}); err == nil {
			t.Fatalf("invalid scheduling query accepted: %s", raw)
		}
	}
}

func TestCalendarWindowsReachConsumerWithoutExpandingWarningScope(t *testing.T) {
	params := json.RawMessage(`{"market":"jp","at":"2026-09-14T12:00:00+09:00","days":1}`)
	res, err := (&Server{}).handleMarketCalendar(&rpc.Request{Params: params})
	if err != nil {
		t.Fatal(err)
	}
	if res.Market != "jp_tse" || res.Session.IsOpen || len(res.Session.Windows) != 2 || len(res.Sessions[0].Windows) != 2 {
		t.Fatalf("RPC lost lunch closure: %+v", res)
	}
	// Tokyo/Hong Kong open while the preexisting warning markets are closed.
	at, _ := time.Parse(time.RFC3339, "2026-09-14T02:00:00Z")
	if anySupportedMarketOpen(at) {
		t.Fatal("calendar catalogue expanded backend warning policy")
	}
}
