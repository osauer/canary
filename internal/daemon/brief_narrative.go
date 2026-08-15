package daemon

import (
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// The narrative Briefing.
//  1. Only served facts. Figures are interpolated from the payload with the
//     never invented. Where the rows assert a cause ("engaged and remains so
//  2. Only served statuses decide tone. A clause may carry the watch or act
//     role only when its own row is attention-class (or, for stress, when the
//     conditions: they are stated in plain words and never tinted.
//  3. Silence is never clean. An unavailable or degraded input is named as
//     unavailable or degraded; it is never dropped and never folded into a

// briefProse accumulates runs into paragraphs. Callers push text spans in
// span, so templates never carry stray leading or trailing spaces.
type briefProse struct {
	paragraphs []rpc.BriefParagraph
	runs       []rpc.BriefRun
	pending    bool
}

func (p *briefProse) push(role, text string) {
	p.pushRun(rpc.BriefRun{Text: text, Role: role})
}

func (p *briefProse) pushRun(run rpc.BriefRun) {
	if run.Text == "" {
		return
	}
	if p.pending {
		p.pending = false
		if len(p.runs) > 0 {
			p.runs = append(p.runs, rpc.BriefRun{Text: " "})
		}
	}
	p.runs = append(p.runs, run)
}

// sentence marks a sentence boundary. It is a no-op at the start of a
// paragraph, so a folded block never opens with whitespace.
func (p *briefProse) sentence() { p.pending = true }

func (p *briefProse) text(text string)   { p.push("", text) }
func (p *briefProse) figure(text string) { p.push(rpc.BriefRunRoleFigure, text) }
func (p *briefProse) accountFigure(text string) {
	p.pushRun(rpc.BriefRun{Text: text, Role: rpc.BriefRunRoleFigure, AccountSensitive: true})
}
func (p *briefProse) tinted(role, text string)      { p.push(role, text) }
func (p *briefProse) tintedFigure(role, fig string) { p.push(briefFigureRole(role), fig) }

// tintedTopic emits a flagged topic label carrying its closed slug, so the
// SPA can route a tap to the surface that owns the row.
func (p *briefProse) tintedTopic(role, text, topic string) {
	p.pushRun(rpc.BriefRun{Text: text, Role: role, Topic: topic})
}

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
func briefMergeRuns(runs []rpc.BriefRun) []rpc.BriefRun {
	out := make([]rpc.BriefRun, 0, len(runs))
	for _, run := range runs {
		if run.Text == "" {
			continue
		}
		if len(out) > 0 && out[len(out)-1].Role == run.Role && out[len(out)-1].AccountSensitive == run.AccountSensitive && out[len(out)-1].Topic == run.Topic {
			out[len(out)-1].Text += run.Text
			continue
		}
		out = append(out, run)
	}
	return out
}

// briefTopic is one narrated row: a fixed label, the row state that decides
type briefTopic struct {
	label   string
	state   rpc.BriefRowState
	role    string
	posture bool
}

// slug is the closed wire identifier for the topic: the fixed label with
// spaces and hyphens folded to underscores ("policy adherence" →
// "policy_adherence"). The label set is closed — fixed literals here plus
// "held-name <kind>" whose kinds come from the briefMarketEventRows
// allowlist — so slugs carry no symbols or account data; loosening that
// allowlist would widen this wire surface too.
func (t briefTopic) slug() string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-':
			return '_'
		}
		return r
	}, t.label)
}

func (t briefTopic) unread() bool {
	return t.state.Status == rpc.BriefStatusDegraded || t.state.Status == rpc.BriefStatusUnavailable
}

// briefRole maps a row state onto the tint its clauses may carry. Attention
// is the brief's risk vocabulary, so only attention tints; act is reserved
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

func briefRulesActClass(row rpc.BriefRulesRow) bool { return row.Act > 0 }

// composeBriefNarrative is the entry point: a pure projection of the composed
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
// they can never disagree with the paragraphs below them.
func briefTopics(res *rpc.BriefResult) []briefTopic {
	review, ready := res.Review, res.Ready
	topics := []briefTopic{
		{label: "session P/L", state: review.SessionPnL.BriefRowState, role: briefRole(review.SessionPnL.BriefRowState, false)},
		{label: "attribution", state: review.Attribution.BriefRowState, role: briefRole(review.Attribution.BriefRowState, false)},
		{label: "policy adherence", state: review.Rules.BriefRowState, role: briefRole(review.Rules.BriefRowState, briefRulesActClass(review.Rules))},
		{label: "proposals", state: review.Proposals.BriefRowState, role: briefRole(review.Proposals.BriefRowState, false)},
		{label: "overrides", state: review.Overrides.BriefRowState, role: briefRole(review.Overrides.BriefRowState, false)},
		{label: "capital events", state: review.CapitalEvents.BriefRowState, role: briefRole(review.CapitalEvents.BriefRowState, review.CapitalEvents.Latched)},
		{label: "reconcile", state: review.Reconcile.BriefRowState, role: briefRole(review.Reconcile.BriefRowState, false)},
		{label: "auto-extend", state: review.AutoExtend.BriefRowState, role: briefRole(review.AutoExtend.BriefRowState, false)},
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
		role := briefRole(event.BriefRowState, false)
		if event.Kind == "earnings" && event.Status == rpc.BriefStatusAttention {
			// An unresolved provider date is context, not an operator decision.
			// The Rulebook names the missing date and waits for a concrete breach.
			role = ""
		}
		topics = append(topics, briefTopic{
			label: "held-name " + briefEventKindLabel(event.Kind),
			state: event.BriefRowState,
			role:  role,
		})
	}
	topics = append(topics,
		briefTopic{label: "capital", state: ready.Capital.BriefRowState, role: briefRole(ready.Capital.BriefRowState, ready.Capital.Tier == risk.CapitalTierBlock)},
		briefTopic{label: "drawdown latch", state: ready.Latch.BriefRowState, role: briefRole(ready.Latch.BriefRowState, ready.Latch.Latched)},
		briefTopic{label: "premium at risk", state: ready.PremiumAtRisk.BriefRowState, role: briefRole(ready.PremiumAtRisk.BriefRowState, false)},
		briefTopic{label: "index-put theta", state: ready.HedgeCost.BriefRowState, role: briefRole(ready.HedgeCost.BriefRowState, false)},
		briefTopic{label: "protection proposals", state: ready.Proposals.BriefRowState, role: briefRole(ready.Proposals.BriefRowState, false)},
		briefTopic{label: "policy drift", state: ready.PolicyDrift.BriefRowState, role: briefRole(ready.PolicyDrift.BriefRowState, false)},
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
	briefStressSentence(p, ready.Stress)
	p.sentence()
	switch {
	case ready.Regime.Status == rpc.BriefStatusUnavailable:
		p.text("The regime read is unavailable.")
	case ready.Regime.Stage == "" && ready.Regime.Verdict == "":
		p.text("The regime read carries no stage or verdict.")
	default:
		p.text(briefUpperFirst(briefRegimeReading(ready.Regime)) + ".")
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
			p.tintedTopic(topic.role, topic.label, topic.slug())
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
			p.tintedTopic(topic.role, topic.label, topic.slug())
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
	p.accountFigure(briefMoney(*row.DailyPnLBase, row.BaseCurrency, true))
	p.text(", captured " + briefCloseCaptureClock(row.CapturedAt) + ".")
}

// briefCloseCaptureClock renders the capture instant on the session's own
func briefCloseCaptureClock(capturedAt time.Time) string {
	if loc, err := time.LoadLocation("America/New_York"); err == nil {
		return "at " + capturedAt.In(loc).Format("15:04:05") + " ET"
	}
	return "at " + capturedAt.UTC().Format("15:04:05") + " UTC"
}

// briefReviewSession opens the movement with the account's money: first the
// then the broker's running value. The broker's daily P/L is a running
// when the served calendar says closed and never claims a completed-session
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
		p.accountFigure(briefMoney(*account.DailyPnLBase, currency, true))
		if sessionClosed {
			p.text(" at off-session marks")
		}
		if account.EquityBase != nil {
			p.text(" on equity of ")
			p.accountFigure(briefMoney(*account.EquityBase, currency, false))
			p.text(".")
		} else {
			p.text("; account equity is unavailable.")
		}
	case account.EquityBase != nil:
		p.text("Daily P/L is unavailable; account equity stands at ")
		p.accountFigure(briefMoney(*account.EquityBase, currency, false))
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
			p.accountFigure(briefFigure(row.DailyPnLBase, true))
		}
		if attribution.OtherPnLBase != nil && attribution.OtherCount > 0 {
			p.text(", and " + briefCountPhrase(attribution.OtherCount, "other name", "other names") + " at ")
			p.accountFigure(briefFigure(*attribution.OtherPnLBase, true))
		}
		p.text(".")
	}
}

// briefReviewDeskEvents narrates what the desk did to itself last session:
func briefReviewDeskEvents(p *briefProse, review rpc.BriefReviewSection) {
	proposals, overrides := review.Proposals, review.Overrides
	rules, events := review.Rules, review.CapitalEvents
	clean := proposals.Status == rpc.BriefStatusOK && overrides.Status == rpc.BriefStatusOK &&
		rules.Status == rpc.BriefStatusOK && events.Status == rpc.BriefStatusOK &&
		len(overrides.Rows) == 0 && !events.Latched

	if clean {
		p.sentence()
		p.text(briefUpperFirst(briefProposalsClause(proposals)) + ", with no overrides, all current policy checks passing, and no capital events.")
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
	briefRulesSentence(p, rules)

	p.sentence()
	switch {
	case events.Status == rpc.BriefStatusUnavailable:
		p.text("Capital events cannot be evaluated: the risk constitution is absent.")
	case events.Latched:
		p.tinted(rpc.BriefRunRoleAct, "The drawdown latch needs review.")
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
	p.accountFigure(briefMoney(*events.AdjustedPeakBase, events.BaseCurrency, false))
	p.text(".")
}

func briefRulesSentence(p *briefProse, row rpc.BriefRulesRow) {
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		p.text("Current policy adherence is unavailable.")
	case row.Status == rpc.BriefStatusAttention:
		p.tinted(rpc.BriefRunRoleAct, briefUpperFirst(briefCountPhrase(row.Act, "current policy check", "current policy checks"))+" require action.")
	case row.Status == rpc.BriefStatusDegraded:
		p.text("Current policy adherence has " + briefCountPhrase(row.Watch, "watch", "watches") + ", " + briefCountPhrase(row.Unknown, "unknown", "unknowns") + ", and " + briefCountPhrase(row.NotEvaluated, "not evaluated", "not evaluated") + ".")
	case row.NotEvaluated > 0:
		p.text("All due current policy checks pass; " + briefCountPhrase(row.NotEvaluated, "check is", "checks are") + " not evaluated.")
	default:
		p.text("All due current policy checks pass.")
	}
}

// briefReviewAdmin narrates process evidence. Clean, it is one folded clause
func briefReviewAdmin(p *briefProse, review rpc.BriefReviewSection) {
	reconcile, autoExtend := review.Reconcile, review.AutoExtend
	orders := review.WorkingOrders
	clean := reconcile.Status == rpc.BriefStatusOK && autoExtend.Status == rpc.BriefStatusOK &&
		orders.Status == rpc.BriefStatusOK && orders.Count != nil

	if clean {
		clauses := []string{
			briefReconcileCleanClause(reconcile),
			briefAutoExtendCleanClause(autoExtend),
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
	if orders.Count == nil {
		p.text("The open-orders journal is unavailable.")
		return
	}
	p.text(briefUpperFirst(briefWorkingOrdersClause(orders)) + ".")
}

func briefReconcileSentence(p *briefProse, row rpc.BriefReconcileRow) {
	switch {
	case row.Status == rpc.BriefStatusUnavailable:
		p.text("No current reconciliation result is available.")
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
// restated here only when they carry tone or could not be read - the movement
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
		p.text(briefUpperFirst(briefRegimeReading(ready.Regime)) + ", using partial inputs.")
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
		p.text("Portfolio risk: ")
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
			p.accountFigure(briefMoney(*capital.DrawdownBase, capital.BaseCurrency, false))
			p.text(" from an adjusted peak of ")
			p.accountFigure(briefMoney(*capital.AdjustedPeakBase, capital.BaseCurrency, false))
			p.text(".")
		}
	}

	p.sentence()
	latch := ready.Latch
	switch {
	case latch.Status == rpc.BriefStatusUnavailable:
		p.text("The drawdown latch state is unavailable.")
	case latch.Latched:
		headline := "The drawdown latch needs review"
		if latch.Provisional {
			headline = "The drawdown latch is engaged provisionally"
		}
		p.tinted(rpc.BriefRunRoleAct, headline)
		if latch.AgeDays != nil {
			p.tinted(rpc.BriefRunRoleAct, ", "+briefCountPhrase(*latch.AgeDays, "day", "days")+" old")
		}
		p.tinted(rpc.BriefRunRoleAct, ".")
		if latch.ConsumedPctAtLatch != nil {
			p.sentence()
			p.text("It engaged at ")
			p.figure(briefPercent(*latch.ConsumedPctAtLatch, false))
			p.text(" consumed.")
		}
		if latch.Provisional {
			p.sentence()
			p.text("A statement-confirmed withdrawal covering the latch day releases it automatically; anything else makes it permanent until you reset it.")
		}
		if !latch.ReportCoverageTo.IsZero() && latch.ReportCoverageTo.Before(latch.At) {
			p.sentence()
			if !latch.ReportCheckedAt.IsZero() {
				p.text("Canary checked IBKR again at " + latch.ReportCheckedAt.In(time.Local).Format("Jan 2 15:04") + ". ")
			}
			p.text("The newest daily report still covers through " + latch.ReportCoverageTo.In(time.Local).Format("Jan 2") + ", so a later cash transfer may not be reflected yet.")
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
		p.accountFigure(briefMoney(*premium.AmountBase, premium.BaseCurrency, false))
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
		p.text("No current index-put theta total is available.")
	case hedge.AmountBase == nil:
		p.text("Index-put theta cannot be totalled because theta is missing.")
	default:
		p.text("Index-put theta ")
		p.accountFigure(briefMoney(*hedge.AmountBase, hedge.BaseCurrency, false))
		p.text(" per day across " + briefCountPhrase(hedge.IncludedLegs, "long index put", "long index puts"))
		if hedge.ExcludedLegs > 0 {
			p.text(", with " + briefCountPhrase(hedge.ExcludedLegs, "position", "positions") + " excluded because theta, delta, price, or FX is missing")
		}
		p.text(".")
	}

	p.sentence()
	briefReadyProposalsSentence(p, ready.Proposals)
}

// briefReadyProposalsSentence states how much protection work is staged for
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

// briefReadyProcess narrates pins, the monthly pulse and held-name events.
func briefReadyProcess(p *briefProse, ready rpc.BriefReadySection) {
	drift := ready.PolicyDrift
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
	clean := drift.Status == rpc.BriefStatusOK && eventsClean && monthlyClean

	if clean {
		// Pins are narrated only under required sign-off; otherwise they are
		// bookkeeping the operator chose not to hear about.
		var clauses []string
		if drift.SignoffRequired {
			clauses = append(clauses, "policy pins match")
		}
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
	case drift.Status != rpc.BriefStatusOK && len(drift.Rows) > 0:
		names := make([]string, 0, len(drift.Rows))
		for _, row := range drift.Rows {
			names = append(names, row.Policy+" "+row.Status)
		}
		p.text(briefUpperFirst(briefCountPhrase(len(drift.Rows), "sibling-policy approval pin", "sibling-policy approval pins")) + " " +
			briefVerb(len(drift.Rows), "does", "do") + " not match: " + strings.Join(names, ", ") + ".")
	case drift.SignoffRequired:
		p.text("Policy pins match.")
	}

	if ready.MonthlyPulse != nil {
		p.sentence()
		p.text(briefUpperFirst(briefMonthlyPulseClause(*ready.MonthlyPulse)) + ".")
	}

	p.sentence()
	briefMarketEventsSentences(p, events)
}

// briefMonthlyPulseClause narrates a SERVED pulse row. A policy version that
// about it exactly like the row render does.
func briefMonthlyPulseClause(row rpc.BriefMonthlyPulseRow) string {
	month := strings.TrimSpace(row.Month)
	switch row.Status {
	case rpc.BriefMonthlyPulseNotDue:
		return "the monthly pulse is not due"
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

func briefStressReading(stress rpc.BriefStressRow) string {
	action := strings.TrimSpace(stress.Action)
	severity := strings.TrimSpace(stress.Severity)
	severityWords := severity
	if strings.EqualFold(severityWords, string(risk.SeverityAct)) {
		severityWords = "action"
	}
	switch {
	case action == "" && severity == "":
		return ""
	case action == "":
		return severityWords
	case severity == "" || strings.EqualFold(action, severity):
		if strings.EqualFold(action, string(risk.SeverityAct)) {
			return "action"
		}
		return strings.ReplaceAll(action, "_", " ")
	default:
		return strings.ReplaceAll(action, "_", " ") + ", with " + severityWords + "-level conditions"
	}
}

func briefRegimeReading(regime rpc.BriefRegimeRow) string {
	stage, verdict := strings.TrimSpace(regime.Stage), strings.TrimSpace(regime.Verdict)
	stageWords := map[string]string{
		"quiet":            "quiet",
		"early_warning":    "in an early warning",
		"confirmed_stress": "in confirmed stress",
		"panic":            "in panic",
		"stabilization":    "stabilizing",
		"opportunity":      "in an opportunity phase",
		"data_quality":     "waiting for current inputs",
	}[strings.ToLower(stage)]
	if stage != "" && stageWords == "" {
		stageWords = "in " + strings.ReplaceAll(strings.ToLower(stage), "_", " ")
	}
	switch {
	case stage != "":
		return "market conditions are " + stageWords
	default:
		return "market conditions: " + verdict
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
// beside it carry the exact values.
func briefMoney(amount float64, currency string, signed bool) string {
	figure := briefFigure(amount, signed)
	if currency = strings.TrimSpace(currency); currency != "" {
		return currency + " " + figure
	}
	return figure
}

// briefFigure is the bare money figure, for lists that state their currency
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
