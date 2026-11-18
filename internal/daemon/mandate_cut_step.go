package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// An unfilled mandate cut steps toward the bid (owner decision 2026-11-17
// 09:33 CET; internal-docs/design/mandate-risk-budget.md).
//
// A cut the pre-authorised scheduler placed is a day limit that used to rest
// where it was priced: halfway to the bid, at the bid, or at the mid. In a
// falling market the mandate's cut could stand unfilled all session while the
// row behind it waited. Now, once [mandate.reduction]
// unfilled_step_after_minutes have passed with any quantity left, the order's
// limit moves to the current bid, at most unfilled_steps times, each step
// that long after the one before.
//
// What bounds a step is what the owner already set: never below the worst
// price the order was placed under (25% under the row's mark), and, while the
// spread is wider than the line's cap, never a give against the mid above
// cheap_leg_give_pct_of_loss of the loss the cut removes. A bid beyond the
// bound moves the limit to the bound; a limit already there rests. A step
// only ever lowers a sale's limit.
//
// A step is taken only while the cut is still wanted and still authorised:
// the breach's row for the contract is served and not held, the session is
// past its opening offset, the quote is live and two-sided, the bucket is
// pre-authorised and the arming the order was placed under still stands. It
// needs no veto window of its own: the window the order waited out covered
// it down to the worst price. Orders the owner confirmed, queued
// authorisations, stops and strategy orders are never stepped.
//
// The write is the ordinary modify path with the daemon-preauthorised origin
// and a grant that names the one order and its new limit
// (automaticStepGrantCovers). Each step is recorded before the broker call,
// so a restart or an uncertain send never repeats it.

const (
	// automaticEventStepAttempted journals a step recorded before its broker
	// call; automaticEventStepped the broker path accepting it.
	automaticEventStepAttempted = "step_attempted"
	automaticEventStepped       = "stepped"
	// automaticEventStepRests journals why a due cut takes no step.
	automaticEventStepRests = "step_rests"

	// mandateCutStepRetry paces a step that could not be taken (no quote,
	// the session's opening offset, a refused preview) and the reading of a
	// limit that rests at its bound.
	mandateCutStepRetry = time.Minute

	// mandateCutStepMaxAge ends the stepping of a record: a day order placed
	// this long ago no longer works.
	mandateCutStepMaxAge = 24 * time.Hour

	mandateCutStepTimeout = 5 * time.Second
)

// automaticCutStep is a placed mandate cut's stepping state: the bounds it
// was placed under and the steps taken since.
type automaticCutStep struct {
	// WorstPrice is the lowest limit a step may set, fixed at placement.
	WorstPrice float64 `json:"worst_price"`
	// SpreadCapPct is the line's spread cap in percent of the mid; beyond it
	// the give bound also holds.
	SpreadCapPct float64 `json:"spread_cap_pct"`
	// GivePerUnitBase is the most one unit may give against the mid, in the
	// base currency, while the spread is beyond the cap; zero when no bound
	// is written, and such a spread then takes no step.
	GivePerUnitBase float64 `json:"give_per_unit_base,omitempty"`
	// Count is the steps spent, each recorded before its broker call; At,
	// From, To and TokenID describe the last, Of how many the policy allowed
	// then. Confirmed says the modify path accepted it; a step spent and not
	// confirmed was attempted with an outcome that is not known.
	Count     int       `json:"count,omitempty"`
	Of        int       `json:"of,omitempty"`
	At        time.Time `json:"at,omitzero"`
	From      float64   `json:"from,omitempty"`
	To        float64   `json:"to,omitempty"`
	TokenID   string    `json:"token_id,omitempty"`
	Confirmed bool      `json:"confirmed,omitempty"`
	// Rests says why the limit takes no step right now.
	Rests string `json:"rests,omitempty"`
	// Done ends the stepping: the order filled, was cancelled or expired.
	Done bool `json:"done,omitempty"`
}

// automaticStepGrant is the step half of the scheduler's write grant: the
// one order whose limit may change, the limit it changes to and the lowest
// limit its placement allows.
type automaticStepGrant struct {
	OrderRef        string
	ReservedOrderID int
	Limit           float64
	Floor           float64
}

// stepDue reports a placed mandate cut whose next step has come due.
func (r automaticSubmissionRecord) stepDue(now time.Time, wait time.Duration, steps int) bool {
	if r.State != rpc.TradeProposalAutomaticSubmitted || r.EngineBucket != rpc.TradeProposalBucketMandateReduction ||
		r.Step == nil || r.Step.Done || r.OrderRef == "" || r.Step.Count >= steps || wait <= 0 || r.SubmittedAt.IsZero() {
		return false
	}
	since := r.SubmittedAt
	if r.Step.Count > 0 {
		since = r.Step.At
	}
	return !now.Before(since.Add(wait)) && now.Sub(r.SubmittedAt) < mandateCutStepMaxAge
}

// constitutionMandate is the [mandate] in force, or nil.
func (e *proposalEngine) constitutionMandate() *risk.ConstitutionMandate {
	if e == nil || e.server == nil || e.server.riskPolicies == nil {
		return nil
	}
	if c := e.server.riskPolicies.snapshot().policy; c != nil {
		return c.Mandate
	}
	return nil
}

// bounded is the bound one step's preview prices inside: the bid, never
// below the worst price, and beyond the spread cap no further from the mid
// than the give allows. fx is base currency per contract currency; without
// it no give can be measured and a spread beyond the cap takes no step.
func (b automaticCutStep) bounded(contract rpc.ContractParams, fx float64) *rpc.OrderBoundedLimit {
	out := &rpc.OrderBoundedLimit{Concession: 1, WorstPrice: b.WorstPrice, MaxSpreadPctOfMid: b.SpreadCapPct, ClampToWorst: true}
	if b.GivePerUnitBase > 0 && positiveFinite(fx) {
		out.MaxGive = b.GivePerUnitBase / (float64(contractMultiplier(contract)) * fx)
	}
	return out
}

// mandateCutStepBounds is the stepping state a mandate sale starts with at
// placement, from the same bounds its price was given under. Nil for any
// other row.
func (e *proposalEngine) mandateCutStepBounds(prop rpc.TradeProposal) *automaticCutStep {
	if !strings.EqualFold(strings.TrimSpace(prop.Action), rpc.OrderActionSell) {
		return nil
	}
	bound := e.mandateReductionGive(prop)
	if bound == nil {
		return nil
	}
	step := &automaticCutStep{WorstPrice: bound.WorstPrice, SpreadCapPct: bound.MaxSpreadPctOfMid}
	if pct, ok := e.constitutionMandate().CheapLegGivePct(); ok && pct > 0 && strings.EqualFold(prop.Contract.SecType, "OPT") &&
		prop.Mandate != nil && prop.Mandate.LossCutBase > 0 && prop.Mandate.Units > 0 {
		step.GivePerUnitBase = pct / 100 * prop.Mandate.LossCutBase / float64(prop.Mandate.Units)
	}
	return step
}

func formatStepPrice(p float64) string {
	if math.Abs(p*100-math.Round(p*100)) < 1e-6 {
		return fmt.Sprintf("%.2f", p)
	}
	return fmt.Sprintf("%.4f", p)
}

// says is the plain statement of where a placed cut's limit stands: the
// last step taken or attempted, and why it rests now.
func (b automaticCutStep) says() string {
	var parts []string
	switch {
	case b.Count > 0 && b.Confirmed:
		parts = append(parts, fmt.Sprintf("unfilled, so Canary moved its limit from %s to %s (step %d of %d)", formatStepPrice(b.From), formatStepPrice(b.To), b.Count, b.Of))
	case b.Count > 0:
		parts = append(parts, fmt.Sprintf("unfilled, so Canary tried to move its limit from %s to %s; whether the broker took the change is not known (step %d of %d)", formatStepPrice(b.From), formatStepPrice(b.To), b.Count, b.Of))
	}
	if b.Rests != "" {
		parts = append(parts, b.Rests)
	}
	return strings.Join(parts, "; ")
}

// automaticPlacedReason is a placed record's reason; a mandate cut adds
// where its limit stands.
const automaticPlacedReason = "placed by the daemon under the pre-authorised protection policy"

func mandateCutStepReason(step automaticCutStep) string {
	if says := step.says(); says != "" {
		return automaticPlacedReason + "; " + says
	}
	return automaticPlacedReason
}

// stepNote is what a served mandate row says about the sale working ahead
// of it: the latest placed record of its key in its breach episode, while
// that order is still being stepped. The row itself is of a later revision
// and waits behind the order, so its own record cannot say it.
func (s *automaticSubmissionStore) stepNote(p rpc.TradeProposal) string {
	if s == nil || p.Bucket != rpc.TradeProposalBucketMandateReduction || p.Mandate == nil || p.Mandate.Episode == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var last *automaticSubmissionRecord
	for _, rec := range s.records {
		if rec.Key != p.Key || rec.Episode != p.Mandate.Episode || rec.State != rpc.TradeProposalAutomaticSubmitted || rec.Step == nil || rec.Step.Done {
			continue
		}
		if last == nil || rec.SubmittedAt.After(last.SubmittedAt) {
			last = rec
		}
	}
	if last == nil {
		return ""
	}
	return last.Step.says()
}

// mandateCutWanted returns the breach's served row for a placed cut: the
// same row key, still a mandate reduction of the record's episode. Without
// it the breach ended or the plan dropped the line, and the order keeps its
// limit.
func mandateCutWanted(snap rpc.TradeProposalSnapshot, rec automaticSubmissionRecord) (rpc.TradeProposal, bool) {
	for _, prop := range snap.Proposals {
		if prop.Key == rec.Key && prop.Bucket == rpc.TradeProposalBucketMandateReduction && prop.Mandate != nil &&
			prop.Mandate.Episode != "" && prop.Mandate.Episode == rec.Episode {
			return prop, true
		}
	}
	return rpc.TradeProposal{}, false
}

// mandateCutSnapshotUnconfirmed says why snap cannot tell whether a placed
// cut is still wanted, or "": the scheduler places nothing from a snapshot
// that was restored from state or carries blockers of its own
// (proposalUnblocked), and a failed or stalled refresh keeps the last rows
// served, so a step asks the same and that the snapshot is no older than two
// cadences.
func (e *proposalEngine) mandateCutSnapshotUnconfirmed(snap rpc.TradeProposalSnapshot, now time.Time) string {
	switch {
	case snap.LoadedFromState:
		return "the proposals on hand are from before the restart; the breach is read again first"
	case len(snap.Blockers) > 0:
		return "the proposal set is blocked (" + snap.Blockers[0].Code + "), so the breach cannot be confirmed right now"
	case now.Sub(snap.AsOf) > e.snapshotMaxAge():
		return "the proposals on hand are too old to confirm the breach"
	}
	return ""
}

// mandateCutChangedByHand reports a modify of the order that the scheduler
// did not issue. The order is then the owner's and keeps the limit it was
// given (owner, 2026-10-22 15:26 CEST). A change made in TWS itself leaves
// no such event and is not seen here.
func mandateCutChangedByHand(events map[string][]rpc.OrderEvent, orderRef string) bool {
	for _, rows := range events {
		for _, ev := range rows {
			if ev.OrderRef == orderRef && ev.Type == orderJournalEventModifyRequested && ev.Origin != rpc.OrderOriginDaemonPreAuthorised {
				return true
			}
		}
	}
	return false
}

// runMandateCutSteps is one stepping pass. It runs after the scheduler's
// cycle in the engine's Run loop.
func (e *proposalEngine) runMandateCutSteps(ctx context.Context) {
	if e == nil || e.server == nil || ctx == nil || ctx.Err() != nil || !e.server.orderBrokerWritesEnabled() || !e.automatic.attached() {
		return
	}
	wait, steps, ok := e.constitutionMandate().UnfilledCutSteps()
	if !ok {
		return
	}
	now := e.clock()
	for _, rec := range e.automatic.list() {
		if !rec.stepDue(now, wait, steps) || !e.mandateCutStepTry(automaticRecordKey(rec.Key, rec.Revision), now) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		key := automaticRecordKey(rec.Key, rec.Revision)
		outcome, err := e.stepMandateCut(ctx, rec, wait, steps)
		switch {
		case err != nil:
			// A step that cannot be attempted is tried again every minute;
			// it is logged once per reason, not once per attempt.
			if e.mandateCutStepFirstLog(key, err.Error()) {
				e.server.infof("mandate cut step: %s order %s not stepped: %v", rec.Symbol, rec.OrderRef, err)
			}
		case outcome != "":
			e.mandateCutStepFirstLog(key, "")
			e.server.infof("mandate cut step: %s order %s: %s", rec.Symbol, rec.OrderRef, outcome)
		}
	}
}

// mandateCutStepFirstLog reports whether reason is new for the record.
func (e *proposalEngine) mandateCutStepFirstLog(key, reason string) bool {
	e.cutStepMu.Lock()
	defer e.cutStepMu.Unlock()
	if e.cutStepLogged == nil {
		e.cutStepLogged = map[string]string{}
	}
	if last, ok := e.cutStepLogged[key]; ok && last == reason {
		return false
	}
	e.cutStepLogged[key] = reason
	return true
}

// mandateCutStepTry paces the attempts for one record and reports whether
// one may be made now.
func (e *proposalEngine) mandateCutStepTry(key string, now time.Time) bool {
	e.cutStepMu.Lock()
	defer e.cutStepMu.Unlock()
	if next, ok := e.cutStepNext[key]; ok && now.Before(next) {
		return false
	}
	if e.cutStepNext == nil {
		e.cutStepNext = map[string]time.Time{}
	}
	for k, next := range e.cutStepNext {
		if now.Sub(next) > mandateCutStepMaxAge {
			delete(e.cutStepNext, k)
			delete(e.cutStepLogged, k)
		}
	}
	e.cutStepNext[key] = now.Add(mandateCutStepRetry)
	return true
}

// noteMandateCutRests records why a due cut takes no step, once per reason;
// done ends the record's stepping.
func (e *proposalEngine) noteMandateCutRests(ctx context.Context, rec automaticSubmissionRecord, rests string, done bool) {
	if rec.Step == nil || (rec.Step.Rests == rests && rec.Step.Done == done) {
		return
	}
	err := e.automatic.update(ctx, func(records map[string]*automaticSubmissionRecord) []automaticSubmissionEvent {
		r, ok := records[automaticRecordKey(rec.Key, rec.Revision)]
		if !ok || r.Step == nil || (r.Step.Rests == rests && r.Step.Done == done) {
			return nil
		}
		step := *r.Step
		step.Rests, step.Done = rests, done
		r.Step = &step
		if !done {
			r.Reason = mandateCutStepReason(step)
		}
		return []automaticSubmissionEvent{{At: e.clock(), Type: automaticEventStepRests, Key: r.Key, Revision: r.Revision, Bucket: r.Bucket, State: r.State, AccountID: r.AccountID, AccountMode: r.AccountMode, OrderRef: r.OrderRef, Reason: rests}}
	})
	if err != nil {
		e.server.warnf("mandate cut step: record why %s rests: %v", rec.Key, err)
	}
}
