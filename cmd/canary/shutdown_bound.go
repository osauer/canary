package main

import (
	"os"
	"runtime/pprof"
	"sync"
	"time"
)

// daemonShutdownGrace is how long the daemon's teardown may take once
// serving has ended or a stop signal has arrived. A clean stop takes well
// under a second; `canary restart` waits 15 s for it by default, so a
// teardown that hangs ends inside that wait and the restart takes over.
const daemonShutdownGrace = 10 * time.Second

// shutdownBound returns the function that starts the teardown clock. The
// first call arms it; later calls do nothing. When the grace passes with
// the process still alive, hung runs: the daemon says why, writes its
// goroutines to the crash stream and exits, so it never keeps its pid and
// instance lock without serving (2026-11-17: a teardown step waited
// without end on a connection lock, and nothing could replace the daemon).
func shutdownBound(grace time.Duration, hung func()) func() {
	var once sync.Once
	return func() {
		once.Do(func() { time.AfterFunc(grace, hung) })
	}
}

// exitHungShutdown is the daemon's hung-teardown exit.
func exitHungShutdown(errorf func(string, ...any)) func() {
	return func() {
		errorf("shutdown did not finish within %s; writing goroutines to the crash log and exiting so a new daemon can start", daemonShutdownGrace)
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		os.Exit(1)
	}
}
