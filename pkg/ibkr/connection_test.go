package ibkr

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConnection_WaitForPositionsEnd(t *testing.T) {
	// Create a test connection
	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
	}

	t.Run("SuccessfulCompletion", func(t *testing.T) {
		// Simulate position end signal arriving quickly
		go func() {
			time.Sleep(100 * time.Millisecond)
			conn.positionsEndChan <- struct{}{}
		}()

		// Wait with 1 second timeout
		err := conn.WaitForPositionsEnd(1 * time.Second)
		if err != nil {
			t.Errorf("WaitForPositionsEnd should succeed when signal received: %v", err)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		// Don't send any signal

		// Wait with short timeout
		err := conn.WaitForPositionsEnd(100 * time.Millisecond)
		if err == nil {
			t.Errorf("WaitForPositionsEnd should timeout when no signal received")
		}
		if err.Error() != "timeout waiting for positions end" {
			t.Errorf("Expected timeout error, got: %v", err)
		}
	})

	t.Run("ImmediateSignal", func(t *testing.T) {
		// Pre-fill the channel
		conn.positionsEndChan <- struct{}{}

		// Should return immediately
		start := time.Now()
		err := conn.WaitForPositionsEnd(1 * time.Second)
		elapsed := time.Since(start)

		if err != nil {
			t.Errorf("WaitForPositionsEnd should succeed immediately: %v", err)
		}
		if elapsed > 100*time.Millisecond {
			t.Errorf("Should return immediately, took %v", elapsed)
		}
	})
}

func TestHandleSystemNotificationClientIDInUseSetsLastError(t *testing.T) {
	t.Parallel()
	conn := &Connection{config: &ConnectionConfig{ClientID: 15}}
	var body []byte
	body = protoAppendInt32(body, 3, 326)
	body = protoAppendString(body, 4, "Unable to connect as the client id is already in use. Retry with a unique client id.")

	conn.handleSystemNotification([]string{"", string(body)})

	conn.statusMu.RLock()
	err := conn.lastError
	conn.statusMu.RUnlock()
	if !errors.Is(err, errClientIDInUse) {
		t.Fatalf("lastError = %v, want errClientIDInUse", err)
	}
	if !strings.HasPrefix(err.Error(), "IBKR: client ID already in use:") {
		t.Fatalf("lastError = %q, want canonical IBKR prefix", err.Error())
	}
	if !strings.Contains(err.Error(), "gateway client ID 15 is already in use") {
		t.Fatalf("lastError = %q, want operator-facing client ID diagnosis", err.Error())
	}
}

func TestConnection_WaitForAccountSummaryEnd(t *testing.T) {
	// Create a test connection
	conn := &Connection{
		acctSummaryEndChan: make(chan struct{}, 1),
	}

	t.Run("SuccessfulCompletion", func(t *testing.T) {
		// Simulate account summary end signal arriving
		go func() {
			time.Sleep(100 * time.Millisecond)
			conn.acctSummaryEndChan <- struct{}{}
		}()

		// Wait with 1 second timeout
		err := conn.WaitForAccountSummaryEnd(1 * time.Second)
		if err != nil {
			t.Errorf("WaitForAccountSummaryEnd should succeed when signal received: %v", err)
		}
	})

	t.Run("Timeout", func(t *testing.T) {
		// Don't send any signal

		// Wait with short timeout
		err := conn.WaitForAccountSummaryEnd(100 * time.Millisecond)
		if err == nil {
			t.Errorf("WaitForAccountSummaryEnd should timeout when no signal received")
		}
		if err.Error() != "timeout waiting for account summary end" {
			t.Errorf("Expected timeout error, got: %v", err)
		}
	})
}

func TestConnection_ClearChannel(t *testing.T) {
	// Test that RequestPositions clears the channel before requesting
	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
		positions:        make(map[string]*RawPosition),
	}

	// Pre-fill channel with old signal
	conn.positionsEndChan <- struct{}{}

	// Simulate RequestPositions clearing the channel
	select {
	case <-conn.positionsEndChan:
		// Channel cleared
	default:
		// Already empty
	}

	// Now channel should be empty
	select {
	case <-conn.positionsEndChan:
		t.Errorf("Channel should be empty after clearing")
	default:
		// Good - channel is empty
	}
}

func TestConnection_HandleAccountSummaryUpdatesAccount(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.account = ""
	conn.accountSummary = make(map[string]string)

	fields := []string{
		"63",    // msgID (handled before call, kept for completeness)
		"2",     // version
		"1",     // reqID
		"DU123", // account code
		"NetLiquidation",
		"150000",
		"USD",
	}

	conn.handleAccountSummary(fields)

	if conn.account != "DU123" {
		t.Fatalf("expected account code to be stored, got %q", conn.account)
	}

	stored, ok := conn.accountSummary["NetLiquidation_USD"]
	if !ok {
		t.Fatalf("expected NetLiquidation_USD to be present in account summary map")
	}
	if stored != "150000" {
		t.Fatalf("expected NetLiquidation value 150000, got %s", stored)
	}
}

func TestAccountSummarySnapshotIsolatedFromStreamingZeros(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.registerSummarySnapshot(7, "U111")
	conn.handleAccountSummary([]string{"63", "2", "7", "U111", "NetLiquidation", "311599.04", "EUR"})

	// A streaming zero batch for the same account lands before the read —
	// the issue #12 sequence. It may clobber the shared map, but not the
	// per-request snapshot.
	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "0.00", "EUR", "U111"})
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 7))

	rows, err := conn.awaitAccountSummarySnapshot(7, time.Second)
	if err != nil {
		t.Fatalf("awaitAccountSummarySnapshot: %v", err)
	}
	if got := rows["NetLiquidation_EUR"]; got != "311599.04" {
		t.Fatalf("snapshot NetLiquidation_EUR = %q, want 311599.04", got)
	}
	if got := conn.GetAccountSummary()["NetLiquidation_EUR"]; got != "0.00" {
		t.Fatalf("shared map NetLiquidation_EUR = %q, want streaming overwrite 0.00", got)
	}
}

func TestAwaitAccountSummarySnapshotTimeoutCleansUp(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.registerSummarySnapshot(9, "U111")
	if _, err := conn.awaitAccountSummarySnapshot(9, 10*time.Millisecond); err == nil {
		t.Fatalf("expected timeout error")
	}
	conn.accountMu.RLock()
	_, present := conn.summarySnapshots[9]
	conn.accountMu.RUnlock()
	if present {
		t.Fatalf("expected snapshot 9 to be dropped after timeout")
	}
	if _, err := conn.awaitAccountSummarySnapshot(9, time.Millisecond); err == nil {
		t.Fatalf("expected unregistered-reqID error after drop")
	}
}

func TestAccountSummarySnapshotRejectsMixedOrUnscopedRows(t *testing.T) {
	for _, test := range []struct {
		name       string
		rowAccount string
		tag        string
		currency   string
	}{
		{name: "foreign ordinary account", rowAccount: "U999", tag: "BuyingPower", currency: "EUR"},
		{name: "blank account", rowAccount: "", tag: "BuyingPower", currency: "EUR"},
		{name: "foreign concrete ledger", rowAccount: "U999", tag: "CashBalance", currency: "USD"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := NewConnection(DefaultConfig())
			if conn == nil {
				t.Fatal("NewConnection returned nil")
			}
			defer conn.rateLimiter.Stop()
			conn.account = "U111"
			conn.registerSummarySnapshot(41, "U111")
			conn.handleAccountSummary([]string{"63", "2", "41", "U111", "NetLiquidation", "100000", "EUR"})
			conn.handleAccountSummary([]string{"63", "2", "41", test.rowAccount, test.tag, "200000", test.currency})
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 41))

			rows, err := conn.awaitAccountSummarySnapshot(41, time.Second)
			if !errors.Is(err, ErrAccountSummaryScopeConflict) || rows != nil {
				t.Fatalf("mixed summary rows=%+v err=%v, want typed scope conflict", rows, err)
			}
			if shared := conn.GetAccountSummary(); len(shared) != 0 {
				t.Fatalf("rejected request mutated shared cache: %+v", shared)
			}
			if got := conn.GetAccountCode(); got != "U111" {
				t.Fatalf("rejected request rebound account to %q", got)
			}
		})
	}
}

func TestAccountSummarySnapshotIgnoresUnmodeledAggregateRows(t *testing.T) {
	for _, test := range []struct {
		name     string
		tag      string
		currency string
	}{
		{name: "aggregate ordinary tag", tag: "BuyingPower", currency: "EUR"},
		{name: "aggregate unknown ledger tag", tag: "AccruedCash", currency: "USD"},
		{name: "aggregate ledger blank currency", tag: "CashBalance", currency: ""},
		{name: "aggregate ledger BASE currency", tag: "CashBalance", currency: "BASE"},
		{name: "aggregate ledger short currency", tag: "CashBalance", currency: "US"},
		{name: "aggregate ledger non-letter currency", tag: "CashBalance", currency: "US1"},
		{name: "aggregate ledger lowercase currency", tag: "CashBalance", currency: "usd"},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn := NewConnection(DefaultConfig())
			if conn == nil {
				t.Fatal("NewConnection returned nil")
			}
			defer conn.rateLimiter.Stop()
			conn.account = "U111"
			conn.registerSummarySnapshot(41, "U111")
			conn.handleAccountSummary([]string{"63", "2", "41", "U111", "NetLiquidation", "100000", "EUR"})
			conn.handleAccountSummary([]string{"63", "2", "41", "All", test.tag, "200000", test.currency})
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 41))

			rows, err := conn.awaitAccountSummarySnapshot(41, time.Second)
			if err != nil {
				t.Fatalf("await account summary: %v", err)
			}
			if len(rows) != 1 || rows["NetLiquidation_EUR"] != "100000" {
				t.Fatalf("snapshot rows=%+v, want only scoped account row", rows)
			}
			if shared := conn.GetAccountSummary(); len(shared) != 0 {
				t.Fatalf("ignored request row mutated shared cache: %+v", shared)
			}
		})
	}
}

func TestAccountSummarySnapshotAcceptsAllCurrencyLedgerRows(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	defer conn.rateLimiter.Stop()
	conn.account = "U111"
	conn.registerSummarySnapshot(42, "U111")

	conn.handleAccountSummary([]string{"63", "2", "42", "U111", "NetLiquidation", "100000", "EUR"})
	conn.handleAccountSummary([]string{"63", "2", "42", "All", "CashBalance", "2500", "USD"})
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 42))

	rows, err := conn.awaitAccountSummarySnapshot(42, time.Second)
	if err != nil {
		t.Fatalf("awaitAccountSummarySnapshot: %v", err)
	}
	if got := rows["NetLiquidation_EUR"]; got != "100000" {
		t.Fatalf("ordinary account row = %q, want 100000", got)
	}
	// Admitted ledger rows are namespaced so they can never share a key with a
	// same-named account-level tag; the bare key must stay account-only.
	if got := rows[accountSummaryLedgerKeyPrefix+"CashBalance_USD"]; got != "2500" {
		t.Fatalf("aggregate ledger row = %q, want 2500 under the ledger namespace", got)
	}
	if _, ok := rows["CashBalance_USD"]; ok {
		t.Fatal("ledger row leaked into the account-level key namespace")
	}

	summary := parseAccountSummary(rows, "U111")
	if summary.NetLiquidation == nil || *summary.NetLiquidation != 100000 {
		t.Fatalf("parsed NetLiquidation = %v, want 100000", summary.NetLiquidation)
	}
	if got := summary.CurrencyLedger["USD"].CashBalance; got != 2500 {
		t.Fatalf("parsed USD CashBalance = %v, want 2500", got)
	}
	if shared := conn.GetAccountSummary(); len(shared) != 0 {
		t.Fatalf("request rows stamped streaming cache: %+v", shared)
	}
}

func TestHandleAccountValueDropsForeignAccountRows(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}
	conn.account = "U111"

	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "0.00", "EUR", "U999"})
	if _, ok := conn.GetAccountSummary()["NetLiquidation_EUR"]; ok {
		t.Fatalf("foreign-account row must not be stored")
	}

	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "5.00", "EUR", "U111"})
	if got := conn.GetAccountSummary()["NetLiquidation_EUR"]; got != "5.00" {
		t.Fatalf("bound-account row = %q, want 5.00", got)
	}

	// Single-account logins may omit the account code — accept those.
	conn.handleAccountValue([]string{"6", "2", "BuyingPower", "7.00", "EUR", ""})
	if got := conn.GetAccountSummary()["BuyingPower_EUR"]; got != "7.00" {
		t.Fatalf("empty-account row = %q, want 7.00", got)
	}
}

func TestConnectionManagedAccountsStoresVersionedAccountList(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatalf("NewConnection returned nil")
	}

	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", "DU123"))

	if got := conn.GetAccountCode(); got != "DU123" {
		t.Fatalf("managed account = %q, want DU123", got)
	}
}

func TestConnectionPortfolioStreamHealthTracksCompletionAndHeartbeat(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn.resetPortfolioStreamHealth("DU123", now)

	conn.processMessage(conn.encodeMsg(msgAcctDownloadEnd, "1", "DU123"))
	_, completed := conn.GetPositionsWithPortfolioHealth()
	if completed.Account != "DU123" || completed.InitialCompletedAt.IsZero() {
		t.Fatalf("completion health=%+v", completed)
	}
	if !completed.LastUpdateAt.IsZero() {
		t.Fatalf("completion manufactured heartbeat=%+v", completed)
	}

	conn.processMessage(conn.encodeMsg(msgAcctUpdateTime, "1", "12:34"))
	_, heartbeat := conn.GetPositionsWithPortfolioHealth()
	if heartbeat.LastUpdateAt.IsZero() || heartbeat.LastUpdateAt.Before(completed.InitialCompletedAt) {
		t.Fatalf("heartbeat health=%+v after completion=%+v", heartbeat, completed)
	}
}

func TestConnectionPortfolioStreamScopeConflictStaysUnavailableUntilReset(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	conn.resetPortfolioStreamHealth("DU123", now)

	conn.processMessage(conn.encodeMsg(msgAcctDownloadEnd, "1", "DU999"))
	_, conflicted := conn.GetPositionsWithPortfolioHealth()
	if conflicted.Account != "DU123" || !conflicted.RequestedAt.IsZero() || !conflicted.InitialCompletedAt.IsZero() || !conflicted.LastUpdateAt.IsZero() || conflicted.ScopeConflictAt.IsZero() {
		t.Fatalf("foreign completion health=%+v", conflicted)
	}

	conn.processMessage(conn.encodeMsg(msgAcctUpdateTime, "1", "12:01"))
	conn.processMessage(conn.encodeMsg(msgAcctDownloadEnd, "1", "DU123"))
	_, stillConflicted := conn.GetPositionsWithPortfolioHealth()
	if !stillConflicted.InitialCompletedAt.IsZero() || !stillConflicted.LastUpdateAt.IsZero() {
		t.Fatalf("matching frames bypassed latched scope conflict: %+v", stillConflicted)
	}

	conn.resetPortfolioStreamHealth("DU123", now.Add(time.Minute))
	conn.processMessage(conn.encodeMsg(msgAcctDownloadEnd, "1", "DU123"))
	_, recovered := conn.GetPositionsWithPortfolioHealth()
	if recovered.Account != "DU123" || recovered.RequestedAt.IsZero() || recovered.InitialCompletedAt.IsZero() || !recovered.ScopeConflictAt.IsZero() {
		t.Fatalf("subscription reset did not recover current scope: %+v", recovered)
	}
}

// The one-shot keys rows tag_currency, so before the ledger namespace the
// account-level UnrealizedPnL total (currency = base) and the $LEDGER:ALL
// base-currency slice wrote the same key and the last writer won — on a
// single-account multi-currency desk `canary account` could serve the base
// ledger slice as the account P&L total, dependent on wire arrival order.
// Both orders must serve the account total, and the ledger must keep its own
// slices, in both directions.
func TestAccountSummaryServesAccountTotalRegardlessOfLedgerWireOrder(t *testing.T) {
	accountRows := [][]string{
		{"63", "2", "43", "U111", "NetLiquidation", "100000", "EUR"},
		{"63", "2", "43", "U111", "UnrealizedPnL", "1200.50", "EUR"},
		{"63", "2", "43", "U111", "RealizedPnL", "-80.25", "EUR"},
	}
	ledgerRows := [][]string{
		// The base currency's own slice: identical tag names and currency
		// suffix as the account-level totals above.
		{"63", "2", "43", "All", "UnrealizedPnL", "999999", "EUR"},
		{"63", "2", "43", "All", "RealizedPnL", "888888", "EUR"},
		{"63", "2", "43", "All", "NetLiquidationByCurrency", "40000", "EUR"},
		{"63", "2", "43", "All", "CashBalance", "2500", "USD"},
		{"63", "2", "43", "All", "UnrealizedPnL", "-77", "USD"},
		{"63", "2", "43", "All", "NetLiquidationByCurrency", "60000", "USD"},
	}
	for _, tc := range []struct {
		name  string
		order [][]string
	}{
		{name: "account rows first", order: append(append([][]string{}, accountRows...), ledgerRows...)},
		{name: "ledger rows first", order: append(append([][]string{}, ledgerRows...), accountRows...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := NewConnection(DefaultConfig())
			if conn == nil {
				t.Fatal("NewConnection returned nil")
			}
			defer conn.rateLimiter.Stop()
			conn.account = "U111"
			conn.registerSummarySnapshot(43, "U111")
			for _, row := range tc.order {
				conn.handleAccountSummary(row)
			}
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 43))

			rows, err := conn.awaitAccountSummarySnapshot(43, time.Second)
			if err != nil {
				t.Fatalf("awaitAccountSummarySnapshot: %v", err)
			}
			summary := parseAccountSummary(rows, "U111")
			if summary.BaseCurrency != "EUR" {
				t.Fatalf("base currency = %q, want EUR", summary.BaseCurrency)
			}
			if summary.UnrealizedPnL == nil || *summary.UnrealizedPnL != 1200.50 {
				t.Fatalf("UnrealizedPnL = %v, want the account total 1200.50 — not a ledger slice", summary.UnrealizedPnL)
			}
			if summary.RealizedPnL == nil || *summary.RealizedPnL != -80.25 {
				t.Fatalf("RealizedPnL = %v, want the account total -80.25", summary.RealizedPnL)
			}
			// The reverse direction: the ledger keeps the slice, and the
			// account total must not overwrite it.
			if got := summary.CurrencyLedger["EUR"].UnrealizedPnL; got != 999999 {
				t.Fatalf("EUR ledger UnrealizedPnL = %v, want the slice 999999 — not the account total", got)
			}
			if got := summary.CurrencyLedger["USD"].UnrealizedPnL; got != -77 {
				t.Fatalf("USD ledger UnrealizedPnL = %v, want -77", got)
			}
			if got := summary.CurrencyLedger["USD"].CashBalance; got != 2500 {
				t.Fatalf("USD ledger CashBalance = %v, want 2500", got)
			}
		})
	}
}

// Gateway/TWS 10.47 introduces an API setting, default-enabled for new users,
// that prepends "$LEDGER-" to per-currency summary tags. The closed field
// allowlist must match the canonical name behind the prefix — checked the
// other way round, every 10.47 ledger row fails the allowlist and the entire
// per-currency ledger vanishes silently. Prefixed rows land in the same
// internal ledger namespace as inferred ones, under the canonical field name.
func TestAccountSummaryAccepts1047PrefixedLedgerRows(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	defer conn.rateLimiter.Stop()
	conn.account = "U111"
	conn.registerSummarySnapshot(44, "U111")

	conn.handleAccountSummary([]string{"63", "2", "44", "U111", "NetLiquidation", "100000", "EUR"})
	conn.handleAccountSummary([]string{"63", "2", "44", "U111", "UnrealizedPnL", "1200.50", "EUR"})
	conn.handleAccountSummary([]string{"63", "2", "44", "All", "$LEDGER-CashBalance", "2500", "USD"})
	conn.handleAccountSummary([]string{"63", "2", "44", "All", "$LEDGER-NetLiquidationByCurrency", "60000", "USD"})
	conn.handleAccountSummary([]string{"63", "2", "44", "All", "$LEDGER-UnrealizedPnL", "-77", "USD"})
	// An unmodeled prefixed aggregate stays ignored — the prefix widens
	// nothing beyond the canonical closed set.
	conn.handleAccountSummary([]string{"63", "2", "44", "All", "$LEDGER-AccruedCash", "12.34", "USD"})
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 44))

	rows, err := conn.awaitAccountSummarySnapshot(44, time.Second)
	if err != nil {
		t.Fatalf("awaitAccountSummarySnapshot: %v", err)
	}
	// Stored under the internal namespace with the canonical field name: one
	// storage shape regardless of which route proved ledger origin.
	if got := rows[accountSummaryLedgerKeyPrefix+"CashBalance_USD"]; got != "2500" {
		t.Fatalf("prefixed ledger row = %q under canonical namespace key, want 2500 (rows: %v)", got, rows)
	}
	if _, ok := rows[accountSummaryLedgerKeyPrefix+"$LEDGER-CashBalance_USD"]; ok {
		t.Fatal("wire prefix leaked into the stored namespace key")
	}
	summary := parseAccountSummary(rows, "U111")
	usd, ok := summary.CurrencyLedger["USD"]
	if !ok {
		t.Fatalf("USD ledger row missing — the 10.47 dialect erased the ledger: %+v", summary.CurrencyLedger)
	}
	if usd.CashBalance != 2500 || usd.NetLiquidationByCurrency != 60000 || usd.UnrealizedPnL != -77 {
		t.Fatalf("USD ledger = %+v, want 2500/60000/-77", usd)
	}
	if summary.UnrealizedPnL == nil || *summary.UnrealizedPnL != 1200.50 {
		t.Fatalf("account UnrealizedPnL = %v, want 1200.50 untouched by prefixed rows", summary.UnrealizedPnL)
	}
}
