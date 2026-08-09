package rpc

import (
	"time"
)

// Brief constants define the daemon methods and the bounded status, kind,
// monthly-pulse, and acknowledgement vocabularies carried on the wire.
const (
	// MethodBriefSnapshot composes the operator's daily brief. It is a pure
	// read: callers do not supply an origin and the daemon must not stamp,
	// journal, or advance any runtime clock while serving it.
	MethodBriefSnapshot = "brief.snapshot"
	// BriefStatusOK is the normal member of the brief row status vocabulary.
	// Brief row statuses separate risk conditions from data conditions:
	// attention means the underlying VALUES describe a state a trader must
	// look at (latched drawdown, breached tier, active override); degraded
	// and unavailable describe input quality only and must never be used to
	// signal a risk condition, nor vice versa.
	BriefStatusOK          = "ok"
	BriefStatusAttention   = "attention"
	BriefStatusDegraded    = "degraded"
	BriefStatusUnavailable = "unavailable"

	// BriefKindMorning identifies the pre-trade morning brief.
	BriefKindMorning = "morning"
	// BriefKindEOD identifies the end-of-day brief.
	BriefKindEOD = "eod"
	// BriefKindMonthly identifies the monthly governance pulse.
	BriefKindMonthly = "monthly"

	// BriefMonthlyPulseNotDue means the monthly pulse has no current action.
	BriefMonthlyPulseNotDue = "not_due"
	// BriefMonthlyPulseCompleted means the current pulse has valid evidence.
	BriefMonthlyPulseCompleted = "completed"
	// BriefMonthlyPulseBlocked means completion prerequisites are unavailable.
	BriefMonthlyPulseBlocked = "blocked"
)

// BriefSnapshotParams is deliberately empty. In particular it carries no
// origin: reads never gain write authority from their caller.
type BriefSnapshotParams struct{}

// BriefRowState is embedded by every brief row and section. Detail is
// human-facing disclosure; Status is one of ok, attention, degraded, or
// unavailable. Sections roll up their worst child (attention outranks
// degraded) and state completeness in Detail.
type BriefRowState struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// BriefMarketSection groups broad-market and Stress rows.
type BriefMarketSection struct {
	BriefRowState
	Regime  BriefRegimeRow  `json:"regime"`
	Breadth BriefBreadthRow `json:"breadth"`
	Gamma   BriefGammaRow   `json:"gamma"`
	Stress  BriefStressRow  `json:"stress"`
}

// BriefRegimeRow summarizes the current regime lifecycle and verdict.
type BriefRegimeRow struct {
	BriefRowState
	Stage   string `json:"stage,omitempty"`
	Verdict string `json:"verdict,omitempty"`
}

// BriefBreadthRow summarizes breadth values and their observation time. Nil
// metrics mean unavailable, not zero.
type BriefBreadthRow struct {
	BriefRowState
	PctAbove50DMA  *float64  `json:"pct_above_50dma,omitempty"`
	PctAbove200DMA *float64  `json:"pct_above_200dma,omitempty"`
	NetNewHighsPct *float64  `json:"net_new_highs_pct,omitempty"`
	AsOf           time.Time `json:"as_of,omitzero"`
	DataType       string    `json:"data_type,omitempty"`
}

// BriefGammaRow summarizes the current zero-gamma relationship. Nil values
// mean unavailable, not zero.
type BriefGammaRow struct {
	BriefRowState
	Spot      *float64  `json:"spot,omitempty"`
	ZeroGamma *float64  `json:"zero_gamma,omitempty"`
	GapPct    *float64  `json:"gap_pct,omitempty"`
	GammaSign string    `json:"gamma_sign,omitempty"`
	AsOf      time.Time `json:"as_of,omitzero"`
}

// BriefStressRow summarizes the current advisory action and severity.
type BriefStressRow struct {
	BriefRowState
	Action   string `json:"action,omitempty"`
	Severity string `json:"severity,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// BriefCalendarSection groups session and held-name event context.
type BriefCalendarSection struct {
	BriefRowState
	Session      BriefSessionRow       `json:"session"`
	MarketEvents []BriefMarketEventRow `json:"market_events"`
}

// BriefSessionRow reports the official market session and next opening time.
type BriefSessionRow struct {
	BriefRowState
	Market   string    `json:"market,omitempty"`
	State    string    `json:"state,omitempty"`
	IsOpen   bool      `json:"is_open"`
	Open     time.Time `json:"open,omitzero"`
	Close    time.Time `json:"close,omitzero"`
	NextOpen time.Time `json:"next_open,omitzero"`
}

// BriefMarketEventRow summarizes one approved held-name event family.
type BriefMarketEventRow struct {
	BriefRowState
	Kind    string   `json:"kind"` // earnings | halt | ssr | borrow
	Count   int      `json:"count"`
	Symbols []string `json:"symbols,omitempty"`
}

// BriefPortfolioSection groups account, attribution, option-money, and order
// observations.
type BriefPortfolioSection struct {
	BriefRowState
	Account       BriefAccountRow       `json:"account"`
	Movers        BriefMoversRow        `json:"movers"`
	PremiumAtRisk BriefMoneyCoverageRow `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow `json:"hedge_cost"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

// BriefAccountRow reports base-currency equity and P&L. Nil amounts mean the
// observation is unavailable.
type BriefAccountRow struct {
	BriefRowState
	EquityBase   *float64  `json:"equity_base,omitempty"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	AsOf         time.Time `json:"as_of,omitzero"`
}

// BriefMover is one underlying's base-currency daily P&L contribution.
type BriefMover struct {
	Symbol       string  `json:"symbol"`
	DailyPnLBase float64 `json:"daily_pnl_base"`
}

// BriefMoversRow aggregates daily P&L by underlying (stock plus option legs
// per name — the same basis as the Underlyings panel) so the two surfaces
// reconcile. OtherPnLBase/OtherCount carry the residual beyond the top rows
// so the row's implied total matches the account daily P&L attribution.
type BriefMoversRow struct {
	BriefRowState
	Rows         []BriefMover `json:"rows"`
	OtherPnLBase *float64     `json:"other_daily_pnl_base,omitempty"`
	OtherCount   int          `json:"other_count,omitempty"`
}

// BriefMoneyCoverageRow reports a base-currency aggregate and explicit leg
// coverage. AmountBase is nil when complete conversion is unavailable.
type BriefMoneyCoverageRow struct {
	BriefRowState
	AmountBase   *float64 `json:"amount_base,omitempty"`
	BaseCurrency string   `json:"base_currency,omitempty"`
	IncludedLegs int      `json:"included_legs"`
	ExcludedLegs int      `json:"excluded_legs"`
}

// BriefCountRow reports an optional count; nil means unavailable, not zero.
type BriefCountRow struct {
	BriefRowState
	Count *int `json:"count,omitempty"`
}

// BriefRiskSection groups capital, latch, override, and policy-drift evidence.
type BriefRiskSection struct {
	BriefRowState
	Capital     BriefCapitalRow     `json:"capital"`
	Latch       BriefLatchRow       `json:"latch"`
	Overrides   BriefOverridesRow   `json:"overrides"`
	PolicyDrift BriefPolicyDriftRow `json:"policy_drift"`
}

// BriefCapitalRow reports drawdown capacity and peak provenance. Pointer
// amounts remain nil when unavailable.
type BriefCapitalRow struct {
	BriefRowState
	Tier             string   `json:"tier,omitempty"`
	Enforcement      string   `json:"enforcement,omitempty"`
	ConsumedPct      *float64 `json:"consumed_pct,omitempty"`
	DrawdownBase     *float64 `json:"drawdown_base,omitempty"`
	AdjustedPeakBase *float64 `json:"adjusted_peak_base,omitempty"`
	// PeakAsOf is when the current adjusted peak was observed. Provenance,
	// not decoration: a peak stamped during a closed session or a reconnect
	// window is the tell that exposes a poisoned observation.
	PeakAsOf     time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency string    `json:"base_currency,omitempty"`
}

// BriefLatchRow reports durable drawdown-latch state and its original trigger.
type BriefLatchRow struct {
	BriefRowState
	Latched bool      `json:"latched"`
	At      time.Time `json:"latched_at,omitzero"`
	AgeDays *int      `json:"age_days,omitempty"`
	// ConsumedPctAtLatch is the consumed share recorded when the latch
	// engaged, so later data glitches cannot rewrite why it fired.
	ConsumedPctAtLatch *float64 `json:"consumed_pct_at_latch,omitempty"`
}

// BriefOverride identifies one active control override and expiry.
type BriefOverride struct {
	Control   string    `json:"control"`
	ExpiresAt time.Time `json:"expires_at"`
}

// BriefOverridesRow lists active overrides; an empty list is conclusive only
// when the embedded row state is OK.
type BriefOverridesRow struct {
	BriefRowState
	Rows []BriefOverride `json:"rows"`
}

// BriefPolicyDriftRow lists sibling-policy pin status.
type BriefPolicyDriftRow struct {
	BriefRowState
	Rows []PolicyPinStatus `json:"rows"`
}

// BriefProcessSection groups reconciliation and recurring process evidence.
type BriefProcessSection struct {
	BriefRowState
	Reconcile    BriefReconcileRow     `json:"reconcile"`
	AutoExtend   BriefAutoExtendRow    `json:"auto_extend"`
	Rules        BriefRulesRow         `json:"rules"`
	MonthlyPulse *BriefMonthlyPulseRow `json:"monthly_pulse,omitempty"`
}

// BriefMonthlyPulseRow has its own status vocabulary rather than embedding
// BriefRowState (whose status is ok|degraded|unavailable). It remains optional
// until the later daemon composition lands, preserving current brief identity.
type BriefMonthlyPulseRow struct {
	Status      string    `json:"status"` // not_due | completed | blocked
	Month       string    `json:"month,omitempty"`
	DueAt       time.Time `json:"due_at,omitzero"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// BriefReconcileRow reports the latest reconciliation and its next deadline.
type BriefReconcileRow struct {
	BriefRowState
	LastReconciledAt time.Time `json:"last_reconciled_at,omitzero"`
	Source           string    `json:"source,omitempty"`
	Deadline         time.Time `json:"deadline,omitzero"`
	DaysRemaining    *int      `json:"days_remaining,omitempty"`
}

// BriefAutoExtendRow reports clean-report automatic extension evidence.
type BriefAutoExtendRow struct {
	BriefRowState
	ReportID string    `json:"report_id,omitempty"`
	At       time.Time `json:"at,omitzero"`
}

// BriefRulesRow summarizes current policy adherence. It deliberately uses no
// acknowledgement or presentation baseline: viewing a brief is a pure read.
type BriefRulesRow struct {
	BriefRowState
	Pass    int `json:"pass"`
	Watch   int `json:"watch"`
	Act     int `json:"act"`
	Unknown int `json:"unknown"`
}

// BriefProposalsRow reports how many protection proposals were offered versus
// acted on over the most recent recorded session, derived read-only from the
// trade-proposal-outcomes journal. It carries counts and the covered day only:
// no proposal keys, symbols, order references, or tokens reach the wire.
type BriefProposalsRow struct {
	BriefRowState
	Day     string `json:"day,omitempty"`
	Offered int    `json:"offered"`
	Acted   int    `json:"acted"`
}

// BriefReadyProposalsRow reports how many protection proposals the daemon
// currently ranks as actionable for the session ahead, read-only from the
// live proposal snapshot. It is the pre-trade twin of BriefProposalsRow's
// post-trade journal counts, and it carries counts only: no proposal keys,
// symbols, contracts, order references, or preview tokens reach the wire.
// Stating that work is staged is not authority to place it — every submit
// keeps its own gating.
type BriefReadyProposalsRow struct {
	BriefRowState
	// Actionable is the served count of proposals with no blockers; Blocked
	// is the remainder of Total. Zero is a measured zero only when the
	// embedded row state is OK.
	Actionable int `json:"actionable"`
	Blocked    int `json:"blocked"`
	Total      int `json:"total"`
}

// BriefCapitalEventsRow frames the drawdown latch and adjusted-peak provenance
// as post-trade capital events for the Review movement. The fields mirror the
// existing latch/peak facts; nothing new is invented.
type BriefCapitalEventsRow struct {
	BriefRowState
	Latched            bool      `json:"latched"`
	LatchedAt          time.Time `json:"latched_at,omitzero"`
	LatchAgeDays       *int      `json:"latch_age_days,omitempty"`
	ConsumedPctAtLatch *float64  `json:"consumed_pct_at_latch,omitempty"`
	AdjustedPeakBase   *float64  `json:"adjusted_peak_base,omitempty"`
	PeakAsOf           time.Time `json:"peak_as_of,omitzero"`
	BaseCurrency       string    `json:"base_currency,omitempty"`
}

// BriefLastSessionRow is the daemon's close capture of the last completed
// session's account Daily P&L: the reqPnL account frame observed at (or on
// the first frame after) that session's official close, keyed by session
// date. Unlike SessionPnL it never moves on off-session marks. Nil
// DailyPnLBase with a populated SessionDate means that close was not captured
// — the daemon was not running and connected inside the capture window — and
// surfaces must say so rather than substitute a drifted running value.
type BriefLastSessionRow struct {
	BriefRowState
	SessionDate  string    `json:"session_date,omitempty"`
	DailyPnLBase *float64  `json:"daily_pnl_base,omitempty"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	SessionClose time.Time `json:"session_close,omitzero"`
	CapturedAt   time.Time `json:"captured_at,omitzero"`
}

// BriefReviewSection is the post-trade movement since the last regular close.
// Its rows are a regrouping of existing brief facts (plus the read-only
// proposals-offered-vs-acted derivation and the last-session close capture);
// the section rolls up its worst child exactly like every other brief section.
type BriefReviewSection struct {
	BriefRowState
	SessionPnL    BriefAccountRow       `json:"session_pnl"`
	LastSession   BriefLastSessionRow   `json:"last_session"`
	Attribution   BriefMoversRow        `json:"attribution"`
	Proposals     BriefProposalsRow     `json:"proposals"`
	Overrides     BriefOverridesRow     `json:"overrides"`
	CapitalEvents BriefCapitalEventsRow `json:"capital_events"`
	Rules         BriefRulesRow         `json:"rules"`
	Reconcile     BriefReconcileRow     `json:"reconcile"`
	AutoExtend    BriefAutoExtendRow    `json:"auto_extend"`
	WorkingOrders BriefCountRow         `json:"working_orders"`
}

// BriefReadySection is the pre-trade movement for today. Its rows regroup the
// existing market, calendar, risk-capacity, and desk-readiness facts.
type BriefReadySection struct {
	BriefRowState
	Regime        BriefRegimeRow         `json:"regime"`
	Breadth       BriefBreadthRow        `json:"breadth"`
	Gamma         BriefGammaRow          `json:"gamma"`
	Stress        BriefStressRow         `json:"stress"`
	Session       BriefSessionRow        `json:"session"`
	MarketEvents  []BriefMarketEventRow  `json:"market_events"`
	Capital       BriefCapitalRow        `json:"capital"`
	Latch         BriefLatchRow          `json:"latch"`
	PremiumAtRisk BriefMoneyCoverageRow  `json:"premium_at_risk"`
	HedgeCost     BriefMoneyCoverageRow  `json:"hedge_cost"`
	Proposals     BriefReadyProposalsRow `json:"proposals"`
	PolicyDrift   BriefPolicyDriftRow    `json:"policy_drift"`
	MonthlyPulse  *BriefMonthlyPulseRow  `json:"monthly_pulse,omitempty"`
}

// Brief narrative run roles. A run carries text plus at most one role, and a
// role is a claim about the underlying row, not a styling hint: figure marks a
// first-class served number, watch and act may appear only on clauses whose
// own row status or served severity is watch- or act-class. Surfaces map the
// roles to their own register and must never re-derive them.
const (
	BriefRunRoleFigure = "figure"
	BriefRunRoleWatch  = "watch"
	BriefRunRoleAct    = "act"
)

// BriefRun is one typed span of composed narrative text. Runs are text, never
// markup: the daemon emits the spans and each surface decides how a role
// renders.
type BriefRun struct {
	Text string `json:"text"`
	Role string `json:"role,omitempty"`
}

// BriefParagraph is one composed paragraph as an ordered run sequence.
type BriefParagraph struct {
	Runs []BriefRun `json:"runs,omitempty"`
}

// BriefNarrative is the daemon-composed prose reading of the same two
// movements BriefResult already carries. It states served facts and their
// served statuses in fixed template language and adds no fact of its own, so
// it stays outside the brief content identity: BriefFingerprint hashes Review
// and Ready only, and a prose revision can never invalidate its identity.
// Absent when an older daemon serves the brief; surfaces fall back to the row
// render.
type BriefNarrative struct {
	Lead   []BriefRun       `json:"lead,omitempty"`
	Review []BriefParagraph `json:"review,omitempty"`
	Ready  []BriefParagraph `json:"ready,omitempty"`
	Coda   []BriefRun       `json:"coda,omitempty"`
}

// BriefResult is the complete typed daily brief, composed as two process
// movements: Review (post-trade since the last regular close) and Ready
// (pre-trade for today). BriefFingerprint hashes the two composed movements
// only; AsOf and Narrative are deliberately outside the content identity. The
// daemon composes both movements; surfaces render them verbatim.
type BriefResult struct {
	AsOf             time.Time          `json:"as_of"`
	BriefFingerprint string             `json:"brief_fingerprint"`
	Review           BriefReviewSection `json:"review"`
	Ready            BriefReadySection  `json:"ready"`
	Narrative        *BriefNarrative    `json:"narrative,omitempty"`
}
