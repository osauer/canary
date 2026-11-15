package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v3/internal/rpc"
	"strings"
	"testing"
)

func TestMarketHistoryWireRequestRetainsRange(t *testing.T) {
	raw, err := json.Marshal(rpc.Request{Method: rpc.MethodMarketHistory, Params: json.RawMessage(`{"contract":{"symbol":"SYNTH","sec_type":"STK","exchange":"SMART","currency":"USD"},"range":"1D"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var req rpc.Request
	if err = json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	_, err = s.handleMarketHistory(context.Background(), &req)
	if err == nil || strings.Contains(err.Error(), "supported ranges") {
		t.Fatalf("request lost range: %s %v", raw, err)
	}
}

func TestFutureExpiryCannotRouteThroughOptionQuote(t *testing.T) {
	p := rpc.ContractParams{ConID: 1, Symbol: "SYNTH", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20260918"}
	if isOptionQuoteContract(p) {
		t.Fatal("future routed as option")
	}
	c, echo, routed, err := normaliseStockQuoteContract(p)
	if err != nil || !routed || c.Expiry != p.Expiry || echo.Expiry != p.Expiry || c.ConID != 1 {
		t.Fatal("dated future identity lost")
	}
}
