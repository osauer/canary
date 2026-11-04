package rpc

import (
	"errors"
	"math"
	"regexp"
	"time"
)

// Execution cost reporting: the quote retained at every order event and fill,
// and the half-spread each fill paid against the mid of that quote.
const (
	MethodReportingCosts        = "reporting.costs"
	ReportingCostsSchemaVersion = "canary-reporting-costs-v2"

	// FillQuoteSourceCanary marks a fill of an order Canary placed.
	FillQuoteSourceCanary = "canary"
	// FillQuoteSourceBroker marks an execution observed from the account that
	// Canary did not place: another API client or an order entered in TWS.
	FillQuoteSourceBroker = "broker"

	// FillQuoteSeenCallback: the fill arrived on this client's own execDetails
	// callback. FillQuoteSeenPoll: the periodic reqExecutions poll found it.
	// FillQuoteSeenBackfill: the first poll of a broker session found it by
	// asking for earlier days; the fill is retained without a quote.
	FillQuoteSeenCallback = "callback"
	FillQuoteSeenPoll     = "poll"
	FillQuoteSeenBackfill = "backfill"

	// FillQuoteQuoted means both sides were read, so a mid exists.
	FillQuoteQuoted = "quoted"
	// FillQuoteUnavailable means no usable two-sided quote; Reason says why.
	FillQuoteUnavailable = "unavailable"

	CostFillMeasured = "measured"
	CostFillUnknown  = "unknown"

	// ExecutionVisibilityAllClients: an execution from another API client or
	// TWS has been seen, so the account's fills reach this daemon.
	// ExecutionVisibilityUnproven: only this client's own executions have been
	// seen; TWS may be withholding the rest (non-master API client).
	ExecutionVisibilityAllClients = "all_clients"
	ExecutionVisibilityUnproven   = "own_orders_unproven"
	// ExecutionVisibilitySetting names the TWS setting that lets this client
	// see fills entered in TWS or by other API clients. The owner decides it;
	// Canary never changes TWS configuration.
	ExecutionVisibilitySetting = "TWS Global Configuration → API → Settings → Master API client ID"

	CostClassStock  = "stock"
	CostClassOption = "option"
	CostClassBond   = "bond"
	CostClassFX     = "fx"
	CostClassFuture = "future"
	CostClassOther  = "other"
)

// FillQuote is the exact-contract quote retained beside an order event or a
// fill. QuoteAt is the quote's own time (the receipt of its price tick), never
// the request time; GapMS is QuoteAt minus the event time, positive when the
// quote was read after the event. Nil prices mean the broker gave none.
type FillQuote struct {
	Status        string    `json:"status"`
	Reason        string    `json:"reason,omitempty"`
	Bid           *float64  `json:"bid,omitempty"`
	Ask           *float64  `json:"ask,omitempty"`
	Mid           *float64  `json:"mid,omitempty"`
	Last          *float64  `json:"last,omitempty"`
	BidSize       *int      `json:"bid_size,omitempty"`
	AskSize       *int      `json:"ask_size,omitempty"`
	DataType      string    `json:"data_type,omitempty"`
	QuoteAt       time.Time `json:"quote_at,omitzero"`
	ReadStartedAt time.Time `json:"read_started_at,omitzero"`
	ReadAt        time.Time `json:"read_at,omitzero"`
	GapMS         *int64    `json:"gap_ms,omitempty"`
}

// FillQuoteRecord is one retained execution with the quote read for it and
// the FX rate (base per contract currency) at that moment. It names no
// account; Mode distinguishes paper from live.
type FillQuoteRecord struct {
	Version        int            `json:"version"`
	ExecID         string         `json:"exec_id"`
	Source         string         `json:"source"`
	Seen           string         `json:"seen"`
	Mode           string         `json:"mode,omitempty"`
	OrderRef       string         `json:"order_ref,omitempty"`
	PermID         int            `json:"perm_id,omitempty"`
	ClientID       *int           `json:"client_id,omitempty"`
	Contract       ContractParams `json:"contract"`
	Venue          string         `json:"venue,omitempty"`
	Side           string         `json:"side"`
	Quantity       float64        `json:"quantity"`
	Price          float64        `json:"price"`
	ExecTime       time.Time      `json:"exec_time,omitzero"`
	ExecTimeSource string         `json:"exec_time_source,omitempty"`
	ObservedAt     time.Time      `json:"observed_at"`
	Quote          FillQuote      `json:"quote"`
	BaseCurrency   string         `json:"base_currency,omitempty"`
	FXRate         *float64       `json:"fx_rate,omitempty"`
	FXSource       string         `json:"fx_source,omitempty"`
	FXReason       string         `json:"fx_reason,omitempty"`
}

// ReportingCostsParams selects one calendar month (YYYY-MM, UTC); empty
// means every retained fill.
type ReportingCostsParams struct {
	Month string `json:"month,omitempty"`
}

// ReportingCostsResult is the reporting.costs envelope. Fills are ascending
// by execution time; totals are sums of the measured fills' half-spread in
// the base currency at each fill's own FX rate. Nothing here names the
// account.
type ReportingCostsResult struct {
	SchemaVersion string               `json:"schema_version"`
	AsOf          time.Time            `json:"as_of"`
	Month         string               `json:"month,omitempty"`
	Mode          string               `json:"mode,omitempty"`
	BaseCurrency  string               `json:"base_currency,omitempty"`
	Coverage      CostCoverage         `json:"coverage"`
	Observation   ExecutionObservation `json:"observation"`
	ByMonth       []CostTotal          `json:"by_month"`
	BySource      []CostTotal          `json:"by_source"`
	ByClass       []CostTotal          `json:"by_class"`
	Fills         []CostFill           `json:"fills"`
}

// CostCoverage counts the fills in the report and how many carry a usable
// two-sided quote. Ratio is Quoted over Fills; nil without fills.
type CostCoverage struct {
	Fills   int      `json:"fills"`
	Quoted  int      `json:"quoted"`
	Unknown int      `json:"unknown"`
	Ratio   *float64 `json:"ratio,omitempty"`
}

// CostCurrencyAmount keeps historically converted money in its own unit.
type CostCurrencyAmount struct {
	Currency       string  `json:"currency"`
	HalfSpreadBase float64 `json:"half_spread_base"`
}

// CostTotal is one breakdown with measured amounts grouped by conversion currency.
// The legacy scalar is present only when the entire report has one base currency.
// Unconverted counts measured fills without a known FX rate or base currency.
type CostTotal struct {
	ByCurrency     []CostCurrencyAmount `json:"by_currency,omitempty"`
	Key            string               `json:"key"`
	Fills          int                  `json:"fills"`
	Measured       int                  `json:"measured"`
	Unknown        int                  `json:"unknown"`
	Unconverted    int                  `json:"unconverted,omitempty"`
	HalfSpreadBase *float64             `json:"half_spread_base,omitempty"`
}

// CostFill is one fill with its quote and the half-spread it paid:
// (fill − mid) × side sign × quantity × multiplier, in the contract currency
// and in the base currency at the fill's FX rate. Positive is cost paid,
// negative is price improvement. Status unknown carries the reason.
type CostFill struct {
	BaseCurrency   string    `json:"base_currency,omitempty"`
	ExecID         string    `json:"exec_id"`
	Source         string    `json:"source"`
	At             time.Time `json:"at,omitzero"`
	Symbol         string    `json:"symbol"`
	LocalSymbol    string    `json:"local_symbol,omitempty"`
	SecType        string    `json:"sec_type"`
	Class          string    `json:"class"`
	Side           string    `json:"side"`
	Quantity       float64   `json:"quantity"`
	Multiplier     float64   `json:"multiplier,omitempty"`
	Price          float64   `json:"price"`
	Currency       string    `json:"currency,omitempty"`
	Quote          FillQuote `json:"quote"`
	HalfSpread     *float64  `json:"half_spread,omitempty"`
	HalfSpreadBase *float64  `json:"half_spread_base,omitempty"`
	FXRate         *float64  `json:"fx_rate,omitempty"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason,omitempty"`
}

// ExecutionObservation says how the daemon learns of fills it did not place:
// the reqExecutions poll's last outcome and whether executions from other
// clients have ever been seen. Action names the TWS setting that widens
// visibility; the owner decides it.
type ExecutionObservation struct {
	Polling            bool      `json:"polling"`
	IntervalSeconds    int       `json:"interval_seconds"`
	LastPollAt         time.Time `json:"last_poll_at,omitzero"`
	LastPollComplete   bool      `json:"last_poll_complete"`
	LastPollExecutions int       `json:"last_poll_executions"`
	LastPollError      string    `json:"last_poll_error,omitempty"`
	Visibility         string    `json:"visibility"`
	// ProvenAt is the poll that first saw an execution from another client in
	// this account, broker mode and client scope; zero while unproven.
	ProvenAt time.Time `json:"proven_at,omitzero"`
	Action   string    `json:"action,omitempty"`
}

// ExecutionVisibilityHealth is the `canary status` row for which of the
// account's executions reach this daemon. Visibility is proven, never
// assumed: ExecutionVisibilityAllClients once an execution from another API
// client or TWS has been seen in this account, broker mode and client scope
// (ProvenAt is the poll that saw it), otherwise ExecutionVisibilityUnproven
// with Setting, the TWS setting that widens it, and Action, what to do.
type ExecutionVisibilityHealth struct {
	Visibility string    `json:"visibility"`
	ProvenAt   time.Time `json:"proven_at,omitzero"`
	ClientID   int       `json:"client_id"`
	Setting    string    `json:"setting"`
	Action     string    `json:"action,omitempty"`
}

var costsMonthPattern = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// ValidCostsMonth reports whether month is a YYYY-MM calendar month.
func ValidCostsMonth(month string) bool {
	return costsMonthPattern.MatchString(month)
}

// ValidateReportingCostsResult rejects an envelope a consumer could misread:
// unknown statuses, counts that disagree, a measured fill without a mid or a
// half-spread, or non-finite money.
func ValidateReportingCostsResult(result ReportingCostsResult) error {
	if result.SchemaVersion != ReportingCostsSchemaVersion {
		return errors.New("invalid reporting costs schema contract")
	}
	if result.Month != "" && !ValidCostsMonth(result.Month) {
		return errors.New("invalid reporting costs month")
	}
	if result.Fills == nil || result.ByMonth == nil || result.BySource == nil || result.ByClass == nil {
		return errors.New("reporting costs lists must be present, possibly empty")
	}
	quoted, unknown := 0, 0
	var previous time.Time
	for _, fill := range result.Fills {
		if fill.ExecID == "" {
			return errors.New("reporting costs fill lacks an execution id")
		}
		if fill.Source != FillQuoteSourceCanary && fill.Source != FillQuoteSourceBroker {
			return errors.New("invalid reporting costs fill source")
		}
		if fill.At.Before(previous) {
			return errors.New("reporting costs fills must be ascending")
		}
		previous = fill.At
		for _, value := range []*float64{fill.HalfSpread, fill.HalfSpreadBase, fill.FXRate, fill.Quote.Bid, fill.Quote.Ask, fill.Quote.Mid, fill.Quote.Last} {
			if value != nil && (math.IsNaN(*value) || math.IsInf(*value, 0)) {
				return errors.New("reporting costs money must be finite")
			}
		}
		switch fill.Status {
		case CostFillMeasured:
			if fill.Quote.DataType != MarketDataLive {
				return errors.New("measured costs require an explicitly live quote")
			}
			if fill.HalfSpreadBase != nil && fill.BaseCurrency == "" {
				return errors.New("converted cost lacks its base currency")
			}
			if fill.Quote.Mid == nil || fill.HalfSpread == nil || fill.Quote.Status != FillQuoteQuoted {
				return errors.New("measured reporting costs fill lacks its mid or half-spread")
			}
			if (fill.HalfSpreadBase == nil) != (fill.FXRate == nil) {
				return errors.New("reporting costs base half-spread and FX rate disagree")
			}
		case CostFillUnknown:
			if fill.Reason == "" || fill.HalfSpread != nil || fill.HalfSpreadBase != nil {
				return errors.New("unknown reporting costs fill must carry a reason and no half-spread")
			}
		default:
			return errors.New("invalid reporting costs fill status")
		}
		switch fill.Quote.Status {
		case FillQuoteQuoted:
			if fill.Quote.Mid == nil {
				return errors.New("quoted reporting costs fill lacks a mid")
			}
			quoted++
		case FillQuoteUnavailable:
			if fill.Quote.Mid != nil {
				return errors.New("unavailable reporting costs quote carries a mid")
			}
			unknown++
		default:
			return errors.New("invalid reporting costs quote status")
		}
	}
	if result.Coverage.Fills != len(result.Fills) || result.Coverage.Quoted != quoted || result.Coverage.Unknown != unknown {
		return errors.New("reporting costs coverage disagrees with its fills")
	}
	if (result.Coverage.Ratio == nil) != (result.Coverage.Fills == 0) {
		return errors.New("reporting costs coverage ratio must exist exactly when there are fills")
	}
	for _, totals := range [][]CostTotal{result.ByMonth, result.BySource, result.ByClass} {
		for _, total := range totals {
			if total.HalfSpreadBase != nil && result.BaseCurrency == "" {
				return errors.New("scalar cost total lacks an unambiguous base currency")
			}
			seen := map[string]bool{}
			for _, amount := range total.ByCurrency {
				if amount.Currency == "" || seen[amount.Currency] || math.IsNaN(amount.HalfSpreadBase) || math.IsInf(amount.HalfSpreadBase, 0) {
					return errors.New("invalid currency cost total")
				}
				seen[amount.Currency] = true
			}
			if total.HalfSpreadBase != nil && len(total.ByCurrency) > 0 && (len(total.ByCurrency) != 1 || total.ByCurrency[0].Currency != result.BaseCurrency || total.ByCurrency[0].HalfSpreadBase != *total.HalfSpreadBase) {
				return errors.New("scalar cost total disagrees with currency totals")
			}
			if total.Key == "" || total.Fills != total.Measured+total.Unknown || total.Unconverted > total.Measured {
				return errors.New("invalid reporting costs total")
			}
			if total.HalfSpreadBase != nil && (math.IsNaN(*total.HalfSpreadBase) || math.IsInf(*total.HalfSpreadBase, 0)) {
				return errors.New("reporting costs total must be finite")
			}
		}
	}
	switch result.Observation.Visibility {
	case ExecutionVisibilityAllClients, ExecutionVisibilityUnproven:
	default:
		return errors.New("invalid reporting costs execution visibility")
	}
	return nil
}
