package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/ibkrledger"
)

func TestCashLedgerCacheIsScopedAndDoesNotReviveFailedEvidence(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	var reads atomic.Int32
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads.Add(1)
		if fail.Load() {
			http.Error(w, "private broker failure", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v1/api/portfolio/accounts" {
			fmt.Fprint(w, `[{"accountId":"DU1234567"},{"accountId":"DU7654321"}]`)
			return
		}
		account := "DU1234567"
		if r.URL.Path == "/v1/api/portfolio/DU7654321/ledger" {
			account = "DU7654321"
		}
		fmt.Fprintf(w, `{"USD":{"acctcode":%q,"currency":"USD","cashbalance":12000,"settledcash":9000,"timestamp":%d}}`, account, now.Unix())
	}))
	defer server.Close()
	op := ibkrledger.Options{URL: server.URL + "/v1/api"}
	a := cashLedgerAuthority{}
	source := accountSnapshotSource{scope: brokerStateScope{Account: "DU1234567", Mode: "paper"}}
	clock := func() time.Time { return now }
	rows, err := a.read(context.Background(), op, source, clock)
	if err != nil || rows["USD"].Settled != 9000 {
		t.Fatal("initial receipt", err)
	}
	rows["USD"] = ibkrledger.Cash{Settled: 999999}
	rows, err = a.read(context.Background(), op, source, clock)
	if err != nil || rows["USD"].Settled != 9000 || reads.Load() != 2 {
		t.Fatal("cache mutation or redundant read")
	}
	source.scope.Account = "DU7654321"
	if _, err = a.read(context.Background(), op, source, clock); err != nil || reads.Load() != 4 {
		t.Fatal("account transition reused old scope", err)
	}
	source.scope.Mode = "live"
	if _, err = a.read(context.Background(), op, source, clock); err != nil || reads.Load() != 6 {
		t.Fatal("mode transition reused old scope", err)
	}
	now = now.Add(16 * time.Second)
	fail.Store(true)
	if rows, err = a.read(context.Background(), op, source, clock); err == nil || len(rows) != 0 {
		t.Fatal("failure revived last good cash")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = a.read(ctx, op, source, clock); err == nil {
		t.Fatal("cancelled observer received cash")
	}
}
