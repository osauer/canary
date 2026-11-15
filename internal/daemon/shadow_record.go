package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/mandate"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/register"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// The shadow record (internal-docs/design/strategy-register.md, section 3;
// owner decision 2026-10-18 09:01 CEST: "shadow on the production path"). A
// strategy in shadow runs its own verdict code on a paper position, the
// shadow holding, which starts at zero and moves only by the record's own
// simulated fills. Its decisions, intents and fills are append-only events
// under stable keys, so a retry or a second evaluation never writes one
// twice. The only writer is the background refresh after the scheduled
// verdict; no order path reads the record, and nothing in it is a proposal.
// Since the strategy evaluator every register entry is recorded alike,
// under its own scope.

const (
	shadowRecordType    = "shadow.record.v1"
	shadowRecordVersion = 1
	// shadowRecordLimit bounds one strategy's read; a monthly strategy
	// writes about three records a month.
	shadowRecordLimit = 10000
	// shadowServeRecords is how many records a read shows, newest first.
	shadowServeRecords = 12
)

// shadowScope is a strategy's event scope; shadowKey a record's stable key.
// Both carry the shadow prefix, so they never collide with an execution
// journal key.
func shadowScope(id string) string { return "shadow/" + id }

func shadowKey(id, rule, session, kind string) string {
	return "shadow:" + id + ":" + rule + ":" + session + ":" + kind
}

// shadowRecords reads one strategy's records in the order they were written.
func (s *Server) shadowRecords(ctx context.Context, id string) ([]rpc.ShadowRecord, error) {
	if s.coreStore == nil {
		return nil, errors.New("the shadow record is unavailable: no daemon store")
	}
	events, err := s.coreStore.LoadEvents(ctx, corestore.EventQuery{ScopeKey: shadowScope(id), Type: shadowRecordType, Limit: shadowRecordLimit})
	if err != nil {
		return nil, err
	}
	out := make([]rpc.ShadowRecord, 0, len(events))
	for _, ev := range events {
		var r rpc.ShadowRecord
		if err := json.Unmarshal(ev.PayloadJSON, &r); err != nil {
			return nil, fmt.Errorf("shadow record %s cannot be read: %w", ev.EventKey, err)
		}
		out = append(out, r)
	}
	return out, nil
}

// shadowHolding is the paper position: the sum of the record's fills.
func shadowHolding(records []rpc.ShadowRecord) int {
	held := 0
	for _, r := range records {
		if r.Kind == rpc.ShadowKindFill && r.Fill != nil {
			held += r.Fill.Quantity
		}
	}
	return held
}

// shadowRecorded reports whether a record of that kind for that session is
// in the record.
func shadowRecorded(records []rpc.ShadowRecord, kind, session string) bool {
	return slices.ContainsFunc(records, func(r rpc.ShadowRecord) bool { return r.Kind == kind && r.Session == session })
}

// appendShadowRecord appends one record once under its stable key; a record
// already there is left as it is.
func (s *Server) appendShadowRecord(ctx context.Context, r rpc.ShadowRecord) error {
	key := shadowKey(r.Strategy, r.Rule, r.Session, r.Kind)
	if _, ok, err := s.coreStore.GetEvent(ctx, shadowScope(r.Strategy), key); err != nil || ok {
		return err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.coreStore.AppendEvents(ctx, []corestore.EventInput{{ScopeKey: shadowScope(r.Strategy), EventKey: key, Type: shadowRecordType, Action: r.Kind,
		Origin: coreEventOriginDaemon, OccurredAt: r.RecordedAt, PayloadJSON: raw}})
	return err
}

// recordStrategyShadow writes a strategy's shadow records that are due while
// it runs in shadow: the month's decision and intent once the month is
// frozen, a hold when the decision was measured but not frozen, and the
// intent's simulated fill once its session's close is in the strategy's
// series. res is the strategy's verdict of this refresh.
func (s *Server) recordStrategyShadow(ctx context.Context, res *rpc.StrategyTargetResult, r register.Resolved, now time.Time) {
	if s.coreStore == nil || res == nil || !res.Active || r.State != register.StateShadow {
		return
	}
	e := r.Entry
	records, err := s.shadowRecords(ctx, e.ID)
	if err != nil {
		s.logShadowFailure(err)
		return
	}
	base := rpc.ShadowRecord{Version: shadowRecordVersion, Strategy: e.ID, Rule: res.Rule, SpecHash: res.SpecHash, SpecCited: e.SpecHash != "",
		Stage: string(r.State), RecordedAt: now}
	if res.PolicyFingerprint != nil {
		base.PolicyFingerprint = res.PolicyFingerprint.Key
	}
	contract, _, _, _ := s.instrumentPinState(s.requestedInstrument(r))
	var series *mandateHistorySeries
	if contract != nil {
		series = s.mandateHistorySeries(ctx, seriesKeyFor(r, contract.ConID))
	}

	records = s.recordShadowDecision(ctx, res, r, base, records, contract, now)
	// The fills due: an intent whose session's close is in.
	for _, intent := range slices.Clone(records) {
		if intent.Kind != rpc.ShadowKindIntent || intent.Intent == nil || intent.Intent.Quantity == 0 || shadowRecorded(records, rpc.ShadowKindFill, intent.Intent.ForSession) || series == nil {
			continue
		}
		fill, due := coreShadowFill(intent, series, shadowHolding(records))
		if !due {
			continue
		}
		rec := base
		rec.Kind, rec.Session, rec.Fill = rpc.ShadowKindFill, intent.Intent.ForSession, fill
		if err := s.appendShadowRecord(ctx, rec); err != nil {
			s.logShadowFailure(err)
			return
		}
		records = append(records, rec)
	}
}

// requestedInstrument is the instrument a resolved entry is resolved by:
// the core's as the policy requests it, a later entry's as resolved.
func (s *Server) requestedInstrument(r register.Resolved) register.Instrument {
	if r.Entry.Form == risk.StrategyFormCore {
		if c := s.riskPolicies.snapshot().policy; c != nil && c.Mandate != nil {
			return coreInstrument(c.Mandate.Core)
		}
	}
	return r.Instrument
}

// recordShadowDecision writes the month's decision and intent once the
// month is frozen, or its hold when the decision was measured and not
// frozen, and returns the records with what it wrote.
func (s *Server) recordShadowDecision(ctx context.Context, res *rpc.StrategyTargetResult, r register.Resolved, base rpc.ShadowRecord, records []rpc.ShadowRecord, contract *coreContract, now time.Time) []rpc.ShadowRecord {
	d := res.Decision
	if d == nil || d.Decision == "" || shadowRecorded(records, rpc.ShadowKindDecision, d.Decision) {
		return records
	}
	if d.FrozenAt.IsZero() || d.Drift || res.Sizing == nil || len(res.Reasons) > 0 {
		if !shadowRecorded(records, rpc.ShadowKindHold, d.Decision) {
			rec := base
			rec.Kind, rec.Session, rec.Status, rec.Reasons = rpc.ShadowKindHold, d.Decision, rpc.CoreStatusHold, slices.Clone(res.Reasons)
			if len(rec.Reasons) == 0 {
				rec.Reasons = []string{"the month's decision is not frozen yet"}
			}
			if err := s.appendShadowRecord(ctx, rec); err != nil {
				s.logShadowFailure(err)
				return records
			}
			records = append(records, rec)
		}
		return records
	}
	if contract == nil {
		return records
	}
	held := shadowHolding(records)
	decision := base
	decision.Kind, decision.Session, decision.Status = rpc.ShadowKindDecision, d.Decision, rpc.CoreStatusReady
	decision.Decision = &rpc.ShadowDecision{TrendOn: d.TrendOn, Sigma: d.Sigma, Binding: d.Binding, TargetShares: res.Sizing.Target, ScalePct: r.ScalePct, Digest: d.Digest,
		Close: d.Close, SleeveBudget: res.Sizing.Current.SleeveBudget}
	intent := base
	intent.Kind, intent.Session = rpc.ShadowKindIntent, d.Decision
	action := s.shadowDecide(res, r, contract, held)
	forSession := coreNextSession(d.Decision, contract.Market, now)
	var terms *rpc.CoreRowTerms
	if r.Entry.Form == risk.StrategyFormCore {
		// The core's session and order terms come from its live row's
		// builder, so the shadow intent carries what its row would
		// (core_rebalance.go); an entry's rows are not built yet.
		t := coreRowTerms(contract, d.Decision, s.currentBrokerStateScope(), now)
		terms = &t
		if len(t.Window) > 0 {
			forSession = t.Window[0]
		}
	}
	intent.Intent = &rpc.ShadowIntent{ForSession: forSession, Kind: action.Kind, Why: action.Why, Diff: action.Diff, BandShares: action.BandShares, HoldingBefore: held}
	switch action.Kind {
	case core.ActionBuy:
		intent.Intent.Quantity, intent.Intent.Terms = action.Quantity, terms
	case core.ActionSell, core.ActionSellAll:
		intent.Intent.Quantity, intent.Intent.Terms = -action.Quantity, terms
	}
	for _, rec := range []rpc.ShadowRecord{decision, intent} {
		if err := s.appendShadowRecord(ctx, rec); err != nil {
			s.logShadowFailure(err)
			return records
		}
		records = append(records, rec)
	}
	return records
}

// shadowDecide is core.Decide on the shadow holding, with the buy cut to
// the headroom a live strategy would have: the book as held with the
// strategy's paper position in its sleeve, and its allocation counted as if
// reserved at its written scale. The kept verdict is read, never changed;
// the hypothetical book is a copy that is never kept.
func (s *Server) shadowDecide(res *rpc.StrategyTargetResult, r register.Resolved, k *coreContract, held int) core.Decision {
	e := r.Entry
	isCore := e.Form == risk.StrategyFormCore
	price := 0.0
	if res.Holding != nil {
		price = res.Holding.Price
	}
	in := core.DecideInput{Target: res.Sizing.Target, TrendOn: res.Decision.TrendOn, Held: held, Price: price, BandPct: r.Terms.BandPct, MinOrderBase: r.Terms.MinOrderBase, Holdable: held}
	if !isCore {
		in.Symbol, in.Name = k.Symbol, strategyName(e)
	}
	result, kept, _, _ := s.currentMandateVerdict()
	var verdict *mandate.Verdict
	if result != nil {
		verdict = result.Verdict
	}
	// The paper buy is judged by the entry's sleeve as the live row's and
	// the gate's are (mandateVerdictHolds), so the record reads what the
	// entry would have done live.
	why, overBudget := mandateVerdictHolds(verdict, e.Sleeve, verdictCurrency(verdict))
	switch {
	case why != "":
		in.BuyBlocked = why
	case kept == nil:
		in.BuyBlocked = "no mandate verdict is kept"
	default:
		if want := res.Sizing.Target - held; want > 0 && price > 0 {
			var reserved []int
			if isCore {
				reserved = s.mandateCoreContracts(context.Background(), s.riskPolicies.snapshot().policy)
			}
			book := shadowBook(kept, k, e.Sleeve, reserved, held, price, r.ScalePct)
			if !isCore {
				book = s.withStrategySeries(book, r, k)
			}
			in.Holdable = held + core.Headroom(want, func(q int) bool { return strategyHeadroomFits(book, k, e.Sleeve, q, price, verdict, overBudget) })
		}
	}
	return core.Decide(in)
}

// withStrategySeries is a copy of a book that measures an entry's fund on
// its own closes, the strategy's series, where the book keeps none (the fund
// is not held, so the mandate's history pass never read it): the hypothetical
// book of the shadow record and the live entry's headroom read it alike.
func (s *Server) withStrategySeries(in *mandate.Input, r register.Resolved, k *coreContract) *mandate.Input {
	if in == nil || k == nil {
		return in
	}
	if kept, ok := in.Series[k.Symbol]; ok && len(kept.Closes) >= mandate.Closes && !kept.Stale {
		return in
	}
	own, _ := s.strategyRiskSeries(context.Background(), r, k, s.nowUTC())
	if own == nil {
		return in
	}
	book := *in
	book.Series = maps.Clone(in.Series)
	if book.Series == nil {
		book.Series = map[string]mandate.Series{}
	}
	book.Series[k.Symbol] = mandateSeriesInput(own, s.nowUTC())
	return &book
}

// shadowBook is the book a live strategy would be judged on: a copy of the
// kept book with the strategy's paper position in its sleeve, and its
// allocation counted as if reserved at scalePct. The account's units of the
// strategy's reserved contracts (the core's fund: every unit is the core's,
// so real units would otherwise count twice) are replaced by the paper
// position; a strategy that reserves nothing keeps the owner's real units of
// the fund beside its own leg, placed in its sleeve.
func shadowBook(in *mandate.Input, k *coreContract, sleeve string, reserved []int, held int, price, scalePct float64) *mandate.Input {
	book := *in
	book.Legs = nil
	template := mandate.Leg{ID: mandateConLegID(k.ConID), Symbol: k.Symbol, Underlying: k.Symbol, Kind: mandate.KindStock, Multiplier: 1, Currency: k.Currency, Spot: price, ISIN: k.ISIN}
	ids := map[string]bool{}
	for _, id := range reserved {
		ids[mandateConLegID(id)] = true
	}
	placed := ""
	for _, l := range in.Legs {
		if l.ID == template.ID {
			template.Symbol, template.Underlying = l.Symbol, l.Underlying
			if l.Spot > 0 {
				template.Spot = l.Spot
			}
		}
		if ids[l.ID] || l.ID == template.ID && len(reserved) > 0 {
			continue
		}
		book.Legs = append(book.Legs, l)
	}
	if len(reserved) == 0 {
		// Not reserved: the paper position is a leg of its own in the
		// strategy's sleeve, beside the owner's real units.
		template.ID = strategyLegID(sleeve, k.ConID)
		placed = sleeve
	}
	if held > 0 {
		template.Quantity = float64(held)
		book.Legs = append(book.Legs, template)
	}
	if plan := in.Allocations; plan != nil {
		copied := &mandate.AllocationPlan{Placed: plan.Placed, Machine: slices.Clone(plan.Machine)}
		for i := range copied.Machine {
			if copied.Machine[i].Sleeve == sleeve {
				copied.Machine[i].StagePct = scalePct
			}
		}
		if placed != "" {
			copied = copied.WithPlaced(template.ID, placed)
		}
		book.Allocations = copied
	}
	return &book
}

// coreShadowFill simulates an intent's fill by the lab's R2 convention: the
// whole quantity at its session's close. Due once the series reaches the
// session; a series past it without its bar records the fill unobserved.
func coreShadowFill(intent rpc.ShadowRecord, series *mandateHistorySeries, held int) (*rpc.ShadowFill, bool) {
	session := intent.Intent.ForSession
	if session == "" || len(series.Dates) == 0 || series.Dates[len(series.Dates)-1] < session {
		return nil, false
	}
	fill := &rpc.ShadowFill{Convention: rpc.ShadowFillConvention, HoldingAfter: held}
	i := slices.Index(series.Dates, session)
	if i < 0 {
		fill.Unobserved = "no close of " + session + " in the series; nothing is guessed"
		return fill, true
	}
	fill.Quantity, fill.Price = intent.Intent.Quantity, series.Closes[i]
	fill.HoldingAfter = held + fill.Quantity
	return fill, true
}

// coreNextSession is the first session after a date on the core's exchange
// calendar, or empty past its coverage.
func coreNextSession(date string, market marketcal.Market, now time.Time) string {
	day, err := time.Parse(time.DateOnly, date)
	if err != nil {
		return ""
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	for range 14 {
		day = day.AddDate(0, 0, 1)
		res, err := cal.Query(marketcal.Query{Market: market, Date: day.Format(time.DateOnly), Days: 1})
		if err != nil {
			return ""
		}
		if res.Session.State == marketcal.StateRegular || res.Session.State == marketcal.StateEarlyClose {
			return day.Format(time.DateOnly)
		}
	}
	return ""
}

// shadowView is a strategy's shadow record as a read serves it: the paper
// position and the newest records. Nil while the strategy is not in shadow
// and has no record.
func (s *Server) shadowView(ctx context.Context, id string, state register.State) *rpc.ShadowView {
	if s.coreStore == nil {
		return nil
	}
	records, err := s.shadowRecords(ctx, id)
	if err != nil {
		return &rpc.ShadowView{Strategy: id, Stage: string(state), Unavailable: err.Error()}
	}
	if state != register.StateShadow && len(records) == 0 {
		return nil
	}
	view := &rpc.ShadowView{Strategy: id, Stage: string(state), Holding: shadowHolding(records), Records: len(records)}
	for i := len(records) - 1; i >= 0 && len(view.Recent) < shadowServeRecords; i-- {
		view.Recent = append(view.Recent, records[i])
	}
	return view
}

// killMark is the current reading, marked by the kill test when its
// session is not yet a recorded decision.
type killMark struct {
	session      string
	close        float64
	sleeveBudget float64
	// scalePct is the stage's scale the reading is sized at: the bound is
	// measured against what the stage invests (stageCapital); units is the
	// line's holding, the core's sleeve at this reading (coreSleeveFills).
	scalePct float64
	units    float64
}

// strategyKill is the kill rule's drawdown test on the shadow record
// (strategy-evaluator.md §3): the paper sleeve's equity, cash and the paper
// position marked at each decision's close (and at the current reading),
// against its peak, in units of sleeve capital K = S / VolFloor at that
// decision. Nil without a store; unmeasured without a decision that carries
// a close.
func (s *Server) strategyKill(ctx context.Context, r register.Resolved, mark killMark) *rpc.KillView {
	if s.coreStore == nil {
		return nil
	}
	records, err := s.shadowRecords(ctx, r.Entry.ID)
	if err != nil {
		return nil
	}
	kill := &rpc.KillView{MaxDrawdownK: r.KillDrawdownK, ScalePct: r.ScalePct}
	cash, held, peak := 0.0, 0, 0.0
	// The paper position is sized at the stage's scale, so the drawdown is
	// in units of the stage's capital (stageCapital), as the live measure's.
	at := func(close, sleeveBudget, scalePct float64) {
		k := stageCapital(sleeveBudget, nonZero(scalePct, r.ScalePct))
		if k <= 0 {
			return
		}
		equity := cash + float64(held)*close
		peak = max(peak, equity)
		if dd := (peak - equity) / k; dd > kill.DrawdownK {
			kill.DrawdownK = dd
		}
		kill.Measured, kill.Decisions = true, kill.Decisions+1
	}
	marked := false
	for _, rec := range records {
		switch {
		case rec.Kind == rpc.ShadowKindFill && rec.Fill != nil:
			cash -= float64(rec.Fill.Quantity) * rec.Fill.Price
			held += rec.Fill.Quantity
		case rec.Kind == rpc.ShadowKindDecision && rec.Decision != nil && rec.Decision.Close > 0 && rec.Decision.SleeveBudget > 0:
			at(rec.Decision.Close, rec.Decision.SleeveBudget, rec.Decision.ScalePct)
			marked = marked || rec.Session == mark.session
		}
	}
	if !marked && mark.session != "" && mark.close > 0 && mark.sleeveBudget > 0 {
		at(mark.close, mark.sleeveBudget, mark.scalePct)
	}
	switch {
	case kill.MaxDrawdownK == nil:
		kill.Note = killUnwrittenNote
	case kill.Measured && kill.DrawdownK > *kill.MaxDrawdownK:
		kill.Breached = true
	case !kill.Measured:
		kill.Note = "no decision marked yet: the drawdown is measured on the shadow record from its first decision"
	}
	return kill
}

func (s *Server) logShadowFailure(err error) {
	if s.logger != nil {
		s.logger.Warnf("shadow record: %v; the next refresh tries again under the same keys", err)
	}
}

// shadowDue reports whether a strategy in shadow has a record to write that
// a frozen month would not prompt: the latest month's decision not yet
// recorded, or an intent whose fill is not in.
func (s *Server) shadowDue(ctx context.Context, r register.Resolved, latest string) bool {
	if s.coreStore == nil || r.State != register.StateShadow {
		return false
	}
	records, err := s.shadowRecords(ctx, r.Entry.ID)
	if err != nil {
		return false
	}
	if !shadowRecorded(records, rpc.ShadowKindDecision, latest) {
		return true
	}
	for _, rec := range records {
		if rec.Kind == rpc.ShadowKindIntent && rec.Intent != nil && rec.Intent.Quantity != 0 && !shadowRecorded(records, rpc.ShadowKindFill, rec.Intent.ForSession) {
			return true
		}
	}
	return false
}
