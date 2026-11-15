package rpc

import (
	"context"
	"time"

	"github.com/osauer/canary/v3/internal/core"
)

// MethodCoreTarget serves the daemon-owned core sleeve target: the month's
// decision under the compiled rule, the target shares, the holding, the
// band test and the action the rule would take
// (internal-docs/design/core-sleeve.md). Reading it changes nothing: while
// the core is live its month's row is served among the trade proposals for
// the owner's device confirmation (Row), and in shadow nothing is proposed.
const MethodCoreTarget = "core.target"

// CoreTargetParams is the input for MethodCoreTarget. Refresh asks the
// daemon to measure the mandate again first.
type CoreTargetParams struct {
	Refresh bool `json:"refresh,omitempty"`
}

// Core target statuses.
const (
	// CoreStatusReady: the month is decided and the action is computed.
	CoreStatusReady = "ready"
	// CoreStatusHold: something the decision needs is missing, stale,
	// suspect or unwritten; Reasons name it. Buys stop; a sale the rule
	// wants goes to the owner.
	CoreStatusHold = "hold"
	// CoreStatusInactive: the constitution carries no [mandate.core].
	CoreStatusInactive = "inactive"
)

// CoreTargetResult is the core.target payload: the strategy target of the
// register's first entry.
type CoreTargetResult = StrategyTargetResult

// StrategyTargetResult is one registered strategy's verdict
// (internal-docs/design/strategy-evaluator.md): the shape core.target has
// served since phase 1, for every entry of the strategy register.
type StrategyTargetResult struct {
	AsOf   time.Time `json:"as_of"`
	Active bool      `json:"active"`
	Status string    `json:"status"`
	// Strategy is the entry's id in the register ("core" for the core) and
	// Sleeve where its positions count.
	Strategy string `json:"strategy,omitempty"`
	Sleeve   string `json:"sleeve,omitempty"`
	// Message says why the core is inactive.
	Message string `json:"message,omitempty"`
	// Reasons name every hold.
	Reasons []string `json:"reasons,omitempty"`
	// ReadOnly is true while the core proposes nothing (shadow, suspended
	// or on hold): the action is then what the rule would do. Live, the
	// action is served as Row, a proposal the owner confirms on a device;
	// the core itself never sends.
	ReadOnly bool `json:"read_only"`
	// Rule and SpecHash name the compiled rule and the lab spec it
	// implements (s0_core_sleeve); PolicyRule is the rule the policy names.
	// For a parameterised rule SpecHash is the spec the entry cites.
	Rule       string `json:"rule"`
	SpecID     string `json:"spec_id,omitempty"`
	SpecHash   string `json:"spec_hash"`
	PolicyRule string `json:"policy_rule,omitempty"`
	// Signal is the trend filter the strategy runs on, from the library.
	Signal string `json:"signal,omitempty"`
	// Kill is the kill rule's drawdown test as measured on the shadow
	// record, and its bound when written.
	Kill *KillView `json:"kill,omitempty"`
	// Stage is the core's entry in the strategy register as this build runs
	// it (internal-docs/design/strategy-register.md).
	Stage *CoreStageView `json:"stage,omitempty"`
	// Shadow is the core's shadow record while it runs in shadow: the paper
	// position on its production code and the newest records.
	Shadow *ShadowView `json:"shadow,omitempty"`
	// PolicyFingerprint identifies the constitution read.
	PolicyFingerprint *Fingerprint     `json:"policy_fingerprint,omitempty"`
	Policy            *CorePolicyView  `json:"policy,omitempty"`
	Instrument        *CoreInstrument  `json:"instrument,omitempty"`
	Decision          *CoreDecision    `json:"decision,omitempty"`
	Sizing            *CoreSizingView  `json:"sizing,omitempty"`
	Holding           *CoreHolding     `json:"holding,omitempty"`
	Action            *core.Decision   `json:"action,omitempty"`
	Mandate           *CoreMandateView `json:"mandate,omitempty"`
	// NextDecision is the next month's decision day on the exchange
	// calendar, when the calendar covers it.
	NextDecision string        `json:"next_decision,omitempty"`
	Evidence     *CoreEvidence `json:"evidence,omitempty"`
	// Row is the core's pending proposal row as the proposal engine serves
	// it, while one stands (live only); LastDecision the last month the
	// core decided while live and what became of it.
	Row          *CoreRowView        `json:"row,omitempty"`
	LastDecision *CoreDecisionRecord `json:"last_decision,omitempty"`
}

// CoreRowTerms are the order terms of the core's row (core-sleeve.md,
// "Window and order terms", owner decision D2): one function builds them
// for the live row and the shadow intent, so the two never disagree.
type CoreRowTerms struct {
	// OrderType and TIF: a plain DAY limit. Limit says how it is priced:
	// the live mid, snapped to the tick grid, never crossing the mid.
	OrderType string `json:"order_type"`
	TIF       string `json:"tif"`
	Limit     string `json:"limit"`
	// Exchange is the route (SMART) and PrimaryExchange the line's venue
	// as IBKR's record names it.
	Exchange        string `json:"exchange"`
	PrimaryExchange string `json:"primary_exchange,omitempty"`
	// Window lists the sessions the row is served for: the first sessions
	// on the line's calendar after the decision day that the US also
	// trades. A sale to zero continues past it until sold.
	Window []string `json:"window,omitempty"`
	// Episode keys the month's rows: core:<account>:<mode>:<YYYY-MM> for
	// the core, strategy:<id>:<account>:<mode>:<YYYY-MM> for another entry.
	Episode string `json:"episode,omitempty"`
}

// CoreRowView is the core's pending proposal row: its key and revision
// for `canary proposals`, the order, the session it is for, and whether
// it stands open or blocked, with the blockers and readiness as served.
type CoreRowView struct {
	Key      string `json:"key"`
	Revision string `json:"revision"`
	Action   string `json:"action"`
	Quantity int    `json:"quantity"`
	// ReferencePrice is the price the row was sized at (the held line's
	// mark, else the last close); the order's limit is the live mid at
	// preview.
	ReferencePrice float64          `json:"reference_price"`
	Session        string           `json:"session"`
	State          string           `json:"state"`
	Blockers       []TradingBlocker `json:"blockers,omitempty"`
	// Readiness is the served readiness code (ready, market_closed, ...).
	Readiness string       `json:"readiness,omitempty"`
	Terms     CoreRowTerms `json:"terms"`
}

// CoreDecisionRecord is the last decision the core made while live, kept
// with the frozen month: the action it decided on the holding then, the
// window, and what became of its row.
type CoreDecisionRecord struct {
	Decision  string    `json:"decision"`
	DecidedAt time.Time `json:"decided_at"`
	Stage     string    `json:"stage"`
	// Kind is buy, sell, sell_all, wait, none or hold; Quantity the shares;
	// Why the rule's reason, the gate's refusal included.
	Kind     string  `json:"kind"`
	Quantity int     `json:"quantity,omitempty"`
	Why      string  `json:"why"`
	Target   int     `json:"target_shares"`
	Held     int     `json:"held_shares"`
	Price    float64 `json:"price,omitempty"`
	// Window is the sessions the row was served for; RowKey the row.
	Window []string `json:"window,omitempty"`
	RowKey string   `json:"row_key,omitempty"`
	// Outcome is what became of the row, from the proposal journal.
	Outcome CoreDecisionOutcome `json:"outcome"`
}

// CoreDecisionOutcome is the row's fate so far: served (a row was made),
// submitted (the owner confirmed it and it went to the broker, with the
// order reference), or the latest refusal recorded on it.
type CoreDecisionOutcome struct {
	Served      bool      `json:"served"`
	Submitted   bool      `json:"submitted,omitempty"`
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	OrderRef    string    `json:"order_ref,omitempty"`
	Refused     string    `json:"refused,omitempty"`
	RefusedAt   time.Time `json:"refused_at,omitzero"`
}

// CoreStageView is the core's ladder stage: as written (empty while
// unwritten, which runs in shadow), as this build runs it (shadow, live,
// suspended or hold), the part of its allocation it reserves in percent, and
// whether the policy cites the lab spec hash or the compiled one stands.
type CoreStageView struct {
	Written       string   `json:"written,omitempty"`
	State         string   `json:"state"`
	ReservedPct   float64  `json:"reserved_pct"`
	SpecHashCited bool     `json:"spec_hash_cited"`
	Notes         []string `json:"notes,omitempty"`
}

// CorePolicyView is the strategy's entry as the daemon read it:
// [mandate.core] with the sleeve share from [mandate.sleeves] (CoreVolPct),
// or a [mandate.strategies.<id>] table with its allocation (AllocationPct)
// and signal; nil numbers are unwritten.
type CorePolicyView struct {
	Rule          string   `json:"rule,omitempty"`
	ISIN          string   `json:"isin,omitempty"`
	ConID         *int     `json:"con_id,omitempty"`
	Exchange      string   `json:"exchange,omitempty"`
	Currency      string   `json:"currency,omitempty"`
	Signal        string   `json:"signal,omitempty"`
	ScalePct      *float64 `json:"scale_pct,omitempty"`
	BandPct       *float64 `json:"band_pct,omitempty"`
	MinOrderBase  *float64 `json:"min_order_base,omitempty"`
	MaxValuePct   *float64 `json:"max_value_pct,omitempty"`
	CoreVolPct    *float64 `json:"core_vol_pct,omitempty"`
	AllocationPct *float64 `json:"allocation_pct,omitempty"`
	Unapproved    []string `json:"unapproved,omitempty"`
}

// KillView is the kill rule's drawdown test (strategy-evaluator.md §3):
// the bound in units of sleeve capital K when the policy writes it, the
// largest peak-to-trough loss measured on the shadow record at its
// decisions, and whether anything was measured. Past the bound the
// strategy holds; the owner suspends it.
type KillView struct {
	MaxDrawdownK *float64 `json:"max_drawdown_k,omitempty"`
	DrawdownK    float64  `json:"drawdown_k"`
	Measured     bool     `json:"measured"`
	Decisions    int      `json:"decisions"`
	Breached     bool     `json:"breached,omitempty"`
	Note         string   `json:"note,omitempty"`
	// Source is what the drawdown is measured on: absent for the shadow
	// record (the paper position), "own_fills" for a live entry's own
	// fills marked at each decision close.
	Source string `json:"source,omitempty"`
	// ScalePct is the stage's scale the drawdown is measured against: K
	// at the stage is the fully invested sleeve's notional times the scale
	// (since 2026-10-19; before, the full allocation a lower stage does not
	// trade, so a bound could not fire in time).
	ScalePct float64 `json:"scale_pct,omitempty"`
}

// KillSourceOwnFills marks a kill test measured on a live entry's own fills.
const KillSourceOwnFills = "own_fills"

// CoreInstrument is the instrument the compiled rule trades: the fund and
// currency the policy names, and the contract IBKR lists under that ISIN in
// that currency once it is resolved. Symbol is IBKR's symbol for the
// resolved line (the rule's own name for the fund before); Exchange is the
// listing's primary exchange, where the series is read, and Calendar the
// exchange calendar its sessions and decision days are judged on.
type CoreInstrument struct {
	Symbol   string `json:"symbol"`
	ISIN     string `json:"isin"`
	ConID    int    `json:"con_id,omitempty"`
	Exchange string `json:"exchange,omitempty"`
	Currency string `json:"currency"`
	// Resolved says IBKR's contract record names the line below; false
	// while the core holds on its contract.
	Resolved      bool      `json:"resolved"`
	LocalSymbol   string    `json:"local_symbol,omitempty"`
	TradingClass  string    `json:"trading_class,omitempty"`
	Calendar      string    `json:"calendar,omitempty"`
	CalendarLabel string    `json:"calendar_label,omitempty"`
	ResolvedAt    time.Time `json:"resolved_at,omitzero"`
}

// CoreDecision is the month's decision: the signal, and when it was frozen.
type CoreDecision struct {
	core.Signal
	// FrozenAt is when the daemon first decided this month; the target is
	// frozen then and only lowered afterwards. Drift says a later reading
	// of the same window's closes differs from the frozen one.
	FrozenAt time.Time `json:"frozen_at,omitzero"`
	Drift    bool      `json:"drift,omitempty"`
	// SignalFrom names the signal series the month was measured on when it
	// is not the traded line's own (another share class of the fund, while
	// the traded line's closes do not fill the window); TradedClose is then
	// the traded line's own close on or before the decision day, which the
	// target is sized on, the row's limit is checked against and the kill
	// rule marks. Both empty on the traded line's own closes.
	SignalFrom  string  `json:"signal_from,omitempty"`
	TradedClose float64 `json:"traded_close,omitempty"`
}

// LineClose is the traded line's close of the decision: TradedClose when
// the month was measured on another share class, else the decision close.
func (d CoreDecision) LineClose() float64 {
	if d.TradedClose > 0 {
		return d.TradedClose
	}
	return d.Close
}

// CoreSizingView is the target in money and shares: frozen at the
// decision, and at today's capital; Target is the lower of the two.
type CoreSizingView struct {
	Frozen     core.Size   `json:"frozen"`
	FrozenOn   core.Sizing `json:"frozen_on"`
	Current    core.Size   `json:"current"`
	CurrentOn  core.Sizing `json:"current_on"`
	Target     int         `json:"target_shares"`
	TargetBase float64     `json:"target_value_base"`
	// Holdable is the largest holding the mandate's headroom admits:
	// the book at most at 0.95 of the budget and the core's gap at most at
	// 0.95 of its cap. Nil when no headroom was measured (no buy wanted).
	Holdable *int `json:"holdable,omitempty"`
}

// CoreHolding is the confirmed holding of the pinned contract.
type CoreHolding struct {
	Shares    int     `json:"shares"`
	Price     float64 `json:"price"`
	ValueBase float64 `json:"value_base"`
	// PriceSource is "mark" (the held line) or "close" (the last close).
	PriceSource string `json:"price_source"`
}

// CoreMandateView is the mandate verdict the core read.
type CoreMandateView struct {
	BaseCurrency   string    `json:"base_currency,omitempty"`
	Status         string    `json:"status"`
	EvaluatedAt    time.Time `json:"evaluated_at,omitzero"`
	BudgetAnnualSD float64   `json:"budget_annual_sd_base,omitempty"`
	RiskAnnualSD   float64   `json:"risk_annual_sd_base,omitempty"`
}

// CoreEvidence is every clock and source behind the target.
type CoreEvidence struct {
	SeriesKey string    `json:"series_key"`
	Closes    int       `json:"closes"`
	LastClose string    `json:"last_close,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Source    string    `json:"source,omitempty"`
	Stale     bool      `json:"stale,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
	// Calendar names the exchange calendar and its coverage.
	Calendar string `json:"calendar,omitempty"`
	// ReadError is IBKR's answer to the last failed series read, while no
	// read has succeeded since; ReadErrorAt is when it came.
	ReadError   string    `json:"read_error,omitempty"`
	ReadErrorAt time.Time `json:"read_error_at,omitzero"`
	// Signal is the entry's signal series (another share class of the
	// fund) while the traded line's own closes do not fill the window; nil
	// when none is written.
	Signal *CoreSignalEvidence `json:"signal,omitempty"`
}

// CoreSignalEvidence is a signal series as the evaluator read it: the
// listing, the series and, when its closes are measured in another
// currency, the exchange-rate series that converts them, and whether it
// decides the month or the traded line's own closes have taken over.
type CoreSignalEvidence struct {
	ISIN       string `json:"isin"`
	Currency   string `json:"currency"`
	MeasuredIn string `json:"measured_in"`
	Symbol     string `json:"symbol,omitempty"`
	ConID      int    `json:"con_id,omitempty"`
	Calendar   string `json:"calendar,omitempty"`
	SeriesKey  string `json:"series_key,omitempty"`
	Closes     int    `json:"closes,omitempty"`
	LastClose  string `json:"last_close,omitempty"`
	FXKey      string `json:"fx_key,omitempty"`
	// InUse says the month is measured on this series; OwnSessions is how
	// many of the window's sessions the traded line's own closes cover.
	InUse       bool `json:"in_use"`
	OwnSessions int  `json:"own_sessions"`
}

// FetchCoreTarget reads the daemon's core target. The daemon evaluates it
// (internal/core), so a reader compiled at an older version still shows
// the installed daemon's decision.
func FetchCoreTarget(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, refresh bool) (CoreTargetResult, error) {
	var out CoreTargetResult
	if err := conn.Call(ctx, MethodCoreTarget, CoreTargetParams{Refresh: refresh}, &out); err != nil {
		return CoreTargetResult{}, err
	}
	return out, nil
}

// MethodStrategyTarget serves a registered strategy's target
// (internal-docs/design/strategy-evaluator.md): the same verdict shape as
// MethodCoreTarget for any entry of the strategy register, by id. Read-only:
// no entry proposes or sends anything in this build.
const MethodStrategyTarget = "strategy.target"

// StrategyTargetParams is the input for MethodStrategyTarget: the entry's
// id ("core" is the core); Refresh measures the mandate again first.
type StrategyTargetParams struct {
	ID      string `json:"id"`
	Refresh bool   `json:"refresh,omitempty"`
}

// FetchStrategyTarget reads the daemon's target for one registered
// strategy.
func FetchStrategyTarget(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, id string, refresh bool) (StrategyTargetResult, error) {
	var out StrategyTargetResult
	if err := conn.Call(ctx, MethodStrategyTarget, StrategyTargetParams{ID: id, Refresh: refresh}, &out); err != nil {
		return StrategyTargetResult{}, err
	}
	return out, nil
}

// MethodCoreResolve lists every listing IBKR knows for an ISIN (by default
// the one [mandate.core] pins, else the compiled rule's), so the owner can
// write the contract id into [mandate.core]. Read-only: it never chooses a
// listing and never writes the policy.
const MethodCoreResolve = "core.resolve"

// CoreResolveParams is the input for MethodCoreResolve. ISIN overrides the
// default; Currency, when set, narrows every request form to it.
type CoreResolveParams struct {
	ISIN     string `json:"isin,omitempty"`
	Currency string `json:"currency,omitempty"`
}

// CoreResolveResult is the core.resolve payload: every listing found, the
// contract id the policy pins (nil while unwritten or 0), and every
// request form with IBKR's answer.
type CoreResolveResult struct {
	AsOf time.Time `json:"as_of"`
	ISIN string    `json:"isin"`
	// ISINSource is "flag", "policy" or "rule": where the ISIN came from.
	ISINSource string        `json:"isin_source"`
	Currency   string        `json:"currency,omitempty"`
	Listings   []CoreListing `json:"listings"`
	// PinnedConID is [mandate.core] con_id when written and positive;
	// PinnedFound says a listing carries it.
	PinnedConID *int                 `json:"pinned_con_id,omitempty"`
	PinnedFound bool                 `json:"pinned_found,omitempty"`
	Attempts    []CoreResolveAttempt `json:"attempts"`
	// Message says what the owner does next.
	Message string `json:"message"`
}

// CoreListing is one listing IBKR answered for the ISIN.
type CoreListing struct {
	ConID        int    `json:"con_id"`
	Symbol       string `json:"symbol"`
	LocalSymbol  string `json:"local_symbol,omitempty"`
	SecType      string `json:"sec_type,omitempty"`
	Exchange     string `json:"exchange,omitempty"`
	PrimaryExch  string `json:"primary_exchange,omitempty"`
	Currency     string `json:"currency,omitempty"`
	TradingClass string `json:"trading_class,omitempty"`
	LongName     string `json:"long_name,omitempty"`
	// ISIN is the one IBKR's frame named; empty when it named none.
	ISIN  string   `json:"isin,omitempty"`
	Forms []string `json:"forms"`
	// Pinned says [mandate.core] con_id names this listing; Used says the
	// core reads and would trade it (its last resolution).
	Pinned bool `json:"pinned,omitempty"`
	Used   bool `json:"used,omitempty"`
}

// CoreResolveAttempt is one request form and IBKR's answer: line (with how
// many listings it kept), no_line, rejected (with IBKR's code and text) or
// failed.
type CoreResolveAttempt struct {
	Form    string `json:"form"`
	Outcome string `json:"outcome"`
	Lines   int    `json:"lines,omitempty"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// FetchCoreResolve asks the daemon for the listings of an ISIN.
func FetchCoreResolve(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, params CoreResolveParams) (CoreResolveResult, error) {
	var out CoreResolveResult
	if err := conn.Call(ctx, MethodCoreResolve, params, &out); err != nil {
		return CoreResolveResult{}, err
	}
	return out, nil
}

// MethodStrategyReplay serves a registered strategy's replay
// (internal-docs/design/strategy-evaluator.md §9): the rule run over the
// month-ends of the entry's instrument on the daemon's own daily history,
// in the lab's table shape, so `lab replay-compare` can hold the daemon's
// decisions against the lab's backtest. Read-only: nothing is frozen,
// recorded or sent.
const MethodStrategyReplay = "strategy.replay"

// StrategyReplayParams is the input for MethodStrategyReplay: the entry's
// id ("core" is the core) and the span, two ISO dates inclusive; To is
// bounded at the last decided month-end.
type StrategyReplayParams struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

// StrategyReplayResult is the decision table and the series it was run on.
type StrategyReplayResult struct {
	AsOf     time.Time `json:"as_of"`
	Strategy string    `json:"strategy"`
	Rule     string    `json:"rule"`
	SpecHash string    `json:"spec_hash,omitempty"`
	Signal   string    `json:"signal"`
	// Instrument is the contract the series was read for, as resolved.
	Instrument *CoreInstrument `json:"instrument,omitempty"`
	// From and To bound the decisions; SeriesFrom and SeriesTo the closes.
	From       string `json:"from"`
	To         string `json:"to"`
	SeriesFrom string `json:"series_from,omitempty"`
	SeriesTo   string `json:"series_to,omitempty"`
	// Calendar is the exchange calendar the sessions and month-ends were
	// judged on (the series' own dates before the embedded coverage).
	Calendar      string `json:"calendar"`
	CalendarLabel string `json:"calendar_label,omitempty"`
	// Source and FetchedAt are the broker read's basis and time.
	Source    string    `json:"source,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	// Dates and Closes are the series replayed; OffCalendar names bars
	// dated on days the exchange was closed, which the calendar skips.
	Dates       []string  `json:"dates"`
	Closes      []float64 `json:"closes"`
	OffCalendar []string  `json:"off_calendar,omitempty"`
	// Sizing is the fixed paper sizing and Terms the entry's band and
	// minimum, printed so the two tables are compared under one sizing.
	Sizing core.Sizing      `json:"sizing"`
	Terms  core.ReplayTerms `json:"terms"`
	Note   string           `json:"note,omitempty"`
	Rows   []core.ReplayRow `json:"rows"`
}

// FetchStrategyReplay reads the daemon's replay for one registered
// strategy over a span.
func FetchStrategyReplay(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, id, from, to string) (StrategyReplayResult, error) {
	var out StrategyReplayResult
	if err := conn.Call(ctx, MethodStrategyReplay, StrategyReplayParams{ID: id, From: from, To: to}, &out); err != nil {
		return StrategyReplayResult{}, err
	}
	return out, nil
}
