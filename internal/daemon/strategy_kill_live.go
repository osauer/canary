package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The kill rule's drawdown test for a live entry (strategy-evaluator.md §3:
// "the live measurement (the sleeve's own fills) comes with the first rule
// that sends"; built 2026-10-19). A live entry keeps no shadow record, so
// its drawdown is measured on its own fills: at each decision close the
// sleeve's equity is the cash its own fills moved plus the units they left
// times the decision close, kept once per decision and scope in the
// daemon's event log (scope strategy/<id>/<account>|<mode>|<conid>, never
// read by an order path; liveMarkBinding). The drawdown is
// the fall from the running peak in units of the stage's capital, K at the
// stage (stageCapital): S / 0.15 times the stage's scale, S the sleeve's
// risk allocation in money at that decision, as on the shadow record. Units
// the owner sells by hand leave the measure alone: it reads the entry's own
// trading, not the line's holding. The core is the exception (refuter,
// 2026-10-19): every unit of its line is the core's whoever bought it
// (placement), and its shadow record is not written while it is live, so
// its sleeve is the line's holding, marked at each decision close, each
// change of holding taken at the previous mark's close (the decision close
// the rule acted on, the paper convention) and the first mark adopting the
// holding at its own close (coreSleeveFills): between decisions the
// sleeve's P&L is the holding times the move of the close, s0's test (a)
// on the sleeve. Marks that cannot be read hold the entry: a kill rule
// that cannot be measured is no kill rule (refuter round 3).

const (
	liveMarkType  = "strategy.mark.v2"
	liveMarkLimit = 10000
	// legacyLiveMarkType is the mark v3.25.4 and earlier kept under scope
	// strategy/<id>, naming no account, mode or line. Those rows stay in the
	// event log for audit and are never read as a measure (liveMarkBinding).
	legacyLiveMarkType = "strategy.mark.v1"
)

// errLiveMarkScopeUnresolved is the kill test's answer while the account or
// its paper/live mode is not resolved: a measure without its account is no
// measure, so the entry holds (fail closed).
var errLiveMarkScopeUnresolved = errors.New("the account and its paper or live mode are not resolved")

// killUnwrittenNote is the verdict's note while no bound is written; a live
// entry holds on it (strategy_target.go), a shadow entry sends nothing.
const killUnwrittenNote = "kill_drawdown_k unwritten: the kill rule cannot fire until the lab's design run writes its bound"

// liveMark is one decision's mark of a live entry's own sleeve.
type liveMark struct {
	Version  int    `json:"version"`
	Strategy string `json:"strategy"`
	// Account, Mode and ConID are the scope the mark's evidence belongs to
	// (liveMarkBinding); a mark is read only under its own.
	Account      string  `json:"account"`
	Mode         string  `json:"mode"`
	ConID        int     `json:"con_id"`
	Session      string  `json:"session"`
	Close        float64 `json:"close"`
	Units        float64 `json:"units"`
	Cash         float64 `json:"cash"`
	SleeveBudget float64 `json:"sleeve_budget"`
	// ScalePct is the stage's scale at the decision, the capital the bound
	// is measured against; a mark written before it was kept (v3.25.1 and
	// earlier) reads the entry's current scale.
	ScalePct   float64   `json:"scale_pct,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// stageCapital is K at the stage: the sleeve's risk allocation S over the
// volatility floor (the notional of a fully invested sleeve, core-sleeve.md
// §1), times the stage's scale, since the stage trades scale_pct of that
// notional (core.SizeTarget). The lab writes its bound in units of a fully
// invested sleeve ("0.35 K is a 35% fall of a fully invested sleeve"), so
// it is measured against what the stage invests: against the full
// allocation a stage of 25 does not trade, a 0.35 K bound was a loss of
// 140% of the invested amount and could not fire in time (review,
// 2026-10-19). Zero while nothing is invested, and the reading is skipped.
func stageCapital(sleeveBudget, scalePct float64) float64 {
	if !(sleeveBudget > 0) || !(scalePct > 0) {
		return 0
	}
	return sleeveBudget * scalePct / 100 / core.VolFloor
}

// coreSleeveFills is the core's sleeve as its marks read it: the line's
// holding as its units, with each change of holding since the previous
// mark taken at that mark's close (the decision close the rule acted on:
// the row's units carry the month that follows, and units the rule sold
// carry none of it) and the first mark adopting the holding at its own
// close, so the sleeve's equity starts at zero as a paper sleeve's does and
// moves with the holding times the close between decisions. A hand trade
// between decisions is measured as if done at the previous decision close.
func coreSleeveFills(prev *liveMark, units, close float64) strategyFills {
	cash := -units * close
	if prev != nil {
		cash = prev.Cash - (units-prev.Units)*nonZero(prev.Close, close)
	}
	return strategyFills{Units: units, Cash: cash, ByRef: map[string]float64{}}
}

// lastLiveMark is the last mark kept, nil without one.
func lastLiveMark(marks []liveMark) *liveMark {
	if len(marks) == 0 {
		return nil
	}
	return &marks[len(marks)-1]
}

// nonZero is v, or fallback while v is not positive.
func nonZero(v, fallback float64) float64 {
	if v > 0 {
		return v
	}
	return fallback
}

// liveMarkBinding is the identity a live entry's marks belong to: the
// strategy, the account with its paper or live mode, and the line (the
// contract id) its own fills trade. The kill test reads only the marks of
// the scope it measures, so another account's, the other mode's or another
// line's mark of the same month never counts as this one's (the v3.25.4
// review's finding 1: a mark keyed by strategy and month alone let account
// A's month suppress account B's drawdown). The binding is the frozen
// month's (coreScope): one account, one mode, one contract.
type liveMarkBinding struct {
	kind     broker.Kind
	strategy string
	account  string
	mode     string
	conID    int
}

// liveMarkBindingFor binds a strategy's marks to scope and its line, or
// fails while the account, its mode or the line is not resolved.
func liveMarkBindingFor(id string, scope brokerStateScope, conID int) (liveMarkBinding, error) {
	if !brokerScopeConcrete(scope) {
		return liveMarkBinding{}, errLiveMarkScopeUnresolved
	}
	if id == "" || conID <= 0 {
		return liveMarkBinding{}, errors.New("the strategy's line is not resolved")
	}
	return liveMarkBinding{kind: scope.Kind, strategy: id, account: strings.ToUpper(strings.TrimSpace(scope.Account)), mode: scope.Mode, conID: conID}, nil
}

// scopeKey is the event-log scope the binding's marks are kept under.
func (b liveMarkBinding) scopeKey() string {
	return "strategy/" + b.strategy + "/" + coreScope(brokerStateScope{Kind: b.kind, Account: b.account, Mode: b.mode}, b.conID)
}

// holds reports whether a decoded mark names this binding.
func (b liveMarkBinding) holds(m liveMark) bool {
	return m.Strategy == b.strategy && strings.EqualFold(strings.TrimSpace(m.Account), b.account) && m.Mode == b.mode && m.ConID == b.conID
}

// legacyLiveMarkScope is where v3.25.4 and earlier kept a strategy's
// marks, for every account alike. Nothing reads it as a measure.
func legacyLiveMarkScope(id string) string { return "strategy/" + id }

func liveMarkKey(id, session string) string { return "strategy:" + id + ":" + session + ":mark" }

// liveMarks reads the binding's marks in the order they were written.
func (s *Server) liveMarks(ctx context.Context, b liveMarkBinding) ([]liveMark, error) {
	if s.coreStore == nil {
		return nil, errors.New("no daemon store")
	}
	events, err := s.coreStore.LoadEvents(ctx, corestore.EventQuery{ScopeKey: b.scopeKey(), Type: liveMarkType, Limit: liveMarkLimit})
	if err != nil {
		return nil, err
	}
	out := make([]liveMark, 0, len(events))
	for _, ev := range events {
		var m liveMark
		if err := json.Unmarshal(ev.PayloadJSON, &m); err != nil {
			// Named by its key, which the owner can find; the decoder's
			// words name Go types and stay out of the hold.
			return nil, fmt.Errorf("the mark %s is not a mark", ev.EventKey)
		}
		if !b.holds(m) {
			// Kept under this scope but naming another: never counted.
			return nil, fmt.Errorf("the mark %s names another account, mode or line than the one it is kept under", ev.EventKey)
		}
		out = append(out, m)
	}
	return out, nil
}

// recordLiveMark keeps a live entry's mark of its frozen month's decision,
// once per scope: the background refresh is its only writer, and the event
// log's key keeps it unique under retries. scope is the account the result
// was evaluated under; a mark is not kept once the account has changed.
func (s *Server) recordLiveMark(ctx context.Context, res *rpc.StrategyTargetResult, r register.Resolved, scope brokerStateScope, now time.Time) {
	if s.coreStore == nil || res == nil || r.State != register.StateLive || r.Entry.ID == "" || res.Decision == nil || res.Decision.FrozenAt.IsZero() || res.Decision.Drift ||
		res.Sizing == nil || !(res.Decision.LineClose() > 0) || !(res.Sizing.Current.SleeveBudget > 0) || res.Instrument == nil || res.Instrument.ConID <= 0 {
		return
	}
	b, err := liveMarkBindingFor(r.Entry.ID, scope, res.Instrument.ConID)
	if err != nil || !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return
	}
	key := liveMarkKey(r.Entry.ID, res.Decision.Decision)
	if _, ok, err := s.coreStore.GetEvent(ctx, b.scopeKey(), key); err != nil || ok {
		return
	}
	var own strategyFills
	switch {
	case s.liveMarkFillsForTest != nil:
		own = s.liveMarkFillsForTest()
	case r.Entry.Form == risk.StrategyFormCore:
		// The core's sleeve is its holding (coreSleeveFills).
		if res.Holding == nil {
			return
		}
		marks, err := s.liveMarks(ctx, b)
		if err != nil {
			return
		}
		own = coreSleeveFills(lastLiveMark(marks), float64(res.Holding.Shares), res.Decision.LineClose())
	default:
		own = s.strategyOwnFills(ctx, r.Entry.ID, res.Instrument.ConID)
		if own.Unread {
			return
		}
	}
	if !sameBrokerScope(scope, s.currentBrokerStateScope()) {
		return // the fills were read under another account
	}
	m := liveMark{Version: 2, Strategy: r.Entry.ID, Account: b.account, Mode: b.mode, ConID: b.conID, Session: res.Decision.Decision, Close: res.Decision.LineClose(), Units: own.Units, Cash: own.Cash,
		SleeveBudget: res.Sizing.Current.SleeveBudget, ScalePct: r.ScalePct, RecordedAt: now}
	raw, err := json.Marshal(m)
	if err != nil {
		return
	}
	if _, err := s.coreStore.AppendEvents(ctx, []corestore.EventInput{{ScopeKey: b.scopeKey(), EventKey: key, Type: liveMarkType, Action: "mark",
		Origin: coreEventOriginDaemon, OccurredAt: now, PayloadJSON: raw}}); err != nil {
		s.warnf("strategy %s mark %s: %v; the next refresh tries again under the same key", r.Entry.ID, m.Session, err)
	}
}

// strategyKillLive is the kill test of a live entry on its own fills: the
// kept marks, and this reading marked at the decision close when it is not
// kept yet. Past the bound the entry holds before the month can be frozen.
func (s *Server) strategyKillLive(ctx context.Context, r register.Resolved, mark killMark, conID int) (*rpc.KillView, error) {
	if s.coreStore == nil {
		return nil, nil
	}
	// The marks and the fills are the current account's, on this line; an
	// unresolved account measures nothing and holds (fail closed).
	scope := s.currentBrokerStateScope()
	b, err := liveMarkBindingFor(r.Entry.ID, scope, conID)
	if err != nil {
		return nil, err
	}
	marks, err := s.liveMarks(ctx, b)
	if err != nil {
		return nil, err
	}
	kill := &rpc.KillView{MaxDrawdownK: r.KillDrawdownK, Source: rpc.KillSourceOwnFills, ScalePct: r.ScalePct}
	// The sleeve's own P&L starts at zero, as the paper position does; the
	// drawdown is in units of the stage's capital (stageCapital).
	peak := 0.0
	at := func(equity, sleeveBudget, scalePct float64) {
		k := stageCapital(sleeveBudget, nonZero(scalePct, r.ScalePct))
		if k <= 0 {
			return
		}
		peak = max(peak, equity)
		if dd := (peak - equity) / k; dd > kill.DrawdownK {
			kill.DrawdownK = dd
		}
		kill.Measured, kill.Decisions = true, kill.Decisions+1
	}
	marked := false
	for _, m := range marks {
		if m.Close > 0 && m.SleeveBudget > 0 {
			at(m.Cash+m.Units*m.Close, m.SleeveBudget, m.ScalePct)
			marked = marked || m.Session == mark.session
		}
	}
	if !marked && mark.session != "" && mark.close > 0 && mark.sleeveBudget > 0 {
		var own strategyFills
		if r.Entry.Form == risk.StrategyFormCore {
			own = coreSleeveFills(lastLiveMark(marks), mark.units, mark.close)
		} else {
			own = s.strategyOwnFills(ctx, r.Entry.ID, conID)
		}
		at(own.Cash+own.Units*mark.close, mark.sleeveBudget, mark.scalePct)
	}
	switch {
	case kill.MaxDrawdownK == nil:
		kill.Note = killUnwrittenNote
	case kill.Measured && kill.DrawdownK > *kill.MaxDrawdownK:
		kill.Breached = true
	case !kill.Measured:
		kill.Note = "no decision marked yet: the drawdown is measured on its own fills from its first decision live"
	}
	return kill, nil
}
