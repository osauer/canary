package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDisplayProjectionKeepsClockUnitsAndZeroVolume(t *testing.T) {
	at := time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)
	pnl := 7.0
	c := ibkr.Contract{ConID: 101, Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	snapshot := ibkr.DisplaySnapshot{PnLAccount: "U_SYNTHETIC", Account: &ibkr.RawAccountSummary{AccountID: "U_SYNTHETIC", BaseCurrency: "USD", BaseCurrencyProvenance: ibkr.AccountBaseCurrencyExplicitTag}, AccountPnL: ibkr.AccountDailyPnL{DailyPnL: &pnl, AsOf: at}, Positions: []*ibkr.RawPosition{{Account: "U_SYNTHETIC", Contract: c, Position: 1}}, PositionPnL: map[int]ibkr.PositionDailyPnL{101: {DailyPnL: &pnl, AsOf: at}}, Quotes: map[string]*ibkr.MarketData{"key": {Last: 10, LastAt: at, VolumeObserved: true, Volume: 0, VolumeAt: at}}, DataTypes: map[string]int{"key": 1}}
	holds := []displayHold{{item: displayInstrument{contract: c}, cacheKey: "key"}}
	scope := rpc.AccountDataScope{AccountID: "U_SYNTHETIC", AccountMode: "paper"}
	out := projectDisplay(snapshot, holds, scope, nil)
	if out.Account.PnLAt != at || out.Quotes[0].PriceReceivedAt != at || out.Quotes[0].Volume == nil || *out.Quotes[0].Volume != 0 || out.Positions[0].DailyPnL == nil {
		t.Fatal("lost clock, zero, or same-currency PnL")
	}
	snapshot.Positions[0].Contract.Currency = "EUR"
	if projectDisplay(snapshot, holds, scope, nil).Positions[0].DailyPnL != nil {
		t.Fatal("unproved PnL FX conversion")
	}
	snapshot.PnLAccount = "U_FOREIGN"
	if projectDisplay(snapshot, holds, scope, nil).Account.DailyPnL != nil {
		t.Fatal("foreign account PnL admitted")
	}
	snapshot.Quotes["key"].Volume = -1
	if projectDisplay(snapshot, holds, scope, nil).Quotes[0].Volume != nil {
		t.Fatal("invalid volume admitted")
	}
}
func TestDisplayRetiredReleaseCannotDestroyReplacement(t *testing.T) {
	m := newSubManager(nil)
	old := &subEntry{sym: "SYNTH", stop: make(chan struct{}), taps: map[*frameTap]struct{}{}}
	newer := &subEntry{sym: "SYNTH", stop: make(chan struct{}), taps: map[*frameTap]struct{}{}, refcount: 1}
	m.subs["SYNTH"] = newer
	m.release("SYNTH", nil, old)
	m.emitEntryError("SYNTH", old, rpc.FrameErrGatewayLost, "old session ended")
	if m.subs["SYNTH"] != newer || newer.refcount != 1 {
		t.Fatal("retired cleanup destroyed replacement")
	}
	select {
	case <-newer.stop:
		t.Fatal("replacement stopped")
	default:
	}
}

func TestDisplayShutdownWaitsForOwnedRuns(t *testing.T) {
	s := &Server{displayRuns: map[*displayRun]struct{}{}}
	ctx, cancel := context.WithCancel(context.Background())
	run := &displayRun{cancel: cancel, done: make(chan struct{})}
	s.displayRuns[run] = struct{}{}
	returned := make(chan struct{})
	go func() { s.stopDisplay(); close(returned) }()
	<-ctx.Done()
	select {
	case <-returned:
		t.Fatal("shutdown skipped stream join")
	default:
	}
	close(run.done)
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not finish after join")
	}
	if !s.displayStopping {
		t.Fatal("shutdown still accepts streams")
	}
}

type displayTransport struct{ subscribes, cancels int }

func (f *displayTransport) SubscribeMarketData(context.Context, string, []string) error {
	f.subscribes++
	return nil
}
func (f *displayTransport) SubscribeMarketDataWithContract(_ context.Context, c ibkr.Contract, _ []string) (string, error) {
	f.subscribes++
	return ibkr.MarketDataKeyForContract(c), nil
}
func (f *displayTransport) UnsubscribeMarketData(string) error              { f.cancels++; return nil }
func (f *displayTransport) MarketDataSnapshot() map[string]*ibkr.MarketData { return nil }
func (f *displayTransport) MarketDataTypeForSymbol(string) int              { return 1 }
func TestDisplaySharedRoutedBorrowerCannotCancelFeed(t *testing.T) {
	transport := &displayTransport{}
	m := newSubManager(func() ibkrMarketConnector { return transport })
	m.coalesce = time.Hour
	contract := ibkr.Contract{Symbol: "SYNTH", ConID: 101, SecType: "STK", Exchange: "SMART", Currency: "USD"}
	_, leaveStream, err := m.HoldContract(t.Context(), contract)
	if err != nil {
		t.Fatal(err)
	}
	_, leaveSnapshot, err := m.HoldContract(t.Context(), contract)
	if err != nil {
		t.Fatal(err)
	}
	leaveSnapshot(t.Context())
	if transport.subscribes != 1 || transport.cancels != 0 || m.activeCount() != 1 {
		t.Fatal("snapshot destroyed stream subscription")
	}
	leaveStream(t.Context())
	if transport.cancels != 1 || m.activeCount() != 0 {
		t.Fatal("last consumer leaked broker line")
	}
}
