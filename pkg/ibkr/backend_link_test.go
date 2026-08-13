package ibkr

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestParseMaintenanceWindows(t *testing.T) {
	windows, err := ParseMaintenanceWindows([]string{
		"Sat-Thu 23:45-00:45 America/New_York",
		"Fri 23:45-00:30 America/New_York",
	})
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("windows = %d, want 2", len(windows))
	}
	if windows[0].Days[time.Friday] {
		t.Fatal("Sat-Thu range must exclude Friday")
	}
	for _, d := range []time.Weekday{time.Saturday, time.Sunday, time.Monday, time.Thursday} {
		if !windows[0].Days[d] {
			t.Fatalf("Sat-Thu range missing %s", d)
		}
	}

	for _, bad := range []string{
		"Someday 23:45-00:45 America/New_York",
		"Fri 23:45 America/New_York",
		"Fri 23:45-00:30",
		"Fri 25:00-00:30 America/New_York",
		"Fri 23:45-00:30 Mars/Olympus",
	} {
		if _, err := ParseMaintenanceWindows([]string{bad}); err == nil {
			t.Errorf("spec %q parsed, want error", bad)
		}
	}
}

func TestMaintenanceWindowContains(t *testing.T) {
	windows, err := DefaultIBKRMaintenanceWindows()
	if err != nil {
		t.Fatal(err)
	}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   time.Time
		want bool
	}{
		// Tue 23:50 ET: inside the Sat-Thu window's start.
		{time.Date(2026, 8, 11, 23, 50, 0, 0, ny), true},
		// Wed 00:15 ET: inside via the midnight wrap of Tuesday's window.
		{time.Date(2026, 8, 12, 0, 15, 0, 0, ny), true},
		// Sat 00:15 ET: inside Friday's shorter window.
		{time.Date(2026, 8, 15, 0, 15, 0, 0, ny), true},
		// Sat 00:40 ET: Friday's window ended at 00:30.
		{time.Date(2026, 8, 15, 0, 40, 0, 0, ny), false},
		// Fri 00:40 ET: inside via Thursday's full window.
		{time.Date(2026, 8, 14, 0, 40, 0, 0, ny), true},
		// Wed noon ET: nowhere near.
		{time.Date(2026, 8, 12, 12, 0, 0, 0, ny), false},
	}
	for _, tc := range cases {
		if got := maintenanceWindowsContain(windows, tc.at); got != tc.want {
			t.Errorf("Contains(%s) = %t, want %t", tc.at, got, tc.want)
		}
	}
}

// captureConnectorLogs routes the package logger into a buffer for the test.
func captureConnectorLogs(t *testing.T) *safeBuffer {
	t.Helper()
	buf := &safeBuffer{}
	SetLogger(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	SetLogLevel("info")
	t.Cleanup(func() { SetLogger(nil) })
	return buf
}

func logLines(buf *safeBuffer, substr string) []string {
	var out []string
	for line := range strings.SplitSeq(strings.TrimSpace(string(buf.Bytes())), "\n") {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
}

// A flurry of blips must produce exactly one loss warning, one restore
// warning, and one episode summary — with mid-episode events demoted to INFO
// — while a session-open loss and a long outage always warn.
func TestBackendFlapEpisodeCoalescesLogging(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.epGap = 80 * time.Millisecond

	t0 := time.Date(2026, 8, 12, 22, 0, 0, 0, time.UTC)
	c.setBackendConnectivityDown(true, t0)
	c.setBackendConnectivityDown(false, t0.Add(13*time.Second))
	// Blips inside the episode gap: same disturbance, demoted.
	for i := range 3 {
		base := t0.Add(time.Duration(i+1) * 7 * time.Minute)
		c.setBackendConnectivityDown(true, base)
		c.setBackendConnectivityDown(false, base.Add(20*time.Second))
	}

	// Let the finalize timer fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(logLines(buf, "episode ended")) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if got := logLines(buf, "episode ended"); len(got) != 1 || !strings.Contains(got[0], "4 losses") {
		t.Fatalf("episode summary = %q, want one summary naming 4 losses", got)
	}
	lossWarns := 0
	for _, line := range logLines(buf, "lost connectivity") {
		if strings.Contains(line, "level=WARN") {
			lossWarns++
		}
	}
	if lossWarns != 1 {
		t.Fatalf("loss WARNs = %d, want exactly 1 (episode start)", lossWarns)
	}
	restoreWarns := 0
	for _, line := range logLines(buf, "restored connectivity") {
		if strings.Contains(line, "level=WARN") {
			restoreWarns++
		}
	}
	if restoreWarns != 1 {
		t.Fatalf("restore WARNs = %d, want exactly 1 (first of episode)", restoreWarns)
	}

	// New episode after the quiet gap: warns again.
	c.setBackendConnectivityDown(true, t0.Add(time.Hour))
	if got := logLines(buf, "lost connectivity"); !strings.Contains(got[len(got)-1], "level=WARN") {
		t.Fatalf("post-gap loss not WARN: %q", got[len(got)-1])
	}
	c.setBackendConnectivityDown(false, t0.Add(time.Hour).Add(10*time.Second))
}

func TestBackendLossDuringOpenSessionAlwaysWarns(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.epGap = time.Hour // keep the episode open for the whole test
	c.SetBackendSessionOpen(func(time.Time) bool { return true })

	t0 := time.Date(2026, 8, 12, 15, 0, 0, 0, time.UTC)
	c.setBackendConnectivityDown(true, t0)
	c.setBackendConnectivityDown(false, t0.Add(10*time.Second))
	// Mid-episode loss — would be INFO, but the session is open.
	c.setBackendConnectivityDown(true, t0.Add(2*time.Minute))

	warns := 0
	for _, line := range logLines(buf, "during open trading session") {
		if strings.Contains(line, "level=WARN") {
			warns++
		}
	}
	if warns != 2 {
		t.Fatalf("session-open loss WARNs = %d, want 2", warns)
	}
}

func TestBackendLongOutageRestoreAlwaysWarns(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.epGap = time.Hour

	t0 := time.Date(2026, 8, 12, 22, 0, 0, 0, time.UTC)
	c.setBackendConnectivityDown(true, t0)
	c.setBackendConnectivityDown(false, t0.Add(10*time.Second))
	// Mid-episode loss with a 6-minute outage: restore must warn.
	c.setBackendConnectivityDown(true, t0.Add(2*time.Minute))
	c.setBackendConnectivityDown(false, t0.Add(8*time.Minute))

	found := false
	for _, line := range logLines(buf, "outage exceeded") {
		if strings.Contains(line, "level=WARN") {
			found = true
		}
	}
	if !found {
		t.Fatal("6-minute outage restore did not warn")
	}
}

// Losses inside a configured maintenance window are annotated and counted
// separately in the status report.
func TestBackendMaintenanceWindowAnnotationAndCounters(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := NewConnector(&ConnectorConfig{})
	c.conn.rateLimiter.Stop()
	c.epGap = time.Hour
	windows, err := DefaultIBKRMaintenanceWindows()
	if err != nil {
		t.Fatal(err)
	}
	c.SetBackendMaintenanceWindows(windows)

	ny, _ := time.LoadLocation("America/New_York")
	inWindow := time.Date(2026, 8, 12, 0, 10, 0, 0, ny) // Wed 00:10 ET, inside nightly reset
	outside := time.Date(2026, 8, 12, 20, 0, 0, 0, ny)  // Wed 20:00 ET, outside
	c.setBackendConnectivityDown(true, inWindow)
	c.setBackendConnectivityDown(false, inWindow.Add(30*time.Second))
	c.setBackendConnectivityDown(true, outside)
	c.setBackendConnectivityDown(false, outside.Add(30*time.Second))

	link := c.BackendLink()
	if link.Losses != 2 || link.LossesInMaintenance != 1 {
		t.Fatalf("losses=%d inMaintenance=%d, want 2/1", link.Losses, link.LossesInMaintenance)
	}
	annotated := logLines(buf, "inside IBKR maintenance window")
	if len(annotated) != 1 {
		t.Fatalf("annotated loss lines = %d, want 1", len(annotated))
	}
}
