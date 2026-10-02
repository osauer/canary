package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"strings"
	"testing"
	"time"
)

func TestCurrencySettlementProbeAdapterDetachedAndNoAuthority(t *testing.T) {
	at := time.Now().UTC()
	in := ibkrlib.AccountCurrencySettlementProbe{Status: "completed", CancelStatus: "sent", ClockSource: "local_receive", SocketEpoch: 9, RequestedAt: at, CompletedAt: at, ReadAt: at, Callbacks: 1, Rows: []ibkrlib.AccountStreamRow{{Key: "SettledCash", Currency: "USD", Source: "currency_only_multi", ValueStatus: "observed"}}}
	out := accountCurrencySettlementProbe(in)
	in.Rows[0].Currency = "EUR"
	if out.Rows[0].Currency != "USD" {
		t.Fatal("adapter aliases mutable source")
	}
	raw, _ := json.Marshal(out)
	for _, forbidden := range []string{"amount", "account_id", "authority", "settled_cash", "available_cash"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("diagnostic gained financial contract", string(raw))
		}
	}
	s := &Server{}
	result, err := s.handleCurrencySettlementProbe(context.Background())
	if err != nil || result.(*rpc.AccountCurrencySettlementProbe).Status != "skipped_budget" {
		t.Fatal(result, err)
	}
	req := &rpc.Request{Params: json.RawMessage(`{"settlement_probe":true,"currency_settlement_probe":true}`)}
	if _, err := s.handleAccountSummaryRequest(context.Background(), req); err == nil {
		t.Fatal("mixed diagnostics reached connector")
	}
}
