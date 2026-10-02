package ibkr

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func streamObservationFixture(t *testing.T) (*Connector, ConnectorSessionBinding, string) {
	t.Helper()
	c := NewConnector(&ConnectorConfig{})
	c.ready, c.conn.status = true, StatusConnected
	account := "U_SYNTHETIC"
	c.conn.account = account
	c.conn.resetPortfolioStreamHealth(account, time.Now().UTC())
	binding, ok := c.CaptureSession()
	if !ok {
		t.Fatal("no fixture session")
	}
	return c, binding, account
}

func feedStreamValue(c *Connection, account, key, value, currency string) {
	c.processMessageAtEpoch([]byte(strings.Join([]string{"6", "2", key, value, currency, account, ""}, "\x00")), c.BrokerSessionEpoch())
}

func TestPassiveStreamObservationPreservesScopeAndReceiptClocks(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	feedStreamValue(c.conn, account, "CashBalance", "0", "USD")
	feedStreamValue(c.conn, account, "SettledCash", "1122334455", "EUR")
	feedStreamValue(c.conn, account, "$LEDGER-SettledCash", "0", "USD")
	feedStreamValue(c.conn, account, "AccountReady", "true", "")
	feedStreamValue(c.conn, account, "TradingType-S", "CASH", "")
	got := c.CaptureAccountStreamObservationForSession(binding, account)
	if got == nil || got.Status != "initial_pending" || got.AccountReady != "ready" || got.TradingType != "cash" || !got.TradingTypeObserved || len(got.Rows) != 3 {
		t.Fatalf("receipt: %+v", got)
	}
	before := got.LastCallbackAt
	feedStreamValue(c.conn, "U_FOREIGN", "$LEDGER-SettledCash", "987654321", "USD")
	feedStreamValue(c.conn, "All", "$LEDGER-SettledCash", "987654321", "USD")
	c.conn.processMessageAtEpoch([]byte("8\x001\x0012:00\x00"), binding.epoch)
	second := c.CaptureAccountStreamObservationForSession(binding, account)
	if !second.LastCallbackAt.Equal(before) || len(second.Rows) != 3 || second.Rows[0].Callbacks != 1 {
		t.Fatal("foreign row/heartbeat refreshed a cash receipt")
	}
	c.conn.processMessageAtEpoch([]byte("54\x001\x00"+account+"\x00"), binding.epoch)
	complete := c.CaptureAccountStreamObservationForSession(binding, account)
	if complete.Status != "initial_complete" || complete.DownloadEndAt.IsZero() || complete.CompletedAt.IsZero() {
		t.Fatalf("completion: %+v", complete)
	}
	for _, row := range complete.Rows {
		if row.Key == "$LEDGER-SettledCash" && (row.Source != "broker_ledger_label" || row.ValueStatus != "observed") {
			t.Fatal("real zero or prefix lost")
		}
		if row.Key == "SettledCash" && row.Source != "account_total" {
			t.Fatal("bare aggregate became currency cash")
		}
	}
	got.Rows[0].Key = "changed"
	if second.Rows[0].Key == "changed" {
		t.Fatal("detached receipts alias")
	}
	if c.CaptureAccountStreamObservationForSession(binding, "U_OTHER").Status != "scope_or_generation_changed" {
		t.Fatal("foreign selected scope admitted")
	}
}

func TestPassiveStreamObservationEpochResetAndInvalidValues(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	for i, value := range []string{"NaN", "+Inf", "not-money", fmt.Sprint(math.MaxFloat64), "0"} {
		feedStreamValue(c.conn, account, "CashBalance", value, fmt.Sprintf("%cAA", 'A'+i))
	}
	feedStreamValue(c.conn, account, "$LEDGER:SettledCash", "secret", "USD")
	feedStreamValue(c.conn, account, "SettledCash_USD", "secret", "USD")
	feedStreamValue(c.conn, account, "CashBalance", "1", "private-currency")
	feedStreamValue(c.conn, account, "AccountReady", "false", "")
	feedStreamValue(c.conn, account, "TradingType-S", "INDIVIDUAL", "")
	got := c.CaptureAccountStreamObservationForSession(binding, account)
	if got.Status != "account_not_ready" || got.TradingType != "unknown" || !got.TradingTypeObserved || len(got.Rows) != 6 {
		t.Fatalf("invalid observations: %+v", got)
	}
	statuses := map[string]string{}
	for _, row := range got.Rows {
		statuses[row.Currency] = row.ValueStatus
		if strings.Contains(row.Key, "secret") || strings.Contains(row.Currency, "private") {
			t.Fatal("untrusted metadata escaped allowlist")
		}
	}
	if statuses["AAA"] != "invalid" || statuses["DAA"] != "unset" || statuses["EAA"] != "observed" {
		t.Fatal("invalid/unset/zero collapsed")
	}
	c.conn.resetOrderIDReadiness()
	if c.CaptureAccountStreamObservationForSession(binding, account) != nil {
		t.Fatal("retired original binding survived")
	}
	newBinding, _ := c.CaptureSession()
	if got := c.CaptureAccountStreamObservationForSession(newBinding, account); got == nil || got.Status != "no_subscription" || len(got.Rows) != 0 || got.TradingTypeObserved {
		t.Fatal("prior receipt crossed socket reset")
	}
	// A frame from the old reader cannot repopulate the passive cache.
	c.conn.processMessageAtEpoch([]byte("6\x002\x00CashBalance\x009\x00USD\x00"+account+"\x00"), binding.epoch)
	if got := c.CaptureAccountStreamObservationForSession(newBinding, account); len(got.Rows) != 0 {
		t.Fatal("retired callback repopulated the stream receipt")
	}
}

func TestPassiveStreamObservationSubscriptionResetAndBoundedRows(t *testing.T) {
	c, binding, account := streamObservationFixture(t)
	for i := range 200 {
		feedStreamValue(c.conn, account, "CashBalance", "0", fmt.Sprintf("%c%cA", 'A'+i/26, 'A'+i%26))
	}
	got := c.CaptureAccountStreamObservationForSession(binding, account)
	if len(got.Rows) != 128 || !got.RowsTruncated {
		t.Fatal("provider could grow diagnostic without bound")
	}
	c.conn.resetPortfolioStreamHealth(account, time.Now().UTC())
	got = c.CaptureAccountStreamObservationForSession(binding, account)
	if got.Status != "initial_pending" || len(got.Rows) != 0 || got.RowsTruncated || got.AccountReady != "unknown" || got.TradingTypeObserved {
		t.Fatal("old subscription observations crossed generation reset")
	}
	for _, value := range []string{"MARGIN", "RegT", "Portfolio", "REG-T-MARGIN", "PORTFOLIO MARGIN"} {
		if classifyStreamTradingType(value) != "margin" {
			t.Fatal("explicit margin label not classified")
		}
	}
	for _, value := range []string{"INDIVIDUAL", "STKCASH", "STKMRGN", "PMRGN", "GPMRGN", "IRAMRGN", "IRA", "RRSP", "Commodity"} {
		if classifyStreamTradingType(value) != "unknown" {
			t.Fatalf("unverified internal or ownership label %q acquired a trading regime", value)
		}
	}
}
