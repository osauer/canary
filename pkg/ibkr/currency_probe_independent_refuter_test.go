package ibkr

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func waitCurrencyRefuterState(t *testing.T, conn *Connection, retired bool) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		conn.accountMu.RLock()
		id := 0
		if retired {
			for req := range conn.retiredCurrencyProbes {
				id = req
				break
			}
		} else {
			for req := range conn.currencyProbes {
				id = req
				break
			}
		}
		conn.accountMu.RUnlock()
		if id != 0 {
			return id
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("diagnostic never reached expected lifecycle boundary")
	return 0
}

func TestCurrencyIndependentRefuterQueuedScopeAndRawCapture(t *testing.T) {
	for _, change := range []string{"scope", "raw_capture"} {
		t.Run(change, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			conn.transportMu.Lock()
			done := startCurrencyProbe(c, t.Context(), time.Second)
			waitCurrencyRefuterState(t, conn, false)
			if change == "scope" {
				conn.accountMu.Lock()
				conn.account = "DU9999999"
				conn.managedAccounts = []string{"DU9999999"}
				conn.accountMu.Unlock()
			} else {
				// The sender performs its final guard under this transport lock.
				conn.logWireHex = true
			}
			conn.transportMu.Unlock()
			select {
			case got := <-done:
				want := "send_error"
				if change == "scope" {
					want = "scope_conflict"
				}
				if got.Status != want || got.CancelStatus != "not_needed" || len(got.Rows) != 0 {
					t.Fatalf("queued guard returned %+v", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("queued guard did not retire")
			}
			if frames := wire.frames(conn); len(frames) != 0 {
				t.Fatalf("guard refusal emitted frames %v", frames)
			}
		})
	}
}

func TestCurrencyIndependentRefuterQueuedCancelNeverReachesSuccessor(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	done := startCurrencyProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	oldEpoch := conn.BrokerSessionEpoch()
	conn.transportMu.Lock()
	currencyEnd(conn, id)
	waitCurrencyRefuterState(t, conn, true)
	conn.resetOrderIDReadiness()
	conn.transportMu.Unlock()
	select {
	case got := <-done:
		if got.Status != "session_changed" || got.CancelStatus != "session_changed" || len(got.Rows) != 0 {
			t.Fatalf("stale cancellation result %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stale cancellation did not retire")
	}
	if conn.BrokerSessionEpoch() == oldEpoch {
		t.Fatal("test did not advance socket epoch")
	}
	if frames := wire.frames(conn); len(frames) != 1 {
		t.Fatalf("old cancellation reached successor: %v", frames)
	}
	conn.accountMu.RLock()
	n := len(conn.retiredCurrencyProbes)
	conn.accountMu.RUnlock()
	if n != 0 {
		t.Fatal("old cleanup contaminated successor quarantine")
	}
}

func TestCurrencyIndependentRefuterPacingStillRunsForCurrentAndRetired(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(strconv.FormatBool(retired), func(t *testing.T) {
			_, conn, _ := settlementConnectorFixture(t)
			epoch := conn.BrokerSessionEpoch()
			const id = 77
			conn.accountMu.Lock()
			if retired {
				conn.retiredCurrencyProbes = map[int]uint64{id: epoch}
			} else {
				conn.currencyProbes = map[int]*currencyProbeSnapshot{id: {epoch: epoch, done: make(chan struct{})}}
			}
			conn.accountMu.Unlock()
			var forwarded []string
			handler := conn.RegisterHandlerAtEpoch(msgErrMsg, func(fields []string, _ uint64) { forwarded = append([]string(nil), fields...) })
			defer conn.UnregisterHandler(msgErrMsg, handler)
			conn.processErrorMessageAtEpoch([]string{"4", "2", strconv.Itoa(id), "100", "synthetic-private-cash-prose"}, epoch)
			conn.rateLimiter.metricsMu.Lock()
			count := conn.rateLimiter.metrics.ConsecutiveErrors
			conn.rateLimiter.metricsMu.Unlock()
			if count != 1 {
				t.Fatalf("currency diagnostic swallowed global pacing signal: %d", count)
			}
			if len(forwarded) == 0 || strings.Contains(strings.Join(forwarded, " "), "synthetic-private-cash-prose") {
				t.Fatalf("broker prose escaped sanitization: %v", forwarded)
			}
		})
	}
}
