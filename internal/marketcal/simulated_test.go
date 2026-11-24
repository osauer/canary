package marketcal

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A simulated session replaces the official calendar for its market only,
// for every Calendar, until cleared.
func TestSimulatedSessionReplacesTheCalendar(t *testing.T) {
	t.Cleanup(ClearSimulatedSessions)
	sunday := time.Date(2026, 11, 22, 15, 0, 0, 0, time.UTC)
	if s, _ := New().SessionAt(MarketUSEquity, sunday); s.IsOpen {
		t.Fatal("the official calendar opens on a Sunday")
	}
	SimulateSession(MarketUSEquity, Window{Open: sunday.Add(-time.Hour), Close: sunday.Add(time.Hour)})
	if s, _ := NewWithClock(func() time.Time { return sunday }).SessionAt(MarketUSEquity, sunday); !s.IsOpen || s.Source != "twssim" {
		t.Fatalf("simulated open: %+v", s)
	}
	if s, _ := New().SessionAt(MarketDEXetra, sunday); s.IsOpen {
		t.Fatal("another market followed the simulated one")
	}
	SimulateSession(MarketUSEquity, Window{Open: sunday.Add(90 * time.Second), Close: sunday.Add(7 * time.Hour)})
	if s, _ := New().SessionAt(MarketUSEquity, sunday); s.IsOpen || s.NextOpen == nil || !s.NextOpen.Equal(sunday.Add(90*time.Second)) {
		t.Fatalf("opens in 90 s: %+v", s)
	}
	SimulateSession(MarketUSEquity, Window{})
	if s, _ := New().SessionAt(MarketUSEquity, sunday.Add(2*time.Hour)); s.IsOpen || s.State != StateClosed {
		t.Fatalf("simulated closed: %+v", s)
	}
	ClearSimulatedSessions()
	if s, _ := New().SessionAt(MarketUSEquity, sunday); s.Source == "twssim" {
		t.Fatal("cleared session still simulated")
	}
}

// Several simulated sessions follow one another: between two the market is
// closed and names the next open, and each is dated a day after the last.
func TestSimulatedSessionsFollowOneAnother(t *testing.T) {
	t.Cleanup(ClearSimulatedSessions)
	at := time.Date(2026, 11, 24, 15, 0, 0, 0, time.UTC)
	day1, day2 := Window{Open: at.Add(-time.Hour), Close: at.Add(time.Minute)}, Window{Open: at.Add(10 * time.Minute), Close: at.Add(time.Hour)}
	SimulateSessions(MarketUSEquity, day1, day2)
	if s, _ := New().SessionAt(MarketUSEquity, at); !s.IsOpen || !s.Close.Equal(day1.Close) || s.Date != "2026-11-24" {
		t.Fatalf("day one: %+v", s)
	}
	night := at.Add(5 * time.Minute)
	if s, _ := New().SessionAt(MarketUSEquity, night); s.IsOpen || s.NextOpen == nil || !s.NextOpen.Equal(day2.Open) || s.Date != "2026-11-25" {
		t.Fatalf("between them: %+v", s)
	}
	if s, _ := New().SessionAt(MarketUSEquity, day2.Open); !s.IsOpen || !s.Close.Equal(day2.Close) {
		t.Fatalf("day two: %+v", s)
	}
	if s, _ := New().SessionAt(MarketUSEquity, day2.Close); s.IsOpen || s.NextOpen != nil {
		t.Fatalf("after the last: %+v", s)
	}
}

// Only the simulator path installs a simulated session: cmd/canary-sim and
// tests. The installed daemon and CLI never call it.
func TestOnlyTheSimulatorInstallsSessions(t *testing.T) {
	root := strings.TrimSpace(run(t, "git", "rev-parse", "--show-toplevel"))
	out, _ := exec.Command("git", "-C", root, "grep", "-l", "marketcal.SimulateSession", "--", "*.go").Output()
	for file := range strings.FieldsSeq(string(out)) {
		if strings.HasSuffix(file, "_test.go") || strings.HasPrefix(file, "cmd/canary-sim/") {
			continue
		}
		t.Errorf("%s installs a simulated session outside the simulator path", file)
	}
}

func run(t *testing.T, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("%s unavailable: %v", name, err)
	}
	return string(out)
}
