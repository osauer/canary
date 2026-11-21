package ibkr

import (
	"strconv"
	"testing"
)

// On 2026-11-21 a held option line opened in the seconds between the
// handshake and the daemon's reqMarketDataType(2) went out in TWS's live
// default. On a shut market it received only TWS's model mark and never a
// market-data-type notice, so it stayed untyped for the whole daemon run.
// With a session mode configured, the first market-data request of every
// broker session carries that mode ahead of it, once; an explicit setter
// (the delayed switch, a competing live session) is never overridden.
func TestSessionMarketDataModePrecedesTheFirstRequest(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	conn.config.SessionMarketDataType = 2
	conn.brokerSessionEpoch.Store(1)
	request := func() {
		t.Helper()
		if _, err := conn.requestMarketDataWithContractForEpochMode(t.Context(), Contract{ConID: 1, Symbol: "SYN", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20261218", Strike: 95, Right: "C", Multiplier: 100}, "", false, false, conn.BrokerSessionEpoch(), false, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	ids := func() []string {
		var got []string
		for _, f := range decodeOutboundFrames(t, conn, out.Bytes()) {
			id := f[0]
			if id == strconv.Itoa(reqMarketDataType) {
				id += "(" + f[2] + ")"
			}
			got = append(got, id)
		}
		return got
	}
	mode, mkt := strconv.Itoa(reqMarketDataType), strconv.Itoa(reqMktData)
	request()
	request()
	if got := ids(); len(got) != 3 || got[0] != mode+"(2)" || got[1] != mkt || got[2] != mkt {
		t.Fatalf("first session frames %v, want the session mode once ahead of the first request", got)
	}

	conn.brokerSessionEpoch.Add(1) // a reconnect
	request()
	if got := ids(); len(got) != 5 || got[3] != mode+"(2)" || got[4] != mkt {
		t.Fatalf("after a reconnect %v, want the session mode again ahead of its first request", got)
	}

	conn.brokerSessionEpoch.Add(1)
	if err := conn.SetMarketDataType(3); err != nil {
		t.Fatal(err)
	}
	request()
	if got := ids(); len(got) != 7 || got[5] != mode+"(3)" || got[6] != mkt {
		t.Fatalf("explicit delayed switch %v, want it kept and no session mode after it", got)
	}
}

// Without a configured session mode the connection behaves as before: no
// mode frame ahead of a plain request.
func TestNoSessionMarketDataModeSendsNoPrefix(t *testing.T) {
	conn, out := newReadyWireTestConnection(t)
	conn.brokerSessionEpoch.Store(1)
	if _, err := conn.requestMarketDataWithContractForEpochMode(t.Context(), Contract{ConID: 1, Symbol: "SYN", SecType: "STK", Exchange: "SMART", Currency: "USD"}, "", false, false, conn.BrokerSessionEpoch(), false, 0, nil); err != nil {
		t.Fatal(err)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 1 || frames[0][0] != strconv.Itoa(reqMktData) {
		t.Fatalf("frames %v, want the request alone", frames)
	}
}
