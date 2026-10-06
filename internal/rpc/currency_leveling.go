package rpc

import (
	"slices"
	"time"
)

// Currency leveling (internal-docs/design/currency-leveling.md; owner
// decisions 2026-10-05 21:28 and 22:02 CEST, settings settled 2026-10-06
// 05:44–06:15 CEST, the several-currency plan confirmed 2026-10-06 07:11
// CEST). A currency whose trade-date cash is negative is a margin loan IBKR
// never repays on its own. Leveling repays each loan beyond the band back to
// between zero and a small cushion, never more, from the currencies whose
// cash earns least, and only from one that earns less than the loan costs, at
// the interest rates measured from the broker's daily statements. A
// conversion must earn back its worst-case cost within payback_days. One
// loan's conversions form a bundle approved as a whole. Every row is an
// ordinary proposal: a CASH/IDEALPRO LMT DAY order on the pair's exact
// contract, previewed under every gate and sent only on the owner's approval;
// it is never pre-authorisable.
const (
	// TradeProposalBucketCurrencyLeveling converts cash into a currency whose
	// trade-date balance is negative beyond the trigger.
	TradeProposalBucketCurrencyLeveling = "currency_leveling"

	// CurrencyLevelingBalanceTradeDate names the balance leveling reads: the
	// broker ledger's trade-date cash per currency. A conversion moves it at
	// once while settled cash follows two days later, so planning on settled
	// cash would propose the same conversion again until it settles.
	CurrencyLevelingBalanceTradeDate = "trade_date"

	// CurrencyLevelingExchange is the only venue a conversion is sent to.
	CurrencyLevelingExchange = "IDEALPRO"

	// CurrencyLevelingRatesBrokerStatements names where interest rates come
	// from: the broker's daily statements, never the policy file.
	CurrencyLevelingRatesBrokerStatements = "broker_statements"

	// CurrencyLevelingRoleLoan marks a currency borrowed beyond the band;
	// CurrencyLevelingRolePayer one whose cash may repay a loan.
	CurrencyLevelingRoleLoan  = "loan"
	CurrencyLevelingRolePayer = "payer"
)

// Currency leveling blocker codes a conversion's preview or submit refuses
// with; each names what is out of bounds.
const (
	// CurrencyLevelingBlockerBeyondTarget: at the live quote's far side the
	// conversion would bring the borrowed currency above its cushion.
	CurrencyLevelingBlockerBeyondTarget = "conversion_beyond_target"
	// CurrencyLevelingBlockerFundingShort: the conversion would spend more of
	// the funding currency than the free cash it was planned against.
	CurrencyLevelingBlockerFundingShort = "conversion_funding_short"
	// CurrencyLevelingBlockerOrderTerms: the preview's order is not the row's
	// conversion (contract, side, venue, quantity, limit bound).
	CurrencyLevelingBlockerOrderTerms = "conversion_terms_drift"
	// CurrencyLevelingBlockerWorking: a conversion is already working at the
	// broker, or sent and not yet acknowledged.
	CurrencyLevelingBlockerWorking = "conversion_already_working"
	// CurrencyLevelingBlockerBundle: the row is one of several conversions
	// that repay one loan together; it is prepared and sent only with its
	// bundle (trade.proposals.prepare_bundle, submit_bundle).
	CurrencyLevelingBlockerBundle = "conversion_bundle_needs_one_approval"
)

// Currency leveling states, one per currency.
const (
	// CurrencyLevelingStateConvert: the currency is borrowed beyond the
	// trigger and a conversion row follows.
	CurrencyLevelingStateConvert = "convert"
	// CurrencyLevelingStateInBand: the currency is not borrowed, or less than
	// the trigger; nothing to do.
	CurrencyLevelingStateInBand = "in_band"
	// CurrencyLevelingStateDeliberateCarry: the owner keeps this currency
	// negative on purpose (deliberate_carry = true); never converted.
	CurrencyLevelingStateDeliberateCarry = "deliberate_carry"
	// CurrencyLevelingStateHold: borrowed beyond the trigger, but no row
	// follows; the reason says why (a conversion already working, no currency
	// that may pay, nothing that pays back in time, no supported pair, rates
	// unknown, a number not written).
	CurrencyLevelingStateHold = "hold"
	// CurrencyLevelingStatePays: the currency's cash pays part of a loan in
	// this generation's conversions.
	CurrencyLevelingStatePays = "pays"
	// CurrencyLevelingStateCashUnavailable: the ledger carries no current
	// cash for the currency; unavailable is never read as zero.
	CurrencyLevelingStateCashUnavailable = "cash_unavailable"
)

// TradeProposalCurrencyLevelingStatus is the bucket's account of one
// generation: the owner's numbers, the order cap in force, the interest rates
// read, one verdict per ledger currency and the bundles. Field names are
// stable for Desk.
type TradeProposalCurrencyLevelingStatus struct {
	// BaseCurrency is the account's base currency; every *_base figure is in
	// it at the ledger rate.
	BaseCurrency string `json:"base_currency,omitempty"`
	// BalanceSource names the cash read: trade_date.
	BalanceSource string   `json:"balance_source"`
	TriggerBase   *float64 `json:"trigger_base,omitempty"`
	CushionBase   *float64 `json:"cushion_base,omitempty"`
	MaxSlippageBP *float64 `json:"max_slippage_bp,omitempty"`
	PaybackDays   *int     `json:"payback_days,omitempty"`
	// OrderCapBase is the order cap in force ([order_limits]) one loan's
	// conversions together are held to; nil while it cannot be read.
	OrderCapBase *float64 `json:"order_cap_base,omitempty"`
	// RatesSource names where interest rates come from (broker_statements);
	// RatesThrough is the latest statement day read and RatesReason says why
	// no rates are available.
	RatesSource  string `json:"rates_source"`
	RatesThrough string `json:"rates_through,omitempty"`
	RatesReason  string `json:"rates_reason,omitempty"`
	// NeedsYourNumber names the keys an enabled bucket still needs; every
	// currency holds until they are written.
	NeedsYourNumber []string `json:"needs_your_number,omitempty"`
	// Reason is set when the whole bucket holds (ledger unavailable, a fill
	// newer than the ledger).
	Reason     string                                  `json:"reason,omitempty"`
	Currencies []TradeProposalCurrencyLevelingCurrency `json:"currencies"`
	// Bundles are the loans with conversion rows, each approved as a whole.
	Bundles []TradeProposalCurrencyLevelingBundle `json:"bundles,omitempty"`
	// Rows counts the conversion rows this generation emitted.
	Rows int `json:"rows"`
}

// TradeProposalCurrencyLevelingBundle is one loan's conversions. Keys lists
// its rows in send order, cheapest payer first; Revision binds their keys and
// revisions and is what prepare_bundle and submit_bundle name. SavingBase is
// the interest the bundle saves within PaybackDays at the statement rates and
// CostBase its worst-case cost, both in base currency.
type TradeProposalCurrencyLevelingBundle struct {
	ID          string   `json:"id"`
	Currency    string   `json:"currency"`
	Keys        []string `json:"keys"`
	Revision    string   `json:"revision"`
	SavingBase  float64  `json:"saving_base"`
	CostBase    float64  `json:"cost_base"`
	PaybackDays int      `json:"payback_days"`
	// LandsAt is where the borrowed currency's trade-date cash lands, in its
	// own unit, at the planning prices (the conversions' estimates added to
	// the loan); Cushion is the cushion in the same unit, which the plan
	// sizes the repayment to stay within. Both are estimates for the card;
	// the exact bounds at each limit come with the prepared terms.
	LandsAt float64 `json:"lands_at"`
	Cushion float64 `json:"cushion"`
}

// TradeProposalCurrencyLevelingCurrency is one currency's verdict. Cash is
// the trade-date balance in the currency's own unit and CashBase the same at
// the ledger rate; both nil while unavailable. Role is loan or payer. LoanRate
// and CashRate are annual decimals measured from the broker's statements,
// each with the latest day it read; *Bound marks a rate standing in from the
// currency's other side (a loan costs at least what its cash earns).
// SpendableBase is what a payer may spend (cash above the cushion after
// working and armed buys) and PaysBase what this generation's conversions
// take of it.
type TradeProposalCurrencyLevelingCurrency struct {
	Currency        string   `json:"currency"`
	State           string   `json:"state"`
	Reason          string   `json:"reason"`
	Role            string   `json:"role,omitempty"`
	Cash            *float64 `json:"cash,omitempty"`
	CashBase        *float64 `json:"cash_base,omitempty"`
	ExchangeRate    *float64 `json:"exchange_rate,omitempty"`
	LoanRate        *float64 `json:"loan_rate,omitempty"`
	LoanRateThrough string   `json:"loan_rate_through,omitempty"`
	LoanRateBound   bool     `json:"loan_rate_bound,omitempty"`
	CashRate        *float64 `json:"cash_rate,omitempty"`
	CashRateThrough string   `json:"cash_rate_through,omitempty"`
	CashRateBound   bool     `json:"cash_rate_bound,omitempty"`
	SpendableBase   *float64 `json:"spendable_base,omitempty"`
	PaysBase        float64  `json:"pays_base,omitempty"`
	DeliberateCarry bool     `json:"deliberate_carry,omitempty"`
}

// TradeProposalCurrencyLeveling is a conversion row's arithmetic. Currency
// is the borrowed currency the conversion repays and FundingCurrency the one
// it spends; Pair is the IDEALPRO pair (PairSymbol.PairCurrency) whose
// quantity counts PairSymbol, and the row's contract carries the pair's
// contract id. Amounts are in their own currency's unit unless named *_base.
// Received and Spent are estimates at PlanningPrice; the preview checks both
// again at the live quote. BundleID groups the conversions that repay one
// loan; Leg numbers this one (1 = cheapest payer) of Legs.
type TradeProposalCurrencyLeveling struct {
	BundleID        string `json:"bundle_id"`
	Leg             int    `json:"leg"`
	Legs            int    `json:"legs"`
	Currency        string `json:"currency"`
	FundingCurrency string `json:"funding_currency"`
	Pair            string `json:"pair"`
	PairSymbol      string `json:"pair_symbol"`
	PairCurrency    string `json:"pair_currency"`
	BalanceSource   string `json:"balance_source"`
	// Cash is the borrowed currency's trade-date cash (negative); Target the
	// most this conversion alone may bring it to (Cash plus its share of the
	// target; the whole bundle never passes the cushion); Received what the
	// conversion brings in.
	Cash     float64 `json:"cash"`
	Target   float64 `json:"target"`
	Received float64 `json:"received"`
	// FundingCash is the funding currency's trade-date cash and
	// FundingCommitted what working and armed buys already hold of it;
	// Allotment is the most this conversion may spend of it (its share of the
	// cash above the cushion); Spent is the estimate, never more.
	FundingCash      float64 `json:"funding_cash"`
	FundingCommitted float64 `json:"funding_committed,omitempty"`
	Allotment        float64 `json:"allotment"`
	Spent            float64 `json:"spent"`
	// ExchangeRate is base units per unit of Currency from the ledger;
	// PlanningPrice the pair price (PairCurrency per PairSymbol) from the
	// ledger rates that the row was sized around.
	ExchangeRate  float64 `json:"exchange_rate"`
	PlanningPrice float64 `json:"planning_price"`
	// ValueBase is the conversion's size in base currency at the ledger rate.
	ValueBase     float64 `json:"value_base"`
	TriggerBase   float64 `json:"trigger_base"`
	CushionBase   float64 `json:"cushion_base"`
	MaxSlippageBP float64 `json:"max_slippage_bp"`
	// OrderCapBase is the order cap in force the bundle was held to.
	OrderCapBase float64 `json:"order_cap_base"`
	// LoanRate is what the borrowed currency costs and FundingRate what the
	// funding currency's cash earns, annual decimals from the broker's
	// statements, each with the latest day read; *Bound marks a stand-in from
	// the currency's other side.
	LoanRate           float64 `json:"loan_rate"`
	LoanRateThrough    string  `json:"loan_rate_through,omitempty"`
	LoanRateBound      bool    `json:"loan_rate_bound,omitempty"`
	FundingRate        float64 `json:"funding_rate"`
	FundingRateThrough string  `json:"funding_rate_through,omitempty"`
	FundingRateBound   bool    `json:"funding_rate_bound,omitempty"`
	// PaybackDays is the window a conversion must earn back its cost in;
	// SavingBase is the interest this conversion saves within it and CostBase
	// its worst-case cost (commission bound plus the slippage bound).
	PaybackDays int     `json:"payback_days"`
	SavingBase  float64 `json:"saving_base"`
	CostBase    float64 `json:"cost_base"`
	// HeldToCap is true when the order cap in force held the bundle below
	// the debit; the next cycle converts the rest.
	HeldToCap bool `json:"held_to_cap,omitempty"`
	// FundingShort is true when the currencies allowed to pay hold less than
	// the debit; the bundle repays what they can.
	FundingShort bool `json:"funding_short,omitempty"`
	// FundingAfter is the funding currency's trade-date cash after this
	// conversion's estimated spend and what working and armed buys hold.
	FundingAfter float64 `json:"funding_after"`
}

// OrderFXTerms are the daemon-internal terms a currency_leveling row's
// CASH/IDEALPRO preview carries (OrderPreviewParams.FX): the borrowed
// currency, its trade-date cash and target, the funding currency and its
// free cash, and the slippage bound. A CASH preview without them is refused,
// so no RPC caller can preview a conversion.
type OrderFXTerms struct {
	Currency        string  `json:"currency"`
	FundingCurrency string  `json:"funding_currency"`
	Cash            float64 `json:"cash"`
	Target          float64 `json:"target"`
	FundingCash     float64 `json:"funding_cash"`
	MaxSlippageBP   float64 `json:"max_slippage_bp"`
	// Bid and Ask are the live quote the preview bounded the limit from.
	Bid *float64 `json:"bid,omitempty"`
	Ask *float64 `json:"ask,omitempty"`
}

// CloneCurrencyLevelingStatus deep-copies the status; nil stays nil.
func CloneCurrencyLevelingStatus(in *TradeProposalCurrencyLevelingStatus) *TradeProposalCurrencyLevelingStatus {
	if in == nil {
		return nil
	}
	out := *in
	for _, f := range []**float64{&out.TriggerBase, &out.CushionBase, &out.MaxSlippageBP, &out.OrderCapBase} {
		*f = cloneCashSweepFloat(*f)
	}
	if in.PaybackDays != nil {
		out.PaybackDays = new(*in.PaybackDays)
	}
	out.NeedsYourNumber = slices.Clone(in.NeedsYourNumber)
	out.Currencies = slices.Clone(in.Currencies)
	for i := range out.Currencies {
		c := &out.Currencies[i]
		for _, f := range []**float64{&c.Cash, &c.CashBase, &c.ExchangeRate, &c.LoanRate, &c.CashRate, &c.SpendableBase} {
			*f = cloneCashSweepFloat(*f)
		}
	}
	out.Bundles = slices.Clone(in.Bundles)
	for i := range out.Bundles {
		out.Bundles[i].Keys = slices.Clone(in.Bundles[i].Keys)
	}
	return &out
}

// CloneProposalCurrencyLeveling copies a row's block; nil stays nil.
func CloneProposalCurrencyLeveling(in *TradeProposalCurrencyLeveling) *TradeProposalCurrencyLeveling {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

// CloneOrderFXTerms copies preview FX terms; nil stays nil.
func CloneOrderFXTerms(in *OrderFXTerms) *OrderFXTerms {
	if in == nil {
		return nil
	}
	out := *in
	out.Bid, out.Ask = cloneCashSweepFloat(in.Bid), cloneCashSweepFloat(in.Ask)
	return &out
}

// Bundle methods: one approval for the several conversions that repay one
// loan (owner answer 2026-10-06 06:38 CEST, "Several sources ... but one
// approval"). prepare_bundle previews every conversion and retains them
// together; submit_bundle checks every retained conversion again, then sends
// them in order and stops at the first refusal. Like the prepared-proposal
// methods they are private CLI/RPC handoffs, not part of the MCP API.
const (
	MethodTradeProposalsPrepareBundle = "trade.proposals.prepare_bundle"
	MethodTradeProposalsSubmitBundle  = "trade.proposals.submit_bundle"
	// MethodTradeProposalsPreparedBundleStatus reads what became of a
	// prepared bundle from Canary's own records; it never sends.
	MethodTradeProposalsPreparedBundleStatus = "trade.proposals.prepared_bundle_status"
)

// Bundle outcomes. A conversion is sent, refused (Canary refused it, so it
// did not reach the broker), not_sent (never attempted: the bundle stopped
// before it, or never started) or unknown (it may have reached the broker;
// its outcome is not confirmed). A bundle is sent (every conversion),
// partly_sent (some, then a refusal), not_sent (none), unknown (a
// conversion's outcome is not confirmed), and, before any send, prepared or
// expired.
const (
	BundleOutcomeSent       = "sent"
	BundleOutcomeRefused    = "refused"
	BundleOutcomeNotSent    = "not_sent"
	BundleOutcomeUnknown    = "unknown"
	BundleOutcomePartlySent = "partly_sent"
	BundleOutcomePrepared   = "prepared"
	BundleOutcomeExpired    = "expired"
)

// LevelingBundleTermsKind and LevelingBundleTermsVersion name the exact
// terms a prepared bundle carries and the owner confirms.
const (
	LevelingBundleTermsKind    = "canary.leveling_bundle"
	LevelingBundleTermsVersion = 1
)

// LevelingBundleTerms are a prepared bundle's exact terms, what the owner
// confirms: the broker scope, the bundle and its revision, the loan, the
// figures at the price limit (pays, receives, what a payer keeps at least,
// where the loan lands at least) and every conversion's identity in send
// order. They are compact JSON with the fields in this order (alphabetical);
// TermsDigest is "sha256:" and the hex SHA-256 of those exact bytes, which a
// reader must keep verbatim and never re-encode. A reader refuses a field it
// does not know rather than show the owner less than is signed.
type LevelingBundleTerms struct {
	AccountID       string                   `json:"account_id"`
	AccountMode     string                   `json:"account_mode"`
	BaseCurrency    string                   `json:"base_currency"`
	BundleID        string                   `json:"bundle_id"`
	Cash            float64                  `json:"cash"`
	CashBase        float64                  `json:"cash_base"`
	ClientID        int                      `json:"client_id"`
	CostBase        float64                  `json:"cost_base"`
	Currency        string                   `json:"currency"`
	Cushion         float64                  `json:"cushion"`
	CushionBase     float64                  `json:"cushion_base"`
	Endpoint        string                   `json:"endpoint"`
	ExpiresAt       time.Time                `json:"expires_at"`
	FundingShort    bool                     `json:"funding_short"`
	HeldToCap       bool                     `json:"held_to_cap"`
	Kind            string                   `json:"kind"`
	LandsAtLeast    float64                  `json:"lands_at_least"`
	Legs            []LevelingBundleTermsLeg `json:"legs"`
	LoanRate        float64                  `json:"loan_rate"`
	LoanRateBound   bool                     `json:"loan_rate_bound"`
	LoanRateThrough string                   `json:"loan_rate_through"`
	OrderCapBase    float64                  `json:"order_cap_base"`
	PaybackDays     int                      `json:"payback_days"`
	PreparationID   string                   `json:"preparation_id"`
	Revision        string                   `json:"revision"`
	SavingBase      float64                  `json:"saving_base"`
	TriggerBase     float64                  `json:"trigger_base"`
	Version         int                      `json:"version"`
}

// LevelingBundleTermsLeg is one conversion of the terms: its row, its
// retained preparation, its order at the limit and the live quote the limit
// was bounded from, what it pays and receives at that limit, what its payer
// keeps at least, and its economics.
type LevelingBundleTermsLeg struct {
	Action             string               `json:"action"`
	Allotment          float64              `json:"allotment"`
	Ask                float64              `json:"ask"`
	Bid                float64              `json:"bid"`
	ConID              int                  `json:"con_id"`
	CostBase           float64              `json:"cost_base"`
	Currency           string               `json:"currency"`
	DraftFingerprint   string               `json:"draft_fingerprint"`
	Exchange           string               `json:"exchange"`
	FundingCurrency    string               `json:"funding_currency"`
	FundingRate        float64              `json:"funding_rate"`
	FundingRateBound   bool                 `json:"funding_rate_bound"`
	FundingRateThrough string               `json:"funding_rate_through"`
	KeepsAtLeast       float64              `json:"keeps_at_least"`
	Key                string               `json:"key"`
	Leg                int                  `json:"leg"`
	LimitPrice         float64              `json:"limit_price"`
	MaxSlippageBP      float64              `json:"max_slippage_bp"`
	OrderRef           string               `json:"order_ref"`
	OrderType          string               `json:"order_type"`
	Pair               string               `json:"pair"`
	Pays               LevelingBundleAmount `json:"pays"`
	PreparationID      string               `json:"preparation_id"`
	PreviewTokenID     string               `json:"preview_token_id"`
	Quantity           int                  `json:"quantity"`
	QuoteAt            time.Time            `json:"quote_at"`
	Receives           LevelingBundleAmount `json:"receives"`
	Revision           string               `json:"revision"`
	SavingBase         float64              `json:"saving_base"`
	Target             float64              `json:"target"`
	TIF                string               `json:"tif"`
}

// LevelingBundleAmount is an amount at the price limit: exact, at_most (a
// buy spends no more) or at_least (a sell receives no less).
type LevelingBundleAmount struct {
	Amount   float64 `json:"amount"`
	Bound    string  `json:"bound"`
	Currency string  `json:"currency"`
}

// Amount bounds of LevelingBundleAmount.
const (
	LevelingBundleBoundExact   = "exact"
	LevelingBundleBoundAtMost  = "at_most"
	LevelingBundleBoundAtLeast = "at_least"
)

// TradeProposalBundleConfirmation is the owner's confirmation as Desk sends
// it with a bundle. Canary keeps it with the bundle's one submission, for
// audit only, and cannot verify it; the conversions' decisions name the
// bundle.
type TradeProposalBundleConfirmation struct {
	DeskActionID string `json:"desk_action_id"`
	Credential   string `json:"credential"`
	Envelope     string `json:"envelope"`
}

// TradeProposalPrepareBundleParams names one bundle as served.
type TradeProposalPrepareBundleParams struct {
	BundleID  string `json:"bundle_id"`
	Revision  string `json:"revision"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

// TradeProposalPrepareBundleResult carries each conversion's preview. The
// BundleRef authorises submit_bundle and, like PreparedRef, must never be
// sent to a browser, logged or passed in argv; it is set only when every
// conversion was prepared.
type TradeProposalPrepareBundleResult struct {
	Accepted  bool                         `json:"accepted"`
	BundleID  string                       `json:"bundle_id"`
	Revision  string                       `json:"revision"`
	BundleRef string                       `json:"bundle_ref,omitempty"`
	ExpiresAt time.Time                    `json:"expires_at,omitzero"`
	Legs      []TradeProposalPreviewResult `json:"legs"`
	Blockers  []TradingBlocker             `json:"blockers,omitempty"`
	AsOf      time.Time                    `json:"as_of"`
	// PreparationID names the retained bundle; Terms are its exact terms
	// (LevelingBundleTerms, compact JSON) and TermsDigest their digest, set
	// only when every conversion was prepared. Each leg's preparation (its
	// id and draft fingerprint) is in the terms.
	PreparationID string `json:"preparation_id,omitempty"`
	Terms         string `json:"terms,omitempty"`
	TermsDigest   string `json:"terms_digest,omitempty"`
}

// TradeProposalSubmitBundleParams confirms a prepared bundle.
// TermsDigest is the digest of the terms the owner confirmed; a send whose
// digest differs from the retained terms' is refused before any check.
type TradeProposalSubmitBundleParams struct {
	BundleRef    string                           `json:"bundle_ref"`
	BundleID     string                           `json:"bundle_id"`
	Revision     string                           `json:"revision"`
	TermsDigest  string                           `json:"terms_digest"`
	Confirmation *TradeProposalBundleConfirmation `json:"confirmation,omitempty"`
	FastPath     bool                             `json:"fast_path,omitempty"`
	TimeoutMs    int                              `json:"timeout_ms,omitempty"`
	Origin       string                           `json:"origin,omitempty"`
}

// TradeProposalSubmitBundleResult reports every conversion in send order,
// each with its Outcome, and the bundle's own Outcome and how many were
// Sent. Accepted is true only when every conversion was sent; a conversion
// after the first refusal, or after one whose outcome is unknown, is not
// sent.
type TradeProposalSubmitBundleResult struct {
	Accepted bool                        `json:"accepted"`
	BundleID string                      `json:"bundle_id"`
	Outcome  string                      `json:"outcome"`
	Sent     int                         `json:"sent"`
	Legs     []TradeProposalSubmitResult `json:"legs"`
	Blockers []TradingBlocker            `json:"blockers,omitempty"`
	AsOf     time.Time                   `json:"as_of"`
}

// TradeProposalPreparedBundleStatusParams names a prepared bundle by its
// private reference.
type TradeProposalPreparedBundleStatusParams struct {
	BundleRef string `json:"bundle_ref"`
}

// TradeProposalPreparedBundleStatusResult is what became of a prepared
// bundle, from Canary's own records: its Outcome (BundleOutcome*), how many
// conversions were Sent, when its one submission started, and each
// conversion in send order with its outcome, its preparation (state,
// consumed) and its local order receipt. Message says why an outcome is not
// settled yet (the bundle is being sent now). It never prepares or sends.
type TradeProposalPreparedBundleStatusResult struct {
	BundleID      string                   `json:"bundle_id,omitempty"`
	Revision      string                   `json:"revision,omitempty"`
	PreparationID string                   `json:"preparation_id,omitempty"`
	TermsDigest   string                   `json:"terms_digest,omitempty"`
	ExpiresAt     time.Time                `json:"expires_at,omitzero"`
	Outcome       string                   `json:"outcome,omitempty"`
	Sent          int                      `json:"sent"`
	SubmittedAt   time.Time                `json:"submitted_at,omitzero"`
	Message       string                   `json:"message,omitempty"`
	Legs          []TradeProposalBundleLeg `json:"legs"`
	Blockers      []TradingBlocker         `json:"blockers,omitempty"`
	AsOf          time.Time                `json:"as_of"`
}

// TradeProposalBundleLeg is one conversion's status in a bundle.
type TradeProposalBundleLeg struct {
	Leg         int                       `json:"leg"`
	Key         string                    `json:"key"`
	Outcome     string                    `json:"outcome"`
	OrderRef    string                    `json:"order_ref,omitempty"`
	Preparation *TradeProposalPreparation `json:"preparation,omitempty"`
	Order       *OrderStatusResult        `json:"order,omitempty"`
}
