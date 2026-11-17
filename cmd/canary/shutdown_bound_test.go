package main

import (
	"testing"
	"time"
)

// A teardown that outlives its grace ends the process once, however often
// the clock is started; one that never starts the clock is left alone.
func TestShutdownBoundRunsTheHungExitOnceAfterTheGrace(t *testing.T) {
	hung := make(chan struct{}, 4)
	arm := shutdownBound(20*time.Millisecond, func() { hung <- struct{}{} })
	select {
	case <-hung:
		t.Fatal("the hung exit ran before the teardown began")
	case <-time.After(60 * time.Millisecond):
	}
	arm()
	arm()
	select {
	case <-hung:
	case <-time.After(5 * time.Second):
		t.Fatal("a teardown past its grace did not run the hung exit")
	}
	select {
	case <-hung:
		t.Fatal("the hung exit ran twice")
	case <-time.After(80 * time.Millisecond):
	}
}
