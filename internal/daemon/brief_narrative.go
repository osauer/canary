package daemon

import (
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The narrative Briefing.
//
// This file is the whole prose surface of the brief, so copy review is one
// file read. It composes a chief-of-staff reading of the two movements the
// daemon has ALREADY composed: it takes rpc.BriefResult and returns runs. It
// reads nothing else - no journals, no snapshots, no clock - so the same
// payload always yields the same prose, and no sentence can outrun the rows
// the surfaces render beside it.
//
// Three rules bind every template below:
//
//  1. Only served facts. Figures are interpolated from the payload with the
//     payload's own base currency; thresholds, causes, and consequences are
//     never invented. Where the rows assert a cause ("engaged and remains so
//     until a human reset"), the prose may repeat that assertion and nothing
//     stronger.
//  2. Only served statuses decide tone. A clause may carry the watch or act
//     role only when its own row is attention-class (or, for stress, when the
//     served severity is watch or act). Degraded and unavailable are data
//     conditions: they are stated in plain words and never tinted.
//  3. Silence is never clean. An unavailable or degraded input is named as
//     unavailable or degraded; it is never dropped and never folded into a
//     clean summary clause.
//
// Shape: quiet composes short. Each movement is built from blocks, and a
// block that is entirely unflagged folds into a single summary clause on the
// previous paragraph instead of opening one of its own.

// briefProse accumulates runs into paragraphs. Callers push text spans in
// reading order; sentence() marks that a separator is owed before the next
// span, so templates never carry stray leading or trailing spaces.
type briefProse struct {
	paragraphs []rpc.BriefParagraph
	runs       []rpc.BriefRun
	pending    bool
}

func (p *briefProse) push(role, text string) {
	if text == "" {
		return
	}
	if p.pending {
		p.pending = false
		if len(p.runs) > 0 {
			p.runs = append(p.runs, rpc.BriefRun{Text: " "})
		}
	}
	p.runs = append(p.runs, rpc.BriefRun{Text: text, Role: role})
}

// sentence marks a sentence boundary. It is a no-op at the start of a
// paragraph, so a folded block never opens with whitespace.
func (p *briefProse) sentence() { p.pending = true }

func (p *briefProse) text(text string)              { p.push("", text) }
func (p *briefProse) figure(text string)            { p.push(rpc.BriefRunRoleFigure, text) }
func (p *briefProse) tinted(role, text string)      { p.push(role, text) }
func (p *briefProse) tintedFigure(role, fig string) { p.push(briefFigureRole(role), fig) }

// paragraph closes the current paragraph. Empty paragraphs are dropped.
func (p *briefProse) paragraph() {
	if len(p.runs) == 0 {
		p.pending = false
		return
	}
	p.paragraphs = append(p.paragraphs, rpc.BriefParagraph{Runs: briefMergeRuns(p.runs)})
	p.runs, p.pending = nil, false
}

func (p *briefProse) done() []rpc.BriefParagraph {
	p.paragraph()
	return p.paragraphs
}

// briefFigureRole keeps a figure inside a tinted clause tinted: a watch-class
// number must not read as a neutral readout in the middle of a watch clause.
func briefFigureRole(role string) string {
	if role == "" {
		return rpc.BriefRunRoleFigure
	}
	return role
}

// briefMergeRuns collapses adjacent same-role runs so the wire carries one
// span per tone change instead of one span per template fragment.
func briefMergeRuns(runs []rpc.BriefRun) []rpc.BriefRun {
	out := make([]rpc.BriefRun, 0, len(runs))
	for _, run := range runs {
		if run.Text == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == run.Role {
			out[len(out)-1].Text += run.Text
			continue
		}
		out = append(out, run)
	}
	return out
}

// briefTopic is one narrated row: a fixed label, the row state that decides
// whether it is flagged, and the role its clauses may carry. Posture marks the
// market reading (stress), which sets the tone of the whole brief but is never
// something the desk signs off, so it is narrated rather than listed as owed.
type briefTopic struct {
	label   string
	state   rpc.BriefRowState
	role    string
	posture bool
}

func (t briefTopic) unread() bool {
	return t.state.Status == rpc.BriefStatusDegraded || t.state.Status == rpc.BriefStatusUnavailable
}

// briefRole maps a row state onto the tint its clauses may carry. Attention
// is the brief's risk vocabulary, so only attention tints; act is reserved
// for rows whose own served fields assert the strongest state their vocabulary
// has (a rule worsened to act, a breached block tier, an engaged latch).
func briefRole(state rpc.BriefRowState, actClass bool) string {
	if state.Status != rpc.BriefStatusAttention {
		return ""
	}
	if actClass {
		return rpc.BriefRunRoleAct
	}
	return rpc.BriefRunRoleWatch
}

// briefStressRole reads the tint off the stress row's own served severity -
// the daemon's severity vocabulary, verbatim - rather than off the row status,
// which reports input quality for this row.
func briefStressRole(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case string(risk.SeverityAct):
		return rpc.BriefRunRoleAct
	case string(risk.SeverityWatch):
		return rpc.BriefRunRoleWatch
	default:
		return ""
	}
}

func briefRulesDeltaActClass(row rpc.BriefRulesDeltaRow) bool {
	for _, transition := range row.Transitions {
		if transition.To == risk.RuleStatusAct {
			return true
		}
	}
	return false
}

// composeBriefNarrative is the entry point: a pure projection of the composed
// movements onto prose. Nil in, nil out.
func composeBriefNarrative(res *rpc.BriefResult) *rpc.BriefNarrative {
	if res == nil {
		return nil
	}
	topics := briefTopics(res)
	return &rpc.BriefNarrative{
		Lead:   briefNarrativeLead(res, topics),
		Review: briefNarrativeReview(res.Review, res.Ready.Session),
		Ready:  briefNarrativeReady(res.Ready),
		Coda:   briefNarrativeCoda(topics),
	}
}

// briefTopics lists every narrated row of both movements in reading order.
// The lead and the coda count and name flagged topics from this one list, so
// they can never disagree with the paragraphs below them.
func briefTopics(res *rpc.BriefResult) []briefTopic {
	review, ready := res.Review, res.Ready
	topics := []briefTopic{
		{label: "session P/L", state: review.SessionPnL.BriefRowState, role: briefRole(review.SessionPnL.BriefRowState, false)},
		{label: "attribution", state: review.Attribution.BriefRowState, role: briefRole(review.Attribution.BriefRowState, false)},
		{label: "rules delta", state: review.RulesDelta.BriefRowState, role: briefRole(review.RulesDelta.BriefRowState, briefRulesDeltaActClass(review.RulesDelta))},
		{label: "proposals", state: review.Proposals.BriefRowState, role: briefRole(review.Proposals.BriefRowState, false)},
		{label: "overrides", state: review.Overrides.BriefRowState, role: briefRole(review.Overrides.BriefRowState, false)},
		{label: "capital events", state: review.CapitalEvents.BriefRowState, role: briefRole(review.CapitalEvents.BriefRowState, review.CapitalEvents.Latched)},
		{label: "reconcile", state: review.Reconcile.BriefRowState, role: briefRole(review.Reconcile.BriefRowState, false)},
		{label: "auto-extend", state: review.AutoExtend.BriefRowState, role: briefRole(review.AutoExtend.BriefRowState, false)},
		{label: "sign-off", state: review.OneTap.BriefRowState, role: briefRole(review.OneTap.BriefRowState, false)},
		{label: "working orders", state: review.WorkingOrders.BriefRowState, role: briefRole(review.WorkingOrders.BriefRowState, false)},
		{label: "regime", state: ready.Regime.BriefRowState, role: briefRole(ready.Regime.BriefRowState, false)},
		{label: "breadth", state: ready.Breadth.BriefRowState, role: briefRole(ready.Breadth.BriefRowState, false)},
		{label: "dealer gamma", state: ready.Gamma.BriefRowState, role: briefRole(ready.Gamma.BriefRowState, false)},
		{label: "stress", state: ready.Stress.BriefRowState, role: briefStressRole(ready.Stress.Severity), posture: true},
		{label: "session", state: ready.Session.BriefRowState, role: briefRole(ready.Session.BriefRowState, false)},
	}
	if len(ready.MarketEvents) == 0 {
		// No rows at all is an unread source, not a clean book: the lead and
		// the coda must count it exactly as the paragraph states it.
		topics = append(topics, briefTopic{label: "held-name events", state: briefUnavailable("held-name event coverage is unavailable")})
	}
	for _, event := range ready.MarketEvents {
		topics = append(topics, briefTopic{
			label: "held-name " + briefEventKindLabel(event.Kind),
			state: event.BriefRowState,
			role:  briefRole(event.BriefRowState, false),
		})
	}
	topics = append(topics,
		briefTopic{label: "capital", state: ready.Capital.BriefRowState, role: briefRole(ready.Capital.BriefRowState, ready.Capital.Tier == risk.CapitalTierBlock)},
		briefTopic{label: "drawdown latch", state: ready.Latch.BriefRowState, role: briefRole(ready.Latch.BriefRowState, ready.Latch.Latched)},
		briefTopic{label: "premium at risk", state: ready.PremiumAtRisk.BriefRowState, role: briefRole(ready.PremiumAtRisk.BriefRowState, false)},
		briefTopic{label: "hedge cost", state: ready.HedgeCost.BriefRowState, role: briefRole(ready.HedgeCost.BriefRowState, false)},
		briefTopic{label: "protection proposals", state: ready.Proposals.BriefRowState, role: briefRole(ready.Proposals.BriefRowState, false)},
		briefTopic{label: "policy drift", state: ready.PolicyDrift.BriefRowState, role: briefRole(ready.PolicyDrift.BriefRowState, false)},
		briefTopic{label: "artefacts", state: ready.Artefacts.BriefRowState, role: briefRole(ready.Artefacts.BriefRowState, false)},
	)
	if ready.MonthlyPulse != nil {
		state := briefMonthlyPulseRollupState(ready.MonthlyPulse.Status)
		topics = append(topics, briefTopic{label: "monthly pulse", state: state, role: briefRole(state, false)})
	}
	return topics
}

// briefFlaggedTopics lists the rows that carry a decision. The posture row is
// excluded: the lead and the coda state the stress reading in their own words,
// and a market reading is not something the desk owes an answer to.
func briefFlaggedTopics(topics []briefTopic) []briefTopic {
	out := make([]briefTopic, 0, len(topics))
	for _, topic := range topics {
		if topic.role != "" && !topic.posture {
			out = append(out, topic)
		}
	}
	return out
}

// briefPostureFlagged reports whether the market reading itself carries tone.
func briefPostureFlagged(topics []briefTopic) bool {
	for _, topic := range topics {
		if topic.posture && topic.role != "" {
			return true
		}
	}
	return false
}

func briefUnreadTopics(topics []briefTopic) []briefTopic {
	out := make([]briefTopic, 0, len(topics))
	for _, topic := range topics {
		if topic.unread() {
			out = append(out, topic)
		}
	}
	return out
}

func briefTopicLabels(topics []briefTopic) []string {
	out := make([]string, 0, len(topics))
	for _, topic := range topics {
		out = append(out, topic.label)
	}
	return out
}

// ---- lead -------------------------------------------------------------

// briefNarrativeLead states the desk posture, then what it owes, then what it
// could not read. The three clauses are fixed; only their operands vary.
func briefNarrativeLead(res *rpc.BriefResult, topics []briefTopic) []rpc.BriefRun {
	p := &briefProse{}
	ready := res.Ready
	// One stress sentence, shared with the Ready tape, so the two can never
	// word the same reading differently or leave an empty reading dangling.
	briefStressSentence(p, ready.Stress)
	p.sentence()
	switch {
	case ready.Regime.Status == rpc.BriefStatusUnavailable:
		p.text("The regime read is unavailable.")
	case ready.Regime.Stage == "" && ready.Regime.Verdict == "":
		p.text("The regime read carries no stage or verdict.")
	default:
		p.text("Regime " + briefRegimeReading(ready.Regime) + ".")
	}

	flagged := briefFlaggedTopics(topics)
	unread := briefUnreadTopics(topics)
	p.sentence()
	switch {
	case len(flagged) == 0 && briefPostureFlagged(topics):
		p.text("No Review or Ready row needs a decision beyond the stress reading above.")
	case len(flagged) == 0:
		p.text("Nothing across Review and Ready needs a decision.")
	default:
		p.text(briefCountPhrase(len(flagged), "row", "rows") + " " + briefVerb(len(flagged), "needs", "need") + " a decision: ")
		for i, topic := range flagged {
			if i > 0 {
				p.text(", ")
			}
			p.tinted(topic.role, topic.label)
		}
		p.text(".")
	}
	if len(unread) > 0 {
		p.sentence()
		p.text(briefCountPhrase(len(unread), "input", "inputs") + " could not be read and " + briefVerb(len(unread), "is", "are") + " named below: " + strings.Join(briefTopicLabels(unread), ", ") + ".")
	}
	return briefMergeRuns(p.runs)
}

// ---- coda -------------------------------------------------------------

// briefNarrativeCoda closes on what is owed. It names only topics that are
// already flagged above; it never predicts, and it never promises that an
// unread input is fine.
func briefNarrativeCoda(topics []briefTopic) []rpc.BriefRun {
	p := &briefProse{}
	flagged := briefFlaggedTopics(topics)
	unread := briefUnreadTopics(topics)
	posture := briefPostureFlagged(topics)
	switch {
	case len(flagged) > 0:
		p.text("Owed before the bell: ")
		for i, topic := range flagged {
			if i > 0 {
				p.text(", ")
			}
			p.tinted(topic.role, topic.label)
		}
		p.text(". Everything else holds.")
	case posture:
		p.text("Nothing owed before the bell beyond the stress reading above.")
	case len(unread) > 0:
		p.text("Nothing on the desk needs a decision, but " + briefCountPhrase(len(unread), "input", "inputs") + " could not be read: unknown is not clean.")
	default:
		p.text("Nothing owed before the bell.")
	}
	if (len(flagged) > 0 || posture) && len(unread) > 0 {
		p.sentence()
		p.text(briefCountPhrase(len(unread), "input", "inputs") + " could not be read: unknown is not clean.")
	}
	return briefMergeRuns(p.runs)
}

// ---- review movement --------------------------------------------------

func briefNarrativeReview(review rpc.BriefReviewSection, session rpc.BriefSessionRow) []rpc.BriefParagraph {
	p := &briefProse{}
	briefReviewSession(p, review, session)
	briefReviewDeskEvents(p, review)
	briefReviewAdmin(p, review)
	return p.done()
}

// briefReviewLastSession states the close capture when one exists for the
// last completed session. Silence would hide a gap, so a resolved session
// without a capture is named as not captured; when even the session date is
// unresolved the live sentence's own neutral basis already says everything
// provable.
func briefReviewLastSession(p *briefProse, row rpc.BriefLastSessionRow) {
	if row.SessionDate == "" {
		return
	}
	if row.DailyPnLBase == nil {
		p.text("The last completed session (" + row.SessionDate + ") has no close-time Daily P/L capture; the daemon records that figure only while observing the official close.")
		return
	}
	p.text("The last completed session (" + row.SessionDate + ") closed with Daily P/L ")
	p.figure(briefMoney(*row.DailyPnLBase, row.BaseCurrency, true))
	p.text(", captured " + briefCloseCaptureClock(row.CapturedAt) + ".")
}

// briefCloseCaptureClock renders the capture instant on the session's own
// exchange clock: the close is a New York fact, and the daemon-side prose
// cannot know the reader's timezone.
func briefCloseCaptureClock(capturedAt time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return "at " + capturedAt.In(loc).Format("15:04:05") + " ET"
	}
	return "at " + capturedAt.UTC().Format("15:04:05") + " UTC"
}

// briefReviewSession opens the movement with the account's money: first the
// last completed session's close-captured Daily P/L when the daemon holds one,
// then the broker's running value. The broker's daily P/L is a running
// recomputation — off-session it keeps moving on extended/overnight marks and
// rolls to the next trading day — so the prose states the since-close basis
// when the served calendar says closed and never claims a completed-session
// result it cannot verify.
func briefReviewSession(p *briefProse, review rpc.BriefReviewSection, session rpc.BriefSessionRow) {
	briefReviewLastSession(p, review.LastSession)
	p.sentence()
	account := review.SessionPnL
	currency := account.BaseCurrency
	sessionClosed := session.Status == rpc.BriefStatusOK && !session.IsOpen
	switch {
	case account.Status == rpc.BriefStatusUnavailable:
		p.text("Account P/L is unavailable, so the session cannot be summarized.")
	case account.DailyPnLBase != nil:
		if sessionClosed {
			p.text("Since the last regular close, Daily P/L stands at ")
		} else {
			p.text("Daily P/L stands at ")
		}
		p.figure(briefMoney(*account.DailyPnLBase, currency, true))
		if sessionClosed {
			p.text(" at off-session marks")
		}
		if account.EquityBase != nil {
			p.text(" on equity of ")
			p.figure(briefMoney(*account.EquityBase, currency, false))
			p.text(".")
		} else {
			p.text("; account equity is unavailable.")
		}
	case account.EquityBase != nil:
		p.text("Daily P/L is unavailable; account equity stands at ")
		p.figure(briefMoney(*account.EquityBase, currency, false))
		p.text(".")
	default:
		p.text("Neither Daily P/L nor account equity is available.")
	}

	attribution := review.Attribution
	p.sentence()
	switch {
	case attribution.Status == rpc.BriefStatusUnavailable:
		p.text("Per-name attribution is unavailable.")
	case len(attribution.Rows) == 0:
		p.text("No per-underlying daily P/L values are available.")
	default:
		// The currency is stated once for the list rather than stamped on
		// every name: same served fact, less noise in running prose.
		if currency = strings.TrimSpace(currency); currency != "" {
			p.text("By name in " + currency + ": ")
		} else {
			p.text("By name: ")
		}
		for i, row := range attribution.Rows {
			if i > 0 {
				p.text(", ")
			}
			p.text(row.Symbol + " ")
			p.figure(briefFigure(row.DailyPnLBase, true))
		}
		if attribution.OtherPnLBase != nil && attribution.OtherCount > 0 {
			p.text(", and " + briefCountPhrase(attribution.OtherCount, "other name", "other names") + " at ")
			p.figure(briefFigure(*attribution.OtherPnLBase, true))
		}
		p.text(".")
	}
}

// briefReviewDeskEvents narrates what the desk did to itself last session:
// proposals, overrides, rule transitions, capital events. Clean, the four fold
// into one clause; flagged, each grows its own sentence.
func briefReviewDeskEvents(p *briefProse, review rpc.BriefReviewSection) {
	proposals, overrides := review.Proposals, review.Overrides
	rulesDelta, events := review.RulesDelta, review.CapitalEvents
	clean := proposals.Status == rpc.BriefStatusOK && overrides.Status == rpc.BriefStatusOK &&
		rulesDelta.Status == rpc.BriefStatusOK && events.Status == rpc.BriefStatusOK &&
		len(overrides.Rows) == 0 && !events.Latched

	if clean {
		p.sentence()
		p.text(briefUpperFirst(briefProposalsClause(proposals)) + ", with no overrides, no rule transitions, and no capital events.")
		p.sentence()
		briefAdjustedPeakSentence(p, events)
		return
	}

	p.paragraph()
	p.text(briefUpperFirst(briefProposalsClause(proposals)) + ".")

	p.sentence()
	overrideRole := briefRole(overrides.BriefRowState, false)
	switch {
	case overrides.Status == rpc.BriefStatusUnavailable:
		p.text("Override state is unavailable.")
	case len(overrides.Rows) == 0:
		p.text("No overrides were used.")
	default:
		controls := make([]string, 0, len(overrides.Rows))
		for _, row := range overrides.Rows {
			controls = append(controls, row.Control)
		}
		p.tinted(overrideRole, briefUpperFirst(briefCountPhrase(len(overrides.Rows), "active override", "active overrides"))+" "+
			briefVerb(len(overrides.Rows), "widens", "widen")+" policy controls: "+strings.Join(controls, ", ")+".")
	}

	p.sentence()
	briefRulesDeltaSentence(p, rulesDelta)

	p.sentence()
	switch {
	case events.Status == rpc.BriefStatusUnavailable:
		p.text("Capital events cannot be evaluated: the risk constitution is absent.")
	case events.Latched:
		p.tinted(rpc.BriefRunRoleAct, "The drawdown latch engaged this episode and remains open until a human reset.")
		p.sentence()
		briefAdjustedPeakSentence(p, events)
	default:
		p.text("No capital events.")
		p.sentence()
		briefAdjustedPeakSentence(p, events)
	}
}

func briefAdjustedPeakSentence(p *briefProse, events rpc.BriefCapitalEventsRow) {
	if events.AdjustedPeakBase == nil {
		p.text("The adjusted peak is unavailable.")
		return
	}
	p.text("The adjusted peak holds at ")
	p.figure(briefMoney(*events.AdjustedPeakBase, events.BaseCurrency, false))
	p.text(".")
}

func briefRulesDeltaSentence(p *briefProse, row rpc.BriefRulesDeltaRow) {
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		p.text("The rulebook delta is unavailable.")
	case row.Status == rpc.BriefStatusAttention:
		worsened := make([]string, 0, len(row.Transitions))
		for _, transition := range row.Transitions {
			if transition.To == risk.RuleStatusAct {
				worsened = append(worsened, briefRuleWords(transition.RuleID))
			}
		}
		if len(worsened) == 0 {
			p.tinted(rpc.BriefRunRoleAct, "The rulebook delta needs attention since the last stamped brief.")
			return
		}
		p.tinted(rpc.BriefRunRoleAct, briefUpperFirst(briefCountPhrase(len(worsened), "rule", "rules"))+" worsened to act since the last stamped brief: "+strings.Join(worsened, ", ")+".")
	case row.BaselineAt.IsZero():
		p.text("There is no rulebook delta baseline yet, so rule changes since the last stamped brief cannot be verified.")
	case row.Status == rpc.BriefStatusDegraded:
		parts := make([]string, 0, 4)
		if len(row.Transitions) > 0 {
			parts = append(parts, briefCountPhrase(len(row.Transitions), "status change", "status changes"))
		}
		if len(row.Added) > 0 {
			parts = append(parts, briefCountPhrase(len(row.Added), "rule added", "rules added"))
		}
		if len(row.Removed) > 0 {
			parts = append(parts, briefCountPhrase(len(row.Removed), "rule removed", "rules removed"))
		}
		if row.RulebookFingerprintChanged {
			parts = append(parts, "a changed rulebook fingerprint")
		}
		if len(parts) == 0 {
			p.text("The rulebook delta is degraded.")
			return
		}
		p.text("The rulebook changed since the last stamped brief: " + briefJoinClauses(parts) + ".")
	default:
		p.text("No rule transitions.")
	}
}

// briefReviewAdmin narrates process evidence. Clean, it is one folded clause
// on the running paragraph; flagged, it opens its own.
func briefReviewAdmin(p *briefProse, review rpc.BriefReviewSection) {
	reconcile, autoExtend := review.Reconcile, review.AutoExtend
	oneTap, orders := review.OneTap, review.WorkingOrders
	clean := reconcile.Status == rpc.BriefStatusOK && autoExtend.Status == rpc.BriefStatusOK &&
		oneTap.Status == rpc.BriefStatusOK && orders.Status == rpc.BriefStatusOK && orders.Count != nil

	if clean {
		clauses := []string{
			briefReconcileCleanClause(reconcile),
			briefAutoExtendCleanClause(autoExtend),
			briefSignoffCleanClause(oneTap),
			briefWorkingOrdersClause(orders),
		}
		p.sentence()
		p.text("Admin is clean: " + briefJoinClauses(clauses) + ".")
		return
	}

	p.paragraph()
	briefReconcileSentence(p, reconcile)
	p.sentence()
	switch {
	case autoExtend.Status == rpc.BriefStatusUnavailable:
		p.text("The reconciliation report is unavailable, so auto-extend evidence cannot be read.")
	case autoExtend.ReportID != "":
		p.text("The latest clean report extended the reconcile horizon automatically.")
	default:
		p.text("Auto-extend needs nothing.")
	}
	p.sentence()
	switch {
	case oneTap.Status == rpc.BriefStatusUnavailable:
		p.text("Sign-off availability is unavailable.")
	case len(oneTap.Blockers) > 0:
		p.text("The current reconcile report is not signable: " + strings.Join(oneTap.Blockers, "; ") + ".")
	case oneTap.Signable:
		p.text("One reconcile report is ready to sign off.")
	default:
		p.text("Nothing waits for sign-off.")
	}
	p.sentence()
	if orders.Count == nil {
		p.text("The open-orders journal is unavailable.")
		return
	}
	p.text(briefUpperFirst(briefWorkingOrdersClause(orders)) + ".")
}

func briefReconcileSentence(p *briefProse, row rpc.BriefReconcileRow) {
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		p.text("Reconcile evidence is unavailable.")
	case row.LastReconciledAt.IsZero():
		p.text("No reconcile evidence has been recorded.")
	case row.DaysRemaining == nil:
		p.text("Reconcile evidence exists, but its horizon is unapproved, so no deadline can be stated.")
	case *row.DaysRemaining < 0:
		p.text("Reconcile evidence is " + briefCountPhrase(-*row.DaysRemaining, "day", "days") + " past its declared horizon.")
	default:
		p.text(briefUpperFirst(briefReconcileCleanClause(row)) + ".")
	}
}

func briefReconcileCleanClause(row rpc.BriefReconcileRow) string {
	if row.DaysRemaining == nil {
		return "reconcile clean"
	}
	return "reconcile clean and due in " + briefCountPhrase(*row.DaysRemaining, "day", "days")
}

func briefAutoExtendCleanClause(row rpc.BriefAutoExtendRow) string {
	if row.ReportID != "" {
		return "the latest clean report extended automatically"
	}
	return "auto-extend needs nothing"
}

func briefSignoffCleanClause(row rpc.BriefOneTapRow) string {
	if row.Signable {
		return "one reconcile report is ready to sign off"
	}
	return "nothing waits for sign-off"
}

func briefWorkingOrdersClause(row rpc.BriefCountRow) string {
	if row.Count == nil {
		return "the open-orders journal is unavailable"
	}
	if *row.Count == 0 {
		return "no orders are working"
	}
	return briefCountPhrase(*row.Count, "order", "orders") + " " + briefVerb(*row.Count, "is", "are") + " working"
}

// briefProposalsClause names the day the journal actually covers: the last
// RECORDED proposal session is not necessarily the session the account P/L
// above belongs to, and the prose must not let the two read as one.
func briefProposalsClause(row rpc.BriefProposalsRow) string {
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		return "the proposal outcome journal is unavailable"
	case row.Day == "":
		return "no protection proposals are recorded yet"
	default:
		return briefCountPhrase(row.Offered, "protection proposal", "protection proposals") + " " +
			briefVerb(row.Offered, "was", "were") + " offered and " + strconv.Itoa(row.Acted) +
			" acted in the last recorded session (" + row.Day + ")"
	}
}

// ---- ready movement ---------------------------------------------------

func briefNarrativeReady(ready rpc.BriefReadySection) []rpc.BriefParagraph {
	p := &briefProse{}
	briefReadyTape(p, ready)
	p.paragraph()
	briefReadyBook(p, ready)
	briefReadyProcess(p, ready)
	return p.done()
}

// briefReadyTape narrates the market read: breadth, dealer gamma, the official
// session. The lead already states the posture, so stress and regime are
// restated here only when they carry tone or could not be read - the movement
// grows where the problem is and stays short when there is none.
func briefReadyTape(p *briefProse, ready rpc.BriefReadySection) {
	stress := ready.Stress
	if briefStressRole(stress.Severity) != "" || stress.Status != rpc.BriefStatusOK ||
		(stress.Action == "" && stress.Severity == "") {
		briefStressSentence(p, stress)
		p.sentence()
	}
	switch {
	case ready.Regime.Status == rpc.BriefStatusUnavailable:
		p.text("The regime read is unavailable.")
		p.sentence()
	case ready.Regime.Stage == "" && ready.Regime.Verdict == "":
		p.text("The regime read carries no stage or verdict.")
		p.sentence()
	case ready.Regime.Status == rpc.BriefStatusDegraded:
		p.text("Regime " + briefRegimeReading(ready.Regime) + ", on partial evidence.")
		p.sentence()
	}

	breadth := ready.Breadth
	switch {
	case breadth.Status == rpc.BriefStatusUnavailable:
		p.text("Breadth is unavailable, so participation cannot be stated.")
	case breadth.PctAbove50DMA == nil || breadth.PctAbove200DMA == nil || breadth.NetNewHighsPct == nil:
		p.text("Breadth is degraded, so participation cannot be stated.")
	default:
		p.text("Breadth has ")
		p.figure(briefPercent(*breadth.PctAbove50DMA, false))
		p.text(" above the 50-DMA and ")
		p.figure(briefPercent(*breadth.PctAbove200DMA, false))
		p.text(" above the 200-DMA, net new highs ")
		p.figure(briefPercent(*breadth.NetNewHighsPct, true))
		p.text(".")
	}

	p.sentence()
	gamma := ready.Gamma
	switch {
	case gamma.Status == rpc.BriefStatusUnavailable:
		p.text("Dealer gamma is unavailable.")
	case gamma.Spot == nil || gamma.ZeroGamma == nil:
		p.text("Dealer gamma is degraded, so the spot-to-zero-gamma relationship cannot be stated.")
	default:
		p.text("Dealer gamma is " + briefGammaSignWords(gamma.GammaSign) + " with spot ")
		p.figure(briefPrice(*gamma.Spot))
		p.text(" against zero gamma ")
		p.figure(briefPrice(*gamma.ZeroGamma))
		if gamma.GapPct != nil {
			p.text(", a gap of ")
			p.figure(briefPercent(*gamma.GapPct, true))
		}
		p.text(".")
	}

	p.sentence()
	switch {
	case ready.Session.Status == rpc.BriefStatusUnavailable:
		p.text("The official market session is unavailable.")
	case ready.Session.State == "":
		p.text("The official market session carries no state.")
	default:
		if market := strings.TrimSpace(ready.Session.Market); market != "" {
			p.text("The " + market + " session is " + ready.Session.State + ".")
		} else {
			p.text("The official session is " + ready.Session.State + ".")
		}
	}
}

func briefStressSentence(p *briefProse, stress rpc.BriefStressRow) {
	role := briefStressRole(stress.Severity)
	switch {
	case stress.Status == rpc.BriefStatusUnavailable:
		p.text("Stress is unavailable, so the desk posture cannot be stated.")
		return
	case stress.Action == "" && stress.Severity == "":
		p.text("Stress carries no action or severity.")
		return
	default:
		p.text("Stress reads ")
		p.tinted(role, briefStressReading(stress))
	}
	if stress.Status == rpc.BriefStatusDegraded {
		p.text(", on partial inputs.")
		return
	}
	p.text(".")
}

// briefReadyBook narrates capacity and carry: what the book can lose and what
// it pays to hold protection.
func briefReadyBook(p *briefProse, ready rpc.BriefReadySection) {
	capital := ready.Capital
	role := briefRole(capital.BriefRowState, capital.Tier == risk.CapitalTierBlock)
	switch {
	case capital.Status == rpc.BriefStatusUnavailable:
		p.text("The risk constitution is absent, so capital controls are unapproved and the drawdown budget cannot be stated.")
	case capital.Tier == "":
		p.text("Capital state cannot be evaluated from current inputs.")
	default:
		p.tinted(role, "Capital sits in the "+capital.Tier+" tier")
		if capital.Enforcement != "" {
			p.tinted(role, " under "+capital.Enforcement+" enforcement")
		}
		if capital.ConsumedPct != nil {
			p.tinted(role, " with ")
			p.tintedFigure(role, briefPercent(*capital.ConsumedPct, false))
			p.tinted(role, " of the drawdown budget consumed")
		} else {
			p.tinted(role, "; the consumed share is unavailable")
		}
		p.tinted(role, ".")
		if capital.DrawdownBase != nil && capital.AdjustedPeakBase != nil {
			p.sentence()
			p.text("Drawdown stands at ")
			p.figure(briefMoney(*capital.DrawdownBase, capital.BaseCurrency, false))
			p.text(" from an adjusted peak of ")
			p.figure(briefMoney(*capital.AdjustedPeakBase, capital.BaseCurrency, false))
			p.text(".")
		}
	}

	p.sentence()
	latch := ready.Latch
	switch {
	case latch.Status == rpc.BriefStatusUnavailable:
		p.text("The drawdown latch state is unavailable.")
	case latch.Latched:
		p.tinted(rpc.BriefRunRoleAct, "The drawdown latch is engaged")
		if latch.AgeDays != nil {
			p.tinted(rpc.BriefRunRoleAct, ", "+briefCountPhrase(*latch.AgeDays, "day", "days")+" old")
		}
		p.tinted(rpc.BriefRunRoleAct, ", and remains so until a human reset.")
		if latch.ConsumedPctAtLatch != nil {
			p.sentence()
			p.text("It engaged at ")
			p.figure(briefPercent(*latch.ConsumedPctAtLatch, false))
			p.text(" consumed.")
		}
	default:
		p.text("The drawdown latch is open.")
	}

	p.sentence()
	premium := ready.PremiumAtRisk
	switch {
	case premium.Status == rpc.BriefStatusUnavailable:
		p.text("Premium at risk is unavailable.")
	case premium.AmountBase == nil:
		p.text("Premium at risk cannot be totalled: no long option leg carries a base market value.")
	default:
		p.text("Premium at risk ")
		p.figure(briefMoney(*premium.AmountBase, premium.BaseCurrency, false))
		p.text(" across " + briefCountPhrase(premium.IncludedLegs, "long option leg", "long option legs"))
		if premium.ExcludedLegs > 0 {
			p.text(", with " + briefCountPhrase(premium.ExcludedLegs, "leg", "legs") + " excluded for missing base market value")
		}
		p.text(".")
	}

	p.sentence()
	hedge := ready.HedgeCost
	switch {
	case hedge.Status == rpc.BriefStatusUnavailable:
		p.text("Hedge cost is unavailable.")
	case hedge.AmountBase == nil:
		p.text("Hedge cost cannot be totalled: no classified hedge leg carries usable theta.")
	default:
		p.text("Hedge cost ")
		p.figure(briefMoney(*hedge.AmountBase, hedge.BaseCurrency, false))
		p.text(" per day across " + briefCountPhrase(hedge.IncludedLegs, "classified hedge leg", "classified hedge legs"))
		if hedge.ExcludedLegs > 0 {
			p.text(", with " + briefCountPhrase(hedge.ExcludedLegs, "candidate leg", "candidate legs") + " excluded for missing classification inputs")
		}
		p.text(".")
	}

	p.sentence()
	briefReadyProposalsSentence(p, ready.Proposals)
}

// briefReadyProposalsSentence states how much protection work is staged for
// the session ahead. It reports the served counts and nothing else: the prose
// never says a proposal should be placed, and staging is not authority.
func briefReadyProposalsSentence(p *briefProse, row rpc.BriefReadyProposalsRow) {
	role := briefRole(row.BriefRowState, false)
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		p.text("The protection proposal snapshot is unavailable, so staged work cannot be stated.")
	case row.Actionable > 0:
		p.tinted(role, briefUpperFirst(briefCountPhrase(row.Actionable, "protection proposal", "protection proposals"))+" "+
			briefVerb(row.Actionable, "is", "are")+" ready to act")
		if row.Blocked > 0 {
			p.tinted(role, ", with "+briefCountPhrase(row.Blocked, "more blocked", "more blocked"))
		}
		p.tinted(role, ".")
	case row.Blocked > 0:
		p.text("No protection proposal is ready to act; " + briefCountPhrase(row.Blocked, "is blocked", "are blocked") + ".")
	default:
		p.text("No protection proposals are staged.")
	}
}

// briefReadyProcess narrates pins, cadence, the monthly pulse and held-name
// events. Clean, all four fold into one clause on the book paragraph.
func briefReadyProcess(p *briefProse, ready rpc.BriefReadySection) {
	drift, artefacts := ready.PolicyDrift, ready.Artefacts
	events := ready.MarketEvents
	eventsClean := len(events) > 0
	for _, event := range events {
		if event.Status != rpc.BriefStatusOK || event.Count > 0 {
			eventsClean = false
			break
		}
	}
	monthlyClean := ready.MonthlyPulse == nil ||
		ready.MonthlyPulse.Status == rpc.BriefMonthlyPulseNotDue ||
		ready.MonthlyPulse.Status == rpc.BriefMonthlyPulseCompleted
	clean := drift.Status == rpc.BriefStatusOK && artefacts.Status == rpc.BriefStatusOK && eventsClean && monthlyClean

	if clean {
		clauses := []string{"policy pins match", briefArtefactsCleanClause(artefacts)}
		if ready.MonthlyPulse != nil {
			clauses = append(clauses, briefMonthlyPulseClause(*ready.MonthlyPulse))
		}
		clauses = append(clauses, "no held-name events")
		p.sentence()
		p.text("Process folds clean: " + briefJoinClauses(clauses) + ".")
		return
	}

	p.paragraph()
	switch {
	case drift.Status == rpc.BriefStatusUnavailable:
		p.text("Policy pin status is unavailable.")
	case len(drift.Rows) > 0:
		names := make([]string, 0, len(drift.Rows))
		for _, row := range drift.Rows {
			names = append(names, row.Policy+" "+row.Status)
		}
		p.text(briefUpperFirst(briefCountPhrase(len(drift.Rows), "sibling-policy approval pin", "sibling-policy approval pins")) + " " +
			briefVerb(len(drift.Rows), "does", "do") + " not match: " + strings.Join(names, ", ") + ".")
	default:
		p.text("Policy pins match.")
	}

	p.sentence()
	briefArtefactsSentence(p, artefacts)

	if ready.MonthlyPulse != nil {
		p.sentence()
		p.text(briefUpperFirst(briefMonthlyPulseClause(*ready.MonthlyPulse)) + ".")
	}

	p.sentence()
	briefMarketEventsSentences(p, events)
}

func briefArtefactsCleanClause(row rpc.BriefArtefactsRow) string {
	completed := 0
	for _, artefact := range row.Rows {
		if artefact.Completed {
			completed++
		}
	}
	return "cadence artefacts are declared with " + strconv.Itoa(completed) + " of " + strconv.Itoa(len(row.Rows)) + " complete"
}

func briefArtefactsSentence(p *briefProse, row rpc.BriefArtefactsRow) {
	if row.Status == rpc.BriefStatusUnavailable {
		p.text("Cadence artefacts are unapproved or undeclared.")
		return
	}
	undeclared := make([]string, 0, len(row.Rows))
	for _, artefact := range row.Rows {
		if !artefact.Declared {
			undeclared = append(undeclared, artefact.Kind)
		}
	}
	if len(undeclared) > 0 {
		p.text("The " + strings.Join(undeclared, " and ") + " " + briefVerb(len(undeclared), "artefact is", "artefacts are") + " not declared.")
		return
	}
	p.text(briefUpperFirst(briefArtefactsCleanClause(row)) + ".")
}

// briefMonthlyPulseClause narrates a SERVED pulse row. A policy version that
// has no monthly pulse omits the row entirely, and the movement stays silent
// about it exactly like the row render does.
func briefMonthlyPulseClause(row rpc.BriefMonthlyPulseRow) string {
	month := strings.TrimSpace(row.Month)
	switch row.Status {
	case rpc.BriefMonthlyPulseNotDue:
		return "the monthly pulse is not due"
	case rpc.BriefMonthlyPulseDue:
		if month == "" {
			return "the monthly pulse is due"
		}
		return "the monthly pulse is due for " + month
	case rpc.BriefMonthlyPulseCompleted:
		if month == "" {
			return "the monthly pulse is complete"
		}
		return "the monthly pulse is complete for " + month
	default:
		return "the monthly pulse is blocked by policy evidence"
	}
}

func briefMarketEventsSentences(p *briefProse, events []rpc.BriefMarketEventRow) {
	if len(events) == 0 {
		p.text("Held-name event coverage is unavailable.")
		return
	}
	flagged, clean := false, 0
	for _, event := range events {
		kind := briefEventKindLabel(event.Kind)
		role := briefRole(event.BriefRowState, false)
		switch {
		case event.Status == rpc.BriefStatusUnavailable:
			flagged = true
			p.text("The " + kind + " event source is unavailable.")
		case event.Status == rpc.BriefStatusDegraded:
			flagged = true
			p.text("The " + kind + " event source is degraded.")
		case event.Count > 0:
			flagged = true
			p.tinted(role, briefUpperFirst(briefCountPhrase(event.Count, "held name", "held names"))+" "+
				briefVerb(event.Count, "carries", "carry")+" "+kind+" context")
			if len(event.Symbols) > 0 {
				p.tinted(role, ": "+strings.Join(event.Symbols, ", "))
			}
			p.tinted(role, ".")
		default:
			clean++
			continue
		}
		p.sentence()
	}
	switch {
	case !flagged:
		p.text("No held-name events.")
	case clean > 0:
		// The unflagged kinds were checked and are clean; saying so keeps the
		// paragraph from leaving their silence ambiguous.
		p.text("The remaining held-name event " + pluralNoun(clean, "source") + " " + briefVerb(clean, "is", "are") + " clean.")
	}
}

// ---- shared readings and formatting -----------------------------------

// briefStressReading prints the daemon's stress vocabulary verbatim. Action
// and severity are usually the same word, so the pair collapses instead of
// stuttering - the same rule the row render uses.
func briefStressReading(stress rpc.BriefStressRow) string {
	action := strings.TrimSpace(stress.Action)
	severity := strings.TrimSpace(stress.Severity)
	switch {
	case action == "" && severity == "":
		return ""
	case action == "":
		return severity + " severity"
	case severity == "" || strings.EqualFold(action, severity):
		return action
	default:
		return action + " at " + severity + " severity"
	}
}

func briefRegimeReading(regime rpc.BriefRegimeRow) string {
	stage, verdict := strings.TrimSpace(regime.Stage), strings.TrimSpace(regime.Verdict)
	switch {
	case stage != "" && verdict != "":
		return "stage " + stage + ", verdict " + verdict
	case stage != "":
		return "stage " + stage + " with no verdict"
	default:
		return "verdict " + verdict + " with no stage"
	}
}

// briefGammaSignWords keeps the served sign word when there is one and says so
// plainly when there is not.
func briefGammaSignWords(sign string) string {
	if strings.TrimSpace(sign) == "" {
		return "unclassified"
	}
	return sign
}

func briefEventKindLabel(kind string) string {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return "market event"
	}
	return kind
}

func briefRuleWords(id string) string {
	return strings.ReplaceAll(strings.TrimSpace(id), "_", " ")
}

// briefCountPhrase prints "1 day" / "3 days" - the count always leads, so a
// reader never has to map a spelled-out number back onto a served figure.
func briefCountPhrase(count int, singular, plural string) string {
	if count == 1 {
		return "1 " + singular
	}
	return strconv.Itoa(count) + " " + plural
}

func briefVerb(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}

// briefJoinClauses renders a clause list as "a, b, and c".
func briefJoinClauses(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + ", and " + parts[len(parts)-1]
	}
}

func briefUpperFirst(text string) string {
	if text == "" {
		return ""
	}
	return strings.ToUpper(text[:1]) + text[1:]
}

// briefMoney renders an amount with its served base currency ahead of it, so
// no figure implies a currency the payload did not state. Amounts below 1000
// keep two decimals (carry figures such as hedge theta live there); larger
// amounts round to whole units, which is what the prose is for - the rows
// beside it carry the exact values.
func briefMoney(amount float64, currency string, signed bool) string {
	figure := briefFigure(amount, signed)
	if currency = strings.TrimSpace(currency); currency != "" {
		return currency + " " + figure
	}
	return figure
}

// briefFigure is the bare money figure, for lists that state their currency
// once.
func briefFigure(amount float64, signed bool) string {
	decimals := 0
	if amount > -1000 && amount < 1000 {
		decimals = 2
	}
	figure := briefThousands(amount, decimals)
	if signed && amount > 0 {
		figure = "+" + figure
	}
	return figure
}

func briefPercent(value float64, signed bool) string {
	figure := strconv.FormatFloat(value, 'f', 1, 64)
	if signed && value > 0 {
		figure = "+" + figure
	}
	return figure + "%"
}

func briefPrice(value float64) string {
	return briefThousands(value, 2)
}

// briefThousands formats a float with grouped thousands. Go has no locale
// formatter and the brief is deliberately locale-free: one grouping, one
// decimal point, everywhere.
func briefThousands(value float64, decimals int) string {
	text := strconv.FormatFloat(value, 'f', decimals, 64)
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	whole, fraction, hasFraction := strings.Cut(text, ".")
	var grouped strings.Builder
	for i, digit := range whole {
		if i > 0 && (len(whole)-i)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	out := sign + grouped.String()
	if hasFraction {
		out += "." + fraction
	}
	return out
}
