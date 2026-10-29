package rpc

import (
	"context"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
)

// MethodMandateRisk serves the daemon-owned mandate risk verdict
// (internal-docs/design/mandate-risk-budget.md): the loss measure, premium
// at risk on every bought option plus the cash loss to every armed share
// stop, against the loss the mandate still allows (the headline, spec
// TM-04.12, owner decision 2026-10-27 19:50 CET), and the ex-ante
// volatility of the book by historical simulation, which sizes the sleeve
// allocations and reports the daily swing.
const MethodMandateRisk = "mandate.risk"

// MandateRiskParams is the input for MethodMandateRisk. Refresh asks the
// daemon to read the account and positions again before judging; otherwise
// a verdict younger than its refresh interval is served as it is.
type MandateRiskParams struct {
	Refresh bool `json:"refresh,omitempty"`
}

// MandateBudgetBlockerCode is the blocker of a risk-adding order the mandate
// gate refuses: the verdict is not within, or the addition would take the
// loss measure over the loss room, a gap over its cap or its sleeve over its
// allocation.
const MandateBudgetBlockerCode = "mandate_budget"

// MandatePremiumBlockerCode is the blocker of an option addition the premium
// limits refuse: the premium at risk on its name past option_name_act_pct
// of NLV, or the book's past the premium budget's act level (owner decision
// 2026-10-23 18:22 CEST: options add risk within premium limits, not within
// the volatility budget).
const MandatePremiumBlockerCode = "mandate_premium"

// MandateRiskResult is the mandate.risk payload. Active is false, with
// Message, while the constitution carries no [mandate] table; the verdict
// is then absent and the gate inactive.
type MandateRiskResult struct {
	AsOf   time.Time `json:"as_of"`
	Active bool      `json:"active"`
	// Message says why the mandate is inactive, or how the verdict was read.
	Message string `json:"message,omitempty"`
	// PolicyFingerprint identifies the constitution the verdict read.
	PolicyFingerprint *Fingerprint `json:"policy_fingerprint,omitempty"`
	// Policy echoes the mandate numbers in force.
	Policy *MandatePolicyView `json:"policy,omitempty"`
	// Verdict is the daemon's evaluation; nil while inactive.
	Verdict *mandate.Verdict `json:"verdict,omitempty"`
	// StatusSince is when the verdict's status last changed, as the daemon
	// has seen it across evaluations and restarts; zero while inactive.
	StatusSince time.Time `json:"status_since,omitzero"`
	// Evidence carries the clocks the verdict rests on.
	Evidence *MandateEvidence `json:"evidence,omitempty"`
	// Gate says how the additions gate reads the verdict right now.
	Gate *MandateGateView `json:"gate,omitempty"`
	// Reduction is the forced reduction as the proposal engine last served
	// it: the batch waiting, held or withdrawn. Nil while inactive or before
	// the engine has generated.
	Reduction *MandateReductionView `json:"reduction,omitempty"`
	// Register is the strategy register as this build runs it
	// (internal-docs/design/strategy-register.md): one entry per machine
	// strategy, the core first.
	Register []MandateStrategyView `json:"register,omitempty"`
	// Exposure is the book through the greeks and the premium at risk, per
	// underlying and in total, read from the same positions as the verdict
	// (owner decision 2026-10-23 18:22 CEST). Nil while no positions read is
	// current.
	Exposure *MandateExposureView `json:"exposure,omitempty"`
}

// MandateExposureView is the book's exposure for the operator to watch, never
// a gate except the premium limits it also serves: per underlying and for the
// book, net delta in money (stock-equivalent exposure), the same through each
// name's beta to the index, gamma (the money change of delta per 1% move),
// theta (money a day), vega (money per volatility point) and the premium at
// risk against its caps. A figure that cannot be measured is absent, with the
// reason, never zero.
type MandateExposureView struct {
	AsOf         time.Time `json:"as_of"`
	BaseCurrency string    `json:"base_currency,omitempty"`
	// NLVBase is the net liquidation value the premium caps are shares of;
	// NLVReason says why it is absent.
	NLVBase   *float64 `json:"nlv_base,omitempty"`
	NLVReason string   `json:"nlv_reason,omitempty"`
	// NamePremiumCapPct is option_name_act_pct; BookPremiumCapPct the premium
	// budget's act level of the regime set in force (BookPremiumSet), and
	// BookPremiumWatchPct its watch level, the budget for new buying.
	NamePremiumCapPct   float64 `json:"name_premium_cap_pct"`
	BookPremiumCapPct   float64 `json:"book_premium_cap_pct"`
	BookPremiumWatchPct float64 `json:"book_premium_watch_pct"`
	BookPremiumSet      string  `json:"book_premium_set,omitempty"`
	// Book is the whole book's row; Names one row per underlying, largest
	// delta exposure first.
	Book  MandateExposureRow   `json:"book"`
	Names []MandateExposureRow `json:"names,omitempty"`
}

// MandateExposureRow is one underlying's exposure, or the book's.
type MandateExposureRow struct {
	Underlying string `json:"underlying,omitempty"`
	// Legs counts the positions in the row; Options the option legs among them.
	Legs    int `json:"legs"`
	Options int `json:"options"`
	// ContributionPct is the row's share of the mandate's risk (Euler) and
	// Beta its beta to the index, both from the verdict; absent while the
	// verdict carries none (an unknown verdict, a name it does not price).
	ContributionPct *float64 `json:"contribution_pct,omitempty"`
	Beta            *float64 `json:"beta,omitempty"`
	// DeltaBase is net delta in base currency: shares plus every option's
	// delta × contracts × multiplier, times the spot. DeltaBetaBase is the
	// same through the beta (index-equivalent exposure). GammaBase is the
	// change of DeltaBase for a 1% move of the underlying; ThetaBase the
	// money a day of time decay (negative costs); VegaBase the money per
	// volatility point.
	DeltaBase     *float64 `json:"delta_base,omitempty"`
	DeltaBetaBase *float64 `json:"delta_beta_base,omitempty"`
	GammaBase     *float64 `json:"gamma_base,omitempty"`
	ThetaBase     *float64 `json:"theta_base,omitempty"`
	VegaBase      *float64 `json:"vega_base,omitempty"`
	// PremiumAtRiskBase is the premium at risk on every long leg, protection
	// included (the higher of price paid and value per leg; owner decision
	// 2026-10-27 19:52 CET, "Count it"), PremiumPctNLV its share of NLV,
	// PremiumCapBase the cap in money (the name's or the book's) and
	// PremiumRoomBase what the cap still leaves; PremiumOver says the cap is
	// reached. HedgePremiumBase is the protection premium within it.
	PremiumAtRiskBase *float64 `json:"premium_at_risk_base,omitempty"`
	PremiumPctNLV     *float64 `json:"premium_pct_nlv,omitempty"`
	PremiumCapBase    *float64 `json:"premium_cap_base,omitempty"`
	PremiumRoomBase   *float64 `json:"premium_room_base,omitempty"`
	PremiumOver       bool     `json:"premium_over,omitempty"`
	HedgePremiumBase  *float64 `json:"hedge_premium_base,omitempty"`
	// Unknown names each absent figure with why: a leg without greeks or
	// spot, a stale quote, no exchange rate, no verdict row.
	Unknown []string `json:"unknown,omitempty"`
}

// MandateStrategyView is one register entry: as the policy writes it (rule,
// cited spec hash, sleeve, allocation, stage and scale) and as this build
// runs it (state, the part of its allocation it reserves, the holds and the
// notes), with the data its rule declares.
type MandateStrategyView struct {
	ID            string   `json:"id"`
	Sleeve        string   `json:"sleeve"`
	Rule          string   `json:"rule,omitempty"`
	RuleCompiled  bool     `json:"rule_compiled"`
	SpecHash      string   `json:"spec_hash,omitempty"`
	SpecCited     bool     `json:"spec_cited"`
	AllocationPct float64  `json:"allocation_pct"`
	AllocationKey string   `json:"allocation_key"`
	Stage         string   `json:"stage,omitempty"`
	State         string   `json:"state"`
	ScalePct      float64  `json:"scale_pct"`
	ReservedPct   float64  `json:"reserved_pct"`
	Sends         bool     `json:"sends"`
	Reasons       []string `json:"reasons,omitempty"`
	Notes         []string `json:"notes,omitempty"`
	// DeclaredSeries and DeclaredChains name the data the rule reads, which
	// Canary collects while the rule is compiled, in any stage.
	DeclaredSeries []string `json:"declared_series,omitempty"`
	DeclaredChains []string `json:"declared_chains,omitempty"`
	// ISIN, Currency and Signal are the instrument and the filter the
	// evaluator runs the entry on, compiled or from its keys; empty on a hold.
	ISIN     string `json:"isin,omitempty"`
	Currency string `json:"currency,omitempty"`
	Signal   string `json:"signal,omitempty"`
}

// MandateReductionView is the forced reduction in brief for the mandate
// surface: how many rows wait, until when the veto runs, whether the owner
// pre-authorised the bucket, and a hold or a withdrawal.
type MandateReductionView struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	// Rows is the batch; Pending the rows the daemon will place after their
	// window (pre-authorised, with a record waiting); VetoUntil the latest
	// window end among them.
	Rows          int       `json:"rows"`
	Pending       int       `json:"pending,omitempty"`
	VetoUntil     time.Time `json:"veto_until,omitzero"`
	PreAuthorised bool      `json:"pre_authorised"`
	Held          bool      `json:"held,omitempty"`
	HoldReason    string    `json:"hold_reason,omitempty"`
	Withdrawn     int       `json:"withdrawn,omitempty"`
	// Orders is the batch in dispatch order.
	Orders []MandateReductionOrder `json:"orders,omitempty"`
}

// MandateReductionOrder is one row of the forced reduction as the mandate
// surface lists it: what it sells or buys back, why, and when the daemon
// places it unless vetoed.
type MandateReductionOrder struct {
	Rank     int            `json:"rank"`
	Key      string         `json:"key"`
	Contract ContractParams `json:"contract"`
	Action   string         `json:"action"`
	Quantity int            `json:"quantity"`
	// Phase is expiry, gap, premium or stop (MandatePhase*); LossBase the
	// leg's part of the loss measure and ContributionPct its share of the
	// variance on the verdict it was sized on.
	Phase           string  `json:"phase"`
	LossBase        float64 `json:"loss_base,omitempty"`
	ContributionPct float64 `json:"contribution_pct"`
	// Automatic is the row's automatic-submission state (pending, deferred,
	// submitting, submitted, vetoed, superseded or failed), empty when no
	// record exists; SubmitAt is when its veto window ends.
	Automatic string    `json:"automatic,omitempty"`
	SubmitAt  time.Time `json:"submit_at,omitzero"`
	// Held says the row waits for a fresh verdict; Blocked names the first
	// blocker when the row cannot be sent right now.
	Held    bool   `json:"held,omitempty"`
	Blocked string `json:"blocked,omitempty"`
}

// MandatePolicyView is the [mandate] table as the daemon read it.
type MandatePolicyView struct {
	MaxLossPct     float64 `json:"max_loss_pct"`
	VolTargetPct   float64 `json:"vol_target_pct"`
	TaxReserveBase float64 `json:"tax_reserve_base"`
	StartedAt      string  `json:"started_at"`
	GapShockPct    float64 `json:"gap_shock_pct"`
	// IndexGapShockPct is the broad index gap; nil while unwritten, when
	// every underlying takes GapShockPct.
	IndexGapShockPct *float64 `json:"index_gap_shock_pct,omitempty"`
	GapRoomPct       float64  `json:"gap_room_pct"`
	// Sleeves are the owner's recorded volatility shares; nil when unwritten.
	Sleeves *MandateSleevesView `json:"sleeves,omitempty"`
	// Unapproved names the material keys still unwritten.
	Unapproved []string `json:"unapproved,omitempty"`
}

// MandateSleevesView echoes [mandate.sleeves]; nil means unwritten.
type MandateSleevesView struct {
	OwnerVolPct   *float64 `json:"owner_vol_pct,omitempty"`
	CoreVolPct    *float64 `json:"core_vol_pct,omitempty"`
	OverlayVolPct *float64 `json:"overlay_vol_pct,omitempty"`
	// OwnerStopPct is the owner sleeve's drawdown stop; nil while unwritten,
	// when the sleeve has no stop of its own (verdict.owner_stop is absent).
	OwnerStopPct *float64 `json:"owner_stop_pct,omitempty"`
}

// MandateEvidence is every clock behind the verdict.
type MandateEvidence struct {
	// EvaluatedAt is when the daemon ran the simulation.
	EvaluatedAt time.Time `json:"evaluated_at,omitzero"`
	// EquityAsOf and PositionsAsOf are the reads the verdict used.
	EquityAsOf    time.Time `json:"equity_as_of,omitzero"`
	PositionsAsOf time.Time `json:"positions_as_of,omitzero"`
	// Series lists each decision-grade series with its clocks.
	Series []MandateSeriesEvidence `json:"series,omitempty"`
}

// MandateSeriesEvidence is one decision-grade series' provenance.
type MandateSeriesEvidence struct {
	Key string `json:"key"`
	// Closes is how many daily closes the series holds; LastClose the date
	// of the newest.
	Closes    int    `json:"closes"`
	LastClose string `json:"last_close,omitempty"`
	// FetchedAt is the source time of the last successful broker read, and
	// Source the broker's basis label.
	FetchedAt time.Time `json:"fetched_at,omitzero"`
	Source    string    `json:"source,omitempty"`
	Stale     bool      `json:"stale,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
}

// MandateGateView is the additions gate as it reads the verdict now.
type MandateGateView struct {
	// RefusesAdditions is true whenever the verdict is not within (the loss
	// measure over the loss room, a gap over its cap, or unmeasured), is
	// older than the gate accepts, or the owner's allocation is over
	// (allocations, 2026-10-18): it reads the gate for the owner's stock and
	// ETF additions.
	RefusesAdditions bool   `json:"refuses_additions"`
	Reason           string `json:"reason,omitempty"`
	// Options reads the gate for the owner's option additions, which the
	// premium limits gate (owner decision 2026-10-23 18:22 CEST).
	Options *MandateOptionsGateView `json:"options,omitempty"`
	// Override is the active one-shot override of the volatility budget's
	// additions gate (mandate.vol_target_pct), nil without one. While it is
	// active RefusesAdditions is false and Reason names it.
	Override *MandateOverrideView `json:"override,omitempty"`
}

// MandateOptionsGateView is the additions gate as it reads for an option buy
// now: refused while the book's premium at risk is at or over the premium
// budget's act level, or while the premium or NLV cannot be measured; per
// name the gate judges each order against option_name_act_pct (the
// exposure view carries each name's room). Warning carries the volatility
// verdict's words while the verdict is not within: an option buy passes
// them with that warning.
type MandateOptionsGateView struct {
	RefusesAdditions bool   `json:"refuses_additions"`
	Reason           string `json:"reason,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// MandateOverrideView is an active override of the additions gate: who
// granted it is the owner (overrides are human-only), when and why, and
// when it expires.
type MandateOverrideView struct {
	ID        string    `json:"id"`
	Control   string    `json:"control"`
	Reason    string    `json:"reason"`
	GrantedAt time.Time `json:"granted_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// GrantedFrom is "console" when the owner granted it on the device from
	// Desk's console, empty from a terminal.
	GrantedFrom string `json:"granted_from,omitempty"`
}

// FetchMandateRisk reads the daemon's mandate verdict. The daemon evaluates
// it (mandate.Evaluate), so a reader compiled at an older version still
// shows the installed daemon's judgement.
func FetchMandateRisk(ctx context.Context, conn interface {
	Call(context.Context, string, any, any) error
}, refresh bool) (MandateRiskResult, error) {
	var out MandateRiskResult
	if err := conn.Call(ctx, MethodMandateRisk, MandateRiskParams{Refresh: refresh}, &out); err != nil {
		return MandateRiskResult{}, err
	}
	return out, nil
}

// MandateRealisedView is the mandate's capital state and the realised
// figures of the risk base as the daemon's capital store measures them,
// carried on policy.snapshot for the realised checks of `canary policy
// check`, which reads the policy files itself and asks the daemon for what
// only it measures. Known is false, with Reason, while the capital cannot
// be measured; DrawdownPct and Room are the drawdown from the high-water
// mark in percent and the share of the loss budget still open; Vol20Pct
// and Vol60Pct the realised volatility of the risk base over its last 20
// and 60 daily samples, annualised in percent, nil while fewer samples
// exist; Samples the count kept. Absent while the constitution carries no
// [mandate] table.
type MandateRealisedView struct {
	Known        bool     `json:"known"`
	Reason       string   `json:"reason,omitempty"`
	DrawdownPct  float64  `json:"drawdown_pct"`
	Room         float64  `json:"room"`
	Samples      int      `json:"samples"`
	Vol20Pct     *float64 `json:"vol_20_pct,omitempty"`
	Vol60Pct     *float64 `json:"vol_60_pct,omitempty"`
	BaseCurrency string   `json:"base_currency,omitempty"`
}
