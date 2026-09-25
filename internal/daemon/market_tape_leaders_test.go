package daemon

import (
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func leaderFixture(t *testing.T) (map[string]map[string]rpc.MarketHistoryPoint, map[string]float64, []marketcal.Session) {
	t.Helper()
	calendar, err := tapeArchiveCalendar(time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), 116)
	if err != nil {
		t.Fatal(err)
	}
	points := make(map[string]map[string]rpc.MarketHistoryPoint)
	bases := make(map[string]float64)
	for _, m := range tapeLeaderWeights {
		points[m.symbol] = make(map[string]rpc.MarketHistoryPoint)
		bases[m.symbol] = 100
		for _, s := range calendar {
			at, _ := time.Parse("2006-01-02", s.Date)
			points[m.symbol][s.Date] = rpc.MarketHistoryPoint{At: at, Value: 100, Volume: new(int64(100))}
		}
	}
	return points, bases, calendar
}

func TestMarketTapeLeadersCountsCompaniesAndKeepsFixedWeights(t *testing.T) {
	p, b, c := leaderFixture(t)
	i := len(c) - 1
	v := p["GOOG"][c[i].Date]
	v.Value = 50
	p["GOOG"][c[i].Date] = v
	r := buildTapeLeaders(p, b, c, i-4, i)
	if r.Price == nil || r.Companies.PctAbove50DMA == nil || *r.Companies.PctAbove50DMA != 100 || *r.Companies.RisingPct != 0 || r.Companies.Unchanged != 10 {
		t.Fatalf("share-class vote or measured zero lost: %+v", r)
	}
	w := 0.0
	for _, m := range r.Members {
		w += m.WeightPct
	}
	if math.Abs(w-100) > 1e-10 || math.Abs(*r.Price.ChangePct+50*2.397521/40.246818) > 1e-10 || math.Abs(*r.Price.RelativeVolume20-1) > 1e-10 {
		t.Fatal("fixed weighted return/activity incorrect")
	}
	// Missing AAPL must not silently increase everybody else's weight or shrink the company denominator.
	delete(p["AAPL"], c[i].Date)
	r = buildTapeLeaders(p, b, c, i-4, i)
	if r.Price != nil || r.Companies.Coverage50 != 9 || r.Companies.PctAbove50DMA != nil || r.Companies.RisingPct != nil || r.VolumeCoverage != 10 {
		t.Fatal("incomplete basket reweighted or called complete")
	}
}

func TestMarketTapeLeadersGapAndWindowIndependentArchive(t *testing.T) {
	p, b, c := leaderFixture(t)
	i := len(c) - 1
	delete(p["AAPL"], c[i-10].Date)
	r := buildTapeLeaders(p, b, c, i-4, i)
	if r.Price == nil || r.Price.ChangePct == nil || r.Price.RelativeVolume20 != nil || r.Companies.PctAbove50DMA != nil {
		t.Fatal("history gap bridged")
	}
	p, b, c = leaderFixture(t)
	r = buildTapeLeaders(p, b, c, i-4, i)
	now := c[i].Close.Add(time.Hour)
	one, hash1, err := tapeArchiveCapture(rpc.MarketTapeSession{Date: c[i].Date, Leaders: r}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	two, hash2, err := tapeArchiveCapture(rpc.MarketTapeSession{Date: c[i].Date, Leaders: buildTapeLeaders(p, b, c, i-19, i)}, nil, now.Add(time.Minute))
	if err != nil || hash1 != hash2 || one.Session.Leaders.Price.WindowChangePct != nil || two.Session.Leaders.Members[0].Price.WindowChangePct != nil {
		t.Fatal("display window changed durable measurements")
	}
	store := tapeArchiveTestStore(t, filepath.Join(privateTestDir(t), "daemon.db"))
	archiveTapeFixture(t, store, now, one.Session)
	loaded, _, ok, err := loadTapeDay(t.Context(), store, c[i].Date)
	if err != nil || !ok || loaded.First.Session.Leaders.Method != tapeLeaderMethod || len(loaded.First.Session.Leaders.Members) != 11 {
		t.Fatal("basket definition/inputs not retained")
	}
}

func TestMarketTapeSessionRecoveryDoesNotInventIntradayTiming(t *testing.T) {
	p, _, c := leaderFixture(t)
	i := len(c) - 1
	v := p["NVDA"][c[i].Date]
	v.Open = new(97.0)
	v.Low = new(95.0)
	v.High = new(101.0)
	p["NVDA"][c[i].Date] = v
	r := tapePriceRow(p["NVDA"], c, i-4, i, true)
	if r.SessionMove == nil || math.Abs(*r.SessionMove.OpenChangePct+3) > 1e-10 || math.Abs(*r.SessionMove.LowChangePct+5) > 1e-10 || *r.ChangePct != 0 || math.Abs(*r.SessionMove.CloseInRangePct-100*5.0/6) > 1e-10 {
		t.Fatal("flat close hid observed intraday range")
	}
	v.High = new(90.0)
	p["NVDA"][c[i].Date] = v
	if tapePriceRow(p["NVDA"], c, i-4, i, true).SessionMove != nil {
		t.Fatal("invalid OHLC accepted")
	}
	v.Open = nil
	v.High = nil
	v.Low = nil
	p["NVDA"][c[i].Date] = v
	if tapePriceRow(p["NVDA"], c, i-4, i, true).SessionMove != nil {
		t.Fatal("legacy close manufactured a range")
	}
}

func TestMarketTapeRetainsSameSessionCompanyAverageWithoutRedating(t *testing.T) {
	when := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	current := &rpc.MarketTapeCompanies{Method: "representative-company-v1", CompanyCount: 500}
	saved := rpc.MarketTapeCapture{CapturedAt: when, Session: rpc.MarketTapeSession{Companies: &rpc.MarketTapeCompanies{Method: current.Method, CompanyCount: 500, Coverage50: 499, PctAbove50DMA: new(25.0)}}}
	saved.Session.Breadth = &rpc.MarketTapeBreadth{Participation: &rpc.BreadthParticipation{MembershipID: "synthetic-membership"}}
	one := retainTapeCompanies(current, saved, "synthetic-membership")
	if one.PctAbove50DMA == nil || *one.PctAbove50DMA != 25 || !one.RetainedAt.Equal(when) {
		t.Fatal("rolling source erased saved company average")
	}
	saved.Session.Companies = one
	saved.CapturedAt = when.Add(time.Hour)
	if two := retainTapeCompanies(current, saved, "synthetic-membership"); !two.RetainedAt.Equal(when) {
		t.Fatal("reread changed source clock")
	}
	current.CompanyCount = 499
	if retainTapeCompanies(current, saved, "synthetic-membership") != current {
		t.Fatal("changed universe accepted retained metric")
	}
	current.CompanyCount = 500
	if retainTapeCompanies(current, saved, "different-membership") == one || retainTapeCompanies(current, saved, "different-membership").PctAbove50DMA != nil {
		t.Fatal("same-size changed universe accepted retained metric")
	}
}
