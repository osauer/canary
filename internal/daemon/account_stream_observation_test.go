package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAccountStreamObservationDetachedRedactedAndRetired(t *testing.T) {
	at := time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)
	in := &ibkrlib.AccountStreamObservation{Status: "initial_complete", ClockSource: "local_receive", SocketEpoch: 7, ReadAt: at, RequestedAt: at, DownloadEndAt: at, CompletedAt: at, AccountReady: "unknown", TradingType: "unknown", Rows: []ibkrlib.AccountStreamRow{{Key: "$LEDGER-SettledCash", Currency: "USD", Source: "broker_ledger_label", ValueStatus: "observed", Callbacks: 1, FirstReceivedAt: at, LastReceivedAt: at}}}
	got := accountStreamObservation(in)
	got.Rows[0].Key = "CashBalance"
	if in.Rows[0].Key != "$LEDGER-SettledCash" {
		t.Fatal("RPC mutation reached connector receipt")
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"account_id", "net_liquidation", "cash_balance", "settled_cash", "authority", "DU1234567"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("financial/identity field escaped diagnostic: %s", forbidden)
		}
	}
	res := &rpc.AccountResult{StreamObservation: got}
	finalizeAccountSummarySession(res, accountSummaryAuthority{}, false)
	if res.StreamObservation != nil {
		t.Fatal("session retirement retained passive receipt")
	}
	if accountStreamObservation(nil) != nil {
		t.Fatal("missing source fabricated empty receipt")
	}
}
