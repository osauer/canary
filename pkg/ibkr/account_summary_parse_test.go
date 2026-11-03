package ibkr

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The typed snapshot reads each tag from its base-currency row, proves the
// base currency from the explicit tag or a uniform value suffix, accepts the
// gateway's alias for the maintenance margin, skips values that are not
// numbers, and keeps every raw row.
func TestParseAccountSummaryBindsTagsByBaseCurrency(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        map[string]string
		netLiq     float64
		currency   string
		base       string
		provenance AccountBaseCurrencyProvenance
		maint      *float64
	}{
		{
			name:   "explicit currency tag",
			raw:    map[string]string{"Currency": "eur", "NetLiquidation_EUR": "100000", "NetLiquidation_USD": "110000", "MaintenanceMarginReq_EUR": "2500", "AccountType": " INDIVIDUAL ", "Cushion": "0.9"},
			netLiq: 100000, currency: "EUR", base: "EUR", provenance: AccountBaseCurrencyExplicitTag, maint: new(2500.0),
		},
		{
			name:   "uniform value suffix",
			raw:    map[string]string{"NetLiquidation_CHF": "100000", "BuyingPower_CHF": "400000", "MaintMarginReq_CHF": "n/a", "UnrealizedPnL_EUR": "5", "UnrealizedPnL_CHF": "7"},
			netLiq: 100000, currency: "CHF", base: "CHF", provenance: AccountBaseCurrencyValueSuffix,
		},
		{
			name:   "bare base rows",
			raw:    map[string]string{"NetLiquidation": "100000", "TotalCashValue": "25000"},
			netLiq: 100000, currency: "", base: "", provenance: AccountBaseCurrencyUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary := parseAccountSummary(tc.raw, "DU1234567")
			if summary.AccountID != "DU1234567" || summary.NetLiquidation == nil || *summary.NetLiquidation != tc.netLiq {
				t.Fatalf("summary = %+v", summary)
			}
			if summary.Currency != tc.currency || summary.BaseCurrency != tc.base || summary.BaseCurrencyProvenance != tc.provenance {
				t.Fatalf("currency=%q base=%q provenance=%q, want %q %q %q", summary.Currency, summary.BaseCurrency, summary.BaseCurrencyProvenance, tc.currency, tc.base, tc.provenance)
			}
			if summary.BaseCurrencyProvenance.Proven() != (tc.base != "") {
				t.Fatalf("Proven() = %t for %q", summary.BaseCurrencyProvenance.Proven(), tc.provenance)
			}
			if (tc.maint == nil) != (summary.MaintenanceMargin == nil) || (tc.maint != nil && *summary.MaintenanceMargin != *tc.maint) {
				t.Fatalf("maintenance margin = %v, want %v", summary.MaintenanceMargin, tc.maint)
			}
			if len(summary.Raw) != len(tc.raw) {
				t.Fatalf("raw rows kept = %d, want %d", len(summary.Raw), len(tc.raw))
			}
			if tc.name == "explicit currency tag" {
				if summary.AccountType != "INDIVIDUAL" || summary.Cushion == nil || *summary.Cushion != 0.9 {
					t.Fatalf("account type / cushion = %q %v", summary.AccountType, summary.Cushion)
				}
			}
			if tc.name == "uniform value suffix" {
				// The account total keeps its own suffix row; the sibling
				// ledger slice does not displace it.
				if summary.UnrealizedPnL == nil || *summary.UnrealizedPnL != 7 || summary.MaintenanceMargin != nil {
					t.Fatalf("unrealized=%v maintenance=%v", summary.UnrealizedPnL, summary.MaintenanceMargin)
				}
			}
		})
	}
	if _, _, ok := lookupAccountValue(map[string]string{"CashBalance_EUR": "1", "CashBalance_USD": "2"}, "CashBalance", ""); ok {
		t.Fatal("two currency slices and no base row were read as the account total")
	}
	if v, ccy, ok := lookupAccountValue(map[string]string{"NetLiquidation_EUR": "1", "NetLiquidation_USD": "2"}, "NetLiquidation", ""); !ok || v != "1" || ccy != "EUR" {
		t.Fatalf("deterministic fallback = %q %q %t, want the first row by key", v, ccy, ok)
	}
}

// The streaming cache is readable as the pinned account's summary only
// while the session itself proves the cache holds that one account; a login
// that carries two accounts cannot attribute its rows and reads as empty.
func TestCachedAccountSummaryFollowsCacheAdmissibility(t *testing.T) {
	c, conn, _ := settlementConnectorFixture(t)
	if c.CachedAccountSummary() != nil || len(c.AccountSummaryRaw()) != 0 || len(c.CurrencyLedgerSnapshot()) != 0 {
		t.Fatal("an empty cache produced a summary")
	}
	conn.handleAccountValue([]string{"6", "2", "AccountType", "INDIVIDUAL", "", "DU1234567"})
	if c.CachedAccountSummary() != nil {
		t.Fatal("a cache without core values produced a summary")
	}
	for _, row := range [][]string{
		{"NetLiquidation", "100000", "EUR"}, {"TotalCashValue", "25000", "EUR"},
		{"CashBalance", "1000", "USD"}, {"NetLiquidationByCurrency", "1000", "USD"}, {"ExchangeRate", "0.9", "USD"},
		{"CashBalance", "0", "GBP"}, {"ExchangeRate", "1.2", "GBP"},
	} {
		conn.handleAccountValue(append([]string{"6", "2"}, append(row, "DU1234567")...))
	}
	summary := c.CachedAccountSummary()
	if summary == nil || summary.AccountID != "DU1234567" || summary.NetLiquidation == nil || *summary.NetLiquidation != 100000 || summary.AccountType != "INDIVIDUAL" {
		t.Fatalf("cached summary = %+v", summary)
	}
	if summary.BaseCurrency != "EUR" || !summary.BaseCurrencyProvenance.Proven() {
		t.Fatalf("base currency = %q %q", summary.BaseCurrency, summary.BaseCurrencyProvenance)
	}
	ledger := c.CurrencyLedgerSnapshot()
	usd, ok := ledger["USD"]
	if !ok || usd.CashBalance != 1000 || !usd.NetLiquidationObserved || !usd.ExchangeRateObserved || usd.ExchangeRate != 0.9 {
		t.Fatalf("USD ledger = %+v, %t", usd, ok)
	}
	if _, ok := ledger["GBP"]; ok {
		t.Fatal("a currency with only an exchange rate and a zero balance was kept")
	}
	raw := c.AccountSummaryRaw()
	if raw["NetLiquidation_EUR"] != "100000" || raw["CashBalance_USD"] != "1000" {
		t.Fatalf("raw cache = %v", raw)
	}
	raw["NetLiquidation_EUR"] = "tampered"
	if c.AccountSummaryRaw()["NetLiquidation_EUR"] != "100000" {
		t.Fatal("the raw copy was not detached from the cache")
	}

	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU1234567,DU7654321"))
	if c.CachedAccountSummary() != nil || len(c.AccountSummaryRaw()) != 0 || len(c.CurrencyLedgerSnapshot()) != 0 {
		t.Fatal("a two-account login read the shared cache as the pinned account")
	}
}

// histTSummaryRow feeds one accountSummary callback for request id.
func histTSummaryRow(conn *Connection, id int, account, tag, value, currency string) {
	conn.handleAccountSummary([]string{strconv.Itoa(msgAccountSummary), "2", strconv.Itoa(id), account, tag, value, currency})
}

// One reqAccountSummary carries the desk's tag list, its rows are read per
// tag and currency into the typed snapshot with the ledger slices apart from
// the account totals, the end marker completes it with a request-provenance
// settlement observation, and the subscription is cancelled on the way out.
func TestRequestAccountSummaryParsesRowsPerTagAndCurrency(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	done := make(chan *RawAccountSummary, 1)
	go func() {
		summary, err := c.RequestAccountSummary(context.Background(), 2*time.Second)
		if err != nil {
			t.Errorf("RequestAccountSummary = %v", err)
		}
		done <- summary
	}()
	frames := probeFrames(t, wire, conn, 1)
	if frames[0][0] != strconv.Itoa(reqAccountSummary) || frames[0][3] != "All" || !strings.Contains(frames[0][4], "NetLiquidation,") || !strings.HasSuffix(frames[0][4], "$LEDGER:ALL") {
		t.Fatalf("request = %v", frames[0])
	}
	id, _ := strconv.Atoi(frames[0][2])
	for _, row := range [][]string{
		{"DU1234567", "NetLiquidation", "100000", "EUR"},
		{"DU1234567", "TotalCashValue", "25000", "EUR"},
		{"DU1234567", "MaintenanceMarginReq", "2500", "EUR"},
		{"DU1234567", "UnrealizedPnL", "700", "EUR"},
		{"DU1234567", "AccountType", "INDIVIDUAL", ""},
		{"DU1234567", "Cushion", "0.95", ""},
		{"DU1234567", "SettledCash", "20000", "EUR"},
		{"All", "$LEDGER-UnrealizedPnL", "5", "EUR"},
		{"All", "$LEDGER-CashBalance", "1000", "USD"},
		{"All", "$LEDGER-NetLiquidationByCurrency", "1000", "USD"},
		{"All", "$LEDGER-SettledCash", "900", "USD"},
		{"All", "$LEDGER-CashBalance", "26000", "BASE"},
		{"All", "$LEDGER-Unmodeled", "1", "USD"},
	} {
		histTSummaryRow(conn, id, row[0], row[1], row[2], row[3])
	}
	probeEnd(conn, id)
	summary := <-done
	if summary == nil {
		t.Fatal("no summary")
	}
	if summary.AccountID != "DU1234567" || summary.NetLiquidation == nil || *summary.NetLiquidation != 100000 || summary.TotalCashValue == nil || *summary.TotalCashValue != 25000 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.MaintenanceMargin == nil || *summary.MaintenanceMargin != 2500 || summary.UnrealizedPnL == nil || *summary.UnrealizedPnL != 700 {
		t.Fatalf("maintenance=%v unrealized=%v; want the alias bound and the account total kept apart from the EUR slice", summary.MaintenanceMargin, summary.UnrealizedPnL)
	}
	if summary.AccountType != "INDIVIDUAL" || summary.Cushion == nil || *summary.Cushion != 0.95 || summary.BaseCurrency != "EUR" || !summary.BaseCurrencyProvenance.Proven() {
		t.Fatalf("type=%q cushion=%v base=%q/%q", summary.AccountType, summary.Cushion, summary.BaseCurrency, summary.BaseCurrencyProvenance)
	}
	usd, ok := summary.CurrencyLedger["USD"]
	if !ok || usd.CashBalance != 1000 || usd.NetLiquidationByCurrency != 1000 || !usd.SettledCashObserved || usd.SettledCash != 900 {
		t.Fatalf("USD ledger = %+v, %t", usd, ok)
	}
	if _, ok := summary.CurrencyLedger["BASE"]; ok {
		t.Fatal("the BASE pseudo-currency entered the ledger")
	}
	if _, ok := summary.CurrencyLedger["EUR"]; ok {
		t.Fatal("a base-currency slice with no balance entered the ledger")
	}
	if summary.SettlementObservation == nil || summary.SettlementObservation.AsOf.IsZero() {
		t.Fatalf("settlement observation = %+v, want request provenance", summary.SettlementObservation)
	}
	frames = probeFrames(t, wire, conn, 2)
	if frames[1][0] != strconv.Itoa(cancelAccountSummary) || frames[1][2] != strconv.Itoa(id) {
		t.Fatalf("cancel = %v, want the request's subscription cancelled", frames[1])
	}
}

// Without the end marker the read fails at its budget and still cancels its
// subscription; a cancelled context fails the same way; a connector without a
// session refuses before the wire.
func TestRequestAccountSummaryTimeoutCancelsTheSubscription(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	start := time.Now()
	_, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), 100*time.Millisecond)
	if err == nil || provenance != "" || !strings.Contains(err.Error(), "await account summary end") {
		t.Fatalf("silent read = %v (%q), want the await failure", err, provenance)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("silent read waited %s", elapsed)
	}
	frames := probeFrames(t, wire, conn, 2)
	if frames[1][0] != strconv.Itoa(cancelAccountSummary) || frames[1][2] != frames[0][2] {
		t.Fatalf("frames = %v, want the request then its cancel", frames)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.RequestAccountSummary(ctx, 5*time.Second)
		done <- err
	}()
	probeFrames(t, wire, conn, 3)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v, want context.Canceled", err)
	}
	probeFrames(t, wire, conn, 4)

	conn.status = StatusDisconnected
	if _, err := c.RequestAccountSummary(context.Background(), time.Second); !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("read without a session = %v, want ErrIBKRUnavailable", err)
	}
}

// The settlement probe refuses a cancelled or missing context and a missing
// session without touching the wire, reports a broker refusal by code only,
// and treats a silent gateway as a timeout that still cancels the request.
func TestSettlementProbeRefusalsBrokerErrorAndTimeout(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	var nilCtx context.Context
	if got := c.RequestSettlementCashProbe(nilCtx, time.Second); got.Status != "cancelled" || got.CancelStatus != "not_started" {
		t.Fatalf("nil context = %+v", got)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got := c.RequestSettlementCashProbe(ctx, time.Second); got.Status != "cancelled" {
		t.Fatalf("cancelled context = %+v", got)
	}
	if len(wire.frames(conn)) != 0 {
		t.Fatal("a refused probe reached the wire")
	}

	done := startSettlementProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	if !conn.consumeSummaryProbeError(strconv.Itoa(id), 321, conn.BrokerSessionEpoch()) {
		t.Fatal("the probe did not own its broker error")
	}
	got := <-done
	if got.Status != "broker_error" || got.BrokerErrorCode != 321 || got.Observation != nil || got.CancelStatus != "sent" {
		t.Fatalf("broker refusal = %+v", got)
	}
	if !conn.consumeSummaryProbeError(strconv.Itoa(id), 321, conn.BrokerSessionEpoch()) {
		t.Fatal("a late error for the retired probe was not swallowed")
	}
	if conn.consumeSummaryProbeError("not-an-id", 321, conn.BrokerSessionEpoch()) || conn.consumeSummaryProbeError(strconv.Itoa(id+500), 321, conn.BrokerSessionEpoch()) {
		t.Fatal("an unrelated error was consumed as a probe error")
	}

	start := time.Now()
	got = c.RequestSettlementCashProbe(t.Context(), 100*time.Millisecond)
	if got.Status != "timeout" || got.CancelStatus != "sent" || got.Observation != nil {
		t.Fatalf("silent probe = %+v", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("silent probe waited %s", elapsed)
	}
	frames = probeFrames(t, wire, conn, 4)
	if frames[3][0] != strconv.Itoa(cancelAccountSummary) || frames[3][2] != frames[2][2] {
		t.Fatalf("frames = %v, want the silent probe cancelled", frames[2:])
	}

	c.mu.Lock()
	c.ready = false
	c.mu.Unlock()
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "unavailable" || got.CancelStatus != "not_started" {
		t.Fatalf("probe without a session = %+v", got)
	}
}

// A one-shot that ends with no rows falls back to the streaming cache and
// says so; it carries no settlement observation. A session whose broker
// account does not match the pinned one is a scope conflict before the wire.
func TestRequestAccountSummaryFallsBackToTheStreamingCacheAndRefusesScopeConflicts(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "100000", "EUR", "DU1234567"})
	type result struct {
		summary    *RawAccountSummary
		provenance AccountSummaryProvenance
		err        error
	}
	done := make(chan result, 1)
	go func() {
		summary, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), 2*time.Second)
		done <- result{summary, provenance, err}
	}()
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	probeEnd(conn, id)
	got := <-done
	if got.err != nil || got.provenance != AccountSummaryProvenanceCachedFallback || got.summary == nil {
		t.Fatalf("empty one-shot = %+v", got)
	}
	if got.summary.NetLiquidation == nil || *got.summary.NetLiquidation != 100000 || got.summary.SettlementObservation != nil || got.summary.AccountID != "DU1234567" {
		t.Fatalf("fallback summary = %+v", got.summary)
	}
	probeFrames(t, wire, conn, 2)

	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU9999999"))
	if _, _, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second); !errors.Is(err, ErrAccountSummaryScopeConflict) {
		t.Fatalf("mismatched broker account = %v, want ErrAccountSummaryScopeConflict", err)
	}
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "scope_conflict" {
		t.Fatalf("probe on a mismatched account = %+v", got)
	}
	if len(wire.frames(conn)) != 2 {
		t.Fatalf("frames = %v, want no request for the conflicting scope", wire.frames(conn))
	}
}
