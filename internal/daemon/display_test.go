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

func TestDisplayUniverseHonoursReviewedTerminalStock(t *testing.T) {
	const deadConID = 900201
	record := earningsTerminalRecord{Contract: earningsTerminalContract{ConID: deadConID, Symbol: "SYNTHDEAD", SecType: "STK"}, Classification: earningsTerminalClassEquityCancelled,
		EffectiveDate: "2026-08-01", VerifiedAt: terminalImportBase.Add(-time.Hour), RevalidateAfter: terminalImportBase.Add(24 * time.Hour)}
	s := &Server{earningsTerminal: &earningsTerminalStore{revision: 2, reviewedAt: terminalImportBase, byConID: map[int]earningsTerminalStored{deadConID: {record: record, fingerprint: earningsTerminalRecordFingerprint(record)}}}}
	stock := ibkr.Contract{ConID: deadConID, Symbol: "SYNTHDEAD", SecType: "STK", Currency: "USD"}
	snapshot := ibkr.DisplaySnapshot{Positions: []*ibkr.RawPosition{{Account: "U_SYNTHETIC", Contract: stock, Position: 1}}}
	snapshot.Health.Account = "U_SYNTHETIC"
	items, _ := s.displayUniverse(snapshot, terminalImportBase)
	for _, item := range items {
		if item.kind == "held" && item.contract.ConID == deadConID {
			t.Fatal("current reviewed terminal stock is still automatically subscribed")
		}
	}
	frame := projectDisplay(snapshot, nil, rpc.AccountDataScope{AccountID: "U_SYNTHETIC", AccountMode: "paper"}, nil)
	if len(frame.Positions) != 1 || frame.Positions[0].Contract.ConID != deadConID || frame.Positions[0].Quantity != 1 {
		t.Fatal("terminal quote exemption removed the broker holding")
	}
	for _, tc := range []struct {
		name     string
		server   *Server
		contract ibkr.Contract
		now      time.Time
	}{
		{"expired", s, stock, record.RevalidateAfter},
		{"other_contract", s, ibkr.Contract{ConID: deadConID + 1, Symbol: stock.Symbol, SecType: "STK", Currency: "USD"}, terminalImportBase},
		{"conflicting_symbol", s, ibkr.Contract{ConID: deadConID, Symbol: "SYNTHOTHER", SecType: "STK", Currency: "USD"}, terminalImportBase},
		{"option_leg", s, ibkr.Contract{ConID: deadConID, Symbol: stock.Symbol, SecType: "OPT", Currency: "USD"}, terminalImportBase},
		{"unconfigured", &Server{}, stock, terminalImportBase},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot.Positions[0].Contract = tc.contract
			items, _ := tc.server.displayUniverse(snapshot, tc.now)
			for _, item := range items {
				if item.kind == "held" && item.contract.ConID == tc.contract.ConID && item.contract.Symbol == tc.contract.Symbol {
					return
				}
			}
			t.Fatal("holding without current exact terminal authority lost subscription demand")
		})
	}
}

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
