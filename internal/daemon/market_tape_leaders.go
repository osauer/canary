package daemon

import (
	"context"
	"slices"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

const tapeLeaderMethod = "spy-leaders-2026-09-23-v1"
const tapeLeaderBase = "2026-09-04"

// Frozen public SPY holding percentages, not portfolio positions. Keeping this
// dated definition fixed makes later observations comparable without reranking.
var tapeLeaderWeights = []struct {
	symbol string
	weight float64
}{
	{"NVDA", 8.217272}, {"AAPL", 7.405949}, {"MSFT", 5.596965}, {"AMZN", 3.684182},
	{"GOOGL", 2.984828}, {"AVGO", 2.543084}, {"META", 2.470680}, {"GOOG", 2.397521},
	{"MU", 1.822860}, {"TSLA", 1.612894}, {"AMD", 1.510583},
}

func tapeResearchContracts() []rpc.ContractParams {
	contracts := []rpc.ContractParams{
		{Symbol: "SPY", SecType: "STK", Exchange: "SMART", PrimaryExch: "ARCA", Currency: "USD"},
		{Symbol: "VIX", SecType: "IND", Exchange: "CBOE", Currency: "USD"},
	}
	for _, m := range tapeLeaderWeights {
		contracts = append(contracts, rpc.ContractParams{Symbol: m.symbol, SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"})
	}
	return contracts
}

func tapeCompanies(p *spx.Companies) *rpc.MarketTapeCompanies {
	if p == nil {
		return nil
	}
	return &rpc.MarketTapeCompanies{Method: p.Method, CompanyCount: p.CompanyCount, Coverage50: p.Coverage50, PctAbove50DMA: p.PctAbove50DMA, CoverageAD: p.CoverageAD, Rising: p.Rising, Falling: p.Falling, Unchanged: p.Unchanged, RisingPct: p.RisingPct}
}

func (s *Server) enrichMarketTape(ctx context.Context, result *rpc.MarketTapeResult, histories []*rpc.MarketHistoryResult) error {
	calendar, err := tapeArchiveCalendar(result.AsOf.AddDate(0, 0, -220), 221)
	if err != nil {
		return err
	}
	calendar = slices.DeleteFunc(calendar, func(v marketcal.Session) bool { return v.Close.Add(15 * time.Minute).After(result.AsOf) })
	indices := make(map[string]int, len(calendar))
	for i, v := range calendar {
		indices[v.Date] = i
	}
	start := indices[result.Sessions[0].Date]
	contracts := tapeResearchContracts()
	points := make(map[string]map[string]rpc.MarketHistoryPoint, len(contracts))
	for i, c := range contracts {
		key := c.Symbol
		if i >= 2 {
			key = "leader:" + c.Symbol
		}
		p, source := tapePriceInput(key, c.Symbol, c.SecType, histories[i], result.AsOf, calendar)
		points[c.Symbol] = p
		source.MissingMetrics = make(map[string]int)
		for _, row := range result.Sessions {
			if _, ok := p[row.Date]; !ok {
				source.MissingSessions++
			}
			if i >= 2 {
				idx := indices[row.Date]
				if tapeAverage50(p, calendar, idx) == nil {
					source.MissingMetrics["average_50"]++
				}
				r := tapePriceRow(p, calendar, start, idx, true)
				if r == nil || r.RelativeVolume20 == nil {
					source.MissingMetrics["relative_volume_20"]++
				}
			}
		}
		switch {
		case source.MissingSessions == len(result.Sessions):
			source.Status = "unavailable"
		case source.MissingSessions > 0 || len(source.MissingMetrics) > 0 || source.Status == "partial":
			source.Status = "partial"
		default:
			source.Status = "available"
		}
		if source.Status != "available" {
			result.CoverageStatus = "partial"
		}
		result.Sources = append(result.Sources, source)
	}
	// Daily histories are rolling read windows. The permanent anchor day keeps
	// fixed quantities reproducible after that day leaves the requested range.
	bases := make(map[string]float64)
	for _, m := range tapeLeaderWeights {
		if p, ok := points[m.symbol][tapeLeaderBase]; ok {
			bases[m.symbol] = p.Value
		}
	}
	if len(bases) < len(tapeLeaderWeights) && s.coreStore != nil {
		day, _, exists, err := loadTapeDay(ctx, s.coreStore, tapeLeaderBase)
		if err != nil {
			return err
		}
		if exists && day.Latest.Session.Leaders != nil && day.Latest.Session.Leaders.Method == tapeLeaderMethod {
			for _, m := range day.Latest.Session.Leaders.Members {
				if _, ok := bases[m.Symbol]; !ok && m.BaseClose != nil && isFinitePrice(*m.BaseClose) {
					bases[m.Symbol] = *m.BaseClose
				}
			}
		}
	}
	dates := make([]string, len(result.Sessions))
	for i, r := range result.Sessions {
		dates[i] = r.Date
	}
	var companies map[string]*spx.Companies
	if s.breadth != nil {
		companies = s.breadth.TapeCompanies(dates, result.AsOf)
	}
	for ri := range result.Sessions {
		row := &result.Sessions[ri]
		i := indices[row.Date]
		row.SPY = tapePriceRow(points["SPY"], calendar, start, i, true)
		row.VIX = tapePriceRow(points["VIX"], calendar, start, i, false)
		row.Leaders = buildTapeLeaders(points, bases, calendar, start, i)
		row.Companies = tapeCompanies(companies[row.Date])
		if row.Companies == nil && row.Breadth != nil && row.Breadth.Participation != nil {
			row.Companies = row.Breadth.Participation.Companies
		}
		if row.Companies != nil && row.Companies.PctAbove50DMA == nil && s.coreStore != nil && row.Breadth != nil && row.Breadth.Participation != nil {
			day, _, exists, err := loadTapeDay(ctx, s.coreStore, row.Date)
			if err != nil {
				return err
			}
			if exists {
				row.Companies = retainTapeCompanies(row.Companies, day.Latest, row.Breadth.Participation.MembershipID)
			}
		}
	}
	result.Notes = append(result.Notes,
		"Leaders: fixed 11 share lines / 10 companies, SPY weights dated 2026-09-23, normalized to 100%; fixed quantities from 2026-09-04. Earlier history is retrospective selection, not a point-in-time backtest.",
		"Company participation counts GOOGL, FOXA and NWSA once for their respective issuers. Missing representative data stays unavailable. Rising-company percentages include unchanged companies in the denominator; legacy share-line breadth is retained separately.",
		"When older company averages cannot be reconstructed from dated inputs, a saved same-session reading may be retained with its original capture time. It is never carried to another session.",
		"Leader activity averages each share line's volume relative to its preceding 20 sessions using the fixed weights. All 11 lines are required. Historical bars can be corrected or split-adjusted; changed measurements append archived revisions.",
		"Daily open/high/low describe the session's range and recovery from its low, but cannot locate a late-session turn. VIX is context, not a leading signal.")
	available := 0
	for _, source := range result.Sources {
		if source.Status != "unavailable" {
			available++
		}
	}
	if available == 0 {
		result.CoverageStatus = "unavailable"
	}
	return nil
}

func retainTapeCompanies(current *rpc.MarketTapeCompanies, saved rpc.MarketTapeCapture, membership string) *rpc.MarketTapeCompanies {
	p := saved.Session.Companies
	if membership == "" || saved.Session.Breadth == nil || saved.Session.Breadth.Participation == nil || saved.Session.Breadth.Participation.MembershipID != membership {
		return current
	}
	if current == nil || current.PctAbove50DMA != nil || p == nil || p.PctAbove50DMA == nil || p.Method != current.Method || p.CompanyCount != current.CompanyCount {
		return current
	}
	copy := *p
	if copy.RetainedAt.IsZero() {
		copy.RetainedAt = saved.CapturedAt
	}
	return &copy
}

func tapeAverage50(points map[string]rpc.MarketHistoryPoint, calendar []marketcal.Session, i int) *float64 {
	if i < 49 {
		return nil
	}
	sum := 0.0
	for j := i - 49; j <= i; j++ {
		p, ok := points[calendar[j].Date]
		if !ok {
			return nil
		}
		sum += p.Value
	}
	return tapeFinite(sum / 50)
}

func buildTapeLeaders(points map[string]map[string]rpc.MarketHistoryPoint, bases map[string]float64, calendar []marketcal.Session, start, i int) *rpc.MarketTapeLeaders {
	out := &rpc.MarketTapeLeaders{Method: tapeLeaderMethod, WeightsAsOf: "2026-09-23", WeightsSource: "https://www.ssga.com/library-content/products/fund-data/etfs/us/holdings-daily-us-en-spy.xlsx", BaseDate: tapeLeaderBase, Companies: rpc.MarketTapeCompanies{Method: "representative-company-v1", CompanyCount: 10}}
	total := 0.0
	for _, m := range tapeLeaderWeights {
		total += m.weight
	}
	level, previous, windowBase, activity := 0.0, 0.0, 0.0, 0.0
	levelN, previousN, baseN, above := 0, 0, 0, 0
	for _, m := range tapeLeaderWeights {
		p := points[m.symbol]
		weight := m.weight / total
		member := rpc.MarketTapeLeader{Symbol: m.symbol, Company: spx.CompanyRepresentative(m.symbol), Representative: spx.CompanyRepresentative(m.symbol) == m.symbol, WeightPct: 100 * weight, Price: tapePriceRow(p, calendar, start, i, true), Average50: tapeAverage50(p, calendar, i)}
		if base, ok := bases[m.symbol]; ok && isFinitePrice(base) {
			member.BaseClose = new(base)
			if member.Price != nil {
				level += 100 * weight * member.Price.Close / base
				levelN++
			}
			if i > 0 {
				if prior, ok := p[calendar[i-1].Date]; ok {
					previous += 100 * weight * prior.Value / base
					previousN++
				}
			}
			if prior, ok := p[calendar[start].Date]; ok {
				windowBase += 100 * weight * prior.Value / base
				baseN++
			}
		}
		if member.Price != nil {
			if member.Price.RelativeVolume20 != nil {
				out.VolumeCoverage++
				activity += weight * *member.Price.RelativeVolume20
			}
			if member.Representative {
				if member.Average50 != nil {
					out.Companies.Coverage50++
					if member.Price.Close >= *member.Average50 {
						above++
					}
				}
				if change := member.Price.ChangePct; change != nil {
					out.Companies.CoverageAD++
					switch {
					case *change > 0:
						out.Companies.Rising++
					case *change < 0:
						out.Companies.Falling++
					default:
						out.Companies.Unchanged++
					}
				}
			}
		}
		out.Members = append(out.Members, member)
	}
	if levelN == len(tapeLeaderWeights) && isFinitePrice(level) {
		out.Price = &rpc.MarketTapePrice{Close: level}
		if previousN == levelN && isFinitePrice(previous) {
			out.Price.ChangePct = tapeFinite(100 * (level/previous - 1))
		}
		if baseN == levelN && isFinitePrice(windowBase) {
			out.Price.WindowChangePct = tapeFinite(100 * (level/windowBase - 1))
		}
		if out.VolumeCoverage == levelN {
			out.Price.RelativeVolume20 = tapeFinite(activity)
		}
	}
	// A small fixed basket must not change its denominator silently.
	if out.Companies.Coverage50 == out.Companies.CompanyCount {
		out.Companies.PctAbove50DMA = new(100 * float64(above) / float64(out.Companies.CompanyCount))
	}
	if out.Companies.CoverageAD == out.Companies.CompanyCount {
		out.Companies.RisingPct = new(100 * float64(out.Companies.Rising) / float64(out.Companies.CompanyCount))
	}
	return out
}
