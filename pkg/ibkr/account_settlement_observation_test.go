package ibkr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type settlementWire struct {
	mu       sync.Mutex
	messages [][]byte
	wrote    chan struct{}
	once     sync.Once
}

func (w *settlementWire) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.messages = append(w.messages, append([]byte(nil), p...))
	w.mu.Unlock()
	w.once.Do(func() { close(w.wrote) })
	return len(p), nil
}
func (w *settlementWire) frames(c *Connection) [][]string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([][]string, 0, len(w.messages))
	for _, p := range w.messages {
		out = append(out, c.decodeOutboundMessage(p[4:]))
	}
	return out
}

func settlementConnectorFixture(t *testing.T) (*Connector, *Connection, *settlementWire) {
	t.Helper()
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU1234567"}
	c := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVersionRequired)
	wire := &settlementWire{wrote: make(chan struct{})}
	conn.writer = bufio.NewWriter(wire)
	c.running = true
	c.ready = true
	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", cfg.Account))
	return c, conn, wire
}

func TestAccountSettlementCompletedRequestAndCancellation(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	done := make(chan *RawAccountSummary, 1)
	failures := make(chan error, 1)
	go func() {
		raw, _, err := c.RequestAccountSummaryWithProvenance(context.Background(), time.Second)
		failures <- err
		done <- raw
	}()
	<-wire.wrote
	frames := wire.frames(conn)
	if len(frames) != 1 || !strings.Contains(strings.Join(frames[0], ","), ",SettledCash,") || !strings.Contains(strings.Join(frames[0], ","), "$LEDGER:ALL") {
		t.Fatalf("missing explicit settled-cash/ledger request: %v", frames)
	}
	for _, row := range [][]string{
		{"DU1234567", "NetLiquidation", "100000", "EUR"},
		{"DU1234567", "SettledCash", "8000", "EUR"},
		{"All", "SettledCash", "50000", "EUR"},
		{"All", "$LEDGER-CashBalance", "1000", "EUR"},
		{"All", "$LEDGER-CashBalance", "1000", "USD"},
		{"All", "$LEDGER-SettledCash", "0", "USD"},
		{"All", "$LEDGER-SettledCash", "NaN", "GBP"},
	} {
		conn.handleAccountSummary(append([]string{"63", "2", "1"}, row...))
	}
	select {
	case <-done:
		t.Fatal("published receipt before request end")
	default:
	}
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))
	if err := <-failures; err != nil {
		t.Fatal(err)
	}
	raw := <-done
	observation := raw.SettlementObservation
	if observation == nil || observation.Callbacks != 4 || len(observation.Rows) != 4 || observation.AsOf.IsZero() {
		t.Fatalf("receipt=%+v", observation)
	}
	if observation.Rows[0].Source != "account_total" || observation.Rows[1].Source != "aggregate_ambiguous" || observation.Rows[2].Source != "broker_ledger_label" || !observation.Rows[2].Finite || observation.Rows[3].Finite {
		t.Fatalf("labels/validity=%+v", observation.Rows)
	}
	if raw.CurrencyLedger["EUR"].SettledCashObserved {
		t.Fatal("bare account/All total authorized EUR")
	}
	if row := raw.CurrencyLedger["USD"]; !row.SettledCashObserved || row.SettledCash != 0 {
		t.Fatalf("genuine ledger zero lost: %+v", row)
	}
	if raw.CurrencyLedger["GBP"].SettledCashObserved {
		t.Fatal("non-finite callback authorized GBP")
	}
	frames = wire.frames(conn)
	if len(frames) != 2 || frames[1][0] != "63" || frames[1][2] != "1" {
		t.Fatalf("did not cancel only own subscription: %v", frames)
	}
	encoded, _ := json.Marshal(observation)
	for _, private := range []string{"DU1234567", "8000", "50000"} {
		if strings.Contains(string(encoded), private) {
			t.Fatalf("private data in receipt: %s", encoded)
		}
	}
}

func TestAccountSettlementNoReceiptOnIncompleteOrChangedSession(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "scope_conflict", "reconnect"} {
		t.Run(mode, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				raw *RawAccountSummary
				err error
			}
			done := make(chan result, 1)
			go func() {
				raw, _, err := c.RequestAccountSummaryWithProvenance(ctx, 50*time.Millisecond)
				done <- result{raw, err}
			}()
			<-wire.wrote
			conn.handleAccountSummary([]string{"63", "2", "1", "DU1234567", "NetLiquidation", "100000", "EUR"})
			conn.handleAccountSummary([]string{"63", "2", "1", "All", "SettledCash", "8000", "EUR"})
			switch mode {
			case "cancel":
				cancel()
			case "scope_conflict":
				conn.handleAccountSummary([]string{"63", "2", "1", "DU9999999", "SettledCash", "10000", "EUR"})
				conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))
			case "reconnect":
				conn.resetOrderIDReadiness()
				conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", 1))
			}
			got := <-done
			if got.raw != nil && got.raw.SettlementObservation != nil {
				t.Fatalf("%s published completed/current receipt", mode)
			}
			if mode != "reconnect" && got.err == nil {
				t.Fatalf("%s accepted incomplete/conflicting request", mode)
			}
			if mode == "cancel" && !errors.Is(got.err, context.Canceled) {
				t.Fatal(got.err)
			}
			frames := wire.frames(conn)
			if mode == "reconnect" {
				if len(frames) != 1 {
					t.Fatalf("old epoch cancelled successor request: %v", frames)
				}
			} else if len(frames) != 2 || frames[1][0] != "63" {
				t.Fatalf("%s missing own cancellation: %v", mode, frames)
			}
		})
	}
}

func TestAccountSettlementRowScopeAndBound(t *testing.T) {
	var observation AccountSettlementObservation
	for _, row := range []struct {
		account, tag, ccy string
		managed           []string
	}{
		{"DU2222222", "SettledCash", "EUR", []string{"DU1234567", "DU2222222"}},
		{"DU9999999", "$LEDGER-SettledCash", "EUR", []string{"DU1234567"}},
	} {
		observeAccountSettlementRow(&observation, row.account, row.tag, "1", row.ccy, "DU1234567", row.managed)
	}
	if observation.Callbacks != 0 {
		t.Fatal("foreign receipt retained")
	}
	observeAccountSettlementRow(&observation, "All", "SettledCash", "1", "BASE", "DU1234567", []string{"DU1234567"})
	if observation.Callbacks != 1 || observation.Rows[0].Source != "aggregate_ambiguous" || observation.Rows[0].Currency != "BASE" {
		t.Fatal("BASE aggregate omitted from diagnostic")
	}
	observeAccountSettlementRow(&observation, "DU1234567", "$LEDGER-SettledCash", "1", "not-a-currency", "DU1234567", []string{"DU1234567"})
	if observation.Rows[1].Source != "invalid_currency" || observation.Rows[1].Currency != "" {
		t.Fatal("unvalidated broker text echoed")
	}
	observation = AccountSettlementObservation{}
	for range 200 {
		observeAccountSettlementRow(&observation, "All", "SettledCash", "1", "EUR", "DU1234567", []string{"DU1234567"})
	}
	if observation.Callbacks != 200 || len(observation.Rows) != 128 {
		t.Fatalf("unbounded receipt: %+v", observation)
	}
	for _, tag := range []string{"SettledCash", "$LEDGER-SettledCash"} {
		got := accountSummaryRequestRowDisposition("All", tag, "EUR", "DU1234567", []string{"DU1234567", "DU2222222"})
		if got != accountSummaryRowIgnore {
			t.Fatal("multi-account aggregate became native")
		}
	}
}

func TestAccountSettlementUnsetIsUnavailableWithGenuineCash(t *testing.T) {
	for _, prefix := range []string{"$LEDGER:", "$LEDGER-"} {
		for _, value := range []string{strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64), "NaN", "+Inf", "-Inf", "bad"} {
			raw := map[string]string{prefix + "CashBalance_EUR": "12000", prefix + "SettledCash_EUR": value}
			ledger := extractCurrencyLedger(raw)
			if row := ledger["EUR"]; row.CashBalance != 12000 || row.SettledCashObserved {
				t.Fatalf("%s %s settled unavailable treated as observed: %+v", prefix, value, row)
			}
		}
		ledger := extractCurrencyLedger(map[string]string{prefix + "CashBalance_EUR": "12000", prefix + "SettledCash_EUR": "0"})
		if row := ledger["EUR"]; !row.SettledCashObserved || row.SettledCash != 0 {
			t.Fatalf("%s genuine zero unavailable: %+v", prefix, row)
		}
	}
	var observation AccountSettlementObservation
	observeAccountSettlementRow(&observation, "DU1234567", "$LEDGER-SettledCash", strconv.FormatFloat(math.MaxFloat64, 'g', -1, 64), "EUR", "DU1234567", []string{"DU1234567"})
	if len(observation.Rows) != 1 || !observation.Rows[0].Finite {
		t.Fatal("finite flag must retain literal mathematical meaning, separate from availability")
	}
}
