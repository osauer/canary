package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
)

func (s *Server) handleMarketTape(ctx context.Context, req *rpc.Request) (*rpc.MarketTapeResult, error) {
	var p rpc.MarketTapeParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	p, err := rpc.NormalizeMarketTapeParams(p)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	if p.History {
		result, err := readMarketTapeHistory(ctx, s.coreStore, p, now)
		if err == nil && s.marketTapeArchiveFailed.Load() {
			result.Archive.Status = "collection_failed"
		}
		return result, err
	}
	return s.buildAndArchiveMarketTape(ctx, p, func(ctx context.Context, p rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error) {
		if p.Contract.Symbol != "SPX" && p.Contract.Symbol != "QQQ" {
			// Research reads use the collector's retained data, never register
			// thirteen additional all-day interactive chart subscriptions.
			key, params, err := marketHistoryIdentity(p)
			if err != nil {
				return nil, err
			}
			saved, storedAt, err := s.loadMarketHistory(ctx, key)
			if err != nil || saved == nil {
				return nil, err
			}
			return selectStoredHistory(saved, storedAt, params, time.Now(), "cache", "", false), nil
		}
		raw, _ := json.Marshal(p)
		return s.handleMarketHistory(ctx, &rpc.Request{Params: raw})
	})
}

func (s *Server) buildAndArchiveMarketTape(ctx context.Context, p rpc.MarketTapeParams, read func(context.Context, rpc.MarketHistoryParams) (*rpc.MarketHistoryResult, error)) (*rpc.MarketTapeResult, error) {
	contracts := []rpc.ContractParams{
		{Symbol: "SPX", SecType: "IND", Exchange: "CBOE", Currency: "USD"},
		{Symbol: "QQQ", SecType: "STK", Exchange: "SMART", PrimaryExch: "NASDAQ", Currency: "USD"},
	}
	contracts = append(contracts, tapeResearchContracts()...)
	history := make([]*rpc.MarketHistoryResult, len(contracts))
	var wg sync.WaitGroup
	for i, contract := range contracts {
		wg.Go(func() {
			// Reuse bounded/coalesced acquisition and the existing durable cache.
			// A failed leg stays unavailable without disclosing raw broker errors.
			history[i], _ = read(ctx, rpc.MarketHistoryParams{Contract: contract, Range: "6M"})
		})
	}
	breadth, _ := s.buildBreadthSPX(&rpc.Request{Params: json.RawMessage(`{"history_days":90}`)}, false)
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	result, err := buildMarketTape(p, now, history[0], history[1], breadth)
	if err != nil {
		return nil, err
	}
	if err := s.enrichMarketTape(ctx, result, history[2:]); err != nil {
		return nil, err
	}
	result.Archive, err = archiveMarketTape(ctx, s.coreStore, result)
	s.marketTapeArchiveFailed.Store(err != nil)
	if err != nil {
		result.Archive = &rpc.MarketTapeArchiveStatus{Status: "collection_failed"}
		result.Notes = append(result.Notes, "Daily archive could not be saved; the displayed observations are not confirmed retained.")
	}
	return result, nil
}

func buildMarketTape(p rpc.MarketTapeParams, now time.Time, spx, qqq *rpc.MarketHistoryResult, breadth *rpc.BreadthSPXResult) (*rpc.MarketTapeResult, error) {
	p, err := rpc.NormalizeMarketTapeParams(p)
	if err != nil {
		return nil, err
	}
	calendar, err := marketcal.NewWithClock(func() time.Time { return now }).Query(marketcal.Query{Market: marketcal.MarketUSEquity, At: now.AddDate(0, 0, -180), Days: 181})
	if err != nil {
		return nil, err
	}
	var sessions []marketcal.Session
	for _, session := range calendar.Sessions {
		if !session.Close.IsZero() && !session.Close.After(now.Add(-15*time.Minute)) {
			sessions = append(sessions, session)
		}
	}
	current, err := marketcal.New().SessionAt(marketcal.MarketUSEquity, now)
	if err != nil || current.State == marketcal.StateUnknown || len(sessions) < p.Sessions+1 {
		return nil, errors.New("completed-session calendar unavailable for market tape")
	}
	start := len(sessions) - p.Sessions
	spxPoints, spxSource := tapePriceInput("spx", "SPX", "IND", spx, now, sessions)
	qqqPoints, qqqSource := tapePriceInput("qqq", "QQQ", "STK", qqq, now, sessions)
	breadthPoints, breadthSource := tapeBreadthInput(breadth, now, sessions)
	qqqSource.MissingMetrics = make(map[string]int)
	breadthSource.MissingMetrics = make(map[string]int)
	out := &rpc.MarketTapeResult{
		SchemaVersion: "market-tape-v1", AsOf: now, Timezone: "America/New_York",
		LatestSession: sessions[len(sessions)-1].Date, NotPredictive: true,
		HistoricalAvailability: "unknown", CoverageStatus: "available",
		Sessions: make([]rpc.MarketTapeSession, 0, p.Sessions),
		Notes: []string{
			"Retrospective daily observations; original per-session availability times are unknown. No predictive claim.",
			"Price returns exclude dividends. Completed regular sessions use a 15-minute settle window; breadth can arrive later.",
			"QQQ volume is IBKR-reported ETF volume, not total-market volume or signed buying/selling flow. Index volume is inapplicable.",
			"Relative volume divides QQQ volume by the mean of the preceding 20 complete sessions. Gaps remain unavailable.",
			"The price chart uses the first displayed close as its base; a missing base leaves that series unavailable. Daily changes remain independently available.",
			"Constituent participation uses separately covered daily closes and volume; historical values stay missing until collected. Unchanged names and volume are excluded from directional shares. This is not trade-by-trade buying or selling flow.",
			"Put/call flow and intraday persistence are not collected by this tape yet. These observations do not establish predictive skill.",
		},
	}
	present := make(map[string]bool)
	// Keep the interpretation's preceding five sessions independent of the
	// displayed chart range, including when inspecting its first row.
	contextRows := make([]rpc.MarketTapeSession, 0, p.Sessions+5)
	for i := max(1, start-5); i < start; i++ {
		contextRows = append(contextRows, rpc.MarketTapeSession{Date: sessions[i].Date, SPX: tapePriceRow(spxPoints, sessions, start, i, false)})
	}
	for i := start; i < len(sessions); i++ {
		date := sessions[i].Date
		row := rpc.MarketTapeSession{Date: date,
			SPX:     tapePriceRow(spxPoints, sessions, start, i, false),
			QQQ:     tapePriceRow(qqqPoints, sessions, start, i, true),
			Breadth: breadthPoints[date],
		}
		present["spx"] = present["spx"] || row.SPX != nil
		present["qqq"] = present["qqq"] || row.QQQ != nil
		present["breadth"] = present["breadth"] || row.Breadth != nil
		if row.SPX == nil {
			spxSource.MissingSessions++
		}
		if row.QQQ == nil {
			qqqSource.MissingSessions++
		}
		if row.QQQ == nil || row.QQQ.Volume == nil {
			qqqSource.MissingMetrics["volume"]++
		}
		if row.QQQ == nil || row.QQQ.RelativeVolume20 == nil {
			qqqSource.MissingMetrics["relative_volume_20"]++
		}
		if row.Breadth == nil || row.Breadth.PctAbove50DMA == nil {
			breadthSource.MissingSessions++
		}
		if row.Breadth == nil || row.Breadth.PctAbove200DMA == nil {
			breadthSource.MissingMetrics["pct_above_200dma"]++
		}
		if row.Breadth == nil || row.Breadth.NewHighs == nil {
			breadthSource.MissingMetrics["highs_lows"]++
		}
		var participation *rpc.BreadthParticipation
		if row.Breadth != nil {
			participation = row.Breadth.Participation
		}
		if participation == nil || participation.PctAbove20DMA == nil {
			breadthSource.MissingMetrics["pct_above_20dma"]++
		}
		if participation == nil || participation.CoverageAD == 0 {
			breadthSource.MissingMetrics["advance_decline"]++
		}
		if participation == nil || participation.CoverageVolume == 0 {
			breadthSource.MissingMetrics["constituent_volume"]++
		}
		if row.Breadth != nil {
			if prev := breadthPoints[sessions[i-1].Date]; comparableTapeBreadth(prev, row.Breadth) {
				row.Breadth.Change50PP = new(*row.Breadth.PctAbove50DMA - *prev.PctAbove50DMA)
			}
		}
		contextRows = append(contextRows, row)
		row.Reading = describeMarketTape(contextRows)
		out.Sessions = append(out.Sessions, row)
	}
	out.Sources = []rpc.MarketTapeSource{spxSource, qqqSource, breadthSource}
	available := 0
	for i := range out.Sources {
		source := &out.Sources[i]
		switch {
		case !present[source.Key]:
			source.Status = "unavailable"
		case source.MissingSessions > 0 || source.Status == "partial" || len(source.MissingMetrics) > 0:
			source.Status = "partial"
		default:
			source.Status = "available"
		}
		if source.Status != "unavailable" {
			available++
		}
		if source.Status != "available" {
			out.CoverageStatus = "partial"
		}
	}
	if available == 0 {
		out.CoverageStatus = "unavailable"
	}
	return out, nil
}

func tapePriceInput(key, symbol, secType string, r *rpc.MarketHistoryResult, now time.Time, sessions []marketcal.Session) (map[string]rpc.MarketHistoryPoint, rpc.MarketTapeSource) {
	points := make(map[string]rpc.MarketHistoryPoint)
	source := rpc.MarketTapeSource{Key: key, Status: "unavailable", Source: "IBKR historical TRADES", Detail: "Daily price history unavailable"}
	if r == nil || r.Contract.ConID <= 0 || r.Contract.Symbol != symbol || r.Contract.SecType != secType || !usChartCalendar(r.Contract) || r.TimestampKind != "session_date" || r.Interval != "1 day" || r.PriceBasis != "TRADES" || !r.RegularHoursOnly || r.AsOf.IsZero() || r.AsOf.After(now) {
		return points, source
	}
	source.AsOf, source.Cache = r.AsOf, r.Cache
	source.Source, source.Detail = r.Source, "Original acquisition time; historical publication times unknown"
	if r.Cache != nil && (r.Cache.RefreshFailed || r.Cache.RefreshDue || r.Cache.PreviousWindow || r.Cache.Detail != "") {
		source.Status = "partial"
		source.Detail = "Recorded history; refresh or range coverage incomplete"
	}
	closes := make(map[string]time.Time, len(sessions))
	for _, s := range sessions {
		closes[s.Date] = s.Close
	}
	duplicate := make(map[string]bool)
	for _, point := range r.Points {
		date := point.At.Format("2006-01-02")
		closeAt, ok := closes[date]
		if !ok || r.AsOf.Before(closeAt) || !isFinitePrice(point.Value) {
			continue
		}
		if _, exists := points[date]; exists || duplicate[date] {
			delete(points, date)
			duplicate[date] = true
			continue
		}
		if secType == "IND" || point.Volume != nil && *point.Volume < 0 {
			point.Volume = nil
		}
		points[date] = point
	}
	for date := range points {
		if date > source.CoveredThrough {
			source.CoveredThrough = date
		}
	}
	return points, source
}

func tapePriceRow(points map[string]rpc.MarketHistoryPoint, sessions []marketcal.Session, start, i int, volume bool) *rpc.MarketTapePrice {
	p, ok := points[sessions[i].Date]
	if !ok {
		return nil
	}
	row := &rpc.MarketTapePrice{Close: p.Value}
	if p.Open != nil && p.High != nil && p.Low != nil && validHistoryRange(*p.Open, *p.High, *p.Low, p.Value) {
		row.SessionMove = &rpc.MarketTapeSessionMove{Open: *p.Open, High: *p.High, Low: *p.Low, CloseFromLowPct: 100 * (p.Value / *p.Low - 1)}
		if *p.High > *p.Low {
			row.SessionMove.CloseInRangePct = tapeFinite(100 * (p.Value - *p.Low) / (*p.High - *p.Low))
		}
	}
	if prev, ok := points[sessions[i-1].Date]; ok {
		row.ChangePct = tapeFinite(100 * (p.Value/prev.Value - 1))
		if row.SessionMove != nil {
			row.SessionMove.OpenChangePct = tapeFinite(100 * (row.SessionMove.Open/prev.Value - 1))
			row.SessionMove.LowChangePct = tapeFinite(100 * (row.SessionMove.Low/prev.Value - 1))
		}
	}
	if base, ok := points[sessions[start].Date]; ok {
		row.WindowChangePct = tapeFinite(100 * (p.Value/base.Value - 1))
	}
	if !volume {
		return row
	}
	row.Volume = p.Volume
	if p.Volume == nil || i < 20 {
		return row
	}
	sum := float64(0)
	for j := i - 20; j < i; j++ {
		prior, ok := points[sessions[j].Date]
		if !ok || prior.Volume == nil {
			return row
		}
		sum += float64(*prior.Volume)
	}
	if sum > 0 {
		row.RelativeVolume20 = new(float64(*p.Volume) / (sum / 20))
	}
	return row
}

func tapeFinite(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return new(v)
}

func tapeBreadthInput(r *rpc.BreadthSPXResult, now time.Time, sessions []marketcal.Session) (map[string]*rpc.MarketTapeBreadth, rpc.MarketTapeSource) {
	points := make(map[string]*rpc.MarketTapeBreadth)
	source := rpc.MarketTapeSource{Key: "breadth", Status: "unavailable", Source: "S&P 500 constituent daily bars", Detail: "Breadth history unavailable"}
	if r == nil || r.AsOf.IsZero() || r.AsOf.After(now) || r.State != rpc.BreadthStateReady && r.State != rpc.BreadthStateDegraded {
		return points, source
	}
	source.AsOf, source.Source = r.AsOf, r.Source
	source.Detail = "Separate constituent coverage for each measure; historical first-availability unknown"
	if r.Stale || r.State == rpc.BreadthStateDegraded {
		source.Status, source.Detail = "partial", "Retained breadth history; latest producer coverage is degraded or stale"
	}
	closes := make(map[string]time.Time, len(sessions))
	for _, s := range sessions {
		closes[s.Date] = s.Close
	}
	duplicate := make(map[string]bool)
	for _, p := range r.History {
		closeAt, ok := closes[p.Date]
		if !ok || r.AsOf.Before(closeAt) || p.MemberCount <= 0 {
			continue
		}
		if _, exists := points[p.Date]; exists || duplicate[p.Date] {
			delete(points, p.Date)
			duplicate[p.Date] = true
			continue
		}
		row := &rpc.MarketTapeBreadth{MemberCount: p.MemberCount, Participation: validTapeParticipation(p.Participation, p.MemberCount, now)}
		validPct := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 100 }
		if p.Coverage50 > 0 && p.Coverage50 <= p.MemberCount && validPct(p.PctAbove50DMA) {
			row.Coverage50, row.PctAbove50DMA = p.Coverage50, new(p.PctAbove50DMA)
		}
		if p.Coverage200 > 0 && p.Coverage200 <= p.MemberCount && p.PctAbove200DMA != nil && validPct(*p.PctAbove200DMA) {
			row.Coverage200, row.PctAbove200DMA = p.Coverage200, p.PctAbove200DMA
		}
		if p.CoverageHighsLows > 0 && p.CoverageHighsLows <= p.MemberCount && p.NewHighs != nil && p.NewLows != nil && *p.NewHighs >= 0 && *p.NewLows >= 0 && *p.NewHighs <= p.CoverageHighsLows && *p.NewLows <= p.CoverageHighsLows && *p.NewHighs+*p.NewLows <= p.CoverageHighsLows {
			row.CoverageHighsLows, row.NewHighs, row.NewLows = p.CoverageHighsLows, p.NewHighs, p.NewLows
		}
		if row.PctAbove50DMA == nil && row.PctAbove200DMA == nil && row.NewHighs == nil {
			continue
		}
		points[p.Date] = row
	}
	for date := range points {
		if date > source.CoveredThrough {
			source.CoveredThrough = date
		}
	}
	return points, source
}

func tapeParticipation(p *spx.Participation) *rpc.BreadthParticipation {
	if p == nil {
		return nil
	}
	return &rpc.BreadthParticipation{Companies: tapeCompanies(p.Companies), Method: p.Method, RecordedAt: p.RecordedAt, InputObservedAt: p.InputObservedAt, MembershipID: p.MembershipID, PctAbove20DMA: p.PctAbove20DMA, Coverage20: p.Coverage20, Advancing: p.Advancing, Declining: p.Declining, Unchanged: p.Unchanged, CoverageAD: p.CoverageAD, AdvancePct: p.AdvancePct, AdvancingVolume: p.AdvancingVolume, DecliningVolume: p.DecliningVolume, UnchangedVolume: p.UnchangedVolume, CoverageVolume: p.CoverageVolume, UpVolumePct: p.UpVolumePct}
}

func comparableTapeBreadth(a, b *rpc.MarketTapeBreadth) bool {
	if a == nil || b == nil || a.PctAbove50DMA == nil || b.PctAbove50DMA == nil || a.MemberCount != b.MemberCount || a.Coverage50 != b.Coverage50 {
		return false
	}
	return a.Participation == nil || b.Participation == nil || a.Participation.MembershipID == b.Participation.MembershipID
}

func validTapeParticipation(p *rpc.BreadthParticipation, members int, now time.Time) *rpc.BreadthParticipation {
	if p == nil || p.Method != "constituent-participation-v1" || p.RecordedAt.IsZero() || p.RecordedAt.After(now) || p.InputObservedAt.After(now) || len(p.MembershipID) != 64 {
		return nil
	}
	for _, n := range []int{p.Coverage20, p.CoverageAD, p.CoverageVolume, p.Advancing, p.Declining, p.Unchanged} {
		if n < 0 || n > members {
			return nil
		}
	}
	if p.Advancing+p.Declining+p.Unchanged != p.CoverageAD || p.CoverageVolume > p.CoverageAD {
		return nil
	}
	for _, v := range []*float64{p.PctAbove20DMA, p.AdvancePct, p.UpVolumePct} {
		if v != nil && (math.IsNaN(*v) || math.IsInf(*v, 0) || *v < 0 || *v > 100) {
			return nil
		}
	}
	for _, v := range []float64{p.AdvancingVolume, p.DecliningVolume, p.UnchangedVolume} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return nil
		}
	}
	if (p.Coverage20 > 0) != (p.PctAbove20DMA != nil) || (p.Advancing+p.Declining > 0) != (p.AdvancePct != nil) || (p.AdvancingVolume+p.DecliningVolume > 0) != (p.UpVolumePct != nil) || p.CoverageAD > 0 && p.InputObservedAt.IsZero() {
		return nil
	}
	return p
}
