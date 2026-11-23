package ibkr

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

func TestDecodeSymbolSamplesFollowsTheOfficialFieldOrder(t *testing.T) {
	const current = minServerVerBondIssuerID
	// symbolSamples (79): reqId, count, then per listing conId, symbol,
	// secType, primary exchange, currency, derivative count and types,
	// description, issuer id; the socket's final NUL is one empty field.
	frame := []string{"79", "7", "2",
		"880001", "9988", "STK", "SEHK", "HKD", "2", "OPT", "WAR", "SYNTHETIC GROUP HOLDINGS", "",
		"880002", "SYNA", "STK", "NASDAQ", "USD", "0", "SYNA INC", "ISS1",
		""}
	got, err := decodeSymbolSamples(frame, 7, current)
	if err != nil || len(got) != 2 {
		t.Fatalf("decode = %+v, %v", got, err)
	}
	if got[0].ConID != 880001 || got[0].Symbol != "9988" || got[0].SecType != "STK" || got[0].PrimaryExch != "SEHK" || got[0].Currency != "HKD" ||
		!slices.Equal(got[0].DerivativeSecTypes, []string{"OPT", "WAR"}) || got[0].Description != "SYNTHETIC GROUP HOLDINGS" || got[0].IssuerID != "" {
		t.Fatalf("first listing = %+v", got[0])
	}
	if got[1].Description != "SYNA INC" || got[1].IssuerID != "ISS1" || len(got[1].DerivativeSecTypes) != 0 {
		t.Fatalf("second listing = %+v", got[1])
	}
	// Before server 176 an entry ends after its derivative types.
	old := []string{"79", "7", "1", "880003", "SYNB", "STK", "NYSE", "USD", "1", "OPT", ""}
	if got, err := decodeSymbolSamples(old, 7, current-1); err != nil || len(got) != 1 || got[0].Description != "" || got[0].Symbol != "SYNB" {
		t.Fatalf("pre-176 decode = %+v, %v", got, err)
	}
	if got, err := decodeSymbolSamples([]string{"79", "7", "0", ""}, 7, current); err != nil || len(got) != 0 {
		t.Fatalf("zero results = %+v, %v", got, err)
	}
	for name, bad := range map[string][]string{
		"other request":  {"79", "8", "0", ""},
		"truncated":      {"79", "7", "1", "880001", "9988", "STK"},
		"trailing field": {"79", "7", "0", "x", ""},
		"bad count":      {"79", "7", "-1", ""},
	} {
		if _, err := decodeSymbolSamples(bad, 7, current); err == nil {
			t.Errorf("%s decoded", name)
		}
	}
}

// The search sends reqMatchingSymbols (81) with the request id and the
// pattern, takes only its own answer on its own session, and refuses a
// second search while one runs.
func TestMatchingSymbolsWire(t *testing.T) {
	c, conn, out, _ := newMissTestConnector(t)
	c.mu.Lock()
	c.ready = true
	c.mu.Unlock()
	binding, ok := c.CaptureHistoricalSession()
	if !ok {
		t.Fatal("missing session")
	}
	type result struct {
		rows []SymbolSample
		err  error
	}
	done := make(chan result, 1)
	go func() {
		rows, err := c.MatchingSymbols(t.Context(), binding, "9988")
		done <- result{rows, err}
	}()
	var frame []string
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && frame == nil; {
		for _, f := range decodeOutboundFrames(t, conn, out.Bytes()) {
			if f[0] == "81" {
				frame = f
			}
		}
		if frame == nil {
			time.Sleep(time.Millisecond)
		}
	}
	if frame = slices.DeleteFunc(slices.Clone(frame), func(s string) bool { return s == "" }); len(frame) != 3 || frame[2] != "9988" {
		t.Fatalf("request frame = %v, want [81 reqId 9988]", frame)
	}
	if _, err := c.MatchingSymbols(t.Context(), binding, "SYNA"); !errors.Is(err, ErrSymbolSearchBusy) {
		t.Fatalf("second search while one runs: %v", err)
	}
	reqID := frame[1]
	conn.dispatchHandlers(msgSymbolSamples, []string{"79", reqID + "9", "0", ""}, conn.BrokerSessionEpoch())
	conn.dispatchHandlers(msgSymbolSamples, []string{"79", reqID, "0", ""}, conn.BrokerSessionEpoch()+1)
	select {
	case <-done:
		t.Fatal("took another request's or session's answer")
	default:
	}
	answer := []string{"79", reqID, "1", "880001", "9988", "STK", "SEHK", "HKD", "0"}
	if conn.ServerVersion() >= minServerVerBondIssuerID {
		answer = append(answer, "SYNTHETIC GROUP HOLDINGS", "")
	}
	conn.dispatchHandlers(msgSymbolSamples, append(answer, ""), conn.BrokerSessionEpoch())
	select {
	case r := <-done:
		if r.err != nil || len(r.rows) != 1 || r.rows[0].ConID != 880001 {
			t.Fatalf("search = %+v, %v", r.rows, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("search did not finish")
	}
	// Within the pacing interval of the last search the next is refused.
	if _, err := c.MatchingSymbols(context.Background(), binding, "SYNA"); !errors.Is(err, ErrSymbolSearchBusy) {
		t.Fatalf("search within a second of the last: %v", err)
	}
}

// The broker's size rules reach the resolved contract; a contract without
// them, or with fractional ones, trades in single shares.
func TestStockLotFromContractDetails(t *testing.T) {
	const current = maxClientVersion
	frame := mdTClassificationFrame(current, 8, "STK")
	at := mdTIndexOf(t, frame, "COMMON") + 1
	frame[at], frame[at+1], frame[at+2] = "100", "100", "100"
	got, ok := parseContractDetailsClassification(frame, current)
	if !ok || got.minSize != 100 || got.sizeIncrement != 100 {
		t.Fatalf("classification = %+v ok=%t", got, ok)
	}
	for _, tc := range []struct {
		min, inc         float64
		wantMin, wantInc int
	}{{100, 100, 100, 100}, {0, 0, 1, 1}, {0.0001, 0.0001, 1, 1}, {500, 100, 500, 100}, {1, 1, 1, 1}} {
		m, i := (ResolvedOrderContract{MinSize: tc.min, SizeIncrement: tc.inc}).StockLot()
		if m != tc.wantMin || i != tc.wantInc {
			t.Errorf("StockLot(%v, %v) = %d, %d, want %d, %d", tc.min, tc.inc, m, i, tc.wantMin, tc.wantInc)
		}
	}
	resolved, err := exactOrderContract(Contract{Symbol: "9988", SecType: "STK", Currency: "HKD", Exchange: "SEHK"},
		[]ContractDetailsLite{{ConID: 880001, Symbol: "9988", SecType: "STK", Currency: "HKD", Exchange: "SEHK", PrimaryExch: "SEHK", MinSize: 100, SizeIncrement: 100}})
	if err != nil || resolved.MinSize != 100 || resolved.SizeIncrement != 100 {
		t.Fatalf("resolved = %+v, %v", resolved, err)
	}
}
