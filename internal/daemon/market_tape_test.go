package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic prices/volumes only; dates exercise the official calendar.
func tapeFixture(t *testing.T) (time.Time, *rpc.MarketHistoryResult, *rpc.MarketHistoryResult, *rpc.BreadthSPXResult) {
	t.Helper()
	now := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	calendar, err := marketcal.New().Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now.AddDate(0, 0, -100), Days: 100})
	if err != nil {
		t.Fatal(err)
	}
	spx := &rpc.MarketHistoryResult{Contract: rpc.ContractParams{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD", ConID: 1}, TimestampKind: "session_date", Interval: "1 day", PriceBasis: "TRADES", RegularHoursOnly: true, AsOf: now.Add(-time.Hour), Source: "synthetic"}
	qqq := *spx
	qqq.Contract = rpc.ContractParams{Symbol: "QQQ", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD", ConID: 2}
	breadth := &rpc.BreadthSPXResult{State: rpc.BreadthStateReady, AsOf: now.Add(-2 * time.Hour), Source: "synthetic"}
	for _, session := range calendar.Sessions {
		if session.Close.IsZero() {
			continue
		}
		date, _ := time.Parse("2006-01-02", session.Date)
		p := rpc.MarketHistoryPoint{At: date, Value: 100 + float64(len(spx.Points)), Volume: new(int64(100))}
		spx.Points = append(spx.Points, p)
		qqq.Points = append(qqq.Points, p)
		breadth.History = append(breadth.History, rpc.BreadthDailyValue{Date: session.Date, PctAbove50DMA: 40, PctAbove200DMA: new(60.0), NewHighs: new(3), NewLows: new(4), MemberCount: 100, Coverage50: 90, Coverage200: 80, CoverageHighsLows: 80, Participation: &rpc.BreadthParticipation{Method: "constituent-participation-v1", RecordedAt: now.Add(-time.Hour), InputObservedAt: now.Add(-2 * time.Hour), MembershipID: strings.Repeat("a", 64), PctAbove20DMA: new(50.0), Coverage20: 90, Advancing: 45, Declining: 45, CoverageAD: 90, AdvancePct: new(50.0), AdvancingVolume: 100, DecliningVolume: 100, CoverageVolume: 90, UpVolumePct: new(50.0)}})
	}
	return now, spx, &qqq, breadth
}

func TestMarketTapeObservedCoverageAndIndependentClocks(t *testing.T) {
	now, spx, qqq, breadth := tapeFixture(t)
	last := len(qqq.Points) - 1
	qqq.Points[last].Volume = new(int64(200))
	got, err := buildMarketTape(rpc.MarketTapeParams{}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	row := got.Sessions[len(got.Sessions)-1]
	if len(got.Sessions) != 20 || got.LatestSession != "2026-09-23" || got.CoverageStatus != "available" || !got.NotPredictive || got.HistoricalAvailability != "unknown" {
		t.Fatalf("wrong tape metadata: %+v", got)
	}
	if row.SPX.Volume != nil || row.SPX.RelativeVolume20 != nil || *row.QQQ.RelativeVolume20 != 2 || *row.Breadth.Change50PP != 0 {
		t.Fatalf("wrong observations: %+v", row)
	}
	if !got.AsOf.Equal(now) || !got.Sources[0].AsOf.Equal(spx.AsOf) || !got.Sources[2].AsOf.Equal(breadth.AsOf) {
		t.Fatal("composition time replaced producer clocks")
	}
	wantChange := 100 * (qqq.Points[last].Value/qqq.Points[last-1].Value - 1)
	if math.Abs(*row.QQQ.ChangePct-wantChange) > 1e-10 {
		t.Fatal("incorrect daily price change")
	}
}

func TestMarketTapeMissingSessionBreaksDailyReturnAndVolumeBaseline(t *testing.T) {
	now, spx, qqq, breadth := tapeFixture(t)
	gap := len(qqq.Points) - 3
	gapDate := qqq.Points[gap].At.Format("2006-01-02")
	qqq.Points = append(qqq.Points[:gap], qqq.Points[gap+1:]...)
	got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions[2].Date != gapDate || got.Sessions[2].QQQ != nil || got.Sessions[3].QQQ.ChangePct != nil || got.Sessions[4].QQQ.ChangePct == nil || got.Sessions[4].QQQ.RelativeVolume20 != nil {
		t.Fatal("gap was bridged, zero-filled, or mislabeled as a daily return")
	}
	if got.CoverageStatus != "partial" || got.Sources[1].MissingSessions != 1 || got.Sources[1].MissingMetrics["relative_volume_20"] != 3 {
		t.Fatalf("missing coverage lost: %+v", got.Sources[1])
	}
	if got.Sessions[2].SPX == nil || got.Sessions[2].Breadth == nil {
		t.Fatal("one missing leg suppressed other evidence")
	}
}

func TestMarketTapeMissingAnchorAndNilVersusZeroVolume(t *testing.T) {
	for _, zero := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "zero"}[zero], func(t *testing.T) {
			now, spx, qqq, breadth := tapeFixture(t)
			qqq.Points[len(qqq.Points)-1].Volume = nil
			if zero {
				qqq.Points[len(qqq.Points)-1].Volume = new(int64(0))
			}
			got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
			if err != nil {
				t.Fatal(err)
			}
			row := got.Sessions[4].QQQ
			if zero && (row.Volume == nil || *row.Volume != 0 || row.RelativeVolume20 == nil || *row.RelativeVolume20 != 0 || got.CoverageStatus != "available") {
				t.Fatal("measured zero was lost")
			}
			if !zero && (row.RelativeVolume20 != nil || got.Sources[1].MissingMetrics["volume"] != 1 || got.CoverageStatus != "partial") {
				t.Fatal("missing volume became available")
			}
		})
	}
	now, spx, qqq, breadth := tapeFixture(t)
	anchor := len(qqq.Points) - 5
	qqq.Points = append(qqq.Points[:anchor], qqq.Points[anchor+1:]...)
	got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range got.Sessions {
		if row.QQQ != nil && row.QQQ.WindowChangePct != nil {
			t.Fatal("missing common base replaced by a different date")
		}
	}
}

func TestMarketTapeRejectsFutureOrWrongEvidenceAndKeepsSecondaryBreadth(t *testing.T) {
	now, spx, qqq, breadth := tapeFixture(t)
	spx.AsOf = now.Add(time.Second)
	qqq.Contract.SecType = "IND"
	for i := range breadth.History {
		breadth.History[i].Coverage50 = 0
		breadth.History[i].PctAbove50DMA = math.NaN()
	}
	got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sources[0].Status != "unavailable" || got.Sources[1].Status != "unavailable" || got.Sources[2].Status != "partial" {
		t.Fatalf("bad evidence accepted or valid secondary measure lost: %+v", got.Sources)
	}
	for _, row := range got.Sessions {
		if row.SPX != nil || row.QQQ != nil || row.Breadth.PctAbove50DMA != nil || row.Breadth.PctAbove200DMA == nil {
			t.Fatal("invalid evidence leaked into tape")
		}
	}
}

func TestMarketTapeCalendarAndAcquisitionBoundaries(t *testing.T) {
	for _, tc := range []struct{ now, latest string }{
		{"2026-09-24T19:59:00Z", "2026-09-23"},
		{"2026-09-24T20:14:59Z", "2026-09-23"},
		{"2026-09-24T20:15:00Z", "2026-09-24"},
		{"2026-11-27T18:14:59Z", "2026-11-25"},
		{"2026-11-27T18:15:00Z", "2026-11-27"},
		{"2026-11-29T22:00:00Z", "2026-11-27"},
	} {
		now, _ := time.Parse(time.RFC3339, tc.now)
		got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, nil, nil, nil)
		if err != nil || got.LatestSession != tc.latest || got.CoverageStatus != "unavailable" {
			t.Fatalf("calendar %s: result=%+v err=%v", tc.now, got, err)
		}
	}
	now, spx, qqq, breadth := tapeFixture(t)
	spx.AsOf = time.Date(2026, 9, 23, 19, 0, 0, 0, time.UTC)
	got, err := buildMarketTape(rpc.MarketTapeParams{Sessions: 5}, now, spx, qqq, breadth)
	if err != nil {
		t.Fatal(err)
	}
	if got.Sessions[4].SPX != nil || got.Sessions[3].SPX == nil {
		t.Fatal("pre-close acquisition was treated as a final close")
	}
	for _, n := range []int{-1, 4, 61} {
		if _, err := buildMarketTape(rpc.MarketTapeParams{Sessions: n}, now, nil, nil, nil); err == nil {
			t.Fatal("unbounded tape accepted")
		}
	}
	if _, err := buildMarketTape(rpc.MarketTapeParams{}, time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil, nil); err == nil {
		t.Fatal("unknown calendar accepted")
	}
}
