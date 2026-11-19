package ibkr

import (
	"bufio"
	"errors"
	"io"
	"strconv"
	"sync"
	"testing"
	"time"
)

// failFirstWriter refuses its first n writes and passes the rest on.
type failFirstWriter struct {
	mu   sync.Mutex
	left int
	next io.Writer
}

func (w *failFirstWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.left > 0 {
		w.left--
		return 0, errors.New("write refused")
	}
	return w.next.Write(p)
}

// A subscription retired after its record was dropped (an inactive symbol, a
// reviewed terminal contract) cancels its line once. When that one cancel
// does not reach the Gateway nobody else can send it: the cancel stays owed
// and is sent again, as it is for an ordinary release (cd452dc6).
func TestRetiredSubscriptionOwesItsCancel(t *testing.T) {
	c, conn, out, _ := newQuoteFallbackFixture(t)
	conn.cancelRetryWait = 50 * time.Millisecond
	conn.optionContractCache[optionContractKey("SYNTH", "SYNTH", "20261218", 100, "C")] = ContractDetailsLite{
		ConID: 900001, Symbol: "SYNTH", SecType: "OPT", TradingClass: "SYNTH", Expiry: "20261218", Strike: 100, Right: "C", Exchange: "SMART", Currency: "USD", Multiplier: 100,
	}
	_, id, _, err := c.acquireOption(t.Context(), "SYNTH", "SYNTH", "20261218", 100, "C")
	if err != nil {
		t.Fatal(err)
	}
	conn.writer = bufio.NewWriter(&failFirstWriter{left: 1, next: out})

	retiredMarketDataSubscription{connection: conn, epoch: conn.BrokerSessionEpoch(), reqID: id, cancel: true}.run()
	if !mdTSlotHeld(conn, id) {
		t.Fatal("the slot was freed although the cancel was never written")
	}
	// A buffered writer keeps its first error; the socket is usable again.
	conn.transportMu.Lock()
	conn.writer = bufio.NewWriter(out)
	conn.transportMu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for mdTSlotHeld(conn, id) {
		if time.Now().After(deadline) {
			t.Fatal("the retired line is still held and its cancel was never sent again")
		}
		time.Sleep(time.Millisecond)
	}
	cancels := outboundFramesByID(t, conn, out, cancelMktData)
	if len(cancels) != 1 || cancels[0][2] != strconv.Itoa(id) {
		t.Fatalf("cancels on the wire = %v, want one for request %d", cancels, id)
	}
}
