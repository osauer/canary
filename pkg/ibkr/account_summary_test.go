package ibkr

import (
	"bufio"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type accountSummaryWriteSignal struct {
	once  sync.Once
	wrote chan struct{}
}

func (w *accountSummaryWriteSignal) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.wrote) })
	return len(p), nil
}

func TestParseAccountSummary_AllTagsBaseCurrency(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation":       "100000.50",
		"BuyingPower":          "400000.00",
		"AvailableFunds":       "95000.25",
		"ExcessLiquidity":      "94000.00",
		"TotalCashValue":       "20000.00",
		"MaintenanceMarginReq": "5000.00",
		"InitMarginReq":        "10000.00",
	}

	got := parseAccountSummary(raw, "U1234567")
	if got.AccountID != "U1234567" {
		t.Fatalf("AccountID = %q, want U1234567", got.AccountID)
	}
	if got.NetLiquidation == nil || *got.NetLiquidation != 100000.50 {
		t.Fatalf("NetLiquidation = %v, want 100000.50", got.NetLiquidation)
	}
	if got.BuyingPower == nil || *got.BuyingPower != 400000.00 {
		t.Fatalf("BuyingPower = %v, want 400000.00", got.BuyingPower)
	}
	if got.AvailableFunds == nil || *got.AvailableFunds != 95000.25 {
		t.Fatalf("AvailableFunds = %v, want 95000.25", got.AvailableFunds)
	}
	if got.ExcessLiquidity == nil || *got.ExcessLiquidity != 94000.00 {
		t.Fatalf("ExcessLiquidity = %v", got.ExcessLiquidity)
	}
	if got.TotalCashValue == nil || *got.TotalCashValue != 20000.00 {
		t.Fatalf("TotalCashValue = %v", got.TotalCashValue)
	}
	if got.MaintenanceMargin == nil || *got.MaintenanceMargin != 5000.00 {
		t.Fatalf("MaintenanceMargin = %v", got.MaintenanceMargin)
	}
	if got.InitMarginReq == nil || *got.InitMarginReq != 10000.00 {
		t.Fatalf("InitMarginReq = %v", got.InitMarginReq)
	}
	if got.Currency != "" {
		t.Fatalf("Currency = %q, want empty for BASE-only summary", got.Currency)
	}
	if got.AsOf.IsZero() {
		t.Fatalf("AsOf should be non-zero")
	}
}

func TestParseAccountSummary_NonBaseCurrencyOverride(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation_USD": "75000.00",
		"BuyingPower_USD":    "300000.00",
	}
	got := parseAccountSummary(raw, "U1234567")
	if got.NetLiquidation == nil || *got.NetLiquidation != 75000.00 {
		t.Fatalf("NetLiquidation = %v, want 75000.00", got.NetLiquidation)
	}
	if got.Currency != "USD" {
		t.Fatalf("Currency = %q, want USD", got.Currency)
	}
}

func TestParseAccountSummary_PrefersBaseOverCurrencySuffix(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation":     "100000.00",
		"NetLiquidation_USD": "99500.00",
	}
	got := parseAccountSummary(raw, "U1234567")
	if got.NetLiquidation == nil || *got.NetLiquidation != 100000.00 {
		t.Fatalf("NetLiquidation = %v, want 100000.00 (base preferred)", got.NetLiquidation)
	}
}

func TestParseAccountSummary_PartialMissingTags(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation": "50000.00",
	}
	got := parseAccountSummary(raw, "")
	if got.NetLiquidation == nil {
		t.Fatalf("NetLiquidation should be present")
	}
	if got.BuyingPower != nil {
		t.Fatalf("BuyingPower should be nil for missing tag")
	}
	if got.MaintenanceMargin != nil {
		t.Fatalf("MaintenanceMargin should be nil for missing tag")
	}
}

func TestParseAccountSummary_GarbageValuesIgnored(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation": "not-a-number",
		"BuyingPower":    "100.00",
	}
	got := parseAccountSummary(raw, "")
	if got.NetLiquidation != nil {
		t.Fatalf("NetLiquidation should be nil when value is unparseable")
	}
	if got.BuyingPower == nil || *got.BuyingPower != 100.00 {
		t.Fatalf("BuyingPower should still parse")
	}
}

func TestCachedAccountSummaryParsesStreamingCache(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	c := NewConnector(&ConnectorConfig{})
	c.conn = conn
	c.running = true
	c.ready = true
	c.SeedAccountIDForTest("DU7654321")
	conn.accountMu.Lock()
	conn.accountSummary["NetLiquidation_EUR"] = "1250000.00"
	conn.accountSummary["BuyingPower_EUR"] = "4800000.00"
	conn.accountSummary["TotalCashValue_EUR"] = "250000.00"
	conn.accountMu.Unlock()

	got := c.CachedAccountSummary()
	if got == nil {
		t.Fatalf("CachedAccountSummary returned nil for core account values")
	}
	if got.AccountID != "DU7654321" {
		t.Fatalf("AccountID = %q, want DU7654321", got.AccountID)
	}
	if got.Currency != "EUR" {
		t.Fatalf("Currency = %q, want EUR", got.Currency)
	}
	if got.NetLiquidation == nil || *got.NetLiquidation != 1250000.00 {
		t.Fatalf("NetLiquidation = %v, want 1250000.00", got.NetLiquidation)
	}
}

func TestCachedAccountSummaryEmptyOrNonCoreCacheReturnsNil(t *testing.T) {
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	c := NewConnector(&ConnectorConfig{})
	c.conn = conn
	c.running = true
	c.ready = true
	if got := c.CachedAccountSummary(); got != nil {
		t.Fatalf("CachedAccountSummary = %+v, want nil for empty cache", got)
	}
	conn.accountMu.Lock()
	conn.accountSummary["AccountType"] = "INDIVIDUAL"
	conn.accountMu.Unlock()
	if got := c.CachedAccountSummary(); got != nil {
		t.Fatalf("CachedAccountSummary = %+v, want nil without core account values", got)
	}
}

func TestAccountSummaryRequestProvenanceDistinguishesStreamingFallback(t *testing.T) {
	requestRows := map[string]string{"NetLiquidation": "100000", "TotalCashValue": "25000"}
	fallbackRows := map[string]string{"NetLiquidation": "90000", "TotalCashValue": "15000"}

	fresh, provenance, err := accountSummaryFromRequestRows(requestRows, fallbackRows, "DU123")
	if err != nil || provenance != AccountSummaryProvenanceRequest || fresh.NetLiquidation == nil || *fresh.NetLiquidation != 100000 {
		t.Fatalf("fresh request = %+v provenance=%q err=%v", fresh, provenance, err)
	}

	cached, provenance, err := accountSummaryFromRequestRows(nil, fallbackRows, "DU123")
	if err != nil || provenance != AccountSummaryProvenanceCachedFallback || cached.NetLiquidation == nil || *cached.NetLiquidation != 90000 {
		t.Fatalf("cached fallback = %+v provenance=%q err=%v", cached, provenance, err)
	}
}

func TestAccountBaseCurrencyEvidenceAcceptsEveryAllowlistedValueSuffix(t *testing.T) {
	for _, tag := range accountBaseCurrencyValueTags {
		t.Run(tag, func(t *testing.T) {
			currency, provenance := accountBaseCurrencyEvidence(map[string]string{
				tag + "_eur": "1",
			})
			if currency != "EUR" || provenance != AccountBaseCurrencyValueSuffix {
				t.Fatalf("evidence = (%q, %q), want (EUR, %q)", currency, provenance, AccountBaseCurrencyValueSuffix)
			}
		})
	}
}

func TestAccountBaseCurrencyEvidenceRejectsConflictingValueSuffixes(t *testing.T) {
	currency, provenance := accountBaseCurrencyEvidence(map[string]string{
		"NetLiquidation_USD": "100000",
		"AvailableFunds_EUR": "100",
	})
	if currency != "" || provenance != AccountBaseCurrencyUnknown {
		t.Fatalf("evidence = (%q, %q), want unknown", currency, provenance)
	}
}

func TestAccountBaseCurrencyEvidenceIgnoresLedgerFamilySuffixes(t *testing.T) {
	currency, provenance := accountBaseCurrencyEvidence(map[string]string{
		"NetLiquidation_EUR": "100000",
		"UnrealizedPnL_EUR":  "100",
		"UnrealizedPnL_USD":  "200",
		"RealizedPnL_EUR":    "10",
		"RealizedPnL_USD":    "20",
		"ExchangeRate_EUR":   "1",
		"ExchangeRate_USD":   "0.9",
	})
	if currency != "EUR" || provenance != AccountBaseCurrencyValueSuffix {
		t.Fatalf("evidence = (%q, %q), want (EUR, %q)", currency, provenance, AccountBaseCurrencyValueSuffix)
	}
}

func TestAccountBaseCurrencyEvidenceDoesNotInferFromExchangeRate(t *testing.T) {
	currency, provenance := accountBaseCurrencyEvidence(map[string]string{
		"ExchangeRate_USD": "1",
		"ExchangeRate_EUR": "0.92",
	})
	if currency != "" || provenance != AccountBaseCurrencyUnknown {
		t.Fatalf("evidence = (%q, %q), want unknown", currency, provenance)
	}
}

func TestLookupAccountValue_OrderingDeterministic(t *testing.T) {
	raw := map[string]string{
		"NetLiquidation_EUR": "90000.00",
		"NetLiquidation_USD": "100000.00",
		"NetLiquidation_GBP": "75000.00",
	}
	val, currency, ok := lookupAccountValue(raw, "NetLiquidation")
	if !ok {
		t.Fatalf("expected lookup to succeed")
	}
	// Sort by suffix → EUR is first lexicographically
	if currency != "EUR" || val != "90000.00" {
		t.Fatalf("got currency=%q val=%q, want EUR/90000.00 (deterministic by sort)", currency, val)
	}
}

func TestRequestAccountSummary_DisconnectedReturnsErrIBKRUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusDisconnected
	c.conn = conn
	c.running = true
	c.ready = false

	_, err := c.RequestAccountSummary(context.Background(), 1*time.Second)
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
	if got := err.Error(); got != "IBKR connection unavailable" {
		t.Fatalf("error text = %q, want canonical broker identity", got)
	}
}

func TestRequestAccountSummary_NoConnectorReturnsErrIBKRUnavailable(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	// c.conn is nil — isConnected() must return false without panic
	_, err := c.RequestAccountSummary(context.Background(), 1*time.Second)
	if !errors.Is(err, ErrIBKRUnavailable) {
		t.Fatalf("expected ErrIBKRUnavailable, got %v", err)
	}
}

// A multi-account login reports the full managed-account list on the session,
// but the one-shot summary must remain scoped to the operator's pinned account.
// Falling back to the list (or its first entry) either blocks the read or can
// return a sibling account's balances to risk and sizing consumers.
func TestRequestAccountSummaryUsesPinnedAccountWithinManagedList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		managed  string
		pinnedID string
	}{
		{name: "single account control", managed: "DU2222222", pinnedID: "DU2222222"},
		{name: "pinned member of managed list", managed: "DU1111111,DU2222222", pinnedID: "DU2222222"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &ConnectionConfig{
				Host:     "127.0.0.1",
				Port:     7497,
				ClientID: 41,
				Account:  tc.pinnedID,
			}
			c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
			conn := c.conn
			t.Cleanup(conn.rateLimiter.Stop)
			conn.status = StatusConnected
			setServerVersionReady(conn, minServerVersionRequired)
			wire := &accountSummaryWriteSignal{wrote: make(chan struct{})}
			conn.writer = bufio.NewWriter(wire)
			c.running = true
			c.ready = true
			conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", tc.managed))

			type result struct {
				summary    *RawAccountSummary
				provenance AccountSummaryProvenance
				err        error
			}
			resultCh := make(chan result, 1)
			go func() {
				summary, provenance, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
				resultCh <- result{summary: summary, provenance: provenance, err: err}
			}()

			select {
			case got := <-resultCh:
				t.Fatalf("summary returned before sending the pinned-account request: provenance=%q err=%v", got.provenance, got.err)
			case <-wire.wrote:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for account-summary request")
			}

			conn.handleAccountSummary([]string{"63", "2", "1", cfg.Account, "NetLiquidation", "100000", "USD"})
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))

			select {
			case got := <-resultCh:
				if got.err != nil {
					t.Fatalf("pinned-account summary failed: %v", got.err)
				}
				if got.provenance != AccountSummaryProvenanceRequest {
					t.Fatalf("provenance=%q, want %q", got.provenance, AccountSummaryProvenanceRequest)
				}
				if got.summary == nil {
					t.Fatal("pinned-account summary is nil")
				}
				if got.summary.AccountID != cfg.Account {
					t.Fatalf("summary account=%q, want pinned account %q", got.summary.AccountID, cfg.Account)
				}
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for pinned-account summary")
			}
		})
	}
}

func TestRequestAccountSummary_TimeoutDoesNotLeakGoroutines(t *testing.T) {
	// A real network failure means RequestAccountSummary will fail to send;
	// we verify the connector returns an error promptly without leaking.
	c := NewConnector(&ConnectorConfig{})
	conn := NewConnection(nil)
	defer conn.rateLimiter.Stop()
	conn.status = StatusDisconnected // forces ErrIBKRUnavailable, no network attempt
	c.conn = conn
	c.running = true
	c.ready = false

	// Snapshot the baseline AFTER construction so the threshold protects only
	// against per-call leaks, not against the rate-limiter / heartbeat
	// goroutines NewConnection always spawns.
	before := runtime.NumGoroutine()

	for range 50 {
		_, _ = c.RequestAccountSummary(context.Background(), 100*time.Millisecond)
	}

	// Allow scheduler to run any GC.
	time.Sleep(50 * time.Millisecond)
	after := runtime.NumGoroutine()
	if after > before+3 {
		t.Fatalf("goroutine leak suspected: before=%d after=%d", before, after)
	}
}

func TestAccountSummaryTags_IncludesAllExpectedTags(t *testing.T) {
	// Guard against accidental tag-list edits that would silently strip
	// fields the daemon's RawAccountSummary path needs.
	wantTags := []string{
		"NetLiquidation",
		"BuyingPower",
		"AvailableFunds",
		"ExcessLiquidity",
		"TotalCashValue",
		"MaintMarginReq",
		"InitMarginReq",
	}
	for _, tag := range wantTags {
		if !strings.Contains(accountSummaryTags, tag) {
			t.Errorf("accountSummaryTags missing %q (got %q)", tag, accountSummaryTags)
		}
	}
}
