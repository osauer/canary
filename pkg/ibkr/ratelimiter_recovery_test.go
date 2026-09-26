package ibkr

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestMarketDataCapacityDoesNotPoisonPacing(t *testing.T) {
	c, out := newReadyWireTestConnection(t)
	// Leave exactly one slot: the final admitted subscription must reach the wire.
	for id := 1000; id < 1099; id++ {
		if err := c.acquireMarketDataSlot(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	id, err := c.RequestMarketDataWithContract(t.Context(), Contract{Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}, "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	findOutboundFrame(t, decodeOutboundFrames(t, c, out.Bytes()), reqMktData)
	for range 5 {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := c.acquireMarketDataSlot(ctx, 2000); !errors.Is(err, context.Canceled) {
			t.Fatalf("full pool admission: %v", err)
		}
	}
	if got := c.rateLimiter.GetMetrics().ConsecutiveErrors; got != 0 {
		t.Fatalf("local capacity counted as broker pacing: %d", got)
	}
	if err := c.rateLimiter.checkCircuit(RequestTypeGeneral); err != nil {
		t.Fatal(err)
	}
	// Cancellation must reach the wire while every slot is occupied.
	if err := c.CancelMarketData(id); err != nil {
		t.Fatal(err)
	}
	findOutboundFrame(t, decodeOutboundFrames(t, c, out.Bytes()), cancelMktData)
	if got := c.rateLimiter.marketDataSubs.Count(); got != 99 {
		t.Fatalf("slots after cancel: %d", got)
	}
}

func TestBrokerPacingCircuitUsesReceiptsNotSocketWrites(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	epoch := c.BrokerSessionEpoch()
	for range 5 {
		c.processErrorMessageAtEpoch([]string{"4", "2", "-1", "100", "synthetic pacing error"}, epoch)
		// Successful local writes are not acknowledgements from the broker.
		if err := c.rateLimiter.executeRequest(&RateLimitedRequest{Context: t.Context(), SendFunc: func(context.Context) error { return nil }}); err != nil {
			t.Fatal(err)
		}
	}
	if c.rateLimiter.GetMetrics().ConsecutiveErrors != 5 || c.rateLimiter.checkCircuit(RequestTypeGeneral) == nil {
		t.Fatal("broker pacing did not open circuit")
	}
	if err := c.acquireMarketDataSlot(t.Context(), 91); err != nil {
		t.Fatal(err)
	}
	if err := c.CancelMarketData(91); err != nil {
		t.Fatalf("read cancellation blocked by circuit: %v", err)
	}
	c.processErrorMessageAtEpoch([]string{"4", "2", "-1", "100", "stale receipt"}, epoch+1)
	if c.rateLimiter.GetMetrics().ConsecutiveErrors != 5 {
		t.Fatal("stale pacing receipt counted")
	}
	c.rateLimiter.metricsMu.Lock()
	c.rateLimiter.metrics.LastRateLimitError = time.Now().Add(-2 * c.rateLimiter.circuitCooldown)
	c.rateLimiter.metricsMu.Unlock()
	c.rateLimiter.recordRateLimitError()
	if c.rateLimiter.GetMetrics().ConsecutiveErrors != 1 {
		t.Fatal("quiet interval did not reset incident count")
	}
}

func TestFailedMarketDataCancelRetainsOwnership(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "epoch"}[exact], func(t *testing.T) {
			c, _ := newReadyWireTestConnection(t)
			if err := c.acquireMarketDataSlot(t.Context(), 91); err != nil {
				t.Fatal(err)
			}
			c.writer = bufio.NewWriter(rejectedPnLWriter{})
			var err error
			if exact {
				err = c.cancelMarketDataForEpoch(t.Context(), 91, c.BrokerSessionEpoch())
			} else {
				err = c.CancelMarketData(91)
			}
			if err == nil {
				t.Fatal("cancel unexpectedly succeeded")
			}
			if got := c.rateLimiter.marketDataSubs.Count(); got != 1 {
				t.Fatalf("failed cancel freed broker-owned slot: %d", got)
			}
			c.invalidateUnstampedObservationAuthority()
			if got := c.rateLimiter.marketDataSubs.Count(); got != 0 {
				t.Fatalf("retired session retained slot: %d", got)
			}
		})
	}
}

func TestReadCancellationStillUsesMessagePacing(t *testing.T) {
	rl := NewRateLimiter(t.Context())
	defer rl.Stop()
	rl.messageRate = NewTokenBucket(1, 0)
	if !rl.messageRate.TryAcquire(1) {
		t.Fatal("empty initial bucket")
	}
	rl.openCircuit(time.Now().Add(time.Minute))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	sent := false
	err := rl.SubmitWithRetriesContext(ctx, RequestTypeMarketDataCancel, func() error { sent = true; return nil }, 0)
	if !errors.Is(err, context.DeadlineExceeded) || sent {
		t.Fatalf("cancel escaped message pacing: sent=%v err=%v", sent, err)
	}
}

func syntheticSystemNotice(id, code int) []string {
	payload := binary.AppendUvarint(nil, 1<<3)
	payload = binary.AppendUvarint(payload, uint64(id))
	payload = binary.AppendUvarint(payload, 3<<3)
	payload = binary.AppendUvarint(payload, uint64(code))
	payload = binary.AppendUvarint(payload, 4<<3|2)
	payload = binary.AppendUvarint(payload, uint64(len("synthetic notice")))
	payload = append(payload, "synthetic notice"...)
	return []string{strconv.Itoa(msgSystemNotification), string(payload)}
}

func TestSystemNoticePacingUsesCurrentEpoch(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	epoch := c.BrokerSessionEpoch()
	for range 5 {
		c.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(-1, 100), epoch)
	}
	if c.rateLimiter.GetMetrics().ConsecutiveErrors != 5 || c.rateLimiter.checkCircuit(RequestTypeGeneral) == nil {
		t.Fatal("modern pacing receipts did not open circuit")
	}
	c.processSystemNoticeMessageAtEpoch(syntheticSystemNotice(-1, 100), epoch+1)
	if c.rateLimiter.GetMetrics().ConsecutiveErrors != 5 {
		t.Fatal("stale modern notice changed limiter")
	}
}
