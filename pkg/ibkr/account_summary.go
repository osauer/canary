package ibkr

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// ErrIBKRUnavailable is returned by request methods when the connector cannot
// reach IBKR (gateway disconnected, connector not started). Callers serving
// trading-critical reads (account values, fresh quotes) should refuse rather
// than fall back to stale data.
var ErrIBKRUnavailable = errors.New("IBKR connection unavailable")

// ErrAccountSummaryScopeConflict means a one-shot account-summary request
// observed a row outside its expected single-account scope. The only aggregate
// rows admitted are the modeled per-currency fields emitted by $LEDGER:ALL.
// Every other blank, aggregate, or foreign row rejects the whole snapshot.
var ErrAccountSummaryScopeConflict = errors.New("account summary account scope conflict")

// RawAccountSummary is a point-in-time view of the account values returned by
// IBKR. Currency-denominated top-level fields use the account's base-currency
// row when IBKR supplied one. If a base row is absent, the parser selects a
// currency-specific row deterministically. Currency records the first such
// fallback; Raw preserves the currency suffix for every field.
//
// Fields are pointers when their absence is meaningful (IBKR may omit tags
// the user does not have permission for, e.g., margin fields on a cash
// account, or LookAhead* on cash). Callers must check for nil before
// dereferencing.
type RawAccountSummary struct {
	AccountID            string
	AccountType          string
	NetLiquidation       *float64
	BuyingPower          *float64
	AvailableFunds       *float64
	ExcessLiquidity      *float64
	TotalCashValue       *float64
	MaintenanceMargin    *float64
	InitMarginReq        *float64
	GrossPositionValue   *float64
	UnrealizedPnL        *float64
	RealizedPnL          *float64
	Cushion              *float64
	LookAheadInitMargin  *float64
	LookAheadMaintMargin *float64
	LookAheadAvailable   *float64
	LookAheadExcess      *float64
	Currency             string
	// BaseCurrency and its provenance are intentionally distinct from
	// Currency. Currency is the legacy deterministic fallback used for numeric
	// rows; it must never be treated as proof of the account's base unit.
	BaseCurrency           string
	BaseCurrencyProvenance AccountBaseCurrencyProvenance
	// CurrencyLedger holds the per-currency rollup the gateway emitted
	// in response to the $LEDGER:ALL tag — one entry per non-BASE
	// currency present in the portfolio. Empty for same-currency
	// accounts. The "BASE" pseudo-currency entry IBKR emits is dropped
	// here because it duplicates the top-level totals already reported.
	CurrencyLedger map[string]CurrencyLedger
	AsOf           time.Time
	// Raw is the unparsed map from IBKR keyed exactly as the gateway returned it
	// (`<tag>` for BASE currency, `<tag>_<currency>` otherwise). Provided for
	// diagnostic and forward-compatibility purposes.
	Raw map[string]string
}

// AccountBaseCurrencyProvenance identifies the broker evidence used to prove
// an account summary's base currency.
type AccountBaseCurrencyProvenance string

const (
	// AccountBaseCurrencyUnknown means no eligible broker field proved the base currency.
	AccountBaseCurrencyUnknown AccountBaseCurrencyProvenance = "unknown"
	// AccountBaseCurrencyExplicitTag means the dedicated Currency field supplied the value.
	AccountBaseCurrencyExplicitTag AccountBaseCurrencyProvenance = "explicit_currency_tag"
	// AccountBaseCurrencyValueSuffix means an allowlisted aggregate value suffix supplied it.
	AccountBaseCurrencyValueSuffix AccountBaseCurrencyProvenance = "account_value_suffix"
	// AccountBaseCurrencyUnitExchangeRate remains for wire/read-model
	// compatibility only. A unit exchange rate is not proof of the account's
	// base currency and accountBaseCurrencyEvidence never emits it.
	AccountBaseCurrencyUnitExchangeRate AccountBaseCurrencyProvenance = "unit_exchange_rate"
)

// accountBaseCurrencyValueTags is the closed allowlist of ordinary aggregate
// account-summary values whose three-letter suffix may prove the account base
// currency. Ledger-family keys are deliberately absent: $LEDGER:ALL rows
// describe held currencies, not the account's base unit. UnrealizedPnL and
// RealizedPnL are ordinary summary tags too, but $LEDGER:ALL reuses those exact
// names for every held currency. The flattened raw map cannot distinguish the
// two origins, so neither tag is eligible base-currency evidence.
var accountBaseCurrencyValueTags = []string{
	"NetLiquidation",
	"BuyingPower",
	"AvailableFunds",
	"ExcessLiquidity",
	"TotalCashValue",
	"MaintMarginReq",
	"MaintenanceMarginReq",
	"InitMarginReq",
	"GrossPositionValue",
	"Cushion",
	"LookAheadInitMarginReq",
	"LookAheadMaintMarginReq",
	"LookAheadAvailableFunds",
	"LookAheadExcessLiquidity",
}

// CurrencyLedger is one non-base-currency IBKR $LEDGER row. Monetary values
// are denominated in that row's currency, not converted to the account base
// currency. ExchangeRate is base-currency units per ledger-currency unit, so
// multiplying a monetary field by ExchangeRate yields its base-currency
// contribution. A zero field may be either an observed zero or an omitted
// value; the wire format does not preserve that distinction here.
type CurrencyLedger struct {
	NetLiquidationByCurrency float64
	CashBalance              float64
	StockMarketValue         float64
	OptionMarketValue        float64
	UnrealizedPnL            float64
	RealizedPnL              float64
	ExchangeRate             float64
}

// currencyLedgerField reports whether tag is one of the closed set of
// $LEDGER:ALL fields represented by CurrencyLedger. Keep request-scope
// admission and parsing on this same allowlist: an aggregate row that cannot
// be projected into the typed ledger must never enter a one-account snapshot.
func currencyLedgerField(tag string) bool {
	switch tag {
	case "NetLiquidationByCurrency",
		"CashBalance",
		"StockMarketValue",
		"OptionMarketValue",
		"UnrealizedPnL",
		"RealizedPnL",
		"ExchangeRate":
		return true
	default:
		return false
	}
}

const (
	defaultAccountSummaryTimeout = 5 * time.Second
	// $LEDGER:ALL asks IBKR to emit per-currency rows (one block per
	// currency present in the portfolio) carrying NetLiquidation, MarketValue,
	// CashBalance, UnrealizedPnL, RealizedPnL, ExchangeRate, etc., each tagged
	// `<Field>_<CCY>`. This is the canonical mechanism for multi-currency
	// exposure surfacing — without it we'd have no FX rate at all.
	//
	// MaintMarginReq is the canonical tag. The parser also accepts the longer
	// MaintenanceMarginReq alias emitted by some gateway/account combinations.
	accountSummaryTags = "NetLiquidation,BuyingPower,AvailableFunds,ExcessLiquidity,TotalCashValue,MaintMarginReq,InitMarginReq,GrossPositionValue,UnrealizedPnL,RealizedPnL,Cushion,LookAheadInitMarginReq,LookAheadMaintMarginReq,LookAheadAvailableFunds,LookAheadExcessLiquidity,AccountType,$LEDGER:ALL"
)

// RequestAccountSummary issues a synchronous reqAccountSummary request and
// returns a caller-owned parsed snapshot. ctx must be non-nil. The call blocks
// until the gateway emits accountSummaryEnd, ctx is cancelled, or timeout
// elapses.
//
// Behavior:
//   - Returns ErrIBKRUnavailable immediately if the connector is not
//     connected; no network traffic is generated.
//   - On timeout the request is cancelled (cancelAccountSummary sent) so the
//     gateway does not continue streaming updates against the consumed reqID.
//   - timeout <= 0 falls back to defaultAccountSummaryTimeout (5s).
//
// The method is safe to call concurrently; each invocation uses a fresh
// request ID and normally reads only that request's rows. If the gateway emits
// an end marker without rows, it falls back to a defensive copy of the
// streaming account-updates cache.
func (c *Connector) RequestAccountSummary(ctx context.Context, timeout time.Duration) (*RawAccountSummary, error) {
	summary, _, err := c.RequestAccountSummaryWithProvenance(ctx, timeout)
	return summary, err
}

// AccountSummaryProvenance identifies whether a returned account snapshot was
// completed by the one-shot request or reparsed from the unstamped streaming
// cache. Callers that require current evidence must accept only Request.
type AccountSummaryProvenance string

const (
	// AccountSummaryProvenanceRequest means the one-shot request supplied a
	// complete row set and matching end marker.
	AccountSummaryProvenanceRequest AccountSummaryProvenance = "request"
	// AccountSummaryProvenanceCachedFallback means an unstamped streaming
	// cache was reparsed after the one-shot request ended without rows.
	AccountSummaryProvenanceCachedFallback AccountSummaryProvenance = "cached_fallback"
)

// RequestAccountSummaryWithProvenance preserves RequestAccountSummary's
// fallback behavior while exposing whether the gateway actually supplied rows
// for this request. CachedFallback has no trustworthy source receipt even
// though parsing gives the caller-owned copy an AsOf timestamp.
func (c *Connector) RequestAccountSummaryWithProvenance(ctx context.Context, timeout time.Duration) (*RawAccountSummary, AccountSummaryProvenance, error) {
	if !c.isConnected() {
		return nil, "", ErrIBKRUnavailable
	}
	if timeout <= 0 {
		timeout = defaultAccountSummaryTimeout
	}
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil || !conn.IsConnected() {
		return nil, "", ErrIBKRUnavailable
	}
	expectedAccount := accountSummaryExpectedAccount(conn)
	if !accountCodeConcrete(expectedAccount) {
		return nil, "", ErrAccountSummaryScopeConflict
	}
	reqID, err := conn.nextRequestIDForForwarding()
	if err != nil {
		return nil, "", err
	}
	defer conn.discardRequestIDReservation(reqID)

	if err := conn.RequestAccountSummaryForAccount(reqID, accountSummaryTags, expectedAccount); err != nil {
		return nil, "", fmt.Errorf("request account summary: %w", err)
	}

	// Always cancel the subscription on the way out: end-of-stream means IBKR
	// has sent the snapshot, but the request remains active until cancelled.
	defer func() {
		if conn.IsConnected() {
			if cancelErr := conn.CancelAccountSummary(reqID); cancelErr != nil {
				connectorLogger.Debugf("CancelAccountSummary(reqID=%d) failed: %v", reqID, cancelErr)
			}
		}
	}()

	type snapshotResult struct {
		rows map[string]string
		err  error
	}
	resCh := make(chan snapshotResult, 1)
	go func() {
		rows, err := conn.awaitAccountSummarySnapshot(reqID, timeout)
		resCh <- snapshotResult{rows: rows, err: err}
	}()

	var raw map[string]string
	select {
	case res := <-resCh:
		if res.err != nil {
			return nil, "", fmt.Errorf("await account summary end: %w", res.err)
		}
		raw = res.rows
	case <-ctx.Done():
		return nil, "", ctx.Err()
	}

	// Keep normal reads isolated from concurrent streaming account updates. An
	// end marker without rows falls back to the streaming cache so callers can
	// still consume a previously observed snapshot.
	var fallback map[string]string
	if len(raw) == 0 && accountSummaryCacheAdmissible(conn, expectedAccount) {
		fallback = conn.GetAccountSummary()
	}
	return accountSummaryFromRequestRows(raw, fallback, expectedAccount)
}

// accountSummaryCacheAdmissible reports whether the connection's streaming
// account cache may be read as expectedAccount's values.
//
// That cache is unstamped: one login's accounts share the map, no row keeps its
// account attribution, and nothing clears it when the account-updates
// subscription rebinds. So a read can never prove after the fact which account
// its rows describe — only the session's own identity can. A login carrying one
// account observes that concrete code and the cache can hold nothing else; a
// multi-account login observes the managedAccounts aggregate and never
// qualifies, because relabeling those rows with the configured pin would
// publish a sibling's values under the pinned account (issue #14).
func accountSummaryCacheAdmissible(conn *Connection, expectedAccount string) bool {
	if conn == nil || !accountCodeConcrete(expectedAccount) {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(conn.GetAccountCode()), expectedAccount)
}

// accountSummaryExpectedAccount resolves the single account a one-shot
// summary may represent. The configured pin is authoritative when the broker
// session confirms it as a managed account; the aggregate managedAccounts
// string itself is never a request scope. An unpinned session remains usable
// only when the broker exposes one concrete account.
func accountSummaryExpectedAccount(conn *Connection) string {
	if conn == nil {
		return ""
	}
	configured := ""
	if conn.config != nil {
		configured = strings.TrimSpace(conn.config.Account)
	}
	observed := strings.TrimSpace(conn.GetAccountCode())
	if configured != "" {
		if !accountCodeConcrete(configured) {
			return ""
		}
		if strings.EqualFold(configured, observed) || conn.managedAccountMember(configured) {
			return configured
		}
		return ""
	}
	if accountCodeConcrete(observed) {
		return observed
	}
	return ""
}

func accountSummaryFromRequestRows(raw, fallback map[string]string, accountID string) (*RawAccountSummary, AccountSummaryProvenance, error) {
	provenance := AccountSummaryProvenanceRequest
	if len(raw) == 0 {
		raw = fallback
		provenance = AccountSummaryProvenanceCachedFallback
	}
	return parseAccountSummary(raw, accountID), provenance, nil
}

// parseAccountSummary converts the IBKR-format key/value map (as returned by
// Connection.GetAccountSummary) into a typed AccountSummary. The IBKR key
// format is `<tag>` for the account base currency and `<tag>_<currency>` for
// explicit currency overrides. We prefer the BASE-currency form; if absent
// for a tag we fall back to the first currency-specific entry encountered.
//
// The $LEDGER:ALL tag (in accountSummaryTags) instructs the gateway to also
// emit per-currency rows — those are aggregated into CurrencyLedger so
// callers can attribute currency exposure without re-fetching.
func parseAccountSummary(raw map[string]string, accountID string) *RawAccountSummary {
	summary := &RawAccountSummary{
		AccountID:      accountID,
		AsOf:           time.Now().UTC(),
		CurrencyLedger: make(map[string]CurrencyLedger),
		Raw:            make(map[string]string, len(raw)),
	}
	maps.Copy(summary.Raw, raw)

	// Each binding accepts one or more accepted tag names — the parser
	// tries each in order and uses the first that resolves. This makes the
	// canonical and legacy names interchangeable so a gateway that still
	// emits the long form (or a future protocol shift) doesn't lose the
	// value silently.
	tagBindings := []struct {
		tags  []string
		field **float64
	}{
		{[]string{"NetLiquidation"}, &summary.NetLiquidation},
		{[]string{"BuyingPower"}, &summary.BuyingPower},
		{[]string{"AvailableFunds"}, &summary.AvailableFunds},
		{[]string{"ExcessLiquidity"}, &summary.ExcessLiquidity},
		{[]string{"TotalCashValue"}, &summary.TotalCashValue},
		{[]string{"MaintMarginReq", "MaintenanceMarginReq"}, &summary.MaintenanceMargin},
		{[]string{"InitMarginReq"}, &summary.InitMarginReq},
		{[]string{"GrossPositionValue"}, &summary.GrossPositionValue},
		{[]string{"UnrealizedPnL"}, &summary.UnrealizedPnL},
		{[]string{"RealizedPnL"}, &summary.RealizedPnL},
		{[]string{"Cushion"}, &summary.Cushion},
		{[]string{"LookAheadInitMarginReq"}, &summary.LookAheadInitMargin},
		{[]string{"LookAheadMaintMarginReq"}, &summary.LookAheadMaintMargin},
		{[]string{"LookAheadAvailableFunds"}, &summary.LookAheadAvailable},
		{[]string{"LookAheadExcessLiquidity"}, &summary.LookAheadExcess},
	}

	for _, b := range tagBindings {
		for _, tag := range b.tags {
			val, currency, ok := lookupAccountValue(raw, tag)
			if !ok {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
			if err != nil {
				continue
			}
			*b.field = &parsed
			if summary.Currency == "" && currency != "" {
				summary.Currency = currency
			}
			break
		}
	}

	// AccountType is a string tag (e.g. "INDIVIDUAL", "IB-MARGIN") rather
	// than a numeric value, so it does not pass through the float-bindings
	// loop. The gateway emits it with an empty currency field.
	if v, _, ok := lookupAccountValue(raw, "AccountType"); ok {
		summary.AccountType = strings.TrimSpace(v)
	}

	summary.CurrencyLedger = extractCurrencyLedger(raw)
	summary.BaseCurrency, summary.BaseCurrencyProvenance = accountBaseCurrencyEvidence(raw)

	return summary
}

func accountBaseCurrencyEvidence(raw map[string]string) (string, AccountBaseCurrencyProvenance) {
	if rawCurrency := strings.ToUpper(strings.TrimSpace(raw["Currency"])); len(rawCurrency) == 3 && rawCurrency != "BASE" {
		return rawCurrency, AccountBaseCurrencyExplicitTag
	}
	valueSuffix := ""
	for _, tag := range accountBaseCurrencyValueTags {
		prefix := tag + "_"
		for key := range raw {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			ccy := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(key, prefix)))
			if len(ccy) != 3 || ccy == "BASE" {
				continue
			}
			if valueSuffix != "" && valueSuffix != ccy {
				return "", AccountBaseCurrencyUnknown
			}
			valueSuffix = ccy
		}
	}
	if valueSuffix != "" {
		return valueSuffix, AccountBaseCurrencyValueSuffix
	}
	return "", AccountBaseCurrencyUnknown
}

// CurrencyLedgerSnapshot returns a caller-owned map derived from the
// connector's streaming account-summary cache. It neither blocks nor issues
// gateway traffic. An empty map means either no non-base exposure was observed
// or the cache is not populated yet; use connection state to distinguish them.
// The method is safe to call concurrently with streaming cache updates.
func (c *Connector) CurrencyLedgerSnapshot() map[string]CurrencyLedger {
	raw := c.AccountSummaryRaw()
	return extractCurrencyLedger(raw)
}

// AccountSummaryRaw returns a defensive copy of the connector's current raw
// account-summary cache. The map uses IBKR keys: bare tags for base-currency
// values and `<tag>_<currency>` for currency-specific values. It is empty when
// no connection or observations are available; emptiness alone does not
// describe connection state. The method is safe to call concurrently with
// streaming cache updates.
func (c *Connector) AccountSummaryRaw() map[string]string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if conn == nil {
		return map[string]string{}
	}
	return conn.GetAccountSummary()
}

// CachedAccountSummary returns a caller-owned typed snapshot of the connector's
// streaming account-summary cache, labeled with the account it belongs to. It
// does not issue gateway traffic and returns nil until at least one core
// account value has been observed, or whenever the cache is not admissible for
// the session's expected account. The method is safe to call concurrently with
// streaming cache updates.
func (c *Connector) CachedAccountSummary() *RawAccountSummary {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	account := accountSummaryExpectedAccount(conn)
	if !accountSummaryCacheAdmissible(conn, account) {
		return nil
	}
	raw := conn.GetAccountSummary()
	if len(raw) == 0 {
		return nil
	}
	summary := parseAccountSummary(raw, account)
	if summary.NetLiquidation == nil && summary.BuyingPower == nil &&
		summary.AvailableFunds == nil && summary.TotalCashValue == nil {
		return nil
	}
	return summary
}

// extractCurrencyLedger walks the raw map for `<field>_<CCY>` entries
// matching the canonical IBKR $LEDGER fields and aggregates them by
// currency. The "BASE" pseudo-currency entry IBKR also emits is
// dropped — it duplicates the top-level totals.
//
// Currencies appearing only in margin-related fields (with no
// NetLiquidationByCurrency or CashBalance) are also dropped — they
// represent zero-balance currencies the gateway happened to include.
func extractCurrencyLedger(raw map[string]string) map[string]CurrencyLedger {
	ledger := map[string]*CurrencyLedger{}
	assign := func(field, ccy, val string) {
		if !currencyLedgerField(field) || ccy == "" || ccy == "BASE" {
			return
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
		if err != nil {
			return
		}
		row, ok := ledger[ccy]
		if !ok {
			row = &CurrencyLedger{}
			ledger[ccy] = row
		}
		switch field {
		case "NetLiquidationByCurrency":
			row.NetLiquidationByCurrency = parsed
		case "CashBalance":
			row.CashBalance = parsed
		case "StockMarketValue":
			row.StockMarketValue = parsed
		case "OptionMarketValue":
			row.OptionMarketValue = parsed
		case "UnrealizedPnL":
			row.UnrealizedPnL = parsed
		case "RealizedPnL":
			row.RealizedPnL = parsed
		case "ExchangeRate":
			row.ExchangeRate = parsed
		}
	}
	for k, v := range raw {
		idx := strings.LastIndexByte(k, '_')
		if idx <= 0 || idx == len(k)-1 {
			continue
		}
		field := k[:idx]
		ccy := k[idx+1:]
		assign(field, ccy, v)
	}
	out := make(map[string]CurrencyLedger, len(ledger))
	for ccy, row := range ledger {
		if row == nil {
			continue
		}
		// Keep only currencies the user actually holds value in. Without
		// NetLiquidation OR a non-zero cash/market value, the row is
		// noise (zero-balance currency the gateway included).
		if row.NetLiquidationByCurrency == 0 && row.CashBalance == 0 &&
			row.StockMarketValue == 0 && row.OptionMarketValue == 0 {
			continue
		}
		out[ccy] = *row
	}
	return out
}

// lookupAccountValue returns the value, currency, and ok flag for a tag.
// IBKR encodes BASE-currency values under the bare tag and non-BASE values
// under `<tag>_<currency>`. We prefer the bare form; otherwise we accept the
// first currency-suffixed entry deterministically (sorted by suffix).
func lookupAccountValue(raw map[string]string, tag string) (string, string, bool) {
	if v, ok := raw[tag]; ok {
		return v, "", true
	}
	prefix := tag + "_"
	var bestKey string
	for k := range raw {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if bestKey == "" || k < bestKey {
			bestKey = k
		}
	}
	if bestKey == "" {
		return "", "", false
	}
	return raw[bestKey], strings.TrimPrefix(bestKey, prefix), true
}
