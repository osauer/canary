package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestCalendarPreservesUnknownCoverageAndEarlyClose(t *testing.T) {
	want := rpc.MarketCalendarResult{
		Timezone: "America/New_York", CoverageStart: "2026-01-01", CoverageEnd: "2028-12-31",
		Source: "official_exchange_calendar", SourceURL: "https://www.nyse.com/markets/hours-calendars",
		Session: rpc.MarketSession{Date: "2029-01-02", State: "unknown", Reason: "outside embedded official calendar coverage"},
	}
	conn := &riskReadConn{result: want}
	var out, stderr bytes.Buffer
	env := &Env{Conn: conn, Stdout: &out, Stderr: &stderr}
	if Run(t.Context(), env, "calendar", []string{"--date", "2029-01-02", "--json"}) != 0 {
		t.Fatal(stderr.String())
	}
	var got rpc.MarketCalendarResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(conn.calls, []string{rpc.MethodMarketCalendar}) {
		t.Fatal("CLI changed the daemon's calendar evidence")
	}
	out.Reset()
	renderCalendar(env, want)
	if !strings.Contains(out.String(), "unknown") || !strings.Contains(out.String(), "2028-12-31") || strings.Contains(out.String(), "Next open") {
		t.Fatal("unknown coverage was presented as a usable schedule", out.String())
	}
	out.Reset()
	closeAt, _ := time.Parse(time.RFC3339, "2026-11-27T13:00:00-05:00")
	want.Session = rpc.MarketSession{Date: "2026-11-27", State: "early_close", Open: closeAt.Add(-210 * time.Minute), Close: closeAt}
	renderCalendar(env, want)
	if !strings.Contains(out.String(), "early_close") || !strings.Contains(out.String(), "13:00:00-05:00") {
		t.Fatal("early close or timezone lost", out.String())
	}
	for _, args := range [][]string{{"--at", "not-an-instant"}, {"--submit"}, {"refresh"}} {
		conn.calls = nil
		if Run(t.Context(), env, "calendar", args) == 0 || len(conn.calls) != 0 {
			t.Fatal("invalid input reached daemon", args)
		}
	}
	out.Reset()
	conn.err = &rpc.Error{Code: rpc.CodeBadRequest, Message: "invalid date"}
	if Run(t.Context(), env, "calendar", []string{"--date", "2026-99-01", "--json"}) == 0 || out.Len() != 0 {
		t.Fatal("rejected calendar request became a successful result")
	}
}
