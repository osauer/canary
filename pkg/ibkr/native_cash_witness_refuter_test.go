package ibkr

import (
	"context"
	"math"
	"strconv"
	"testing"
	"time"
)

// Account=All is native authority only with a proven single-account login.
func TestNativeCashRefuterUnknownManagedInventoryCannotCertifyAggregateLedger(t *testing.T) {
	for _, managed := range [][]string{nil, {}, {"DU2222222"}, {"DU1234567", "DU2222222"}} {
		if got := accountSummaryRequestRowDisposition("All", "$LEDGER-SettledCash", "EUR", "DU1234567", managed); got != accountSummaryRowIgnore {
			t.Fatalf("unknown or wrong managed inventory certified All native settlement: disposition=%v", got)
		}
	}

	if got := accountSummaryRequestRowDisposition("All", "$LEDGER-SettledCash", "EUR", "DU1234567", []string{"DU1234567"}); got != accountSummaryRowAcceptLedger {
		t.Fatal("known matching singleton lost broker native ledger", got)
	}
	if got := accountSummaryRequestRowDisposition("All", "SettledCash", "EUR", "DU1234567", []string{"DU1234567"}); got != accountSummaryRowIgnore {
		t.Fatal("bare aggregate became native despite matching singleton", got)
	}
}

// Exercise callback assembly and the completed-request parser, including the
// unset sentinel alongside real cash. An account total cannot fill the gap.
func TestNativeCashRefuterWireUnsetCannotReleaseRealCurrencyCash(t *testing.T) {
	for _, unavailable := range []string{strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64), "NaN", "+Inf", "-Inf"} {
		t.Run(unavailable, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			type result struct {
				raw *RawAccountSummary
				err error
			}
			done := make(chan result, 1)
			go func() {
				raw, _, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
				done <- result{raw, err}
			}()
			<-wire.wrote
			for _, row := range [][]string{
				{"DU1234567", "NetLiquidation", "100000", "EUR"},
				{"All", "$LEDGER-CashBalance", "16000", "EUR"},
				{"DU1234567", "SettledCash", "41000", "EUR"},
				{"All", "SettledCash", "49000", "EUR"},
				{"All", "$LEDGER-SettledCash", unavailable, "EUR"},
			} {
				conn.handleAccountSummary(append([]string{"63", "2", "1"}, row...))
			}
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))
			got := <-done
			if got.err != nil || got.raw == nil {
				t.Fatal("fixture completion failed", got.err)
			}
			row := got.raw.CurrencyLedger["EUR"]
			if row.CashBalance != 16000 || row.SettledCashObserved {
				t.Fatalf("unset/aggregate settlement could fund native cash: %+v", row)
			}
			if got.raw.SettlementObservation == nil || got.raw.SettlementObservation.Callbacks != 3 {
				t.Fatal("sanitized diagnostic lost callback witness")
			}
		})
	}
}

func TestNativeCashRefuterUnsupportedWireTagsCannotForgeNativeStorageKeys(t *testing.T) {
	for _, candidate := range []struct {
		tag, currency string
		cashForgery   bool
	}{
		{"$LEDGER:SettledCash", "EUR", false},
		{"$LEDGER-SettledCash_EUR", "", false},
		{"$LEDGER-SettledCash_EUR", "BASE", false},
		{"CashBalance_USD", "", true},
		{"CashBalance_USD", "BASE", true},
	} {
		t.Run(candidate.tag+"/"+candidate.currency, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			type result struct {
				raw *RawAccountSummary
				err error
			}
			done := make(chan result, 1)
			go func() {
				raw, _, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
				done <- result{raw, err}
			}()
			<-wire.wrote
			rows := [][]string{{"DU1234567", "NetLiquidation", "100000", "EUR"}}
			// Avoid the internal ledger namespace for the legacy CashBalance forgery.
			if !candidate.cashForgery {
				rows = append(rows, []string{"DU1234567", "CashBalance", "16000", "EUR"})
			}
			rows = append(rows, []string{"DU1234567", candidate.tag, "16000", candidate.currency})
			for _, row := range rows {
				conn.handleAccountSummary(append([]string{"63", "2", "1"}, row...))
			}
			conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))
			got := <-done
			if got.err != nil || got.raw == nil {
				t.Fatal("fixture completion failed", got.err)
			}
			for currency, row := range got.raw.CurrencyLedger {
				if row.SettledCashObserved || (candidate.cashForgery && currency == "USD" && row.CashBalance != 0) {
					t.Fatalf("unsupported raw wire tag forged native field authority: %+v", row)
				}
			}
		})
	}
}

// End is the observation cutoff, even if the waiter has not yet been scheduled.
func TestNativeCashRefuterRequestEndFreezesReceiptAndNativeRows(t *testing.T) {
	_, conn, _ := settlementConnectorFixture(t)
	conn.registerSummarySnapshot(42, "DU1234567")
	for _, row := range [][]string{
		{"DU1234567", "NetLiquidation", "100000", "EUR"},
		{"All", "$LEDGER-CashBalance", "16000", "EUR"},
		{"All", "$LEDGER-SettledCash", "0", "EUR"},
	} {
		conn.handleAccountSummary(append([]string{"63", "2", "42"}, row...))
	}
	conn.signalSummaryEnd([]string{"64", "1", "42"})
	// IBKR subscriptions can keep emitting after End until cancellation arrives.
	conn.handleAccountSummary([]string{"63", "2", "42", "All", "$LEDGER-SettledCash", "16000", "EUR"})
	raw, observation, err := conn.awaitAccountSummarySnapshot(42, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ledger := extractCurrencyLedger(raw)
	if observation.Callbacks != 1 || len(observation.Rows) != 1 || ledger["EUR"].SettledCash != 0 {
		t.Fatalf("post-End callback rewrote completed request receipt/native snapshot: callbacks=%d settled=%v", observation.Callbacks, ledger["EUR"].SettledCash)
	}
}
