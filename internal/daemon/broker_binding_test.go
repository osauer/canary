package daemon

import (
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A neutral binding names one connector generation and one session epoch
// of it. After a session loss the binding is no longer current on its own
// adapter, and after the daemon rebuilds the connector the new adapter,
// whose epoch counter starts again at 1, does not accept it either: the
// generation differs. The daemon runs its connections without the client's
// auto-reconnect (server.go newConnector), so a lost session is always
// followed by a rebuild; the lost-session case covers the same adapter and
// the rebuild case the repeated epoch.
func TestBrokerBindingIsNotCurrentAcrossSessionLossAndRebuild(t *testing.T) {
	f := startFakeTWS(t)
	s, _ := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	first := s.brokerReads()
	if first == nil || !first.Wraps(s.gatewayConnectorForTest()) {
		t.Fatal("no adapter over the seated connector")
	}
	b, ok := first.Capture()
	if !ok || b.IsZero() || b.Kind != broker.KindIBKR || b.Epoch != 1 {
		t.Fatalf("captured %+v %v", b, ok)
	}
	if again := s.brokerReads(); again != first {
		t.Fatal("a second read built a second adapter over the same connector")
	}
	if !first.Current(b) {
		t.Fatal("the binding is not current on the session that issued it")
	}

	f.drop()
	waitSessionLost(t, s.gatewayConnectorForTest())
	if first.Current(b) {
		t.Fatal("binding current after the session was lost")
	}
	if _, ok := first.Capture(); ok {
		t.Fatal("a lost session was captured")
	}

	reconnectForTest(t, s)
	second := s.brokerReads()
	if second == nil || second == first || !second.Wraps(s.gatewayConnectorForTest()) {
		t.Fatal("the rebuilt connector has no adapter of its own")
	}
	if second.Generation() <= first.Generation() {
		t.Fatalf("generation did not advance: %d then %d", first.Generation(), second.Generation())
	}
	b2, ok := second.Capture()
	if !ok || b2.Epoch != 1 || b2.Generation != second.Generation() {
		t.Fatalf("new session captured as %+v %v", b2, ok)
	}
	if b2 == b {
		t.Fatal("a binding of the rebuilt connector equals one of the old")
	}
	if second.Current(b) {
		t.Fatal("the new adapter accepts the old generation's binding with the repeated epoch")
	}
	if first.Current(b2) {
		t.Fatal("the old adapter accepts the new generation's binding")
	}
	if !second.Current(b2) {
		t.Fatal("the new binding is not current on its own adapter")
	}
	if _, err := second.Resolve(t.Context(), b, broker.Instrument{Symbol: "SPY", SecType: "STK", Currency: "USD"}, time.Second); err == nil {
		t.Fatal("resolve with a stale binding succeeded")
	}
}

// The subscription manager's line follows the seam's binding: a session
// loss ends the stream with the gateway_lost frame as before, and the next
// subscriber after the rebuild opens a fresh line on the new adapter rather
// than reusing the entry of the old one (today's resubscribe behaviour,
// now decided by Current on the neutral binding).
func TestSubscriptionLineFollowsTheSessionAcrossARebuild(t *testing.T) {
	f := startFakeTWS(t)
	s, _ := sessionLossServer(t, f, "2026-10-07T18:00:00Z")
	s.subs.coalesce = 5 * time.Millisecond
	t.Cleanup(s.subs.Close)
	first := s.brokerReads()

	frames, release, err := s.subs.Subscribe(t.Context(), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if s.subs.activeCount() != 1 {
		t.Fatal("no line held")
	}
	s.subs.subsMu.Lock()
	entry := s.subs.subs["SPY"]
	s.subs.subsMu.Unlock()
	if entry == nil || entry.owner != first || !first.Current(entry.binding) {
		t.Fatalf("entry %+v is not bound to the live session", entry)
	}

	f.drop()
	waitSessionLost(t, s.gatewayConnectorForTest())
	select {
	case fr, ok := <-frames:
		if !ok || fr.Error == nil || fr.Error.Code != rpc.FrameErrGatewayLost {
			t.Fatalf("after the session loss: %+v %v, want the gateway_lost frame", fr, ok)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no terminal frame after the session loss")
	}
	if s.subs.activeCount() != 0 {
		t.Fatal("the lost line is still held")
	}

	reconnectForTest(t, s)
	second := s.brokerReads()
	if second == first {
		t.Fatal("no new adapter after the rebuild")
	}
	_, release2, err := s.subs.Subscribe(t.Context(), "SPY")
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	s.subs.subsMu.Lock()
	entry = s.subs.subs["SPY"]
	s.subs.subsMu.Unlock()
	if entry == nil || entry.owner != second || !second.Current(entry.binding) || first.Current(entry.binding) {
		t.Fatalf("the new line %+v is not bound to the rebuilt session", entry)
	}
}
