package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// IBKR allows a client two account-summary subscriptions and refuses a third
// with code 322. The daemon has many readers of the account, and on 2026-11-11
// and 2026-11-12, while the terminal answered late, three overlapped 28 times.
// A third read waits for a slot; it never reaches the wire as a third
// subscription.
func TestAccountSummaryThirdReadWaitsForASlot(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	read := func() <-chan error {
		done := make(chan error, 1)
		go func() {
			_, _, err := c.RequestAccountSummaryWithProvenance(context.Background(), 2*time.Second)
			done <- err
		}()
		return done
	}
	answer := func(frame []string) {
		id, _ := strconv.Atoi(frame[2])
		probeCallback(conn, id, "DU1234567", "NetLiquidation", "12345", "EUR")
		probeEnd(conn, id)
	}

	first, second := read(), read()
	frames := probeFrames(t, wire, conn, 2)
	third := read()
	time.Sleep(50 * time.Millisecond)
	if got := wire.frames(conn); len(got) != 2 {
		t.Fatalf("a third subscription reached the wire beside two open ones: %v", got)
	}

	answer(frames[0])
	frames = probeFrames(t, wire, conn, 4)
	if frames[2][0] != "63" || frames[2][2] != frames[0][2] || frames[3][0] != "62" {
		t.Fatalf("the third read did not follow the first one's cancel: %v", frames)
	}
	answer(frames[1])
	answer(frames[3])
	for _, done := range []<-chan error{first, second, third} {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestAccountSummarySlotWaitIsBounded(t *testing.T) {
	s := accountSummarySchedule{admitWait: 20 * time.Millisecond}
	for _, req := range []int{1, 2} {
		if err := s.enterOrdinary(t.Context(), req, 1); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.enterOrdinary(t.Context(), 3, 1); !errors.Is(err, ErrAccountSummaryBusy) {
		t.Fatalf("a third read past the wait: %v, want ErrAccountSummaryBusy", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.enterOrdinary(ctx, 3, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled caller kept waiting: %v", err)
	}

	// A new socket ends every subscription at IBKR: a waiter is admitted.
	s.admitWait = 2 * time.Second
	admitted := make(chan error, 1)
	go func() { admitted <- s.enterOrdinary(context.Background(), 3, 2) }()
	time.Sleep(20 * time.Millisecond)
	s.reset()
	if err := <-admitted; err != nil {
		t.Fatalf("a waiter was not admitted after the socket changed: %v", err)
	}
}
