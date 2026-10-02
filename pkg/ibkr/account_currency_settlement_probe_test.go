package ibkr

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func startCurrencyProbe(c *Connector, ctx context.Context, budget time.Duration) <-chan AccountCurrencySettlementProbe {
	done := make(chan AccountCurrencySettlementProbe, 1)
	go func() { done <- c.RequestCurrencySettlementProbe(ctx, budget) }()
	return done
}
func currencyCallback(conn *Connection, id int, account, model, key, value, currency string) {
	conn.processMessage(conn.encodeMsg(msgAccountUpdateMulti, 1, id, account, model, key, value, currency))
}
func currencyEnd(conn *Connection, id int) {
	conn.processMessage(conn.encodeMsg(msgAccountUpdateMultiEnd, 1, id))
}
func TestCurrencyProbeCompletedReceiptAndIsolation(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(strconv.FormatBool(populated), func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			probeCallback(conn, 900, "DU1234567", "NetLiquidation", "45678901", "EUR")
			before := conn.GetAccountSummary()
			done := startCurrencyProbe(c, t.Context(), time.Second)
			frames := probeFrames(t, wire, conn, 1)
			if !reflect.DeepEqual(frames[0], []string{"76", "1", "1", "DU1234567", "", "1", ""}) {
				t.Fatalf("official Multi request shape %v", frames)
			}
			id, _ := strconv.Atoi(frames[0][2])
			if populated {
				currencyCallback(conn, id, "DU1234567", "", "SettledCash", "0", "USD")
				currencyCallback(conn, id, "DU1234567", "", "CashBalance", "7654321", "EUR")
				currencyCallback(conn, id, "DU1234567", "", "SettledCash", "1.7976931348623157e+308", "EUR")
				currencyCallback(conn, id, "DU1234567", "", "TotalCashBalance", "NaN", "GBP")
				currencyCallback(conn, id, "DU1234567", "", "SettledCash", "9999999", "BASE")
				currencyCallback(conn, id, "DU1234567", "", "SettledCash", "NaN", "")
				currencyCallback(conn, id, "DU1234567", "", "SettledCash", "0", "PRIVATE-USD")
				currencyCallback(conn, id, "DU1234567", "", "NetLiquidation", "9876543", "EUR")
			}
			select {
			case <-done:
				t.Fatal("completed before initial End")
			default:
			}
			currencyEnd(conn, id)
			got := <-done
			want := "completed_empty"
			if populated {
				want = "completed"
			}
			if got.Status != want || got.CancelStatus != "sent" || got.SocketEpoch != conn.BrokerSessionEpoch() || got.CompletedAt.IsZero() || got.ReadAt.Before(got.CompletedAt) {
				t.Fatalf("result %+v", got)
			}
			if populated {
				if got.Callbacks != 8 || len(got.Rows) != 7 {
					t.Fatalf("metadata callback coverage %+v", got)
				}
				states := map[string]string{}
				for _, r := range got.Rows {
					states[r.Key+"/"+r.Currency+"/"+r.Source] = r.ValueStatus
				}
				for key, want := range map[string]string{"SettledCash/USD/currency_only_multi": "observed", "SettledCash/EUR/currency_only_multi": "unset", "TotalCashBalance/GBP/currency_only_multi": "invalid", "SettledCash/BASE/base_currency_total": "observed", "SettledCash//unlabelled": "invalid", "SettledCash//invalid_currency": "observed"} {
					if states[key] != want {
						t.Fatalf("%s => %s want%s", key, states[key], want)
					}
				}
			}
			raw, _ := json.Marshal(got)
			for _, secret := range []string{"DU1234567", "7654321", "9999999", "9876543", "PRIVATE-USD", "NetLiquidation", "45678901"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("private value retained: %s", raw)
				}
			}
			frames = probeFrames(t, wire, conn, 2)
			if !reflect.DeepEqual(frames[1], []string{"77", "1", strconv.Itoa(id), ""}) {
				t.Fatalf("cancel shape %v", frames)
			}
			currencyCallback(conn, id, "DU1234567", "", "CashBalance", "9876543", "GBP")
			currencyEnd(conn, id)
			currencyCallback(conn, id+999, "DU1234567", "", "NetLiquidation", "9876543", "EUR")
			if !reflect.DeepEqual(before, conn.GetAccountSummary()) {
				t.Fatal("Multi callback mutated financial cache")
			}
			conn.accountMu.RLock()
			n := len(conn.currencyProbes)
			conn.accountMu.RUnlock()
			if n != 0 {
				t.Fatal("accumulator retained")
			}
		})
	}
}
func TestCurrencyProbeScopeModelAndTerminalBoundaries(t *testing.T) {
	for _, mode := range []string{"foreign", "model", "malformed", "timeout", "cancel", "scope_changed", "epoch", "error"} {
		t.Run(mode, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := startCurrencyProbe(c, ctx, 50*time.Millisecond)
			frames := probeFrames(t, wire, conn, 1)
			id, _ := strconv.Atoi(frames[0][2])
			currencyCallback(conn, id, "DU1234567", "", "SettledCash", "12345678", "EUR")
			want := mode
			switch mode {
			case "foreign":
				currencyCallback(conn, id, "DU9999999", "", "SettledCash", "98765432", "EUR")
				want = "scope_conflict"
			case "model":
				currencyCallback(conn, id, "DU1234567", "MODEL", "SettledCash", "98765432", "EUR")
				want = "scope_conflict"
			case "malformed":
				conn.processMessage(conn.encodeMsg(msgAccountUpdateMulti, 1, id, "DU1234567"))
				want = "invalid_callback"
			case "timeout":
				want = "timeout"
			case "cancel":
				cancel()
				want = "cancelled"
			case "scope_changed":
				conn.accountMu.Lock()
				conn.account = "DU9999999"
				conn.managedAccounts = []string{"DU9999999"}
				conn.accountMu.Unlock()
				currencyEnd(conn, id)
				want = "scope_conflict"
			case "epoch":
				conn.resetOrderIDReadiness()
				currencyEnd(conn, id)
				want = "session_changed"
			case "error":
				conn.processMessage(conn.encodeMsg(msgErrMsg, 2, id, 322, "private DU1234567 money98765432"))
				want = "broker_error"
			}
			got := <-done
			if got.Status != want || len(got.Rows) != 0 {
				t.Fatalf("%s => %+v", mode, got)
			}
			if mode == "epoch" {
				if got.CancelStatus != "session_changed" || len(wire.frames(conn)) != 1 {
					t.Fatal("cancel reached successor", got)
				}
			} else {
				if got.CancelStatus != "sent" || len(wire.frames(conn)) != 2 {
					t.Fatal("original subscription not cancelled", got)
				}
			}
		})
	}
}
func TestCurrencyProbeOrdinaryPriorityAndUncertainCancel(t *testing.T) {
	t.Run("priority", func(t *testing.T) {
		c, conn, wire := settlementConnectorFixture(t)
		done := startCurrencyProbe(c, t.Context(), time.Second)
		probeFrames(t, wire, conn, 1)
		ordinary := make(chan error, 1)
		go func() { ordinary <- conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567") }()
		frames := probeFrames(t, wire, conn, 3)
		if frames[1][0] != "77" || frames[2][0] != "62" {
			t.Fatalf("ordinary overtook cancellation %v", frames)
		}
		if got := <-done; got.Status != "yielded" || got.CancelStatus != "sent" || len(got.Rows) > 0 {
			t.Fatal(got)
		}
		if err := <-ordinary; err != nil {
			t.Fatal(err)
		}
		if got := c.RequestCurrencySettlementProbe(t.Context(), time.Second); got.Status != "skipped_busy" {
			t.Fatal("ordinary subscription ignored", got)
		}
		if err := conn.CancelAccountSummary(100); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("uncertainty", func(t *testing.T) {
		c, conn, wire := settlementConnectorFixture(t)
		conn.writer = bufio.NewWriter(&settlementCancelFailWriter{wire: wire})
		done := startCurrencyProbe(c, t.Context(), time.Second)
		frames := probeFrames(t, wire, conn, 1)
		id, _ := strconv.Atoi(frames[0][2])
		currencyEnd(conn, id)
		got := <-done
		if got.CancelStatus != "failed" || len(got.Rows) != 0 {
			t.Fatal(got)
		}
		if got := c.RequestCurrencySettlementProbe(t.Context(), time.Second); got.Status != "skipped_busy" {
			t.Fatal("uncertain Multi admitted next Multi", got)
		}
		if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "skipped_busy" {
			t.Fatal("uncertain Multi admitted summary diagnostic", got)
		}
		conn.transportMu.Lock()
		conn.writer = bufio.NewWriter(wire)
		conn.transportMu.Unlock()
		// Multi has its own broker service: both ordinary summary slots remain free.
		for _, id := range []int{100, 101} {
			if err := conn.RequestAccountSummaryForAccount(id, "NetLiquidation", "DU1234567"); err != nil {
				t.Fatal("Multi uncertainty consumed summary capacity", err)
			}
		}
		for _, id := range []int{100, 101} {
			if err := conn.CancelAccountSummary(id); err != nil {
				t.Fatal(err)
			}
		}
		conn.resetOrderIDReadiness()
		setServerVersionReady(conn, minServerVersionRequired)
		conn.processMessage(conn.encodeMsg(msgManagedAccts, 1, "DU1234567"))
		done = startCurrencyProbe(c, t.Context(), time.Second)
		frames = probeFrames(t, wire, conn, 6)
		id, _ = strconv.Atoi(frames[5][2])
		currencyEnd(conn, id)
		if got := <-done; got.Status != "completed_empty" || got.CancelStatus != "sent" {
			t.Fatal("epoch did not clear diagnostic uncertainty", got)
		}
	})
}
func TestCurrencyProbeLimitsRawCaptureAndFrozenEnd(t *testing.T) {
	for _, mode := range []string{"wire_hex", "wire_tap", "packet_path", "protocol", "quarantine"} {
		t.Run(mode, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			want := "skipped_raw_capture"
			switch mode {
			case "wire_hex":
				conn.logWireHex = true
			case "wire_tap":
				conn.wireTap = &WireInterceptor{enabled: true}
			case "packet_path":
				conn.config.PacketLogPath = "synthetic-private-capture"
			case "protocol":
				conn.serverVersion = 207
				want = "unsupported_protocol"
			case "quarantine":
				conn.retiredCurrencyProbes = map[int]uint64{}
				for i := range 256 {
					conn.retiredCurrencyProbes[i] = conn.BrokerSessionEpoch()
				}
				want = "skipped_quarantine_limit"
			}
			if got := c.RequestCurrencySettlementProbe(t.Context(), time.Second); got.Status != want || len(wire.frames(conn)) != 0 {
				t.Fatal("unsafe preflight sent", got)
			}
		})
	}
	c, conn, wire := settlementConnectorFixture(t)
	done := startCurrencyProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	for i := range 150 {
		ccy := string([]byte{'A' + byte(i/26/26), 'A' + byte(i/26%26), 'A' + byte(i%26)})
		currencyCallback(conn, id, "DU1234567", "", "SettledCash", strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64), ccy)
	}
	currencyEnd(conn, id)
	currencyCallback(conn, id, "DU1234567", "", "SettledCash", "0", "ZZZ")
	got := <-done
	if len(got.Rows) != 128 || !got.RowsTruncated || got.Callbacks != 150 {
		t.Fatalf("bounds or frozen End %+v", got)
	}
}
func TestCurrencyProbeBoundedQueuedSend(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.transportMu.Lock()
	done := startCurrencyProbe(c, t.Context(), 20*time.Millisecond)
	var got AccountCurrencySettlementProbe
	select {
	case got = <-done:
	case <-time.After(1300 * time.Millisecond):
		conn.transportMu.Unlock()
		t.Fatal("ignored bounded send/cancel budget")
	}
	conn.transportMu.Unlock()
	if got.Status != "timeout" || len(got.Rows) > 0 {
		t.Fatal(got)
	}
	time.Sleep(20 * time.Millisecond)
	if len(wire.frames(conn)) != 0 {
		t.Fatal("expired request escaped queue")
	}
}
