package ibkr

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
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

// accountSummaryLedgerKeyPrefix namespaces $LEDGER rows admitted into a
// one-shot summary snapshot apart from account-level rows. $LEDGER:ALL reuses
// account-level tag names (UnrealizedPnL, RealizedPnL) for every held
// currency, and the account-level tags themselves arrive suffixed with the
// account's base currency — so in one flat `tag_currency` namespace the
// account total and the base-currency ledger slice share a key and the last
// writer wins, in wire-arrival order. With the namespace, lookupAccountValue
// reads only account-level keys and the ledger extraction reads only ledger
// keys. This is a storage namespace, not a wire form: since gateway 10.47,
// wire tags themselves may begin with '$' ("$LEDGER-…"), so the '$' alone
// guarantees nothing — the ':' separator is what keeps this prefix apart
// from every wire tag, and admission canonicalizes wire-prefixed tags before
// storing, so namespaced keys always carry the bare field name.
const accountSummaryLedgerKeyPrefix = "$LEDGER:"

// gatewayLedgerTagPrefix is the wire dialect Gateway/TWS 10.47 introduces: an
// API setting, default-enabled for new users, prepends "$LEDGER-" to each
// per-currency summary tag, so the gateway itself labels a row's ledger
// origin. A prefixed tag is therefore stronger origin evidence than this
// client's Account=All inference — both routes land the row in the same
// internal ledger namespace on the one-shot path.
const gatewayLedgerTagPrefix = "$LEDGER-"

// splitGatewayLedgerTag strips the 10.47 wire prefix from a tag. It must run
// BEFORE any currencyLedgerField allowlist check: checked the other way
// round, every 10.47 ledger row fails the closed allowlist and vanishes
// silently — the missing-ledger bug arriving by a new route.
func splitGatewayLedgerTag(tag string) (field string, wirePrefixed bool) {
	if rest, ok := strings.CutPrefix(tag, gatewayLedgerTagPrefix); ok {
		return rest, true
	}
	return tag, false
}

// dualUseSummaryTag reports whether field is simultaneously an ordinary
// account-level summary tag and a $LEDGER per-currency field. These are the
// keys whose flat-map collision the one-shot namespace removed; in a
// legacy-shaped map that speaks the 10.47 wire prefix, an unprefixed such
// key is the account-level row — 10.47 finally disambiguates the two — and
// must not be read as a ledger slice.
func dualUseSummaryTag(field string) bool {
	switch field {
	case "UnrealizedPnL", "RealizedPnL":
		return true
	}
	return false
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
	if !expectedAccount.valid() {
		return nil, "", ErrAccountSummaryScopeConflict
	}
	reqID, err := conn.nextRequestIDForForwarding()
	if err != nil {
		return nil, "", err
	}
	defer conn.discardRequestIDReservation(reqID)

	if err := conn.RequestAccountSummaryForAccount(reqID, accountSummaryTags, string(expectedAccount)); err != nil {
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
	return accountSummaryFromRequestRows(raw, fallback, string(expectedAccount))
}

// accountCode is one concrete broker account code — the only identity an
// account-scoped request may carry or a snapshot may be stamped with. The zero
// value means "no account". newAccountCode is the only way to obtain a non-zero
// one, so the aggregates that keep being mistaken for an account — a
// comma-joined managedAccounts list, "All", a blank field — cannot reach a
// consumer that asked for one account. The underlying type stays string, so
// config, daemon.db and the JSON contract are unchanged.
type accountCode string

// newAccountCode returns the concrete account named by raw, and whether raw
// named one at all. It is accountCodeConcrete's validation with a value
// attached: a caller that forgets to check ok gets the zero accountCode rather
// than an aggregate wearing an account's type.
func newAccountCode(raw string) (accountCode, bool) {
	raw = strings.TrimSpace(raw)
	if !accountCodeConcrete(raw) {
		return "", false
	}
	return accountCode(raw), true
}

func (a accountCode) valid() bool { return a != "" }

// equal compares two account codes the way the broker does — IBKR echoes codes
// back with inconsistent case across message families.
func (a accountCode) equal(other accountCode) bool {
	return a.valid() && other.valid() && strings.EqualFold(string(a), string(other))
}

// managedAccountSet is the inventory of accounts one login carries, as
// announced by msgManagedAccts. It is deliberately a different type from
// accountCode: every bug in this family came from one field being asked to mean
// both "the account" and "the accounts this login can reach".
type managedAccountSet []accountCode

func newManagedAccountSet(codes []string) managedAccountSet {
	out := make(managedAccountSet, 0, len(codes))
	for _, raw := range codes {
		if code, ok := newAccountCode(raw); ok {
			out = append(out, code)
		}
	}
	return out
}

func (m managedAccountSet) contains(a accountCode) bool {
	return slices.ContainsFunc(m, a.equal)
}

// multiAccount reports whether the login carries more than one account, which
// is the condition under which an unattributed value cannot be assigned to the
// pinned account.
func (m managedAccountSet) multiAccount() bool { return len(m) > 1 }

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
func accountSummaryCacheAdmissible(conn *Connection, expectedAccount accountCode) bool {
	if conn == nil || !expectedAccount.valid() {
		return false
	}
	observed, ok := newAccountCode(conn.GetAccountCode())
	return ok && observed.equal(expectedAccount)
}

// accountSummaryExpectedAccount resolves the single account a one-shot
// summary may represent. The configured pin is authoritative when the broker
// session confirms it as a managed account; the aggregate managedAccounts
// string itself is never a request scope. An unpinned session remains usable
// only when the broker exposes one concrete account.
func accountSummaryExpectedAccount(conn *Connection) accountCode {
	if conn == nil {
		return ""
	}
	observed, observedOK := newAccountCode(conn.GetAccountCode())
	if conn.config != nil && strings.TrimSpace(conn.config.Account) != "" {
		configured, ok := newAccountCode(conn.config.Account)
		if !ok {
			return ""
		}
		if configured.equal(observed) || conn.managedAccountMember(string(configured)) {
			return configured
		}
		return ""
	}
	if observedOK {
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
	// Resolve the base currency before the value bindings: it is what tells an
	// account-level tag apart from a same-named $LEDGER row.
	baseCurrency, baseProvenance := accountBaseCurrencyEvidence(raw)
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
			val, currency, ok := lookupAccountValue(raw, tag, baseCurrency)
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
	if v, _, ok := lookupAccountValue(raw, "AccountType", baseCurrency); ok {
		summary.AccountType = strings.TrimSpace(v)
	}

	summary.CurrencyLedger = extractCurrencyLedger(raw)
	summary.BaseCurrency, summary.BaseCurrencyProvenance = baseCurrency, baseProvenance

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
// values and `<tag>_<currency>` for currency-specific values.
//
// It is empty when no connection or observations are available, and also
// whenever the cache cannot be attributed to the session's expected account —
// the same admissibility rule CachedAccountSummary applies, because it is the
// same unstamped map. Emptiness alone does not describe connection state. The
// method is safe to call concurrently with streaming cache updates.
func (c *Connector) AccountSummaryRaw() map[string]string {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()
	if !accountSummaryCacheAdmissible(conn, accountSummaryExpectedAccount(conn)) {
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
	summary := parseAccountSummary(raw, string(account))
	if summary.NetLiquidation == nil && summary.BuyingPower == nil &&
		summary.AvailableFunds == nil && summary.TotalCashValue == nil {
		return nil
	}
	return summary
}

// extractCurrencyLedger aggregates the raw map's $LEDGER rows by currency.
// The "BASE" pseudo-currency entry IBKR also emits is dropped — it duplicates
// the top-level totals.
//
// Two key shapes exist. One-shot snapshots namespace every admitted ledger
// row under accountSummaryLedgerKeyPrefix; in such a map the namespace is the
// only ledger source, because every legacy-shaped `<field>_<CCY>` key left in
// it is an account-level row (the base-suffixed UnrealizedPnL total, for one)
// that must not be misread as a ledger slice. The streaming cache predates
// the namespace and keeps the legacy flat shape, so a map carrying no
// namespaced ledger key falls back to the legacy scan.
func extractCurrencyLedger(raw map[string]string) map[string]CurrencyLedger {
	if out, ok := namespacedCurrencyLedger(raw); ok {
		return out
	}
	return legacyCurrencyLedger(raw)
}

// namespacedCurrencyLedger reads the internal one-shot ledger namespace. ok
// reports whether the namespace held at least one recognizable ledger key —
// distinct from the returned map being non-empty, because the zero-balance
// filter may drop every recognized row and that must still count as "the
// namespace answered" rather than falling back to a scan that would
// misattribute account-level rows. Namespaced keys always carry the bare
// canonical field name: admission strips the 10.47 wire prefix before
// storing.
func namespacedCurrencyLedger(raw map[string]string) (map[string]CurrencyLedger, bool) {
	ledger := map[string]*CurrencyLedger{}
	recognized := false
	for k, v := range raw {
		if !strings.HasPrefix(k, accountSummaryLedgerKeyPrefix) {
			continue
		}
		field, ccy, ok := splitLedgerKey(strings.TrimPrefix(k, accountSummaryLedgerKeyPrefix))
		if !ok {
			continue
		}
		recognized = true
		assignCurrencyLedgerValue(ledger, field, ccy, v)
	}
	return finishCurrencyLedger(ledger), recognized
}

// legacyCurrencyLedger parses a flat map with no internal namespace: the
// streaming cache, and snapshots recorded before the namespace existed. Two
// wire dialects meet here — bare tags (pre-10.47) and "$LEDGER-"-prefixed
// tags (10.47) — and a mid-upgrade map may carry both. Resolution is by
// evidence, never by map iteration order:
//
//   - A wire-prefixed key is the gateway's own statement of ledger origin and
//     always serves.
//   - A bare dual-use key (UnrealizedPnL, RealizedPnL) in a map that speaks
//     the prefix anywhere is the account-level row, not a ledger slice, and
//     never serves as ledger.
//   - A bare ledger-only key still serves — a mixed mid-upgrade response must
//     parse — unless its prefixed twin carries a different value, which is a
//     contradiction: the duplicate is refused with a warning rather than
//     letting either value win by arrival or iteration order.
func legacyCurrencyLedger(raw map[string]string) map[string]CurrencyLedger {
	type cell struct {
		bare, prefixed       string
		hasBare, hasPrefixed bool
	}
	cells := map[string]map[string]*cell{} // ccy → canonical field → forms seen
	anyPrefixed := false
	for k, v := range raw {
		if strings.HasPrefix(k, accountSummaryLedgerKeyPrefix) {
			// Defensive: the legacy scan must never re-read namespaced rows.
			continue
		}
		idx := strings.LastIndexByte(k, '_')
		if idx <= 0 || idx == len(k)-1 {
			continue
		}
		field, wirePrefixed := splitGatewayLedgerTag(k[:idx])
		ccy := k[idx+1:]
		if !currencyLedgerField(field) || ccy == "" || ccy == "BASE" {
			continue
		}
		if wirePrefixed {
			anyPrefixed = true
		}
		byField := cells[ccy]
		if byField == nil {
			byField = map[string]*cell{}
			cells[ccy] = byField
		}
		cl := byField[field]
		if cl == nil {
			cl = &cell{}
			byField[field] = cl
		}
		if wirePrefixed {
			cl.prefixed, cl.hasPrefixed = v, true
		} else {
			cl.bare, cl.hasBare = v, true
		}
	}
	ledger := map[string]*CurrencyLedger{}
	for ccy, byField := range cells {
		for field, cl := range byField {
			switch {
			case cl.hasPrefixed && cl.hasBare:
				if dualUseSummaryTag(field) {
					// Expected coexistence, not a duplicate: the bare form is
					// the account-level total, the prefixed form the slice.
					assignCurrencyLedgerValue(ledger, field, ccy, cl.prefixed)
					continue
				}
				if cl.bare == cl.prefixed {
					assignCurrencyLedgerValue(ledger, field, ccy, cl.prefixed)
					continue
				}
				connectorLogger.Warnf("account summary ledger: %s_%s arrived in both the bare and %s form with different values; refusing the ambiguous duplicate", field, ccy, gatewayLedgerTagPrefix)
			case cl.hasPrefixed:
				assignCurrencyLedgerValue(ledger, field, ccy, cl.prefixed)
			case dualUseSummaryTag(field) && anyPrefixed:
				// In a prefixed-dialect map a bare dual-use key is the
				// account-level row; reading it as ledger would resurrect the
				// exact confusion the dialect exists to end.
			default:
				assignCurrencyLedgerValue(ledger, field, ccy, cl.bare)
			}
		}
	}
	return finishCurrencyLedger(ledger)
}

// splitLedgerKey parses a namespace-stripped `<field>_<CCY>` ledger key.
func splitLedgerKey(k string) (field, ccy string, ok bool) {
	idx := strings.LastIndexByte(k, '_')
	if idx <= 0 || idx == len(k)-1 {
		return "", "", false
	}
	field, ccy = k[:idx], k[idx+1:]
	if !currencyLedgerField(field) || ccy == "" || ccy == "BASE" {
		return "", "", false
	}
	return field, ccy, true
}

// assignCurrencyLedgerValue parses and stores one canonical ledger field.
// Callers have already vetted the field against currencyLedgerField and the
// currency against the BASE pseudo-row rule.
func assignCurrencyLedgerValue(ledger map[string]*CurrencyLedger, field, ccy, val string) {
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

// finishCurrencyLedger applies the zero-balance filter: currencies appearing
// only in margin-related fields (no NetLiquidationByCurrency, cash, or
// market value) are noise the gateway happened to include.
func finishCurrencyLedger(ledger map[string]*CurrencyLedger) map[string]CurrencyLedger {
	out := make(map[string]CurrencyLedger, len(ledger))
	for ccy, row := range ledger {
		if row == nil {
			continue
		}
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
// under `<tag>_<currency>`. The bare form wins; then the account's own base
// currency when one is proven; then, only when the tag has exactly one
// currency-suffixed row, that row.
//
// This function reads only account-level keys: one-shot snapshots store every
// admitted $LEDGER row under accountSummaryLedgerKeyPrefix, which no tag
// prefix can match, so an account total can no longer be displaced by a
// same-named ledger slice there. The preferences below still matter for the
// legacy-shaped streaming cache, where $LEDGER:ALL's reuse of UnrealizedPnL
// and RealizedPnL for every held currency remains flattened into one
// namespace — the same ambiguity accountBaseCurrencyValueTags already refuses
// to draw base-currency evidence from. Picking the lexicographically smallest
// suffix resolved a CHF/EUR/USD account's UnrealizedPnL to the CHF slice
// rather than the account total.
func lookupAccountValue(raw map[string]string, tag, baseCcy string) (string, string, bool) {
	if v, ok := raw[tag]; ok {
		return v, "", true
	}
	prefix := tag + "_"
	baseCcy = strings.ToUpper(strings.TrimSpace(baseCcy))
	var bestKey, baseKey string
	matches := 0
	for k := range raw {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		matches++
		if bestKey == "" || k < bestKey {
			bestKey = k
		}
		if baseCcy != "" && strings.EqualFold(strings.TrimPrefix(k, prefix), baseCcy) {
			baseKey = k
		}
	}
	if baseKey != "" {
		return raw[baseKey], strings.TrimPrefix(baseKey, prefix), true
	}
	if bestKey == "" {
		return "", "", false
	}
	// Several currency rows and no base-currency one to choose among them: for
	// a tag the ledger family reuses, any pick is a guess. Report nothing
	// rather than one currency's slice as the account total.
	if matches > 1 && currencyLedgerField(tag) {
		return "", "", false
	}
	return raw[bestKey], strings.TrimPrefix(bestKey, prefix), true
}
