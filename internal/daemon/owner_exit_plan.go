package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Exit plans (queued authorisations, version 3; see
// internal-docs/design/queued-authorisations.md, "Exit plans").
//
// The owner signs one plan on a held long single-leg option: an optional
// first sale of part of the line once the session has been open for the
// plan's wait, near the mid and never below a signed minimum; and a trail on
// the underlying stock from the same moment. The trail's exit line starts at
// the signed starting stop and rises with the stock's high; when the stock's
// midpoint is at or below it on two checks at least five seconds apart, the
// trail sells what is still held of the option, never more than its signed
// maximum, near the mid, inside the option spread limit, with no minimum. A
// version-3 plan lasts that session; a version-4 plan carries over sessions,
// to the end of its signed validity and never past the option's last trading
// day, watching only inside the regular session of both the option and the
// stock (owner, 2026-11-24 07:00 CET: "exit function refined with a GTC or x
// days validity to be able to carry over").
//
// The plan is two ordinary single-send records linked by a plan id, so the
// executor's state machine, its holds and its restart recovery apply to each
// unchanged. They are prepared in one store update and armed together or not
// at all. They may coexist on one contract and side; everything else stays
// one live intent per contract and side. The trail withdraws an unsent first
// sale when it fires, and cancels one still working at the broker before it
// sells, so two sells never together exceed the position.

const (
	queuedPlanTermsVersion = 3
	// queuedPlanSessionsTermsVersion is an exit plan that carries over
	// sessions: its plan link adds validity, valid_until and
	// resume_wait_minutes, and the trail's window runs to the close of the
	// valid_until session.
	queuedPlanSessionsTermsVersion = 4
	// queuedPlanValidityGTC is the until-cancelled validity.
	queuedPlanValidityGTC = "gtc"
	// queuedPlanValidityMax bounds a carry-over trail's window: the trading
	// calendar's own forward horizon.
	queuedPlanValidityMax = 400 * 24 * time.Hour
	// queuedPlanMaxWaitMinutes bounds the signed wait after the open.
	queuedPlanMaxWaitMinutes = 120
	// queuedPlanMaxTrailPercent bounds the trail's give-back.
	queuedPlanMaxTrailPercent = 50
	// queuedTrailWindowMax bounds a trail's window: one regular session.
	queuedTrailWindowMax = 24 * time.Hour
	// queuedTrailCheckEvery is the longest the executor waits between two
	// reads of the stock while a trail is inside its window.
	queuedTrailCheckEvery = 10 * time.Second
	// queuedTrailConfirmAfter is how far apart two reads at or below the line
	// must be before the trail fires: one bad print never sells.
	queuedTrailConfirmAfter = 5 * time.Second
	// queuedTrailQuoteTimeout bounds one read of the stock.
	queuedTrailQuoteTimeout = 3 * time.Second
	// queuedTrailWatchFresh is how recent the last usable read must be for
	// the view to say the trail is being watched.
	queuedTrailWatchFresh = 3 * queuedTrailCheckEvery
	// queuedPlanCancelRetry spaces repeated broker cancels of a first sale
	// still working when the trail fired.
	queuedPlanCancelRetry = 30 * time.Second
	// queuedPlanIDMaxBytes bounds a stored plan id.
	queuedPlanIDMaxBytes = 64

	// Hold codes of a plan's records.
	queuedHoldStockQuote          = "stock_quote_unusable"
	queuedHoldFirstSaleCancelling = "plan_first_sale_cancelling"
	// queuedHoldPolicyChanged holds an unfired trail whose protection policy
	// changed after signing, until the plan is signed again.
	queuedHoldPolicyChanged = "queue_policy_changed"
	// queuedReasonPlanStopTriggered withdraws an unsent first sale when the
	// trail fires.
	queuedReasonPlanStopTriggered = "plan_stop_triggered"
	// End codes of a trail that did not sell the line: its signed validity
	// ended, or the option's last trading day closed with it.
	queuedReasonPlanValidityEnded   = "plan_validity_ended"
	queuedReasonPlanContractExpired = "plan_contract_expired"
	// queuedEventSessionStarted journals a carry-over trail entering a new
	// regular session; a fired trail's sale starts again at the mid.
	queuedEventSessionStarted = "queued_auth.session_started"

	// Journaled transitions of a trail: the stop rose, and the line was
	// crossed (the trigger is latched).
	queuedEventStopRaised = "queued_auth.stop_raised"
	queuedEventTriggered  = "queued_auth.triggered"
	// queuedEventSaleCancel journals a fired trail cancelling its own
	// working sale to send again at the bid.
	queuedEventSaleCancel = "queued_auth.sale_cancel"

	// queuedTrailRepriceAfter is how long a fired trail's sale works
	// unfilled before it is cancelled and sent again at the fresh bid; at
	// most one re-price per this interval.
	queuedTrailRepriceAfter = 30 * time.Second
	// Hold codes of a fired trail between attempts: its order ended at the
	// broker and is sent again, or its end could not be confirmed and it is
	// never resent.
	queuedHoldTrailSaleEnded = "trail_sale_ended"
	// View reasons of a fired trail whose sale is at the broker.
	queuedTrailSaleWorking         = "trail_sale_working"
	queuedTrailSaleCancelling      = "trail_sale_cancelling"
	queuedHoldTrailSaleUnconfirmed = "trail_sale_unconfirmed"
)

// queuedTrailState is a trail record's durable watch: the high water, the
// stop it sets, and the latched trigger. It is written whenever the stop
// rises and when the trail fires, so a restart resumes from it and never
// lowers the stop.
type queuedTrailState struct {
	HighWater    float64   `json:"high_water,omitempty"`
	HighAt       time.Time `json:"high_at,omitzero"`
	StopPrice    float64   `json:"stop_price,omitempty"`
	TriggeredAt  time.Time `json:"triggered_at,omitzero"`
	TriggerPrice float64   `json:"trigger_price,omitempty"`
	// FirstSaleCancelAt is when the trail last asked the broker to cancel
	// its plan's first sale, still working when it fired.
	FirstSaleCancelAt time.Time `json:"first_sale_cancel_at,omitzero"`
	// SaleAttempts counts the fired trail's sends; SaleFilled is what its
	// ended orders sold; SaleCancelAt is when it last cancelled its own
	// working sale to send again at the bid.
	SaleAttempts int       `json:"sale_attempts,omitempty"`
	SaleFilled   int       `json:"sale_filled,omitempty"`
	SaleCancelAt time.Time `json:"sale_cancel_at,omitzero"`
	// LineAtTrigger is the option line held when the exit line was crossed:
	// what a whole-line trail sells in all.
	LineAtTrigger int `json:"line_at_trigger,omitempty"`
	// SaleStep is the price step of the next or working attempt: the mid,
	// halfway to the bid, the bid (trailSaleSteps). LastLimit, LastBid and
	// LastAsk are the ended attempt's limit and quote, for skipping a step
	// that would not change the price.
	SaleStep  int     `json:"sale_step,omitempty"`
	LastLimit float64 `json:"last_limit,omitempty"`
	LastBid   float64 `json:"last_bid,omitempty"`
	LastAsk   float64 `json:"last_ask,omitempty"`
	// SessionOpen is the open of the regular session a carry-over trail last
	// entered (version 4 only): a new one restarts a fired sale at the mid.
	SessionOpen time.Time `json:"session_open,omitzero"`
}

// trailSaleSteps are a fired trail's price steps: each attempt starts at
// the next, from the mid (concession 0) to halfway to the bid (0.5) to the
// bid (1); every later attempt stays at the bid.
var trailSaleSteps = []struct {
	name       string
	concession float64
}{{"mid", 0}, {"half", 0.5}, {"bid", 1}}

func trailStep(step int) int { return min(max(step, 0), len(trailSaleSteps)-1) }

// trailStepWorks is how long an attempt at a step works unfilled before it
// is cancelled for the next: the trail's tick on the way down, the
// re-price interval at the bid.
func trailStepWorks(step int) time.Duration {
	if trailStep(step) < len(trailSaleSteps)-1 {
		return queuedTrailCheckEvery
	}
	return queuedTrailRepriceAfter
}

// IsZero reports a record with no watch (omitzero).
func (w queuedTrailState) IsZero() bool { return w == queuedTrailState{} }

// queuedOwnerOrder reports owner-order terms: v2, or a v3 or v4 exit plan's
// record. All run through the owner path (resolveOwnerQueue, submitOwnerQueue).
func queuedOwnerOrder(t rpc.QueuedAuthTerms) bool {
	return t.Version == 2 || queuedExitPlanVersion(t.Version)
}

// queuedExitPlanVersion reports the terms versions of an exit plan: 3 lasts
// one session, 4 carries over sessions.
func queuedExitPlanVersion(v int) bool {
	return v == queuedPlanTermsVersion || v == queuedPlanSessionsTermsVersion
}

// queuedPlanOf is a record's plan link, or nil.
func queuedPlanOf(t rpc.QueuedAuthTerms) *rpc.QueuedPlan {
	if !queuedExitPlanVersion(t.Version) || t.OwnerOrder == nil {
		return nil
	}
	return t.OwnerOrder.Plan
}

// exitPlanValidity reads a requested validity: "" or "day" is this session
// (carry false); "Nd" is N sessions counting the plan's first, "1d" being
// the day; "gtc" is every session to the option's last trading day
// (sessions 0). norm is the form signed in the terms.
func exitPlanValidity(v string) (norm string, sessions int, carry, ok bool) {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "", "day":
		return "", 1, false, true
	case queuedPlanValidityGTC:
		return queuedPlanValidityGTC, 0, true, true
	}
	digits, found := strings.CutSuffix(v, "d")
	n, err := strconv.Atoi(digits)
	if !found || err != nil || n < 1 || n > 1000 || strconv.Itoa(n) != digits {
		return "", 0, false, false
	}
	if n == 1 {
		return "", 1, false, true
	}
	return strconv.Itoa(n) + "d", n, true, true
}

// optionLastTradingDate is the option's expiry as a calendar date
// (YYYY-MM-DD), or "" when the contract does not name one.
func optionLastTradingDate(c rpc.ContractParams) string {
	day, err := time.Parse("20060102", strings.TrimSpace(c.Expiry))
	if err != nil {
		return ""
	}
	return day.Format(time.DateOnly)
}

func queuedPlanTrail(t rpc.QueuedAuthTerms) bool {
	plan := queuedPlanOf(t)
	return plan != nil && plan.Role == rpc.QueuedPlanRoleTrail
}

func queuedPlanFirstSale(t rpc.QueuedAuthTerms) bool {
	plan := queuedPlanOf(t)
	return plan != nil && plan.Role == rpc.QueuedPlanRoleFirstSale
}

// queuedSamePlan reports two records of one plan in one account and mode.
func queuedSamePlan(a, b queuedAuthRecord) bool {
	pa, pb := queuedPlanOf(a.Terms), queuedPlanOf(b.Terms)
	return pa != nil && pb != nil && pa.PlanID == pb.PlanID && sameBrokerScope(a.scope(), b.scope())
}

// exitPlanTermsProblem validates a v3 record as strictly as ownerQueueProblem
// validates v2: anything inconsistent is refused at prepare and ends a
// stored record unreadable.
func exitPlanTermsProblem(t rpc.QueuedAuthTerms) string {
	o := t.OwnerOrder
	if o == nil || o.Plan == nil {
		return "an exit plan's record without its plan"
	}
	plan := o.Plan
	waitForm := plan.StartInMinutes == nil && plan.WaitMinutes >= 1 && plan.WaitMinutes <= queuedPlanMaxWaitMinutes
	startForm := plan.StartInMinutes != nil && plan.WaitMinutes == 0 && *plan.StartInMinutes >= 0 && *plan.StartInMinutes <= queuedPlanMaxWaitMinutes
	if plan.PlanID == "" || len(plan.PlanID) > queuedPlanIDMaxBytes || strings.TrimSpace(plan.PlanID) != plan.PlanID || !waitForm && !startForm {
		return "invalid exit plan link"
	}
	carry := t.Version == queuedPlanSessionsTermsVersion
	window := queuedTrailWindowMax
	if carry {
		norm, _, carries, _ := exitPlanValidity(plan.Validity)
		last := optionLastTradingDate(t.Contract)
		if !carries || norm != plan.Validity || plan.ResumeWaitMinutes < 1 || plan.ResumeWaitMinutes > queuedPlanMaxWaitMinutes ||
			!isCalendarDate(plan.ValidUntil) || !isCalendarDate(t.SessionDate) || plan.ValidUntil < t.SessionDate || last == "" || plan.ValidUntil > last {
			return "invalid exit plan validity"
		}
		window = queuedPlanValidityMax
	} else if plan.Validity != "" || plan.ValidUntil != "" || plan.ResumeWaitMinutes != 0 {
		return "invalid exit plan validity"
	}
	if t.Contract.ConID <= 0 || t.Contract.SecType != "OPT" || t.Contract.Right != "C" || t.AccountID == "" || !slices.Contains([]string{"paper", "live"}, t.AccountMode) ||
		t.Action != rpc.OrderActionSell || t.Currency == "" || t.Currency != t.Contract.Currency || t.ArmDeadline.IsZero() || t.TIF != rpc.OrderTIFDay ||
		t.MaxQuantity <= 0 || t.MaxQuantity > 1000000 || !positiveFinite(t.PositionQuantity) || math.Trunc(t.PositionQuantity) != t.PositionQuantity ||
		float64(t.MaxQuantity) > t.PositionQuantity {
		return "invalid exit plan identity or quantity"
	}
	if o.OrderType != rpc.OrderTypeLMT || o.Trail != nil || o.TriggerMethod != 0 || o.OutsideRTH || o.AdaptivePriority != "" || o.FullPosition ||
		o.Add || o.AddWarnings != nil || t.Concession != 0 || !positiveFinite(t.MaxSpreadPctOfMid) || t.MaxSpreadPctOfMid > 100 ||
		!t.NotAfter.After(t.NotBefore) || t.NotBefore.IsZero() {
		return "invalid exit plan order terms"
	}
	switch plan.Role {
	case rpc.QueuedPlanRoleFirstSale:
		// A first sale has a positive minimum or none (worst_price omitted).
		if o.StockTrail != nil || t.Style != "midpoint_bounded" || t.WorstPrice != 0 && !positiveFinite(t.WorstPrice) || t.PositionEffect != "reduce" ||
			float64(t.MaxQuantity) >= t.PositionQuantity || t.NotAfter.Sub(t.NotBefore) > queuedSendWindow {
			return "invalid exit plan first sale"
		}
	case rpc.QueuedPlanRoleTrail:
		st := o.StockTrail
		if st == nil || t.Style != rpc.QueuedAuthStyleStockTrail || t.WorstPrice != 0 || t.PositionEffect != "close" ||
			float64(t.MaxQuantity) != t.PositionQuantity || t.NotAfter.Sub(t.NotBefore) > window {
			return "invalid exit plan trail"
		}
		if st.Underlying.ConID <= 0 || st.Underlying.SecType != "STK" || strings.TrimSpace(st.Underlying.Symbol) == "" ||
			st.Underlying.Currency == "" || !strings.EqualFold(st.Underlying.Symbol, t.Contract.Symbol) ||
			!positiveFinite(st.TrailPercent) || st.TrailPercent > queuedPlanMaxTrailPercent || !positiveFinite(st.InitialStopPrice) ||
			math.IsNaN(st.ReferencePrice) || math.IsInf(st.ReferencePrice, 0) || st.ReferencePrice < 0 {
			return "invalid exit plan stock line"
		}
	default:
		return "invalid exit plan role"
	}
	return ""
}

// queuedPlanStageConflict reads r's plan at the moment r's send intent is
// staged: stop when r is a first sale whose trail has fired, busy when r is
// a trail whose first sale is live or working at the broker.
func queuedPlanStageConflict(records map[string]*queuedAuthRecord, r *queuedAuthRecord) (stop, busy bool) {
	if queuedPlanOf(r.Terms) == nil {
		return false, false
	}
	for _, other := range records {
		if other == r || !queuedSamePlan(*other, *r) {
			continue
		}
		switch {
		case queuedPlanFirstSale(r.Terms) && queuedPlanTrail(other.Terms) && !other.Watch.TriggeredAt.IsZero():
			stop = true
		case queuedPlanTrail(r.Terms) && queuedPlanFirstSale(other.Terms) && (other.liveIntent() || other.State == rpc.QueuedAuthSent):
			busy = true
		}
	}
	return stop, busy
}

// trailLineCap is what a fired trail may sell in all: the line held when it
// fired for a whole-line trail, else the signed maximum.
func trailLineCap(r queuedAuthRecord) int {
	if st := r.Terms.OwnerOrder.StockTrail; st != nil && st.WholeLine && r.Watch.LineAtTrigger > 0 {
		return r.Watch.LineAtTrigger
	}
	return r.Terms.MaxQuantity
}

// trailStop is the exit line: the signed starting stop, raised to the high
// less the trail percent once the stock has risen far enough, to four
// decimals as prices are.
func trailStop(initial, high, trailPercent float64) float64 {
	return max(initial, roundPrice(high*(1-trailPercent/100)))
}

// trailAdvance folds one usable stock price into the durable watch: the
// first price seeds the high, a higher one raises it, and the stop follows
// and never falls. raised reports a stop or high that must be written.
func trailAdvance(w queuedTrailState, st rpc.QueuedStockTrail, price float64, at time.Time) (queuedTrailState, bool) {
	if !positiveFinite(price) {
		return w, false
	}
	next := w
	if price > next.HighWater {
		next.HighWater, next.HighAt = price, at.UTC()
	}
	next.StopPrice = max(w.StopPrice, trailStop(st.InitialStopPrice, next.HighWater, st.TrailPercent))
	return next, next.HighWater != w.HighWater || next.StopPrice != w.StopPrice
}

// isCalendarDate reports a YYYY-MM-DD date.
func isCalendarDate(v string) bool {
	_, err := time.Parse(time.DateOnly, v)
	return err == nil
}

// exitPlanLastSession walks the regular sessions the option's and the
// stock's markets both hold, from the plan's first (day, stockDay): the
// last of sessions (0: every one), never past the option's last trading
// date, and the earlier of the two closes on it. A day only one market
// trades does not count. The walk stops where the calendar stops naming a
// next session, so the signed date says how far it reached.
func (s *Server) exitPlanLastSession(market, stockMarket marketcal.Market, day, stockDay marketcal.Session, sessions int, lastTrading string) (string, time.Time) {
	date, end := day.Date, earlier(day.Close, stockDay.Close)
	at := day.Close
	for count := 1; sessions <= 0 || count < sessions; {
		closed, ok := s.previewSession(market, at)
		if !ok || closed.NextOpen == nil || !closed.NextOpen.After(at) {
			break
		}
		open := *closed.NextOpen
		next, ok := s.previewSession(market, open)
		if !ok || !next.IsOpen || next.Close.IsZero() || next.Date > lastTrading || next.Close.Sub(day.Close) > queuedPlanValidityMax {
			break
		}
		at = next.Close
		if stock, ok := s.previewSession(stockMarket, open); ok && stock.IsOpen && !stock.Close.IsZero() {
			date, end = next.Date, earlier(next.Close, stock.Close)
			count++
		}
	}
	return date, end.UTC()
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// exitPlanDigest is the lowercase hex SHA-256 of the records' terms digests
// joined, first sale then trail, by one newline.
func exitPlanDigest(digests []string) string {
	sum := sha256.Sum256([]byte(strings.Join(digests, "\n")))
	return hex.EncodeToString(sum[:])
}

// planRoleOrder sorts a plan's records first sale first.
func planRoleOrder(a, b queuedAuthRecord) int {
	rank := func(r queuedAuthRecord) int {
		if queuedPlanFirstSale(r.Terms) {
			return 0
		}
		return 1
	}
	return rank(a) - rank(b)
}

// prepareExitPlan fixes the terms of an exit plan's records from the
// broker's contract and position and returns them with their private
// references. It sends nothing.
func (e *proposalEngine) prepareExitPlan(ctx context.Context, p rpc.TradeProposalQueuePrepareParams) (out rpc.TradeProposalQueuePrepareResult, err error) {
	now, s := e.clock(), e.server
	out.AsOf = now
	block := func(code, message string) (rpc.TradeProposalQueuePrepareResult, error) {
		out.Blockers = preparedBlocker(code, message)
		return out, nil
	}
	plan := p.ExitPlan
	if p.Order != nil || p.Key != "" || p.Revision != "" || p.FullPosition || p.Quantity != 0 || p.WorstPrice != 0 {
		return block("queue_request_invalid", "An exit plan cannot be combined with an order, a proposal or a full-position mandate.")
	}
	switch {
	case plan.Trail == nil:
		return block("queue_request_invalid", "An exit plan needs its trail on the stock.")
	case !positiveFinite(plan.Trail.TrailPercent) || plan.Trail.TrailPercent > queuedPlanMaxTrailPercent:
		return block("queue_request_invalid", fmt.Sprintf("The trail percent must be above 0 and at most %d.", queuedPlanMaxTrailPercent))
	case !positiveFinite(plan.Trail.InitialStopPrice):
		return block("queue_stop_missing", "The starting stop must be a positive stock price.")
	case plan.FirstSale != nil && (plan.FirstSale.Quantity < 1 || plan.FirstSale.MinPrice != 0 && !positiveFinite(plan.FirstSale.MinPrice)):
		return block("queue_request_invalid", "The first sale needs a quantity of at least 1, and a minimum price that is positive or left out.")
	case plan.StartInMinutes != nil && (plan.WaitMinutes != 0 || *plan.StartInMinutes < 0 || *plan.StartInMinutes > queuedPlanMaxWaitMinutes):
		return block("queue_wait_invalid", fmt.Sprintf("A plan signed in the session starts now or within %d minutes, given as start_in_minutes alone.", queuedPlanMaxWaitMinutes))
	case plan.StartInMinutes == nil && (plan.WaitMinutes > queuedPlanMaxWaitMinutes || plan.WaitMinutes < 1):
		return block("queue_wait_invalid", fmt.Sprintf("The wait after the open must be between the market's opening offset and %d minutes.", queuedPlanMaxWaitMinutes))
	case plan.Contract.ConID <= 0 || !strings.EqualFold(strings.TrimSpace(plan.Contract.SecType), "OPT"):
		return block("queue_contract_inexact", "An exit plan needs the exact contract id of a held option.")
	}
	validity, sessions, carry, ok := exitPlanValidity(plan.Validity)
	if !ok {
		return block("queue_request_invalid", "The validity is day, a number of sessions such as 3d, or gtc (until cancelled).")
	}
	if !e.queued.attached() || s == nil || s.protectionPolicies == nil {
		return block("queue_unavailable", "Queued authorisations are unavailable.")
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) || p.AccountID != scope.Account || p.AccountMode != scope.Mode {
		return block("queued_scope_mismatch", "Confirm the connected account and mode before queueing.")
	}
	if s.tradingFrozen() {
		return block(rpc.ReadinessTradingFrozen, tradingFrozenBlockerMessage)
	}
	authority, err := s.captureOrderPreviewBrokerAuthority()
	if err != nil {
		return block(rpc.ReadinessBrokerUnavailable, "Broker identity could not be verified.")
	}
	contract, err := s.resolvePreviewOrderContract(ctx, authority, plan.Contract, previewMinTickTimeout)
	if err != nil {
		return block(previewContractUnresolvedCode, err.Error())
	}
	if contract.SecType != "OPT" || contract.ConID <= 0 {
		return block("queue_contract_inexact", "An exit plan covers one held single-leg option.")
	}
	// The trail sells when the stock falls to its line, which protects a
	// call; a put gains as the stock falls, so the same line would sell it
	// at its best, not protect it.
	if !strings.EqualFold(contract.Right, "C") {
		return block("queue_right_unsupported", "An exit plan's trail protects a long call; a put is not covered.")
	}
	if _, index := rpc.IndexUnderlyingContract(contract.Symbol); index {
		return block("queue_underlying_unsupported", "An exit plan trails a stock; this option's underlying is an index.")
	}
	market, known := quoteSessionMarketForContract(contract)
	session, ok := s.previewSession(market, now)
	if !known || !ok || session.State == marketcal.StateUnknown {
		return block("queue_session_unknown", "The current exchange session cannot be verified.")
	}
	offset, _ := s.readinessOpeningOffset(market, "owner_order", now)
	if plan.StartInMinutes == nil && time.Duration(plan.WaitMinutes)*time.Minute < offset {
		return block("queue_wait_invalid", fmt.Sprintf("The wait after the open must be at least the market's opening offset of %d minutes.", int(offset/time.Minute)))
	}
	if plan.StartInMinutes != nil && !session.IsOpen {
		return block("queue_wait_invalid", "start_in_minutes applies while the session is open; before the open the plan waits wait_minutes after it.")
	}
	var opens time.Time
	switch {
	case session.IsOpen && !session.Open.IsZero():
		opens = session.Open
	case session.NextOpen != nil:
		opens = *session.NextOpen
	default:
		return block("queue_session_unknown", "The trading calendar dates no next regular open for this market.")
	}
	start := opens.Add(time.Duration(plan.WaitMinutes) * time.Minute).UTC()
	switch {
	case plan.StartInMinutes != nil:
		// Signed during the session: it starts now or in a few minutes.
		start = now.Add(time.Duration(*plan.StartInMinutes) * time.Minute).UTC()
	case session.IsOpen && !now.Before(start):
		if plan.FirstSale != nil {
			return block("queue_not_offered", "The market is open past the plan's wait; give start_in_minutes to start the plan now or in a few minutes.")
		}
		start = now.UTC()
	}
	// At a stress open (the latched regime stage reads confirmed stress when
	// the plan is signed) the first sale, a discretionary sale, waits the
	// longer of the plan's wait and the stress open's offset. The trail is a
	// protective exit: it starts at the plan's own wait, never later.
	// Once the session has been open longer than the stress offset, the
	// rule has nothing left to wait for.
	firstStart := start
	if stressStart := opens.Add(readinessStressOptionsOpeningOffset).UTC(); market == marketcal.MarketUSOptions && s.regimeStressOpen(now) && stressStart.After(firstStart) && now.Before(stressStart) {
		firstStart = stressStart
	}
	day, ok := s.previewSession(market, start)
	if !ok || !day.IsOpen || day.Close.IsZero() {
		return block("queue_session_unknown", "The trading calendar does not show the regular session open at the plan's start.")
	}
	underlying, err := s.resolvePreviewOrderContract(ctx, authority, mandateOptionUnderlyingContract(strings.ToUpper(strings.TrimSpace(contract.Symbol)), contract.Currency), previewMinTickTimeout)
	if err != nil || underlying.ConID <= 0 || underlying.SecType != "STK" {
		return block("queue_underlying_unresolved", "The option's underlying stock could not be resolved at the broker.")
	}
	stockMarket, known := quoteSessionMarketForContract(underlying)
	stockDay, ok := s.previewSession(stockMarket, start)
	if !known || !ok || !stockDay.IsOpen || stockDay.Close.IsZero() {
		return block("queue_session_unknown", "The stock's regular session cannot be verified for the plan's day.")
	}
	trailEnd := earlier(day.Close, stockDay.Close).UTC()
	if !trailEnd.After(start) || !trailEnd.After(now) {
		return block("queue_session_unknown", "The plan's window would be empty.")
	}
	// A carry-over plan runs to the close of its last valid session, never
	// past the option's last trading day; on each later session the trail
	// waits the plan's wait after the open, at least the market's opening
	// offset, before it watches or sells.
	version, validUntil, resumeWait := queuedPlanTermsVersion, "", 0
	if carry {
		lastTrading := optionLastTradingDate(contract)
		if lastTrading == "" {
			return block("queue_contract_inexact", "The option's last trading day is not known, so a plan cannot carry over sessions.")
		}
		version, resumeWait = queuedPlanSessionsTermsVersion, max(plan.WaitMinutes, int(offset/time.Minute))
		validUntil, trailEnd = s.exitPlanLastSession(market, stockMarket, day, stockDay, sessions, lastTrading)
	}
	position, err := s.capturePreviewOrderPositionAuthority(ctx, s.currentTradingStatus(), contract, rpc.OrderActionSell, 1)
	if err != nil {
		return block(previewPositionUnavailableCode, err.Error())
	}
	held := position.Impact.Before
	if held < 1 || math.Trunc(held) != held || held > 1000000 {
		return block("queue_position_invalid", "An exit plan covers a held long option line of whole contracts.")
	}
	if plan.FirstSale != nil && float64(plan.FirstSale.Quantity) >= held {
		return block("queue_quantity", "The first sale must be below the held quantity; the trail covers the rest.")
	}
	if plan.FirstSale != nil && !day.Close.After(firstStart) {
		return block("queue_session_unknown", "The first sale would start at or after the close.")
	}
	var startIn *int
	if plan.StartInMinutes != nil {
		startIn = new(*plan.StartInMinutes)
	}
	reference, referenceAt := e.exitPlanReferencePrice(ctx, underlying, now)
	policy, status := s.protectionPolicies.Active()
	planID, err := randomTokenID()
	if err != nil {
		return out, err
	}
	base := rpc.TradeProposal{Contract: contract, Action: rpc.OrderActionSell, Bucket: "owner_order", OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay}
	terms := func(role string, qty int, effect string) rpc.QueuedAuthTerms {
		prop := base
		prop.Key, prop.Quantity, prop.PositionEffect, prop.PositionQuantity = fmt.Sprintf("owner:%d:SELL:%s", contract.ConID, role), qty, effect, held
		return rpc.QueuedAuthTerms{
			Version: version, Broker: scope.brokerField(), AccountID: scope.Account, AccountMode: scope.Mode, Key: prop.Key, Bucket: prop.Bucket,
			RevisionAtQueue: "owner", Contract: contract, Action: rpc.OrderActionSell, PositionEffect: effect, PositionQuantity: held, MaxQuantity: qty,
			ReferenceMarkAt: now.UTC(), MaxSpreadPctOfMid: queuedSpreadLimit(policy, prop), Currency: contract.Currency, Market: string(market),
			SessionDate: day.Date, NotBefore: start, TIF: rpc.OrderTIFDay, ArmDeadline: now.Add(queuedArmWindow).UTC(),
			RowTermsDigest: queuedRowTermsDigest(prop), PolicyFingerprint: status.EffectiveFingerprint,
			OwnerOrder: &rpc.QueuedOwnerOrder{OrderType: rpc.OrderTypeLMT, Plan: &rpc.QueuedPlan{PlanID: planID, Role: role, WaitMinutes: plan.WaitMinutes, StartInMinutes: startIn,
				Validity: validity, ValidUntil: validUntil, ResumeWaitMinutes: resumeWait}},
		}
	}
	var all []rpc.QueuedAuthTerms
	if fs := plan.FirstSale; fs != nil {
		t := terms(rpc.QueuedPlanRoleFirstSale, fs.Quantity, "reduce")
		t.Style, t.WorstPrice, t.ReferenceMark, t.NotBefore, t.NotAfter = "midpoint_bounded", fs.MinPrice, fs.MinPrice, firstStart, firstStart.Add(queuedSendWindow)
		if t.NotAfter.After(day.Close) {
			t.NotAfter = day.Close.UTC()
		}
		all = append(all, t)
	}
	trail := terms(rpc.QueuedPlanRoleTrail, int(held), "close")
	trail.Style, trail.NotAfter = rpc.QueuedAuthStyleStockTrail, trailEnd
	trail.OwnerOrder.StockTrail = &rpc.QueuedStockTrail{Underlying: underlying, TrailPercent: plan.Trail.TrailPercent, InitialStopPrice: plan.Trail.InitialStopPrice,
		ReferencePrice: reference, ReferenceAt: referenceAt, WholeLine: true}
	all = append(all, trail)
	for _, t := range all {
		if problem := ownerQueueProblem(t); problem != "" {
			return block("queue_terms_invalid", problem)
		}
	}
	if blockers := e.automaticIntentBlockersFor("", sameContractSide(base), scope); len(blockers) > 0 {
		out.Blockers = blockers
		return out, nil
	}
	recs := make([]*queuedAuthRecord, 0, len(all))
	refs := make([]string, 0, len(all))
	digests := make([]string, 0, len(all))
	for _, t := range all {
		id, reference, err := mintQueuedReference()
		if err != nil {
			return out, err
		}
		t.QueueID = id
		digest, err := queuedTermsDigest(t)
		if err != nil {
			return out, err
		}
		recs = append(recs, &queuedAuthRecord{Version: queuedDocumentVersion, ReferenceHash: sha256.Sum256([]byte(reference)), Terms: t, TermsDigest: digest,
			ContractSide: sameContractSide(base), CreatedAt: now.UTC()})
		refs, digests = append(refs, reference), append(digests, digest)
	}
	side := recs[0].ContractSide
	var refused []rpc.TradingBlocker
	err = e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, other := range records {
			if other.ContractSide != side || !sameBrokerScope(other.scope(), scope) {
				continue
			}
			switch {
			case other.liveIntent():
				refused = queuedIntentExistsBlocker(other.Terms.Key)
				return nil
			case other.State == rpc.QueuedAuthSent:
				refused = queuedOrderWorkingBlocker(other.Terms.Key)
				return nil
			case other.State == rpc.QueuedAuthPrepared:
				// A newer sheet for the contract replaces an unarmed one, a
				// plan's records included; none of them carried authority.
				events = append(events, other.resolve(queuedEventCancelled, rpc.QueuedAuthCancelled, queuedReasonSuperseded, "replaced before confirmation", now))
			}
		}
		for _, rec := range recs {
			records[rec.Terms.QueueID] = rec
			events = append(events, rec.transition(queuedEventPrepared, rpc.QueuedAuthPrepared, now))
		}
		return events
	})
	if err != nil {
		out.Blockers = preparedBlocker("queue_not_persisted", "The exit plan could not be stored; nothing was queued.")
		return out, err
	}
	if len(refused) > 0 {
		out.Blockers = refused
		return out, nil
	}
	prepared := &rpc.QueuedPlanPrepared{PlanID: planID, PlanDigest: exitPlanDigest(digests), QueuedRefs: refs}
	for _, rec := range recs {
		prepared.Queues = append(prepared.Queues, e.queuedView(*rec))
	}
	out.Accepted, out.Plan = true, prepared
	return out, nil
}

// exitPlanReferencePrice is the stock price shown with the plan for the
// owner's review only: the live midpoint, else the last or the close. Zero
// when none is known; nothing executes on it.
func (e *proposalEngine) exitPlanReferencePrice(ctx context.Context, underlying rpc.ContractParams, now time.Time) (float64, time.Time) {
	q, err := e.server.previewQuote(ctx, underlying, queuedTrailQuoteTimeout)
	if err != nil {
		return 0, now.UTC()
	}
	at := q.AsOf
	if at.IsZero() {
		at = now
	}
	for _, v := range []*float64{q.Midpoint, q.Last, q.Mark} {
		if v != nil && positiveFinite(*v) {
			return roundPrice(*v), at.UTC()
		}
	}
	return 0, at.UTC()
}

// armExitPlan arms every record of one plan in one store update, or none.
// The plan digest binds the set of terms the owner signed.
func (e *proposalEngine) armExitPlan(ctx context.Context, p rpc.TradeProposalQueueArmParams, out rpc.TradeProposalQueueResult) (rpc.TradeProposalQueueResult, error) {
	now := e.clock()
	block := func(code, message string) (rpc.TradeProposalQueueResult, error) {
		out.Blockers = preparedBlocker(code, message)
		return out, nil
	}
	if p.QueuedRef != "" || p.TermsDigest != "" || len(p.QueuedRefs) > 4 {
		return block("queue_request_invalid", "Arm a plan with its references and plan digest only.")
	}
	var recs []queuedAuthRecord
	for _, ref := range p.QueuedRefs {
		rec, ok := e.loadQueuedByReference(ref)
		if !ok {
			return block("queued_reference_unavailable", "A queued authorisation reference cannot be resolved safely; nothing was armed.")
		}
		if slices.ContainsFunc(recs, func(r queuedAuthRecord) bool { return r.Terms.QueueID == rec.Terms.QueueID }) {
			return block("queue_request_invalid", "A plan reference was given twice; nothing was armed.")
		}
		recs = append(recs, rec)
	}
	if len(recs) == 0 || queuedPlanOf(recs[0].Terms) == nil {
		return block("queued_plan_invalid", "These references do not name an exit plan; nothing was armed.")
	}
	planID := queuedPlanOf(recs[0].Terms).PlanID
	members := 0
	for _, r := range e.queued.list() {
		if queuedSamePlan(r, recs[0]) {
			members++
		}
	}
	for _, r := range recs {
		if !queuedSamePlan(r, recs[0]) {
			return block("queued_plan_invalid", "The references name records of different plans; nothing was armed.")
		}
	}
	if members != len(recs) {
		return block("queued_plan_incomplete", "Every record of the plan must be armed together; nothing was armed.")
	}
	slices.SortFunc(recs, planRoleOrder)
	digests := make([]string, 0, len(recs))
	for _, r := range recs {
		if problem := queuedRecordProblem(r); problem != "" {
			return block("queued_record_invalid", "This exit plan cannot be armed: "+problem+"; nothing was armed.")
		}
		digests = append(digests, r.TermsDigest)
	}
	signed := strings.ToLower(strings.TrimSpace(p.PlanDigest))
	if subtle.ConstantTimeCompare([]byte(signed), []byte(exitPlanDigest(digests))) != 1 {
		return block("queued_terms_mismatch", "The signed plan digest does not match the prepared terms; nothing was armed.")
	}
	for _, r := range recs {
		if r.State != rpc.QueuedAuthPrepared {
			return block("queued_not_prepared", "A record of this plan is already "+r.State+"; nothing was armed.")
		}
		if !now.Before(r.Terms.ArmDeadline) {
			for _, r := range recs {
				e.expireUnarmed(ctx, r.Terms.QueueID, now)
			}
			return block("queued_arm_deadline_passed", "The owner's signature arrived after the ten-minute arm deadline; prepare the plan again.")
		}
	}
	scope := e.currentScope()
	if !brokerScopeConcrete(scope) || !sameBrokerScope(scope, recs[0].scope()) {
		return block("queued_scope_mismatch", "The connected account or paper/live mode differs from the one the plan was prepared for.")
	}
	for _, r := range recs {
		if blockers := e.automaticIntentBlockersFor(r.Terms.Key, r.ContractSide, scope); len(blockers) > 0 {
			out.Blockers = blockers
			return out, nil
		}
	}
	book := e.automaticSettlingBook(ctx, scope, false)
	var refused []rpc.TradingBlocker
	var armed []queuedAuthRecord
	err := e.queued.update(ctx, now, func(records map[string]*queuedAuthRecord) []queuedAuthEvent {
		var events []queuedAuthEvent
		for _, rec := range recs {
			r := records[rec.Terms.QueueID]
			if r == nil || r.State != rpc.QueuedAuthPrepared || !now.Before(r.Terms.ArmDeadline) || r.TermsDigest != rec.TermsDigest {
				refused = preparedBlocker("queued_not_prepared", "The exit plan changed while it was being armed; nothing was armed.")
				return nil
			}
			for _, other := range records {
				if other == r || other.ContractSide != r.ContractSide || !sameBrokerScope(other.scope(), r.scope()) || queuedSamePlan(*other, *r) {
					continue
				}
				switch {
				case other.liveIntent():
					refused = queuedIntentExistsBlocker(other.Terms.Key)
					return nil
				case other.State == rpc.QueuedAuthSent:
					refused = queuedOrderWorkingBlocker(other.Terms.Key)
					return nil
				}
			}
		}
		armed = armed[:0]
		for _, rec := range recs {
			r := records[rec.Terms.QueueID]
			r.ArmedAt, r.DeskActionID, r.Credential, r.Envelope = now.UTC(), strings.TrimSpace(p.DeskActionID), strings.TrimSpace(p.Credential), p.Envelope
			r.ArmOrigin = normalizedWriteOrigin(p.Origin)
			r.SettlingBaseline, r.SettlingBaselineKnown = book.marks(), book.ok
			ev := r.transition(queuedEventArmed, rpc.QueuedAuthArmed, now)
			ev.Origin, ev.Credential = r.ArmOrigin, r.Credential
			events = append(events, ev)
			armed = append(armed, *r)
		}
		return events
	})
	if err != nil {
		return block("queue_not_persisted", "The arm could not be stored; nothing was armed.")
	}
	if len(refused) > 0 {
		out.Blockers = refused
		return out, nil
	}
	for _, r := range armed {
		out.Queues = append(out.Queues, e.queuedView(r))
	}
	out.Accepted = true
	out.Message = fmt.Sprintf("armed: exit plan %s starts at %s and ends at %s if every gate still passes", planID,
		armed[0].Terms.NotBefore.Format(time.RFC3339), armed[len(armed)-1].Terms.NotAfter.Format(time.RFC3339))
	e.server.infof("exit plan %s armed (%d records); starts %s", planID, len(armed), armed[0].Terms.NotBefore.Format(time.RFC3339))
	e.Kick()
	return out, nil
}

// queuedView is a record's read view; a trail record carries its watch.
func (e *proposalEngine) queuedView(r queuedAuthRecord) rpc.QueuedAuth {
	v := r.view()
	if !queuedPlanTrail(r.Terms) {
		return v
	}
	now := e.clock()
	w := &rpc.QueuedTrailWatch{HighWater: r.Watch.HighWater, StopPrice: r.Watch.StopPrice, TriggeredAt: r.Watch.TriggeredAt, TriggerPrice: r.Watch.TriggerPrice,
		SaleAttempts: r.Watch.SaleAttempts, SaleFilled: r.Watch.SaleFilled}
	if !r.Watch.TriggeredAt.IsZero() {
		w.SaleStep = trailSaleSteps[trailStep(r.Watch.SaleStep)].name
	}
	if r.State == rpc.QueuedAuthSent || r.State == rpc.QueuedAuthSending || r.OrderRef != "" {
		w.SaleLimit = r.LimitPrice
		if r.State == rpc.QueuedAuthSent {
			w.SaleFilled += int(math.Round(r.FilledQuantity))
		}
	}
	if w.StopPrice == 0 && r.Terms.OwnerOrder.StockTrail != nil {
		w.StopPrice = r.Terms.OwnerOrder.StockTrail.InitialStopPrice
	}
	live, seen := e.trailLiveFor(r.Terms.QueueID)
	if seen && live.usableAt.After(time.Time{}) {
		w.StockPrice, w.StockPriceAt = live.price, live.usableAt
	}
	// Between sessions, or in a later session's opening wait, a carry-over
	// trail says when it watches again.
	sess := trailSession{in: true}
	if r.waiting() && !now.Before(r.Terms.NotBefore) && now.Before(r.Terms.NotAfter) {
		sess = e.trailSessionAt(r, now)
	}
	switch {
	case !r.Watch.TriggeredAt.IsZero():
		w.State = rpc.QueuedWatchTriggered
		switch {
		case !sess.in:
			w.ReasonCode, w.Reason, w.NextWatchAt = sess.code, sess.reason, sess.next
		case r.HoldCode != "":
			w.ReasonCode, w.Reason = r.HoldCode, r.HoldReason
		case r.State == rpc.QueuedAuthSent && r.Watch.SaleCancelAt.After(r.SentAt):
			w.ReasonCode, w.Reason = queuedTrailSaleCancelling, "the sale did not fill at its limit; it is being cancelled at the broker to send again at the bid once it has ended"
		case r.State == rpc.QueuedAuthSent:
			w.ReasonCode, w.Reason = queuedTrailSaleWorking, "the sale is working at the broker at the bid; it is sent again at the fresh bid if it has not filled within 30 seconds"
		}
	case r.State == rpc.QueuedAuthPrepared || now.Before(r.Terms.NotBefore):
		w.State, w.NextWatchAt = rpc.QueuedWatchWaiting, r.Terms.NotBefore
	case !sess.in:
		w.State, w.ReasonCode, w.Reason, w.NextWatchAt = rpc.QueuedWatchWaiting, sess.code, sess.reason, sess.next
	case r.State == rpc.QueuedAuthHeld && r.HoldCode == queuedHoldPolicyChanged:
		// Signed authority: the plan stops watching until the owner signs it
		// again, and says so at once.
		w.State, w.ReasonCode, w.Reason = rpc.QueuedWatchUnwatched, r.HoldCode, r.HoldReason
	case r.waiting() && seen && live.code == "" && now.Sub(live.usableAt) <= queuedTrailWatchFresh:
		w.State = rpc.QueuedWatchWatching
	default:
		w.State = rpc.QueuedWatchUnwatched
		switch {
		case r.final():
			w.ReasonCode, w.Reason = nonEmptyString(r.ReasonCode, r.State), nonEmptyString(r.Reason, "the trail has ended")
		case r.State == rpc.QueuedAuthHeld && r.HoldCode != "" && r.HoldCode != queuedHoldStockQuote:
			// A hold other than the stock's quote (a changed protection
			// policy, a freeze) is what stops the trail; say that.
			w.ReasonCode, w.Reason = r.HoldCode, r.HoldReason
		case seen && live.code != "":
			w.ReasonCode, w.Reason = live.code, live.reason
		default:
			w.ReasonCode, w.Reason = queuedHoldStockQuote, "no usable stock price has been read recently; nothing can fire until one is"
		}
	}
	v.Watch = w
	return v
}
