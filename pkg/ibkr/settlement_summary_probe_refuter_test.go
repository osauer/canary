package ibkr

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A receipt-only probe must still preserve connector-wide pacing protection.
func TestSettlementProbeRefuterPacingErrorsRetainSafetyEffects(t *testing.T) {
	for _, retired := range []bool{false, true} {
		t.Run(strconv.FormatBool(retired), func(t *testing.T) {
			_, conn, _ := settlementConnectorFixture(t)
			epoch := conn.BrokerSessionEpoch()
			const reqID = 77
			conn.accountMu.Lock()
			if retired {
				conn.retiredSummaryProbes = map[int]uint64{reqID: epoch}
			} else {
				conn.summarySnapshots[reqID] = &summarySnapshot{diagnosticOnly: true, done: make(chan struct{})}
			}
			conn.accountMu.Unlock()
			var forwarded []string
			handler := conn.RegisterHandlerAtEpoch(msgErrMsg, func(fields []string, received uint64) {
				forwarded = append([]string(nil), fields...)
			})
			defer conn.UnregisterHandler(msgErrMsg, handler)
			conn.processErrorMessageAtEpoch([]string{"4", "2", strconv.Itoa(reqID), "100", "synthetic-private-prose"}, epoch)
			conn.rateLimiter.metricsMu.Lock()
			got := conn.rateLimiter.metrics.ConsecutiveErrors
			conn.rateLimiter.metricsMu.Unlock()
			if got != 1 {
				t.Fatalf("diagnostic swallowed connector-wide pacing protection: errors=%d", got)
			}
			if len(forwarded) == 0 || strings.Contains(strings.Join(forwarded, " "), "synthetic-private-prose") {
				t.Fatal("diagnostic broker prose escaped into registered recovery handlers")
			}
		})
	}
}

// The comparison's ordinary phase must honor its reserved wire/send budget.
func TestSettlementProbeRefuterOrdinaryPhaseHonorsSendDeadline(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.transportMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := c.RequestAccountSummaryWithProvenance(ctx, time.Second)
		done <- err
	}()
	<-ctx.Done()
	var got error
	returned := false
	select {
	case got = <-done:
		returned = true
	case <-time.After(60 * time.Millisecond):
	}
	conn.transportMu.Unlock()
	if !returned {
		select {
		case got = <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("ordinary phase did not retire after transport release")
		}
		t.Error("ordinary phase ignored caller deadline while waiting for transport")
	}
	if !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("expected caller deadline, got %v", got)
	}
	if frames := wire.frames(conn); len(frames) != 0 {
		t.Errorf("expired unsent ordinary phase emitted %d frames", len(frames))
	}
}

func TestSettlementProbeRefuterUncertainCancelPreservesOneOrdinarySlot(t *testing.T) {
	const epoch = 3
	s := accountSummarySchedule{ordinary: map[int]uint64{77: epoch}, uncertainProbe: true}
	if err := s.enterOrdinary(t.Context(), 78, epoch); err != nil {
		t.Fatalf("uncertain probe starved first ordinary read: %v", err)
	}
	if err := s.enterOrdinary(t.Context(), 79, epoch); !errors.Is(err, ErrAccountSummaryProbeCancelUncertain) {
		t.Fatalf("third broker subscription admitted or unnamed hold: %v", err)
	}
	if p := s.beginProbe(func() {}); p != nil {
		t.Fatal("uncertain cancelled probe admitted another diagnostic")
	}
	s.leaveOrdinary(78, epoch)
	if err := s.enterOrdinary(t.Context(), 79, epoch); err != nil {
		t.Fatalf("released ordinary slot could not serve next read: %v", err)
	}
	s.reset()
	if p := s.beginProbe(func() {}); p == nil {
		t.Fatal("socket reset retained cancelled probe scheduling hold")
	} else {
		s.finishProbe(p)
	}
}

func TestSettlementProbeRefuterOrdinaryConcurrencyRemainsAvailable(t *testing.T) {
	var s accountSummarySchedule
	for _, req := range []int{71, 72} {
		if err := s.enterOrdinary(t.Context(), req, 1); err != nil {
			t.Fatalf("normal concurrent read denied: %v", err)
		}
	}
	if p := s.beginProbe(func() {}); p != nil {
		t.Fatal("diagnostic competed with ordinary subscriptions")
	}
	s.leaveOrdinary(71, 1)
	s.leaveOrdinary(72, 1)
	if p := s.beginProbe(func() {}); p == nil {
		t.Fatal("fully cancelled ordinary reads retained diagnostic busy state")
	} else {
		s.finishProbe(p)
	}
}
