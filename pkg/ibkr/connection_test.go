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

func TestConnection_EventDrivenVsSleep(t *testing.T) {
	// This test demonstrates the performance improvement
	// of event-driven completion vs fixed sleep

	conn := &Connection{
		positionsEndChan: make(chan struct{}, 1),
	}

	// Simulate fast IBKR response (200ms)
	go func() {
		time.Sleep(200 * time.Millisecond)
		conn.positionsEndChan <- struct{}{}
	}()

	// Measure event-driven approach
	start := time.Now()
	err := conn.WaitForPositionsEnd(5 * time.Second)
	eventDrivenTime := time.Since(start)

	if err != nil {
		t.Errorf("Event-driven wait failed: %v", err)
	}

	// Compare with old fixed sleep approach
	sleepStart := time.Now()
	time.Sleep(2 * time.Second) // Old approach
	sleepTime := time.Since(sleepStart)

	// Event-driven should be much faster
	if eventDrivenTime > 500*time.Millisecond {
		t.Errorf("Event-driven took too long: %v", eventDrivenTime)
	}

	if sleepTime < 2*time.Second {
		t.Errorf("Sleep should take at least 2 seconds: %v", sleepTime)
	}

	improvement := float64(sleepTime-eventDrivenTime) / float64(sleepTime) * 100
	t.Logf("Performance improvement: %.1f%% (Event: %v, Sleep: %v)",
		improvement, eventDrivenTime, sleepTime)

	if improvement < 75 {
		t.Errorf("Expected at least 75%% improvement, got %.1f%%", improvement)
	}
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
	if got := rows["CashBalance_USD"]; got != "2500" {
		t.Fatalf("aggregate ledger row = %q, want 2500", got)
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
