package daemon

import (
	"math"
	"time"

	"github.com/osauer/canary/v3/internal/breadth/spx"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/scorecard"
)

// scorecardFillQuoteTolerance bounds the time match between a statement
// fill and a retained fill quote on the same contract when no execution id
// matches: the broker's execution time is kept to the second and a receipt
// stands in for it only within a minute, and one order's partial fills on
// one contract rarely spread over more than a few minutes, while two
// distinct decisions on one contract minutes apart are rare in the
// statements. Five minutes covers the former without reaching the latter.
const scorecardFillQuoteTolerance = 5 * time.Minute

// scorecardFillCosts measures the half-spread each ledger decision paid from
// the quotes the daemon retained at its fills: every execution id of every
// decision is looked up first; a decision none of whose ids is retained then
// takes the retained fill on the same contract nearest in time within the
// tolerance, among the records no decision claimed by execution id, so one
// record never counts for two decisions. The statement's execution time is
// the instant the parser placed in the statement's zone, so it meets the
// record's broker execution time directly. A decision without either match
// is absent from the result, and the pure core names the reason from its
// date.
func (s *Server) scorecardFillCosts(ledger []scorecard.Decision) map[string]scorecard.FillCost {
	if s == nil || s.fillQuotes == nil {
		return nil
	}
	out := map[string]scorecard.FillCost{}
	claimed := map[string]bool{}
	var unmatched []scorecard.Decision
	for _, d := range ledger {
		var records []rpc.FillQuoteRecord
		for _, execID := range d.Lineage.ExecutionIDs {
			if rec, ok := s.FillQuoteByExecID(execID); ok {
				records = append(records, rec)
				claimed[rec.ExecID] = true
			}
		}
		if len(records) == 0 {
			unmatched = append(unmatched, d)
			continue
		}
		out[d.ID] = scorecardFillCostFromRecords(records, "exec_id", max(len(d.Lineage.ExecutionIDs), len(records)))
	}
	for _, d := range unmatched {
		rec, ok := s.FillQuoteNear(int(d.ConID), d.ExecutedAt, scorecardFillQuoteTolerance, claimed)
		if !ok {
			continue
		}
		claimed[rec.ExecID] = true
		out[d.ID] = scorecardFillCostFromRecords([]rpc.FillQuoteRecord{rec}, "time", max(len(d.Lineage.ExecutionIDs), 1))
	}
	return out
}

// scorecardFillCostFromRecords sums the measured half-spreads of a
// decision's retained fills. The base sum is nil when any quoted fill lacks
// an FX rate; the basis points are quantity-weighted over the quoted fills;
// the gap is the longest quote delay among them. Without any quoted fill
// the first record's reason stands.
func scorecardFillCostFromRecords(records []rpc.FillQuoteRecord, match string, fills int) scorecard.FillCost {
	out := scorecard.FillCost{Match: match, Fills: fills}
	half, base, weightedBp, quantity := 0.0, 0.0, 0.0, 0.0
	baseOK := true
	var gap *int64
	for _, rec := range records {
		fill := costFillFromRecord(rec)
		if fill.Status != rpc.CostFillMeasured || fill.HalfSpread == nil {
			if out.Reason == "" {
				out.Reason = fill.Reason
			}
			continue
		}
		out.Quoted++
		out.Currency = fill.Currency
		half += *fill.HalfSpread
		if fill.HalfSpreadBase != nil {
			base += *fill.HalfSpreadBase
		} else {
			baseOK = false
		}
		if rec.Quote.Mid != nil && *rec.Quote.Mid > 0 && rec.Quantity > 0 {
			sign := 1.0
			if rec.Side == rpc.OrderActionSell {
				sign = -1
			}
			weightedBp += (rec.Price - *rec.Quote.Mid) * sign / *rec.Quote.Mid * 1e4 * rec.Quantity
			quantity += rec.Quantity
		}
		if rec.Quote.GapMS != nil && (gap == nil || *rec.Quote.GapMS > *gap) {
			gap = new(*rec.Quote.GapMS)
		}
	}
	if out.Quoted == 0 {
		return out
	}
	out.Reason = ""
	out.HalfSpread, out.GapMS = &half, gap
	if baseOK {
		out.HalfSpreadBase = &base
	} else {
		out.Reason = scorecard.ReasonFXUnavailable
	}
	if quantity > 0 && !math.IsNaN(weightedBp) {
		out.Bp = new(weightedBp / quantity)
	}
	return out
}

// scorecardSectorMap names each ledger underlying's GICS sector and sector
// fund from the S&P 500 sector registry (sectorOf, spx.SectorOf in the
// daemon); an underlying off the list, or an index, has none and is
// adjusted on the market alone.
func scorecardSectorMap(ledger []scorecard.Decision, sectorOf func(string) (string, bool)) (funds, names map[int64]string) {
	funds, names = map[int64]string{}, map[int64]string{}
	if sectorOf == nil {
		sectorOf = spx.SectorOf
	}
	for _, d := range ledger {
		if d.UnderlyingConID == 0 || scorecard.IndexUnderlying(d.Underlying) {
			continue
		}
		if _, seen := funds[d.UnderlyingConID]; seen {
			continue
		}
		sector, ok := sectorOf(d.Underlying)
		if !ok {
			continue
		}
		if fund := scorecard.SectorFund(sector); fund != "" {
			funds[d.UnderlyingConID], names[d.UnderlyingConID] = fund, sector
		}
	}
	return funds, names
}
