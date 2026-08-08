//go:build trading

package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// TestReduceSubmitSerializesAgainstConcurrentOrderPlace asserts that the
// discretionary reduce-submit handler holds brokerWriteMu across its whole
// check-then-act, so it cannot interleave with another broker write. Without the
// lock in handleTradeProposalsReduceSubmit the reduce body (preview -> place)
// would run while an order place was mid-flight, reintroducing the double-submit
// TOCTOU the mutex exists to prevent (see server.go). handleTradeProposalsSubmit
// and handleOrderPlace share that same mutex, so an order place stands in here
// for every other broker writer.
//
// The serialization guarantee lives entirely in the handler's Lock(); the reduce
// body itself fails fast at the positions lookup in this harness (no gateway
// connector), which is fine — we are asserting on the lock, not the outcome.
func TestReduceSubmitSerializesAgainstConcurrentOrderPlace(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }

	// The order place parks inside the broker hook while still holding
	// brokerWriteMu, giving a deterministic window in which a competing broker
	// write must be excluded.
	placeEntered := make(chan struct{})
	releasePlace := make(chan struct{})
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		close(placeEntered)
		<-releasePlace
		return nil
	}

	token := mintPreviewTokenForConfirmTest(t, srv, rpc.OrderWhatIfResult{
		Status:    rpc.OrderWhatIfStatusAccepted,
		Available: true,
	})

	type placeOutcome struct {
		res *rpc.OrderPlaceResult
		err error
	}
	placeCh := make(chan placeOutcome, 1)
	go func() {
		res, err := srv.handleOrderPlace(context.Background(), &rpc.Request{
			Params: mustJSON(t, rpc.OrderPlaceParams{PreviewToken: token}),
		})
		placeCh <- placeOutcome{res, err}
	}()

	// Once the broker hook is reached, the order place holds brokerWriteMu.
	<-placeEntered

	reduceReturned := make(chan struct{})
	go func() {
		_, _ = srv.handleTradeProposalsReduceSubmit(context.Background(), &rpc.Request{
			Params: mustJSON(t, rpc.TradeProposalReduceParams{ConID: 4391, Percent: 50}),
		})
		close(reduceReturned)
	}()

	// While the order place holds the mutex the reduce submit must not return.
	// An unserialized reduce body completes in microseconds (no I/O), so this
	// short window reliably distinguishes "blocked on the mutex" from "ran".
	select {
	case <-reduceReturned:
		t.Fatal("reduce submit completed while an order place held brokerWriteMu; the reduce path is not serialized against concurrent broker writes")
	case <-time.After(250 * time.Millisecond):
	}

	// Release the place; the reduce must now make progress (the deferred Unlock
	// in handleOrderPlace happens-before the reduce handler can acquire the lock).
	close(releasePlace)

	select {
	case out := <-placeCh:
		if out.err != nil || out.res == nil || !out.res.Accepted {
			t.Fatalf("order place outcome = %+v, err = %v; want accepted", out.res, out.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("order place did not complete after release")
	}

	select {
	case <-reduceReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("reduce submit did not proceed after the order place released brokerWriteMu")
	}
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
