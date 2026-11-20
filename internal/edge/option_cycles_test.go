package edge

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/flexstmt"
)

func TestOptionCyclesRequireProvenFlatBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		deltas  []float64
		markers []string
		final   float64
		want    int
		reason  string
	}{
		{"long adds and partial closes", []float64{2, 1, -1, -2}, []string{"O", "O", "C", "C"}, 0, 1, ""},
		{"short closes", []float64{-2, 1, 1}, []string{"O", "C", "C"}, 0, 1, ""},
		{"reopen", []float64{1, -1, 1, -1}, []string{"O", "C", "O", "C"}, 0, 2, ""},
		{"still open", []float64{2, -1}, []string{"O", "C"}, 1, 0, ""},
		{"missing opening", []float64{-2}, []string{"C"}, 0, 0, "opening_unproved"},
		{"add to missing opening", []float64{1, -2}, []string{"O", "C"}, 0, 0, "opening_unproved"},
		{"cross zero", []float64{1, -2}, []string{"O", "C"}, -1, 0, "cross_zero_execution"},
		{"unknown intent marker", []float64{1, -1}, []string{"", "C"}, 0, 0, "open_close_conflict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := cycleTestStatement(tc.deltas, tc.markers, tc.final)
			result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
			if err != nil {
				t.Fatal(err)
			}
			c := result.Options.Cycles
			if c.CompletedCount != tc.want || tc.reason != "" && c.Reasons[tc.reason] == 0 {
				t.Fatalf("cycles=%+v", c)
			}
			for _, row := range c.Cycles {
				if row.PNLStatus != OptionPNLComplete || row.ExecutionCount < 2 {
					t.Fatalf("cycle evidence=%+v", row)
				}
			}
			if tc.final != 0 && tc.reason == "" && c.OpenContractCount != 1 {
				t.Fatal("open inventory lost")
			}
		})
	}
}

func TestOptionCycleAmbiguityAndLifecycleRemainUnproved(t *testing.T) {
	for _, kind := range []string{"same_time", "missing_quantity", "lifecycle", "anchor_mismatch", "missing_anchor_quantity"} {
		st := cycleTestStatement([]float64{1, -1}, []string{"O", "C"}, 0)
		switch kind {
		case "same_time":
			st.Trades[1].ExecutedAt = st.Trades[0].ExecutedAt
		case "missing_quantity":
			st.Trades[0].Quantity = nil
		case "lifecycle":
			st.OptionEvents = []flexstmt.OptionEvent{{ConID: 456, Date: day("2026-01-15"), Quantity: new(float64(-1))}}
		case "missing_anchor_quantity":
			st.Positions[0].Quantity = nil
		case "anchor_mismatch":
			st.Positions = append(st.Positions, flexstmt.OpenPosition{RecordID: "early", AccountID: "U", ConID: 456, ReportDate: day("2026-01-01"), Quantity: new(float64(5))})
		}
		result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
		if err != nil {
			t.Fatal(err)
		}
		if result.Options.Cycles.CompletedCount != 0 || result.Options.Cycles.ExcludedContracts != 1 {
			t.Fatalf("%s became complete: %+v", kind, result.Options.Cycles)
		}
	}
}

func cycleTestStatement(deltas []float64, markers []string, final float64) flexstmt.Statement {
	st := edgeStatement()
	st.Trades = nil
	st.Positions = []flexstmt.OpenPosition{{RecordID: "end", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-02-01"), Quantity: &final}}
	st.Instruments = []flexstmt.Instrument{{RecordID: "i456", ConID: 456, Symbol: "SYN CALL", AssetClass: "OPT", ListingExchange: "CBOE", ReportDate: day("2026-02-01")}}
	for i, delta := range deltas {
		side := "BUY"
		if delta < 0 {
			side = "SELL"
			delta = -delta
		}
		pnl := 0.0
		if markers[i] == "C" {
			pnl = 10
		}
		id := fmt.Sprint(i)
		st.Trades = append(st.Trades, flexstmt.Trade{RecordID: id, AccountID: "U", ConID: 456, Symbol: "SYN CALL", AssetClass: "OPT", Currency: "EUR", OrderID: id, ExecutionID: id, ExecutedAt: day("2026-01-05").AddDate(0, 0, i), Side: side, OpenClose: markers[i], Quantity: &delta, Price: new(float64(2)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: &pnl, LevelOfDetail: "EXECUTION"})
	}
	return st
}

// The derived as_of is the last statement day at 00:00Z. Every execution
// reported on that day belongs to the window; a row reported on a later day
// does not, whatever its UTC clock says.
func TestLastStatementDayExecutionsAreInTheWindow(t *testing.T) {
	st := edgeStatement()
	opt := func(id string, conid int64, executed time.Time, report, side, marker string, pnl float64) flexstmt.Trade {
		return flexstmt.Trade{RecordID: id, AccountID: "U", ConID: conid, Symbol: "SYNX CALL", AssetClass: "OPT", Currency: "EUR", OrderID: id, ExecutionID: id, ExecutedAt: executed, ReportDate: day(report), Side: side, OpenClose: marker, Quantity: new(float64(1)), Price: new(float64(2)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(pnl), LevelOfDetail: "EXECUTION"}
	}
	st.Trades = append(st.Trades,
		// Opened inside the window, closed on the last statement day at
		// 15:00 New York (20:00Z).
		opt("open-456", 456, dayTime("2026-01-20", 15), "2026-01-20", "BUY", "O", 0),
		opt("close-456", 456, dayTime("2026-02-01", 20), "2026-02-01", "SELL", "C", 40),
		// An overnight-session fill: 21:00 New York on the prior calendar
		// day, reported on the last statement day.
		opt("open-457", 457, dayTime("2026-01-21", 15), "2026-01-21", "BUY", "O", 0),
		opt("close-457", 457, dayTime("2026-02-01", 2), "2026-02-01", "SELL", "C", 5),
		// Reported the day after the statement: never inside this window.
		opt("open-458", 458, dayTime("2026-01-22", 15), "2026-01-22", "BUY", "O", 0),
		opt("close-458", 458, dayTime("2026-02-02", 15), "2026-02-02", "SELL", "C", 99),
		flexstmt.Trade{RecordID: "stock-last-day", AccountID: "U", ConID: 123, Symbol: "SYNX", AssetClass: "STK", Currency: "USD", FXRateToBase: new(.9), OrderID: "o-last", ExecutionID: "e-last", ExecutedAt: dayTime("2026-02-01", 18), ReportDate: day("2026-02-01"), Side: "BUY", Quantity: new(float64(5)), Price: new(float64(100)), Multiplier: new(float64(1)), Commission: new(float64(-1)), Taxes: new(float64(0)), LevelOfDetail: "EXECUTION"},
	)
	st.Positions[0].Quantity = new(float64(15))
	st.Positions = append(st.Positions,
		flexstmt.OpenPosition{RecordID: "p456", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-02-01"), Quantity: new(float64(0))},
		flexstmt.OpenPosition{RecordID: "p457", AccountID: "U", ConID: 457, AssetClass: "OPT", ReportDate: day("2026-02-01"), Quantity: new(float64(0))},
		flexstmt.OpenPosition{RecordID: "p458", AccountID: "U", ConID: 458, AssetClass: "OPT", ReportDate: day("2026-02-01"), Quantity: new(float64(1))},
	)
	result, err := Analyze(Input{WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AsOf.Equal(day("2026-02-01")) {
		t.Fatalf("as_of=%s, want the last statement day", result.AsOf)
	}
	realized := result.Options.Realized
	if realized.KnownPNLBase == nil || *realized.KnownPNLBase != 45 {
		t.Fatalf("realized known P/L=%v, want 45 from the last statement day only: %+v", realized.KnownPNLBase, realized)
	}
	cycles := result.Options.Cycles
	if cycles.CompletedCount != 2 || cycles.KnownPNLBase == nil || *cycles.KnownPNLBase != 45 || cycles.OpenContractCount != 1 {
		t.Fatalf("cycles=%+v, want the two last-day closes completed and the later one open", cycles)
	}
	lastDay := 0
	for _, change := range result.Changes {
		if change.ExecutedAt.Equal(dayTime("2026-02-01", 18)) {
			lastDay++
		}
	}
	if lastDay != 1 {
		t.Fatalf("last statement day stock change missing from changes: %+v", result.Changes)
	}
}

// IBKR reports an expiry, assignment or exercise twice: as an OptionEAE row
// and as a BookTrade execution with the same trade ID, the same signed
// quantity and the broker's realized P/L. The pair closes the round trip
// once; an OptionEAE row without its execution still keeps the contract out.
func TestLifecycleClosedOptionCyclesAreCompleted(t *testing.T) {
	for _, tc := range []struct {
		name, eventType, closedBy string
		openSide                  string
		pnl                       float64
		grade, gradeReason        string
		decisions                 int
	}{
		// An expiry is how the position closed: the buy or the sale that
		// opened it is the only decision, and the grade carries the expiry.
		{"long expires worthless", "Expiration", "expiry", "BUY", -200, OptionGradeLostMoney, "", 1},
		{"short expires worthless", "EXPIRATION", "expiry", "SELL", 150, OptionGradeMadeMoney, "", 1},
		// The broker books the option leg alone at a delivery, so the
		// position has no grade.
		{"short assigned", "Assignment", "assignment", "SELL", 80, OptionGradeNone, OptionOutcomeDelivered, 2},
		{"long exercised", "Exercise", "exercise", "BUY", 0, OptionGradeNone, OptionOutcomeDelivered, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := edgeStatement()
			closeSide, quantity := "SELL", -1.0
			if tc.openSide == "SELL" {
				closeSide, quantity = "BUY", 1
			}
			trade := func(id, side, marker string, at time.Time, pnl float64) flexstmt.Trade {
				return flexstmt.Trade{RecordID: id, AccountID: "U", ConID: 456, Symbol: "SYNX 260116C00100000", AssetClass: "OPT", Currency: "EUR", TradeID: "t-" + id, OrderID: id, ExecutionID: id, ExecutedAt: at, ReportDate: dayStart(at), Side: side, OpenClose: marker, Quantity: new(float64(1)), Price: new(float64(2)), Multiplier: new(float64(100)), Commission: new(float64(-1)), Taxes: new(float64(0)), RealizedPNL: new(pnl), LevelOfDetail: "EXECUTION"}
			}
			st.Trades = append(st.Trades,
				trade("open", tc.openSide, "O", dayTime("2026-01-06", 15), 0),
				trade("book", closeSide, "C", dayTime("2026-01-16", 21), tc.pnl),
			)
			st.Trades[2].Price = new(float64(0))
			st.OptionEvents = []flexstmt.OptionEvent{{RecordID: "eae", AccountID: "U", ConID: 456, Symbol: "SYNX 260116C00100000", AssetClass: "OPT", Currency: "EUR", Date: day("2026-01-16"), TransactionType: strings.ToUpper(tc.eventType), Quantity: new(quantity), RealizedPNL: new(tc.pnl), TradeID: "t-book"}}
			// A dated anchor between the open and the event: replaying the
			// event and its execution as two mutations would not reconcile.
			held := -quantity
			st.Positions = append(st.Positions,
				flexstmt.OpenPosition{RecordID: "mid", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-01-10"), Quantity: &held},
				flexstmt.OpenPosition{RecordID: "end", AccountID: "U", ConID: 456, AssetClass: "OPT", ReportDate: day("2026-02-01"), Quantity: new(float64(0))},
			)
			result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
			if err != nil {
				t.Fatal(err)
			}
			c := result.Options.Cycles
			if c.CompletedCount != 1 || c.ExcludedContracts != 0 || len(c.Cycles) != 1 {
				t.Fatalf("cycles=%+v, want one completed round trip", c)
			}
			row := c.Cycles[0]
			if row.ClosedBy != tc.closedBy || row.RealizedPNLBase == nil || *row.RealizedPNLBase != tc.pnl || row.PNLStatus != OptionPNLComplete || row.ExecutionCount != 2 {
				t.Fatalf("cycle=%+v, want closed by %s with the broker's %.0f once", row, tc.closedBy, tc.pnl)
			}
			if row.Grade != tc.grade || row.GradeReason != tc.gradeReason {
				t.Fatalf("grade=%q reason=%q, want %q %q", row.Grade, row.GradeReason, tc.grade, tc.gradeReason)
			}
			decisions := 0
			for _, change := range result.Changes {
				if change.AssetClass != "OPT" {
					continue
				}
				decisions++
				if change.Verdict.Label != VerdictNone || change.Verdict.Reason != VerdictGradedByPosition || change.Position == nil || change.Position.PositionGrade != tc.grade {
					t.Fatalf("option change=%+v, want no verdict and the position's grade %q", change, tc.grade)
				}
			}
			if decisions != tc.decisions {
				t.Fatalf("option decisions=%d, want %d", decisions, tc.decisions)
			}
		})
	}
	t.Run("event without its execution stays excluded", func(t *testing.T) {
		st := cycleTestStatement([]float64{1}, []string{"O"}, 0)
		st.OptionEvents = []flexstmt.OptionEvent{{RecordID: "eae", ConID: 456, AssetClass: "OPT", Date: day("2026-01-16"), TransactionType: "EXPIRATION", Quantity: new(float64(-1)), TradeID: "t-missing"}}
		result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
		if err != nil {
			t.Fatal(err)
		}
		if c := result.Options.Cycles; c.CompletedCount != 0 || c.Reasons["lifecycle_or_transfer"] != 1 {
			t.Fatalf("cycles=%+v", c)
		}
	})
	t.Run("plain trade close says trade", func(t *testing.T) {
		st := cycleTestStatement([]float64{1, -1}, []string{"O", "C"}, 0)
		result, err := Analyze(Input{AsOf: day("2026-02-10"), WindowDays: 90, BaseCurrency: "EUR", Statements: []flexstmt.Statement{st}})
		if err != nil {
			t.Fatal(err)
		}
		if c := result.Options.Cycles; len(c.Cycles) != 1 || c.Cycles[0].ClosedBy != "trade" {
			t.Fatalf("cycles=%+v", c)
		}
	})
}
