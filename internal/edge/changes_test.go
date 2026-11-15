package edge

import (
	"testing"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

// The scorecard's ledger must describe the same decisions Edge classifies:
// the exported changes match Analyze's own changes, carry the fill facts and
// the lineage the ledger needs, and stay stable under reordered evidence.
func TestPositionChangesMatchAnalyzeAndCarryLineage(t *testing.T) {
	t.Parallel()
	st := edgeStatement()
	st.Instruments = append(st.Instruments,
		flexstmt.Instrument{RecordID: "call-instrument", ConID: 456, UnderlyingConID: 123, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", Multiplier: new(float64(100)), Strike: new(float64(100)), Expiry: "20260320", PutCall: "C"},
	)
	st.Trades = append(st.Trades,
		flexstmt.Trade{RecordID: "call-open", AccountID: "U", ConID: 456, UnderlyingConID: 123, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "o-call", ExecutionID: "e-call", ExecutedAt: dayTime("2026-01-12", 10), Side: "BUY", OpenClose: "O", Quantity: new(float64(1)), Price: new(float64(3)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), ClosePrice: new(float64(3.2)), LevelOfDetail: "EXECUTION"},
		flexstmt.Trade{RecordID: "call-close", AccountID: "U", ConID: 456, UnderlyingConID: 123, Symbol: "ACME CALL", UnderlyingSymbol: "ACME", AssetClass: "OPT", Currency: "USD", FXRateToBase: new(.9), OrderID: "o-call-2", ExecutionID: "e-call-2", ExecutedAt: dayTime("2026-01-20", 10), Side: "SELL", OpenClose: "C", Quantity: new(float64(1)), Price: new(float64(5)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(float64(200)), LevelOfDetail: "EXECUTION"},
	)
	st.Positions = append(st.Positions, flexstmt.OpenPosition{RecordID: "p-call", AccountID: "U", ConID: 456, Symbol: "ACME CALL", AssetClass: "OPT", Currency: "USD", ReportDate: day("2026-01-15"), Quantity: new(float64(1)), Multiplier: new(float64(100)), MarkPrice: new(float64(4))})
	st.Trades[0].TradeID = "t-stock"
	st.OptionEvents = append(st.OptionEvents, flexstmt.OptionEvent{RecordID: "assign", AccountID: "U", ConID: 123, Symbol: "ACME", AssetClass: "STK", Currency: "USD", Date: dayTime("2026-01-05", 12), TransactionType: "ASSIGNMENT", Quantity: new(float64(10)), TradeID: "t-stock"})
	analyzed, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 365, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}, Bars: barsMap(123, "2026-01-06", 20, 105)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := PositionChanges([]flexstmt.Statement{st}, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Changes) != len(analyzed.Changes) {
		t.Fatalf("exported %d changes, Analyze %d", len(got.Changes), len(analyzed.Changes))
	}
	for i, change := range got.Changes {
		want := analyzed.Changes[i]
		if change.ConID != want.ConID || change.Action != want.Action || change.Direction != want.Direction || !change.ExecutedAt.Equal(want.ExecutedAt) || change.DeltaQuantity != want.DeltaQuantity || change.PositionBefore != want.PositionBefore {
			t.Fatalf("change %d = %+v, Analyze %+v", i, change, want)
		}
	}
	stock := got.Changes[0]
	if stock.Symbol != "ACME" || stock.ExecutionFX == nil || *stock.ExecutionFX != 0.9 || stock.ExecutionNotionalBase == nil || *stock.ExecutionNotionalBase != 900 || len(stock.ExecutionIDs) != 1 || stock.ExecutionIDs[0] != "e1" || stock.OrderIDs[0] != "o1" {
		t.Fatalf("stock change = %+v", stock)
	}
	call := got.Changes[1]
	if call.UnderlyingConID != 123 || call.UnderlyingSymbol != "ACME" || call.ClosePrice == nil || *call.ClosePrice != 3.2 || call.Action != ActionOpen || call.Direction != DirectionLong {
		t.Fatalf("call change = %+v", call)
	}
	if got.Changes[2].Action != ActionExit || got.Changes[2].PositionAfter != 0 {
		t.Fatalf("call exit = %+v", got.Changes[2])
	}
	if !got.LifecycleTradeIDs["t-stock"] || len(stock.TradeIDs) != 1 || stock.TradeIDs[0] != "t-stock" {
		t.Fatalf("lifecycle lineage = %v / %v", got.LifecycleTradeIDs, stock.TradeIDs)
	}
	if got.Instruments[456].PutCall != "C" || len(got.Marks[456]) != 1 || got.Marks[456][0].Price != 4 || !got.PositionsAnchored {
		t.Fatalf("evidence = instruments %d marks %+v anchored %v", len(got.Instruments), got.Marks, got.PositionsAnchored)
	}
	// Reordered trades yield the same changes.
	shuffled := st
	shuffled.Trades = []flexstmt.Trade{st.Trades[2], st.Trades[0], st.Trades[1]}
	again, err := PositionChanges([]flexstmt.Statement{shuffled}, "EUR")
	if err != nil {
		t.Fatal(err)
	}
	for i := range got.Changes {
		if again.Changes[i].Key != got.Changes[i].Key || again.Changes[i].DeltaQuantity != got.Changes[i].DeltaQuantity {
			t.Fatalf("reordered evidence changed the result: %+v vs %+v", again.Changes[i], got.Changes[i])
		}
	}
}
