package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/core"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/register"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The strategy evaluator (internal-docs/design/strategy-evaluator.md): one
// verdict per entry of the strategy register, in the shape the core sleeve's
// target has had since phase 1 (core-sleeve.md). The daemon keeps the
// entry's instrument's own closes (the line IBKR lists under its ISIN and
// currency, core_resolve.go, read by contract id on its primary exchange,
// judged fresh on that exchange's calendar, and never read by the book's
// verdict), decides each month at the exchange's last session's close with
// internal/core under the entry's trend filter, freezes the month's target
// with the digest of its inputs, sizes it on the mandate's capital against
// the entry's sleeve's risk allocation, cuts a buy to the mandate's headroom
// (the book at most at 0.95 of the budget, the strategy's gap at most at
// 0.95 of its cap, the sleeve inside its allocation) and serves what the
// rule would do. The core is the first instance; every entry runs through
// the same code. In shadow that is all (read-only); live, each entry's
// month's action is served as one proposal row the owner confirms on a
// device or Desk's controller sends under the standing mandate
// (core_rebalance.go, core-sleeve.md phases 2 and 3). The daemon itself
// sends no order.

const (
	coreSeriesKeyPrefix     = "core:con:"
	strategySeriesKeyPrefix = "strategy:"
	stateKindCoreTarget     = "core_target"
	coreTargetDocVer        = 1
	// coreDecisionAfter is how long after a decision day's scheduled close
	// its close counts as in: the closing auction's end with margin (18:00
	// Berlin on a regular Xetra or London day).
	coreDecisionAfter = 30 * time.Minute
)

// coreSeriesKey keys the core's series by its contract id.
func coreSeriesKey(conID int) string { return fmt.Sprintf("%s%d", coreSeriesKeyPrefix, conID) }

// strategySeriesKey keys a later entry's series by its id and contract: its
// own namespace, never a book key, so it never enters the book's verdict
// (strategy-register.md, section 4).
func strategySeriesKey(id string, conID int) string {
	return fmt.Sprintf("%s%s:con:%d", strategySeriesKeyPrefix, id, conID)
}

// seriesKeyFor keys a resolved entry's series: the core's as before the
// evaluator, every other entry's under its own namespace.
func seriesKeyFor(r register.Resolved, conID int) string {
	if r.Entry.Form == risk.StrategyFormCore {
		return coreSeriesKey(conID)
	}
	return strategySeriesKey(r.Entry.ID, conID)
}

// coreSeriesNeed is the core's series need: the contract IBKR lists under
// the ISIN, asked by its id with IBKR's symbol on its primary exchange,
// never SMART, and judged fresh on that exchange's calendar.
func coreSeriesNeed(k *coreContract) mandateHistoryNeed {
	return instrumentSeriesNeed(coreSeriesKey(k.ConID), k)
}

// instrumentSeriesNeed is a strategy instrument's series need under a key.
func instrumentSeriesNeed(key string, k *coreContract) mandateHistoryNeed {
	return mandateHistoryNeed{Key: key, Market: k.Market, Contract: rpc.ContractParams{ConID: k.ConID, Symbol: k.Symbol, SecType: "STK",
		Currency: k.Currency, Exchange: k.PrimaryExch, PrimaryExch: k.PrimaryExch}}
}

// coreFrozenTarget is one month's decision as first made: the signal, the
// target and the capital it was sized on. Persisted per account and
// contract; a later reading of the same window that differs holds.
type coreFrozenTarget struct {
	Version  int         `json:"version"`
	Scope    string      `json:"scope"`
	Decision string      `json:"decision"`
	Digest   string      `json:"digest"`
	FrozenAt time.Time   `json:"frozen_at"`
	Signal   core.Signal `json:"signal"`
	On       core.Sizing `json:"on"`
	Size     core.Size   `json:"size"`
	// Row is the month's decision as recorded while the core was live
	// (core_rebalance.go); nil while the core decided the month in shadow.
	Row *coreDecisionRow `json:"row,omitempty"`
	// Measure is "signal" when the month was measured on the entry's signal
	// series (strategy_signal.go): the month keeps that series until its
	// next decision, so a hand-over never decides one month twice.
	Measure string `json:"measure,omitempty"`
}

type coreTargetStore struct {
	mu        sync.Mutex
	frozen    map[string]*coreFrozenTarget
	revisions map[string]int64
	loaded    map[string]bool
	// pins are the last resolutions of each strategy instrument's request,
	// by request key, and used the contract each last used per ISIN and
	// currency (core_resolve.go).
	pins          map[string]corePin
	used          map[string]*coreContract
	usedRevisions map[string]int64
	usedLoaded    map[string]bool
}

func coreTargetScope(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return "core/target/" + hex.EncodeToString(sum[:])
}

func (s *Server) coreFrozen(ctx context.Context, scope string) *coreFrozenTarget {
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.frozen == nil {
		st.frozen, st.revisions, st.loaded = map[string]*coreFrozenTarget{}, map[string]int64{}, map[string]bool{}
	}
	if !st.loaded[scope] {
		st.loaded[scope] = true
		if s.coreStore != nil {
			doc, ok, err := s.coreStore.GetStateDocument(ctx, coreTargetScope(scope), stateKindCoreTarget)
			if err == nil && ok {
				var saved coreFrozenTarget
				if json.Unmarshal(doc.JSON, &saved) == nil && saved.Version == coreTargetDocVer && saved.Scope == scope {
					st.frozen[scope] = &saved
					st.revisions[scope] = doc.Revision
				}
			}
		}
	}
	return st.frozen[scope]
}

// freezeCore keeps a month's decision; a failed write keeps it in memory
// for this process and says so.
func (s *Server) freezeCore(ctx context.Context, f *coreFrozenTarget) error {
	st := &s.coreTargets
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.frozen == nil {
		st.frozen, st.revisions, st.loaded = map[string]*coreFrozenTarget{}, map[string]int64{}, map[string]bool{}
	}
	st.frozen[f.Scope], st.loaded[f.Scope] = f, true
	if s.coreStore == nil {
		return nil
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	saved, err := s.coreStore.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: coreTargetScope(f.Scope), Kind: stateKindCoreTarget, ExpectedRevision: st.revisions[f.Scope], JSON: raw})
	if err != nil {
		var conflict *corestore.RevisionConflictError
		if errors.As(err, &conflict) && conflict.Exists {
			// Another writer kept the month first: the store's document is
			// the month, so the in-memory copy is dropped and the next read
			// re-reads the store instead of serving a month the store never
			// kept (quality loop review, 2026-10-18).
			st.revisions[f.Scope] = conflict.Actual
			delete(st.frozen, f.Scope)
			st.loaded[f.Scope] = false
		}
		return err
	}
	st.revisions[f.Scope] = saved.Revision
	return nil
}

// coreDecisionDays returns the latest decided month-end (the last session
// of a month on the core's exchange calendar whose close is in: an earlier
// day, or the same day from coreDecisionAfter past its scheduled close) and
// the next one. Empty with a reason when the calendar does not cover them.
func coreDecisionDays(now time.Time, market marketcal.Market) (latest, next string, why string) {
	cal := marketcal.NewWithClock(func() time.Time { return now })
	probe, err := cal.Query(marketcal.Query{Market: market, At: now, Days: 1})
	if err != nil {
		return "", "", "no exchange calendar for " + string(market)
	}
	loc, err := time.LoadLocation(probe.Session.Timezone)
	if err != nil {
		return "", "", "the " + probe.Session.Timezone + " time zone is not available"
	}
	label := nonEmptyString(marketcal.Label(market), string(market))
	local := now.In(loc)
	type monthEndSession struct {
		date  string
		close time.Time
	}
	monthEnd := func(year int, month time.Month) (monthEndSession, bool) {
		first := time.Date(year, month, 1, 12, 0, 0, 0, loc)
		for day := first.AddDate(0, 1, -1); !day.Before(first); day = day.AddDate(0, 0, -1) {
			res, err := cal.Query(marketcal.Query{Market: market, Date: day.Format(time.DateOnly), Days: 1})
			if err != nil || res.Session.State == marketcal.StateUnknown {
				return monthEndSession{}, false
			}
			if res.Session.State == marketcal.StateRegular || res.Session.State == marketcal.StateEarlyClose {
				return monthEndSession{date: res.Session.Date, close: res.Session.Close}, true
			}
		}
		return monthEndSession{}, false
	}
	this, ok := monthEnd(local.Year(), local.Month())
	if !ok {
		return "", "", "the " + label + " calendar does not cover " + local.Format("January 2006")
	}
	if !now.Before(this.close.Add(coreDecisionAfter)) {
		nextMonth := time.Date(local.Year(), local.Month()+1, 1, 12, 0, 0, 0, loc)
		after, _ := monthEnd(nextMonth.Year(), nextMonth.Month())
		return this.date, after.date, ""
	}
	prevMonth := time.Date(local.Year(), local.Month()-1, 1, 12, 0, 0, 0, loc)
	before, ok := monthEnd(prevMonth.Year(), prevMonth.Month())
	if !ok {
		return "", this.date, "the " + label + " calendar does not cover " + prevMonth.Format("January 2006")
	}
	return before.date, this.date, ""
}

// coreCalendar is the session list Measure reads: the core exchange's
// official sessions where the embedded calendar covers them, the series'
// own dates before its coverage starts. It also names any bar dated on a
// day the exchange was closed.
func coreCalendar(dates []string, to string, now time.Time, market marketcal.Market) (sessions []string, offCalendar []string) {
	if len(dates) == 0 {
		return nil, nil
	}
	cal := marketcal.NewWithClock(func() time.Time { return now })
	have := make(map[string]bool, len(dates))
	for _, d := range dates {
		have[d] = true
	}
	start, err := time.Parse(time.DateOnly, dates[0])
	if err != nil {
		return nil, nil
	}
	end, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return nil, nil
	}
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		d := day.Format(time.DateOnly)
		res, err := cal.Query(marketcal.Query{Market: market, Date: d, Days: 1})
		state := marketcal.StateUnknown
		if err == nil {
			state = res.Session.State
		}
		switch state {
		case marketcal.StateRegular, marketcal.StateEarlyClose:
			sessions = append(sessions, d)
		case marketcal.StateUnknown:
			if have[d] {
				sessions = append(sessions, d)
			}
		default:
			if have[d] {
				offCalendar = append(offCalendar, d)
			}
		}
	}
	return sessions, offCalendar
}

// coreInactiveResult is the payload while the constitution carries no
// [mandate.core]: nothing is decided and nothing is read.
func coreInactiveResult(now time.Time, c *risk.Constitution) *rpc.CoreTargetResult {
	msg := "the risk constitution carries no [mandate.core] table; the core sleeve is inactive"
	switch {
	case c == nil:
		msg = "no risk constitution is in force; the core sleeve is inactive"
	case c.Mandate == nil:
		msg = "the risk constitution carries no [mandate] table; the core sleeve is inactive"
	}
	return &rpc.CoreTargetResult{AsOf: now, Active: false, Status: rpc.CoreStatusInactive, Strategy: risk.CoreStrategyID, Sleeve: risk.CoreStrategyID, Message: msg, ReadOnly: true, Rule: core.RuleID, SpecID: core.SpecID, SpecHash: core.SpecHash}
}

// handleCoreTarget serves the core's target on the mandate verdict: the
// kept one while it is younger than mandateVerdictServeWithin, else a
// fresh evaluation after an account read, as mandate.risk serves it.
func (s *Server) handleCoreTarget(ctx context.Context, req *rpc.Request) (*rpc.CoreTargetResult, error) {
	var p rpc.CoreTargetParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	now := s.nowUTC()
	c := s.riskPolicies.snapshot().policy
	if c == nil || c.Mandate == nil || c.Mandate.Core == nil {
		return coreInactiveResult(now, c), nil
	}
	s.refreshMandateForTarget(ctx, c, p.Refresh, now)
	return s.evaluateCoreTarget(ctx, now), nil
}

// handleStrategyTarget serves any register entry's target by id, the core
// included, as handleCoreTarget serves the core's.
func (s *Server) handleStrategyTarget(ctx context.Context, req *rpc.Request) (*rpc.StrategyTargetResult, error) {
	var p rpc.StrategyTargetParams
	if len(req.Params) > 0 && string(req.Params) != "null" {
		if err := decodeParams(req.Params, &p); err != nil {
			return nil, err
		}
	}
	id := strings.TrimSpace(p.ID)
	if id == "" {
		return nil, errBadRequest("strategy.target needs the strategy's id; `canary mandate` lists the register")
	}
	now := s.nowUTC()
	c := s.riskPolicies.snapshot().policy
	if id == risk.CoreStrategyID && (c == nil || c.Mandate == nil || c.Mandate.Core == nil) {
		return coreInactiveResult(now, c), nil
	}
	r, ok := s.resolvedStrategy(c, id)
	if !ok {
		return nil, errBadRequest(unknownStrategyMessage(id, s.resolvedRegister(c)))
	}
	s.refreshMandateForTarget(ctx, c, p.Refresh, now)
	return s.evaluateStrategyTarget(ctx, now, r), nil
}

// unknownStrategyMessage names what a read may ask for when its id is not
// in the register: the ids there, and, when the id is one of their rules
// (`canary mandate` prints each entry's id and rule side by side), which
// entry runs it.
func unknownStrategyMessage(id string, resolved []register.Resolved) string {
	var ids, runs []string
	for _, r := range resolved {
		ids = append(ids, r.Entry.ID)
		if r.Entry.Rule == id {
			runs = append(runs, r.Entry.ID)
		}
	}
	if len(runs) > 0 {
		return fmt.Sprintf("%q is a rule, not a strategy id; the strategy running it is %s (`canary strategy %s`)", id, strings.Join(runs, ", "), runs[0])
	}
	if len(ids) == 0 {
		return fmt.Sprintf("no strategy %q: the register is empty; `canary mandate` shows it", id)
	}
	return fmt.Sprintf("no strategy %q in the register; its ids are %s (`canary mandate` lists each with its rule)", id, strings.Join(ids, ", "))
}

// refreshMandateForTarget measures the mandate again when a target read
// asks for it, when none is kept for the policy in force, or when the kept
// one is older than mandateVerdictServeWithin.
func (s *Server) refreshMandateForTarget(ctx context.Context, c *risk.Constitution, refresh bool, now time.Time) {
	if result, _, at, fp := s.currentMandateVerdict(); refresh || result == nil || fp != c.FingerprintKey() || now.Sub(at) >= mandateVerdictServeWithin {
		s.evaluateMandate(ctx, true)
	}
}

// resolvedStrategy is one entry of the resolved register by id.
func (s *Server) resolvedStrategy(c *risk.Constitution, id string) (register.Resolved, bool) {
	for _, r := range s.resolvedRegister(c) {
		if r.Entry.ID == id {
			return r, true
		}
	}
	return register.Resolved{}, false
}

// refreshCoreTarget runs on the background cadence after the mandate
// verdict, so a month is frozen at its first evaluation after the decision
// close whether or not anyone reads it. It reads nothing from the broker.
func (s *Server) refreshCoreTarget(ctx context.Context) {
	if s.riskPolicies == nil {
		return
	}
	c := s.riskPolicies.snapshot().policy
	r, ok := resolvedCore(s.resolvedRegister(c))
	if !ok {
		return
	}
	s.refreshStrategyTarget(ctx, r)
}

// refreshStrategies runs every register entry on the background cadence
// after the mandate verdict: the core as before the evaluator, then each
// later entry, whose instrument is resolved and whose series is read here,
// after the verdict, so no entry ages the book's verdict (strategy-
// evaluator.md §5).
func (s *Server) refreshStrategies(ctx context.Context) {
	if s.riskPolicies == nil {
		return
	}
	s.refreshCoreTarget(ctx)
	c := s.riskPolicies.snapshot().policy
	for _, r := range s.resolvedRegister(c) {
		if ctx.Err() != nil {
			return
		}
		if r.Entry.Form == risk.StrategyFormCore || r.Rule == nil || r.Instrument.ISIN == "" {
			continue
		}
		if k := s.resolveInstrument(ctx, r.Instrument, strategyName(r.Entry), strategyTable(r.Entry)); k != nil {
			if conn := s.gatewayConnector(); conn != nil {
				s.refreshMandateSeries(ctx, conn, instrumentSeriesNeed(seriesKeyFor(r, k.ConID), k))
			}
		}
		s.refreshStrategySignal(ctx, r)
		s.refreshStrategyTarget(ctx, r)
	}
}

// refreshStrategyTarget evaluates one entry on the background cadence and
// writes its shadow records when they are due.
func (s *Server) refreshStrategyTarget(ctx context.Context, r register.Resolved) {
	contract, _, _, _ := s.instrumentPinState(s.requestedInstrument(r))
	if contract == nil {
		return // nothing to decide on; a read names the hold
	}
	now := s.nowUTC()
	latest, _, why := coreDecisionDays(now, contract.Market)
	if why != "" {
		return
	}
	brokerScope := s.currentBrokerStateScope()
	scope := frozenScopeFor(r, brokerScope, contract.ConID)
	f := s.coreFrozen(ctx, scope)
	decided := f != nil && f.Decision == latest
	switch {
	case decided && r.State == register.StateLive && f.Row != nil && f.Row.Served && s.coreDecisionSubmitted(f, scope):
		// The month's row was sent: its record is final. Until then the
		// evaluation runs on, so a row served later in the window (the
		// verdict read reduce at the decision, within since) is recorded
		// (core_rebalance.go, recordStrategyDecision).
		return
	case decided && r.State != register.StateLive && !s.shadowDue(ctx, r, latest):
		return
	}
	// An entry measured on a signal line decides on that line's calendar,
	// whose month-end can differ from the traded line's: it is evaluated
	// every cadence (its records are written once each all the same).
	res := s.evaluateStrategyTarget(ctx, now, r)
	// The shadow record's only writer (strategy-register.md, section 3),
	// every live entry's decision record's (core_rebalance.go) and a live
	// entry's kill marks' (strategy_kill_live.go).
	s.recordStrategyShadow(ctx, res, r, now)
	s.recordStrategyDecision(ctx, res, r, now)
	s.recordLiveMark(ctx, res, r, brokerScope, now)
}

// coreScope keys a frozen month by account, mode and the core's contract.
func coreScope(scope brokerStateScope, conID int) string {
	return fmt.Sprintf("%s|%s|%d", strings.TrimSpace(scope.Account), scope.Mode, conID)
}

// frozenScopeFor keys a resolved entry's frozen month: the core's scope as
// before the evaluator, every other entry's under its id.
func frozenScopeFor(r register.Resolved, scope brokerStateScope, conID int) string {
	if r.Entry.Form == risk.StrategyFormCore {
		return coreScope(scope, conID)
	}
	return "strategy|" + r.Entry.ID + "|" + coreScope(scope, conID)
}

// strategyName is how a verdict names a strategy: "the core", or "the
// <id> strategy".
func strategyName(e risk.StrategyEntry) string {
	if e.Form == risk.StrategyFormCore {
		return "the core"
	}
	return "the " + e.ID + " strategy"
}

// strategyTable is the policy table an entry is written in.
func strategyTable(e risk.StrategyEntry) string { return "[" + e.KeyPrefix + "]" }

// evaluateCoreTarget decides the core: evaluateStrategyTarget on the
// register's first entry.
func (s *Server) evaluateCoreTarget(ctx context.Context, now time.Time) *rpc.CoreTargetResult {
	c := s.riskPolicies.snapshot().policy
	r, ok := resolvedCore(s.resolvedRegister(c))
	if !ok {
		return coreInactiveResult(now, c)
	}
	return s.evaluateStrategyTarget(ctx, now, r)
}

// evaluateStrategyTarget decides one entry on the kept mandate verdict, its
// instrument's contract as IBKR lists it and its series. Anything the
// decision lacks holds, and every hold is named.
func (s *Server) evaluateStrategyTarget(ctx context.Context, now time.Time, r register.Resolved) *rpc.StrategyTargetResult {
	c := s.riskPolicies.snapshot().policy
	e := r.Entry
	isCore := e.Form == risk.StrategyFormCore
	if isCore && (c == nil || c.Mandate == nil || c.Mandate.Core == nil) {
		return coreInactiveResult(now, c)
	}
	name := strategyName(e)
	ruleID, specID := e.Rule, ""
	if r.Rule != nil {
		ruleID, specID = r.Rule.ID, r.Rule.SpecID
	}
	// The instrument IBKR is asked for: the core's as the policy requests
	// it (the compiled fund while unwritten, core_resolve.go), a later
	// entry's as the register resolved it (empty on a hold).
	inst := s.requestedInstrument(r)
	res := &rpc.StrategyTargetResult{AsOf: now, Active: true, Status: rpc.CoreStatusHold, Strategy: e.ID, Sleeve: e.Sleeve, ReadOnly: true,
		Rule: ruleID, SpecID: specID, SpecHash: nonEmptyString(r.SpecHash, e.SpecHash), PolicyRule: e.Rule, Signal: nonEmptyString(r.Trend, e.Signal),
		PolicyFingerprint: &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: c.FingerprintKey()},
		Policy:            strategyPolicyView(c.Mandate, e),
		Instrument:        &rpc.CoreInstrument{Symbol: nonEmptyString(inst.Symbol, inst.ISIN), ISIN: inst.ISIN, ConID: inst.ConID, Currency: inst.Currency}}
	if isCore {
		res.Rule, res.SpecID, res.SpecHash = core.RuleID, core.SpecID, core.SpecHash
	}
	hold := func(reason string) { res.Reasons = append(res.Reasons, reason) }
	res.Stage = &rpc.CoreStageView{Written: e.Stage, State: string(r.State), ReservedPct: r.ReservedPct, SpecHashCited: e.SpecHash != "", Notes: r.Notes}
	res.Shadow = s.shadowView(ctx, e.ID, r.State)
	if isCore {
		// The core's holds in its own words, as since phase 1; the register
		// resolves the same facts for every other entry.
		p := c.Mandate.Core
		if keys := c.Mandate.CoreUnapprovedKeys(); len(keys) > 0 {
			hold("core keys are unwritten: " + strings.Join(keys, ", "))
		}
		if p.Rule != "" && p.Rule != core.RuleID {
			hold(fmt.Sprintf("the policy names rule %q; this daemon carries %s", p.Rule, core.RuleID))
		}
		if p.SpecHash != "" && p.SpecHash != core.SpecHash {
			hold(fmt.Sprintf("the policy cites spec_hash %s; %s implements %s", p.SpecHash, core.RuleID, core.SpecHash))
		}
		if p.Stage == risk.StageLive && p.SpecHash == "" {
			hold("mandate.core.spec_hash is unwritten: the core goes live only with the lab spec it implements cited")
		}
		if p.ISIN != "" && p.ISIN != core.ISIN {
			hold(fmt.Sprintf("the policy pins ISIN %s; %s is compiled for %s (%s)", p.ISIN, core.RuleID, core.ISIN, core.Symbol))
		}
		if p.Currency != "" && p.Currency != core.Currency {
			hold(fmt.Sprintf("the policy names currency %s; %s is %s", p.Currency, core.Symbol, core.Currency))
		}
	} else {
		for _, reason := range r.Reasons {
			hold(reason)
		}
	}
	if base := normCcy(c.Capital.BaseCurrency); isCore && base != core.Currency {
		hold(fmt.Sprintf("the account base currency %s is not %s: the core is funded and measured in %s only", nonEmptyString(base, "(unwritten)"), core.Currency, core.Currency))
	} else if !isCore && inst.Currency != "" && base != inst.Currency {
		hold(fmt.Sprintf("the account base currency %s is not %s: %s trades in %s, and the rule sizes and funds in the base currency only", nonEmptyString(base, "(unwritten)"), inst.Currency, name, inst.Currency))
	}

	// The contract IBKR lists under the ISIN (core_resolve.go).
	var contract *coreContract
	if inst.ISIN != "" {
		var pinHold, lookupErr string
		var asked bool
		contract, pinHold, lookupErr, asked = s.instrumentPinState(inst)
		switch {
		case contract != nil:
			res.Instrument = &rpc.CoreInstrument{Symbol: contract.Symbol, ISIN: contract.ISIN, ConID: contract.ConID, Exchange: contract.PrimaryExch, Currency: contract.Currency, Resolved: true,
				LocalSymbol: contract.LocalSymbol, TradingClass: contract.TradingClass, Calendar: string(contract.Market), CalendarLabel: marketcal.Label(contract.Market), ResolvedAt: contract.At}
		case pinHold != "":
			hold(pinHold)
		case lookupErr != "":
			hold(fmt.Sprintf("%s's contract is not resolved: IBKR gave no answer for %s in %s (%s); the desk asks again on the background lane", name, inst.ISIN, inst.Currency, lookupErr))
		case !asked:
			hold(fmt.Sprintf("%s's contract is not resolved yet: the desk asks IBKR for %s in %s on the background lane", name, inst.ISIN, inst.Currency))
		}
	}
	var latest string
	sym, venue := res.Instrument.Symbol, ""
	if contract != nil {
		venue = nonEmptyString(marketcal.Label(contract.Market), contract.PrimaryExch)
		var next, why string
		latest, next, why = coreDecisionDays(now, contract.Market)
		res.NextDecision = next
		if why != "" {
			hold(why)
		}
	}

	// The instrument's own series.
	var series *mandateHistorySeries
	if contract != nil {
		need := instrumentSeriesNeed(seriesKeyFor(r, contract.ConID), contract)
		series = s.mandateHistoryForNeed(ctx, need)
		candidate := series
		if candidate == nil {
			candidate = s.mandateHistorySeries(ctx, need.Key)
		}
		ev := &rpc.CoreEvidence{SeriesKey: need.Key, Missing: series == nil, Calendar: string(contract.Market)}
		if candidate != nil {
			ev.Closes, ev.LastClose, ev.FetchedAt, ev.Source, ev.Stale = len(candidate.Closes), candidate.Dates[len(candidate.Dates)-1], candidate.FetchedAt, candidate.Source, mandateSeriesStale(candidate, now)
		}
		refused := ""
		if ev.ReadError, ev.ReadErrorAt = s.mandateSeriesFailure(bindMandateHistoryNeed(need).Key); ev.ReadError != "" {
			refused = fmt.Sprintf("the last read of %s failed at %s UTC: %s", contract.label(), ev.ReadErrorAt.UTC().Format(time.DateTime), ev.ReadError)
			if _, ok := s.mandateHistoryRefusalFor(ctx, bindMandateHistoryNeed(need).Key); ok {
				refused += "; add the exchange's market data subscription at IBKR, and Canary reads it at the next trading session"
			}
		}
		res.Evidence = ev
		switch {
		case candidate == nil && refused != "":
			hold(fmt.Sprintf("no %s daily history: %s", sym, refused))
		case candidate == nil:
			hold(fmt.Sprintf("no %s daily history yet; the desk reads it on %s on the background lane", sym, contract.PrimaryExch))
		case candidate.Market != contract.Market:
			hold(fmt.Sprintf("the kept %s history was read for the %s calendar, not %s; the desk reads it again on %s", sym, nonEmptyString(string(candidate.Market), "Xetra"), venue, contract.PrimaryExch))
			series = nil
		case candidate.Contract.ConID > 0 && candidate.Contract.ConID != contract.ConID:
			hold(fmt.Sprintf("the series' contract %d is not %s's contract %d", candidate.Contract.ConID, name, contract.ConID))
		case !mandateHistoryContractMatches(candidate.Contract, need.Contract):
			hold(fmt.Sprintf("the kept %s history does not match its resolved contract identity; the desk reads it again", sym))
			series = nil
		case ev.Stale && refused != "":
			hold(fmt.Sprintf("the %s daily history is stale: last close %s on the %s calendar; %s", sym, ev.LastClose, venue, refused))
		case ev.Stale:
			hold(fmt.Sprintf("the %s daily history is stale: last close %s on the %s calendar", sym, ev.LastClose, venue))
		}
	}

	// The month's decision under the entry's filter: the core's compiled
	// one whatever its holds, as since phase 1, so a held month is still
	// measured and recorded as a hold; an entry's from its key.
	trendID := e.Signal
	if isCore {
		trendID = core.TrendSMA10.ID
	}
	trend, trendOK := core.FindTrend(trendID)
	var sig core.Signal
	var window core.Window
	// What the month is measured on: the traded line's own closes, or,
	// while they do not fill the window, the entry's signal series, another
	// share class of the fund (strategy_signal.go). Nothing falls back: a
	// signal series that cannot be read holds the entry, named.
	var mDates []string
	var mCloses []float64
	mMarket, mLatest, mSym, mVenue, mLabel := marketcal.Market(""), latest, sym, venue, digestLabel(r)
	var mContract *coreContract
	if series != nil && contract != nil {
		mDates, mCloses, mMarket, mContract = series.Dates, series.Closes, contract.Market, contract
	}
	useSignal := false
	if !isCore && r.Signal.ISIN != "" && contract != nil && latest != "" {
		own := strategyOwnSessions(series, latest, now, contract.Market)
		ev := &rpc.CoreSignalEvidence{ISIN: r.Signal.ISIN, Currency: r.Signal.Currency, MeasuredIn: r.SignalIn, OwnSessions: own}
		// A month frozen on the signal series keeps it: the line's own
		// closes take over at the next decision, never inside a month whose
		// two calendars end on different days.
		stay := false
		if f := s.coreFrozen(ctx, frozenScopeFor(r, s.currentBrokerStateScope(), contract.ConID)); f != nil && f.Measure == "signal" && len(f.Decision) >= 7 && len(latest) >= 7 && f.Decision[:7] == latest[:7] {
			stay = true
		}
		if own < core.WindowCloses || stay {
			m, holds := s.strategySignalMeasure(ctx, r, name, now)
			for _, h := range holds {
				hold(h)
			}
			mDates, mCloses = nil, nil
			if m != nil {
				ev = m.Evidence
				ev.OwnSessions = own
				sLatest, sNext, why := coreDecisionDays(now, m.Market)
				if why != "" {
					hold(why)
				}
				useSignal = true
				mDates, mCloses, mMarket, mContract, mLatest = m.Dates, m.Closes, m.Market, m.Contract, sLatest
				mSym, mVenue, mLabel = m.Contract.Symbol, nonEmptyString(marketcal.Label(m.Market), m.Contract.PrimaryExch), mLabel+m.Label
				res.NextDecision = sNext
			}
		}
		if res.Evidence != nil {
			res.Evidence.Signal = ev
		}
	}
	if len(mDates) > 0 && mLatest != "" && trendOK {
		sessions, off := coreCalendar(mDates, mLatest, now, mMarket)
		for _, d := range off {
			hold(fmt.Sprintf("the %s history carries a bar on %s, when %s was closed", mSym, d, mVenue))
		}
		if mDates[len(mDates)-1] < mLatest {
			hold(fmt.Sprintf("the decision waits for %s's close of %s", mSym, mLatest))
		} else {
			sig, window = core.MeasureWith(trend, mLabel, sessions, mDates, mCloses, mLatest)
			if sig.Status != core.StatusOK {
				hold(fmt.Sprintf("no decision for %s: %s", mLatest, sig.Reason))
			} else {
				// The data guard corroborates a jump on the instrument's
				// returns proxy where the mandate has one; without one a
				// jump holds for review.
				proxy := mandate.ReturnsSeriesFor(mContract.Symbol, mContract.Currency, mContract.ISIN)
				if proxy == mContract.Symbol {
					proxy = ""
				}
				index := core.Series{}
				if proxy != "" {
					if spy := s.mandateHistoryForNeed(ctx, mandateIndexHistoryNeed()); spy != nil && proxy == mandate.IndexSymbol {
						index = core.Series{Dates: spy.Dates, Closes: spy.Closes}
					}
				}
				for _, g := range core.GuardWith(sig, window, index, proxy) {
					hold(g)
				}
			}
			res.Decision = &rpc.CoreDecision{Signal: sig}
			if useSignal {
				// Sized, limit-checked and marked on the traded line's own
				// close on or before the decision day.
				res.Decision.SignalFrom = fmt.Sprintf("%s (%s) in %s", mSym, r.Signal.ISIN, nonEmptyString(r.SignalIn, r.Signal.Currency))
				// The traded line's close of its own last session on or
				// before the decision day, never an older one.
				want := strategyLastSessionOnOrBefore(sig.Decision, contract.Market, now)
				if c, d, ok := strategyLineCloseOn(series, sig.Decision); ok && d == want {
					res.Decision.TradedClose = c
				} else {
					hold(fmt.Sprintf("the decision waits for %s's close of %s to size the target on", sym, nonEmptyString(want, sig.Decision)))
				}
			}
		}
	}
	// sizeSig is the signal the target is sized on: the decision's close is
	// the traded line's own when the month was measured on another class.
	sizeSig := sig
	if res.Decision != nil && res.Decision.TradedClose > 0 {
		sizeSig.Close = res.Decision.TradedClose
	}

	// The mandate: capital for the size, the book for the holding and the
	// headroom.
	result, input, at, fp := s.currentMandateVerdict()
	var verdict *mandate.Verdict
	switch {
	case result == nil || result.Verdict == nil || input == nil || fp != c.FingerprintKey():
		hold("no mandate verdict has been measured for the policy in force")
	default:
		verdict = result.Verdict
		res.Mandate = &rpc.CoreMandateView{BaseCurrency: verdict.BaseCurrency, Status: verdict.Status, EvaluatedAt: at, BudgetAnnualSD: verdict.Capital.BudgetAnnualSD}
		if verdict.Risk != nil {
			res.Mandate.RiskAnnualSD = verdict.Risk.SDAnnualBase
		}
		if !verdict.Capital.Known {
			hold("the mandate's capital is unknown: " + strings.Join(verdict.Capital.Reasons, "; "))
		}
		if verdict.Status == mandate.StatusUnknown {
			// mandate.Evaluate copies an unknown capital's reasons into the
			// verdict's, so a reason the capital hold already names is not
			// said twice.
			why := "no reason given"
			for _, reason := range verdict.Reasons {
				if verdict.Capital.Known || !slices.Contains(verdict.Capital.Reasons, reason) {
					why = reason
					break
				}
			}
			if verdict.Capital.Known || why != "no reason given" {
				hold("the mandate verdict is unknown, so the holding cannot be confirmed: " + why)
			}
		}
		if age := now.Sub(at); age > mandateVerdictMaxAge {
			hold(fmt.Sprintf("the mandate verdict is %s old", formatQuoteAge(age)))
		}
	}

	// The holding: every unit of the core's contract is the core's, and any
	// other line of the fund holds the core; a later entry's holding is what
	// the placement puts in its sleeve: the units its own fills left
	// (strategy_placement.go). While the owner holds units of the line too,
	// the entry holds, naming them: it trades only a line it holds alone.
	held, price, priceSource := 0, 0.0, ""
	if input != nil && contract != nil && !isCore && strategyReserves(e) {
		if s.strategyOwnFills(ctx, e.ID, contract.ConID).Unread {
			hold("the order journal could not be read, so " + name + "'s own units of its line are not known; it holds until the journal reads")
		} else if why := strategyForeignHold(name, sym, strategyOwnerUnitsInBook(input, contract.ConID)); why != "" {
			hold(why)
		}
	}
	if input != nil && contract != nil {
		id := mandateConLegID(contract.ConID)
		qty := 0.0
		for _, l := range input.Legs {
			if l.ID == id && l.Spot > 0 {
				price, priceSource = l.Spot, "mark"
			}
			switch {
			case isCore && l.ID == id:
				qty += l.Quantity
			case isCore && (input.LineISINs[l.ID] == contract.ISIN || (l.Underlying == core.Symbol || l.Underlying == contract.Symbol) && normCcy(l.Currency) == contract.Currency):
				hold(fmt.Sprintf("a second line of the fund (%s) is held under %s; the core counts only %s", l.Symbol, l.ID, contract.label()))
			}
		}
		if !isCore {
			qty = strategyUnitsInBook(input, e.Sleeve, contract.ConID)
		}
		if qty < 0 {
			hold(fmt.Sprintf("the book is short %s; %s never shorts", sym, name))
		}
		held = int(math.Max(0, math.Floor(qty)))
	}
	if price <= 0 && series != nil {
		price, priceSource = series.Closes[len(series.Closes)-1], "close"
	}
	res.Holding = &rpc.CoreHolding{Shares: held, Price: price, ValueBase: float64(held) * price, PriceSource: priceSource}

	// The kill rule's drawdown test, on a live entry's own fills (the core's
	// holding) or in shadow on the paper sleeve, with this reading marked
	// when it is not recorded yet; past the bound the strategy holds before
	// the month can be frozen.
	killUnreadable := false
	killOf := func(mark killMark) {
		on := "the shadow record"
		if r.State == register.StateLive && contract != nil {
			// A live entry keeps no shadow record: its own fills are the
			// measure, and the core's sleeve is its line's holding marked
			// at each decision (strategy_kill_live.go).
			kill, err := s.strategyKillLive(ctx, r, mark, contract.ConID)
			switch {
			case errors.Is(err, errLiveMarkScopeUnresolved):
				// Marks belong to one account and mode: without them
				// resolved nothing is measured, and the entry holds.
				killUnreadable = true
				hold(fmt.Sprintf("kill rule: %s, so the drawdown of %s cannot be measured: buys stop until the account resolves", errLiveMarkScopeUnresolved, name))
				return
			case err != nil:
				// A kill rule that cannot be measured is no kill rule:
				// the entry holds until the marks read (refuter round 3).
				killUnreadable = true
				hold(fmt.Sprintf("kill rule: the marks of %s cannot be read, so its drawdown cannot be measured: buys stop until the mark is repaired (%v)", name, err))
				return
			}
			res.Kill, on = kill, "its own fills"
			if isCore {
				on = "its holding"
			}
		} else {
			res.Kill = s.strategyKill(ctx, r, mark)
		}
		switch {
		case res.Kill == nil:
		case res.Kill.Breached:
			hold(fmt.Sprintf("kill rule: the drawdown on %s is %.2f K, past the bound %g K: buys stop; suspend %s with stage = \"suspended\"", on, res.Kill.DrawdownK, *res.Kill.MaxDrawdownK, name))
		case res.Kill.MaxDrawdownK == nil && r.State == register.StateLive:
			// A live entry without a bound has a kill rule that cannot
			// fire: it holds, as any unwritten key holds, until the owner
			// writes the bound (strategy-register.md, 2026-10-19).
			hold(fmt.Sprintf("kill rule: kill_drawdown_k is unwritten in [%s], so the kill rule cannot fire: buys stop until the bound is written", register.Table(r.Entry)))
		}
	}

	// The size: frozen at the decision, lowered to today's capital.
	sizable := e.ScalePct != nil && e.AllocationPct != nil && e.MaxValuePct != nil
	if isCore {
		sizable = len(c.Mandate.CoreUnapprovedKeys()) == 0
	}
	if sig.Status == core.StatusOK && verdict != nil && verdict.Capital.Known && sizable && contract != nil {
		on := core.Sizing{AllocationPct: *e.AllocationPct, ScalePct: *e.ScalePct, MaxValuePct: *e.MaxValuePct, BudgetBase: verdict.Capital.BudgetBase, Room: verdict.Capital.Room}
		current := core.SizeTarget(sizeSig, on)
		killOf(killMark{session: sig.Decision, close: sizeSig.Close, sleeveBudget: current.SleeveBudget, scalePct: on.ScalePct, units: float64(held)})
		scope := frozenScopeFor(r, s.currentBrokerStateScope(), contract.ConID)
		frozen := s.coreFrozen(ctx, scope)
		switch {
		case frozen != nil && frozen.Decision == sig.Decision && frozen.Digest != sig.Digest:
			hold(fmt.Sprintf("core_target_drift: the closes behind %s's decision read differently than when it was frozen at %s; the month holds for review", sig.Decision, frozen.FrozenAt.UTC().Format(time.DateTime)))
			res.Decision = &rpc.CoreDecision{Signal: frozen.Signal, FrozenAt: frozen.FrozenAt, Drift: true}
		case frozen != nil && frozen.Decision == sig.Decision:
			res.Decision.FrozenAt = frozen.FrozenAt
		case len(res.Reasons) == 0:
			frozen = &coreFrozenTarget{Version: coreTargetDocVer, Scope: scope, Decision: sig.Decision, Digest: sig.Digest, FrozenAt: now, Signal: sig, On: on, Size: current}
			if res.Decision.SignalFrom != "" {
				frozen.Measure = "signal"
			}
			if err := s.freezeCore(ctx, frozen); err != nil && s.logger != nil {
				s.logger.Warnf("%s target %s: frozen in memory, the database save failed: %v", e.ID, sig.Decision, err)
			}
			res.Decision.FrozenAt = now
		default:
			frozen = nil // a month is frozen only on a clean reading
		}
		if frozen != nil && frozen.Decision == sig.Decision {
			target := min(frozen.Size.TargetShares, current.TargetShares)
			res.Sizing = &rpc.CoreSizingView{Frozen: frozen.Size, FrozenOn: frozen.On, Current: current, CurrentOn: on, Target: target, TargetBase: float64(target) * price}
		}
	}

	if res.Kill == nil && !killUnreadable {
		killOf(killMark{})
	}

	if len(res.Reasons) > 0 {
		res.Action = &core.Decision{Kind: core.ActionHold, Why: name + " holds: buys stop, and a sale the rule wants goes to you for review"}
		return s.coreFinish(ctx, res, contract, r)
	}
	if res.Sizing == nil {
		res.Action = &core.Decision{Kind: core.ActionHold, Why: "no target is sized"}
		return s.coreFinish(ctx, res, contract, r)
	}

	// The action, with a buy cut to the mandate's headroom.
	in := core.DecideInput{Target: res.Sizing.Target, TrendOn: sig.TrendOn, Held: held, Price: price, BandPct: r.Terms.BandPct, MinOrderBase: r.Terms.MinOrderBase, Holdable: held}
	if !isCore {
		in.Symbol, in.Name = sym, name
	}
	// The buy is judged by the entry's own sleeve as the order gate judges
	// an order (mandateVerdictHolds): a book over budget holds it only while
	// its sleeve is over its allocation or at the maximum loss; an unknown
	// verdict holds it; within the budget it passes (owner, 2026-10-18
	// 15:31 CEST; until 2026-10-19 any verdict but within blocked it).
	why, overBudget := mandateVerdictHolds(verdict, e.Sleeve, verdict.BaseCurrency)
	switch {
	case why != "":
		in.BuyBlocked = why
	case r.State != register.StateLive:
		// The allocation is reserved only while the strategy is live
		// (strategy-register.md, section 2); in shadow it sends nothing.
		in.BuyBlocked = name + " runs in " + string(r.State) + " and sends no orders, so it reserves no allocation"
	default:
		if want := res.Sizing.Target - held; want > 0 && price > 0 {
			book := input
			if !isCore {
				// The entry's fund measured on its own closes while the book
				// keeps none, as the shadow record's hypothetical book is.
				book = s.withStrategySeries(input, r, contract)
			}
			add := core.Headroom(want, func(q int) bool { return strategyHeadroomFits(book, contract, e.Sleeve, q, price, verdict, overBudget) })
			in.Holdable = held + add
			res.Sizing.Holdable = &in.Holdable
		}
	}
	d := core.Decide(in)
	res.Action = &d
	res.Status = rpc.CoreStatusReady
	return s.coreFinish(ctx, res, contract, r)
}

// digestLabel names what a frozen month was decided under: the core's
// rule and spec, as since phase 1; for a later entry its rule, the spec it
// cites, its filter and its instrument, as the entry writes them, so the
// label is the same whether or not the entry holds.
func digestLabel(r register.Resolved) string {
	e := r.Entry
	if e.Form == risk.StrategyFormCore {
		return core.CoreLabel
	}
	return e.Rule + "|" + e.SpecHash + "|" + e.Signal + "|" + e.ISIN + "|" + e.Currency
}

// strategyLegID is a non-core strategy's own leg in a hypothetical book: its
// units beside the owner's real units of the same contract, which stay the
// owner's (strategy-evaluator.md §5).
func strategyLegID(sleeve string, conID int) string {
	return "sleeve:" + sleeve + ":" + mandateConLegID(conID)
}

// strategyHeadroomFits reports whether the book with q more shares of the
// strategy's contract stays at most at core.HeadroomFactor of the budget,
// carries no gap-cap breach, keeps the strategy's own gap at most at that
// share of its cap, and keeps its sleeve inside its allocation: the forced
// reduction's target with margin, so a buy cannot set up a forced sale by
// itself (core-sleeve.md §3; review B1), and the gate's allocation check
// (allocations, 2026-10-18). The leg is judged in the strategy's sleeve; a
// non-core strategy's leg is its own, so the owner's real units of the fund
// are never moved into its sleeve.
func strategyHeadroomFits(book *mandate.Input, k *coreContract, sleeve string, q int, price float64, standing *mandate.Verdict, overBudget bool) bool {
	if book == nil || k == nil {
		return false
	}
	if q <= 0 {
		return true
	}
	real := mandateConLegID(k.ConID)
	leg := mandate.Leg{ID: real, Symbol: k.Symbol, Underlying: k.Symbol, Kind: mandate.KindStock, Quantity: float64(q), Multiplier: 1, Currency: k.Currency, Spot: price, ISIN: k.ISIN}
	// A line the plan reserves to the sleeve is the sleeve's leg itself
	// (strategy_placement.go); otherwise a non-core strategy's units are a
	// leg of their own beside the owner's real units, which stay the owner's.
	if sleeve != mandate.SleeveCore && book.Allocations.SleeveOf(real) != sleeve {
		leg.ID = strategyLegID(sleeve, k.ConID)
	}
	for _, l := range book.Legs {
		if l.ID == real || l.ID == leg.ID {
			leg.Symbol, leg.Underlying = l.Symbol, l.Underlying // the held line's names, so the merged leg is one underlying
			if l.Spot > 0 {
				leg.Spot = l.Spot
			}
		}
	}
	judged := mandate.WithLeg(*book, leg)
	if sleeve != mandate.SleeveCore {
		judged.Allocations = judged.Allocations.WithPlaced(leg.ID, sleeve)
	}
	with := mandate.Evaluate(judged)
	if with.Status == mandate.StatusUnknown || with.Risk == nil || with.Stresses == nil {
		return false
	}
	// The whole-book checks as the gate applies them (mandate_verdict.go),
	// with the row's margin: the book at most at HeadroomFactor of the
	// budget, unless it is over the budget already and the sleeve adds
	// inside its own allocation (owner, 2026-10-18 15:31 CEST); no gap-cap
	// breach created or deepened, and the strategy's own gap at most at
	// that share of its cap; the sleeve inside its allocation by use and on
	// its own risk (mandateSleeveOver), as the gate binds it.
	if !overBudget && with.Risk.SDAnnualBase > core.HeadroomFactor*with.Capital.BudgetAnnualSD {
		return false
	}
	if _, added := mandateGapBreachAdded(standing, with); added {
		return false
	}
	for _, u := range with.Underlyings {
		if u.Underlying == leg.Underlying && u.GapLossBase > core.HeadroomFactor*u.GapCapBase {
			return false
		}
	}
	if allocation, ok := with.Allocation(sleeve); ok {
		if over, _ := mandateSleeveOver(allocation); over {
			return false
		}
	}
	return true
}

// strategyPolicyView is the entry as the daemon read it: [mandate.core]
// with the sleeve share from [mandate.sleeves], or a [mandate.strategies]
// table with its allocation and signal.
func strategyPolicyView(m *risk.ConstitutionMandate, e risk.StrategyEntry) *rpc.CorePolicyView {
	if e.Form == risk.StrategyFormCore {
		return corePolicyView(m)
	}
	return &rpc.CorePolicyView{Rule: e.Rule, ISIN: e.ISIN, ConID: e.ConID, Exchange: e.Exchange, Currency: e.Currency, Signal: e.Signal, ScalePct: e.ScalePct, BandPct: e.BandPct,
		MinOrderBase: e.MinOrderBase, MaxValuePct: e.MaxValuePct, AllocationPct: e.AllocationPct, Unapproved: e.Unwritten}
}

func corePolicyView(m *risk.ConstitutionMandate) *rpc.CorePolicyView {
	if m == nil || m.Core == nil {
		return nil
	}
	p := m.Core
	v := &rpc.CorePolicyView{Rule: p.Rule, ISIN: p.ISIN, ConID: p.ConID, Exchange: p.Exchange, Currency: p.Currency, ScalePct: p.ScalePct, BandPct: p.BandPct,
		MinOrderBase: p.MinOrderBase, MaxValuePct: p.MaxValuePct, Unapproved: m.CoreUnapprovedKeys()}
	if m.Sleeves != nil {
		v.CoreVolPct = m.Sleeves.CoreVolPct
	}
	return v
}
