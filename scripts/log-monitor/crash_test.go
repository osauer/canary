package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Crash output is one finding: the first line names the cause and the count
// says how much landed. A missing crash log is normal, and consumed content
// is not reported twice.
func TestCrashOutputIsOneSignal(t *testing.T) {
	opts := testMonitorOptions(t)
	now := time.Now()
	writeTestFile(t, opts.daemonLog, "level=INFO msg=ready\n")
	got, err := run(opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.CrashLog != "" || len(got.Daemon.Signals) != 0 {
		t.Fatalf("missing crash log reported: %+v", got)
	}

	crashLog, _ := opts.crashPaths()
	if filepath.Dir(crashLog) != filepath.Dir(opts.daemonLog) || filepath.Base(crashLog) != "daemon.crash.log" {
		t.Fatalf("derived crash log path = %s", crashLog)
	}
	dump := "SIGQUIT: quit\nPC=0x1811b450c m=0 sigcode=0\n\ngoroutine 0 gp=0x102828660 m=0 mp=0x1028298a0 [idle]:\nruntime.pthread_cond_wait(0x102829e08, 0x102829dc8)\n\t/opt/homebrew/Cellar/go/1.27.0/libexec/src/runtime/sys_darwin.go:547 +0x2c\ngoroutine 1 gp=0x1 m=nil [select, 3 minutes]:\n"
	writeTestFile(t, crashLog, dump)
	got, err = run(opts, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.CrashLog != crashLog {
		t.Fatalf("crash log path not reported: %+v", got)
	}
	if len(got.Daemon.Signals) != 1 || got.Daemon.Signals[0].Kind != "crash_output" || got.Daemon.Signals[0].Severity != "ERROR" {
		t.Fatalf("crash signals = %+v, want one ERROR crash_output", got.Daemon.Signals)
	}
	if s := got.Daemon.Signals[0]; !strings.Contains(s.Message, "SIGQUIT: quit") || s.Count != 6 {
		t.Fatalf("crash signal = %+v, want the first line and 6 non-empty lines", s)
	}
	if !got.NeedsAttention {
		t.Fatal("crash output did not need attention")
	}

	got, err = run(opts, now)
	if err != nil || len(got.Daemon.Signals) != 0 {
		t.Fatalf("consumed crash output reported again: %+v %v", got.Daemon.Signals, err)
	}
}
