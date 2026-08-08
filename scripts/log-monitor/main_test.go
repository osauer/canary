package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunClassifiesAndCommitsBoundedReports(t *testing.T) {
	dir := t.TempDir()
	daemonLog := filepath.Join(dir, "daemon.log")
	appLog := filepath.Join(dir, "app.log")
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	writeTestFile(t, daemonLog, strings.Join([]string{
		`time=2026-08-08T06:00:00Z level=INFO msg="Connected to IB Gateway"`,
		`time=2026-08-08T06:00:10Z level=WARN msg="RateLimiter draining"`,
		`time=2026-08-08T06:02:00Z level=WARN msg="unexpected feed state for DU1234567; protocol misalignment for IWM via reqMktData symbol=IWM reqid=12"`,
		`time=2026-08-08T06:03:00Z level=ERROR msg="No security definition has been found" code=200`,
	}, "\n")+"\n")
	writeTestFile(t, appLog, strings.Join([]string{
		`time=2026-08-08T06:00:00Z level=INFO msg="Request completed" status=200`,
		`time=2026-08-08T06:00:01Z level=INFO msg="Request completed" status=503`,
		`2026/08/08 06:00:02 WARN legacy warning`,
		`2026/08/08 06:00:03 raw production line`,
	}, "\n")+"\n")

	got, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: 2, commit: true,
	}, time.Date(2026, 8, 8, 6, 5, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if !got.NeedsAttention {
		t.Fatal("report should require attention")
	}
	if got.Daemon.NewLines != 4 || got.Daemon.KnownBenign != 2 || len(got.Daemon.Signals) != 1 {
		t.Fatalf("daemon report = %+v", got.Daemon)
	}
	if strings.Contains(got.Daemon.Signals[0].Message, "DU1234567") || strings.Contains(got.Daemon.Signals[0].Message, "IWM") {
		t.Fatalf("daemon signal leaked private identity: %+v", got.Daemon.Signals[0])
	}
	if got.App.NewLines != 4 || got.App.KnownBenign != 1 || len(got.App.Signals) != 2 || got.App.SuppressedSignals != 1 {
		t.Fatalf("app report = %+v", got.App)
	}
	assertTestFile(t, dOffset, "4\n")
	assertTestFile(t, aOffset, "4\n")

	again, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: 2, commit: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if again.NeedsAttention || again.Daemon.State != "unchanged" || again.App.State != "unchanged" {
		t.Fatalf("unchanged report = %+v", again)
	}
}

func TestRunResetsOffsetAfterRotation(t *testing.T) {
	dir := t.TempDir()
	daemonLog := filepath.Join(dir, "daemon.log")
	appLog := filepath.Join(dir, "app.log")
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	writeTestFile(t, daemonLog, "time=2026-08-08T06:00:00Z level=INFO msg=ready\n")
	writeTestFile(t, appLog, "time=2026-08-08T06:00:00Z level=INFO msg=ready\n")
	writeTestFile(t, dOffset, "99\n")
	writeTestFile(t, aOffset, "99\n")

	got, err := run(options{
		daemonLog: daemonLog, appLog: appLog,
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: defaultMaxSignals, commit: false,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !got.Daemon.OffsetReset || !got.App.OffsetReset || got.Daemon.NewLines != 1 || got.App.NewLines != 1 {
		t.Fatalf("rotated report = %+v", got)
	}
}

func TestMissingLogsAreNeutralAndDoNotCreateOffsets(t *testing.T) {
	dir := t.TempDir()
	dOffset := filepath.Join(dir, "daemon.offset")
	aOffset := filepath.Join(dir, "app.offset")
	got, err := run(options{
		daemonLog:    filepath.Join(dir, "missing-daemon"),
		appLog:       filepath.Join(dir, "missing-app"),
		daemonOffset: dOffset, appOffset: aOffset,
		maxSignals: defaultMaxSignals, commit: true,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got.NeedsAttention || got.Daemon.State != "missing" || got.App.State != "missing" {
		t.Fatalf("missing report = %+v", got)
	}
	for _, path := range []string{dOffset, aOffset} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("offset %s should not exist, stat err=%v", path, err)
		}
	}
}

func TestPanicStackIsOneSignal(t *testing.T) {
	scanned := scannedLog{state: "scanned", lines: []string{
		"panic: broken",
		"goroutine 1 [running]:",
		"\tmain.main()",
		"\t/tmp/main.go:12 +0x1",
	}}
	got := classifyApp(scanned, defaultMaxSignals)
	if len(got.Signals) != 1 || got.Signals[0].Kind != "panic" {
		t.Fatalf("panic report = %+v", got)
	}
}

func TestKnownNoiseExplosionBecomesOneSignal(t *testing.T) {
	lines := make([]string, 151)
	for i := range lines {
		lines[i] = `time=2026-08-08T06:00:00Z level=ERROR msg="No security definition has been found" code=200`
	}
	got := classifyDaemon(scannedLog{state: "scanned", lines: lines}, defaultMaxSignals)
	if len(got.Signals) != 1 || got.Signals[0].Kind != "noise_loop" || got.Signals[0].Count != 151 {
		t.Fatalf("noise report = %+v", got)
	}
}

func writeTestFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertTestFile(t *testing.T, path, want string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(raw); got != want {
		t.Fatalf("%s = %q, want %q", path, got, want)
	}
}
