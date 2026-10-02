package ibkr

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func probeFrames(t *testing.T, wire *settlementWire, conn *Connection, n int) [][]string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		frames := wire.frames(conn)
		if len(frames) >= n {
			return frames
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("wanted %d wire frames, got %v", n, wire.frames(conn))
	return nil
}
func startSettlementProbe(c *Connector, ctx context.Context, budget time.Duration) <-chan AccountSettlementProbe {
	done := make(chan AccountSettlementProbe, 1)
	go func() { done <- c.RequestSettlementCashProbe(ctx, budget) }()
	return done
}
func probeCallback(conn *Connection, id int, account, tag, value, currency string) {
	conn.handleAccountSummary([]string{"63", "2", strconv.Itoa(id), account, tag, value, currency})
}
func probeEnd(conn *Connection, id int) {
	conn.processMessage(conn.encodeMsg(msgAccountSummaryEnd, "1", id))
}

func TestSettlementProbeCompletedEmptyAndReceiptOnly(t *testing.T) {
	for _, populated := range []bool{false, true} {
		t.Run(strconv.FormatBool(populated), func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			probeCallback(conn, 800, "DU1234567", "CashBalance", "7654321", "EUR")
			before := conn.GetAccountSummary()
			done := startSettlementProbe(c, t.Context(), time.Second)
			frames := probeFrames(t, wire, conn, 1)
			if len(frames[0]) != 6 || frames[0][0] != "62" || frames[0][3] != "All" || frames[0][4] != "SettledCash" {
				t.Fatalf("unexpected probe request: %v", frames)
			}
			id, _ := strconv.Atoi(frames[0][2])
			if populated {
				probeCallback(conn, id, "DU1234567", "SettledCash", "9876543", "EUR")
				probeCallback(conn, id, "All", "SettledCash", "8765432", "EUR")
				probeCallback(conn, id, "All", "$LEDGER-SettledCash", "0", "USD")
				probeCallback(conn, id, "DU1234567", "NetLiquidation", "9999999", "EUR")
			}
			conn.accountMu.RLock()
			if snap := conn.summarySnapshots[id]; snap == nil || snap.values != nil {
				t.Fatal("diagnostic accumulated financial values")
			}
			conn.accountMu.RUnlock()
			select {
			case <-done:
				t.Fatal("receipt before End")
			default:
			}
			probeEnd(conn, id)
			got := <-done
			want := "completed_empty"
			count := 0
			if populated {
				want = "completed"
				count = 3
			}
			if got.Status != want || got.CancelStatus != "sent" || got.Observation == nil || got.Observation.Callbacks != count || got.Observation.AsOf.IsZero() {
				t.Fatalf("result=%+v", got)
			}
			if !reflect.DeepEqual(before, conn.GetAccountSummary()) {
				t.Fatal("diagnostic changed account cache")
			}
			raw, _ := json.Marshal(got)
			for _, secret := range []string{"DU1234567", "9876543", "8765432", "9999999", "NetLiquidation", "CashBalance"} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("private or financial field in receipt: %s", raw)
				}
			}
			frames = probeFrames(t, wire, conn, 2)
			if frames[1][0] != "63" || frames[1][2] != strconv.Itoa(id) {
				t.Fatal("wrong request cancelled", frames)
			}
			// Every late tag for the retired probe stays quarantined, including
			// otherwise valid account rows, forged namespaces and malformed currencies.
			for _, tag := range []string{"NetLiquidation", "CashBalance", "SettledCash", "$LEDGER:CashBalance", "CashBalance_USD"} {
				probeCallback(conn, id, "DU1234567", tag, "9999999", "EUR")
			}
			probeEnd(conn, id)
			if !reflect.DeepEqual(before, conn.GetAccountSummary()) {
				t.Fatal("retired probe seeded ordinary fallback cache")
			}
		})
	}
}

func TestSettlementProbeIncompleteAndCrossScope(t *testing.T) {
	for _, mode := range []string{"timeout", "cancel", "foreign", "changed_account", "epoch", "broker_error"} {
		t.Run(mode, func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := startSettlementProbe(c, ctx, 40*time.Millisecond)
			frames := probeFrames(t, wire, conn, 1)
			id, _ := strconv.Atoi(frames[0][2])
			probeCallback(conn, id, "DU1234567", "SettledCash", "12345678", "EUR")
			want := mode
			switch mode {
			case "timeout":
				want = "timeout"
			case "cancel":
				cancel()
				want = "cancelled"
			case "foreign":
				probeCallback(conn, id, "DU9999999", "SettledCash", "98765432", "EUR")
				probeEnd(conn, id)
				want = "scope_conflict"
			case "changed_account":
				conn.accountMu.Lock()
				conn.account = "DU9999999"
				conn.managedAccounts = []string{"DU9999999"}
				conn.accountMu.Unlock()
				probeEnd(conn, id)
				want = "scope_conflict"
			case "epoch":
				conn.resetOrderIDReadiness()
				probeEnd(conn, id)
				want = "session_changed"
			case "broker_error":
				conn.processMessage(conn.encodeMsg(msgErrMsg, "2", id, 322, "DU1234567 private money 98765432"))
				want = "broker_error"
			}
			got := <-done
			if got.Status != want || got.Observation != nil {
				t.Fatalf("%s => %+v", mode, got)
			}
			if mode == "broker_error" && got.BrokerErrorCode != 322 {
				t.Fatal("broker error not distinguished")
			}
			frames = wire.frames(conn)
			if mode == "epoch" {
				if len(frames) != 1 || got.CancelStatus != "session_changed" {
					t.Fatal("old epoch cancelled successor", frames, got)
				}
			} else if len(frames) != 2 || got.CancelStatus != "sent" {
				t.Fatal("subscription retained", frames, got)
			}
			conn.accountMu.RLock()
			count := len(conn.summarySnapshots)
			conn.accountMu.RUnlock()
			if count != 0 {
				t.Fatal("diagnostic accumulator retained")
			}
		})
	}
}

func TestSettlementProbeSkipsEndedButUncancelledOrdinarySubscription(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	if err := conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567"); err != nil {
		t.Fatal(err)
	}
	probeEnd(conn, 100)
	if _, _, err := conn.awaitAccountSummarySnapshot(100, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "skipped_busy" || len(wire.frames(conn)) != 1 {
		t.Fatal("End was treated as subscription cancellation", got)
	}
	if err := conn.CancelAccountSummary(100); err != nil {
		t.Fatal(err)
	}
	done := startSettlementProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 3)
	id, _ := strconv.Atoi(frames[2][2])
	probeEnd(conn, id)
	if got := <-done; got.Status != "completed_empty" {
		t.Fatal(got)
	}
}

func TestSettlementProbeOrdinaryPriorityAndNormalIsolation(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(strconv.FormatBool(direct), func(t *testing.T) {
			c, conn, wire := settlementConnectorFixture(t)
			done := startSettlementProbe(c, t.Context(), time.Second)
			first := probeFrames(t, wire, conn, 1)
			probeID, _ := strconv.Atoi(first[0][2])
			normalDone := make(chan error, 1)
			go func() {
				if direct {
					normalDone <- conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567")
					return
				}
				raw, provenance, err := c.RequestAccountSummaryWithProvenance(t.Context(), time.Second)
				if err == nil && (raw.NetLiquidation == nil || *raw.NetLiquidation != 12345 || provenance != AccountSummaryProvenanceRequest) {
					err = ErrAccountSummaryScopeConflict
				}
				normalDone <- err
			}()
			frames := probeFrames(t, wire, conn, 3)
			if frames[1][0] != "63" || frames[1][2] != strconv.Itoa(probeID) || frames[2][0] != "62" {
				t.Fatal("normal request overtook probe cancellation", frames)
			}
			if got := <-done; got.Status != "yielded" || got.Observation != nil || got.CancelStatus != "sent" {
				t.Fatal("ordinary priority not applied", got)
			}
			normalID, _ := strconv.Atoi(frames[2][2])
			probeCallback(conn, probeID, "DU1234567", "NetLiquidation", "999999", "EUR")
			probeCallback(conn, normalID, "DU1234567", "NetLiquidation", "12345", "EUR")
			probeEnd(conn, normalID)
			if err := <-normalDone; err != nil {
				t.Fatal(err)
			}
			if direct {
				if err := conn.CancelAccountSummary(normalID); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSettlementProbeRequestEndFreezesReceipt(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	done := startSettlementProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	conn.accountMu.Lock()
	snap := conn.summarySnapshots[id]
	conn.accountMu.Unlock()
	probeCallback(conn, id, "DU1234567", "SettledCash", "1", "EUR")
	probeEnd(conn, id)
	probeCallback(conn, id, "DU1234567", "SettledCash", "2", "USD")
	got := <-done
	if got.Observation == nil || got.Observation.Callbacks != 1 || len(snap.settlementObservation.Rows) != 1 {
		t.Fatal("post-End callback rewrote receipt", got)
	}
}

// A failed cancel can occupy one broker slot. It never leaves an unbounded
// scheduler wait: one ordinary read remains admissible, its competitor holds.
type settlementCancelFailWriter struct {
	wire   *settlementWire
	writes int
}

func (w *settlementCancelFailWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes > 1 {
		return 0, errors.New("synthetic cancel transport failure")
	}
	return w.wire.Write(p)
}
func TestSettlementProbeFailedCancelPreservesBoundedOrdinaryCapacity(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.writer = bufio.NewWriter(&settlementCancelFailWriter{wire: wire})
	done := startSettlementProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 1)
	id, _ := strconv.Atoi(frames[0][2])
	probeEnd(conn, id)
	got := <-done
	if got.Status != "completed_empty" || got.CancelStatus != "failed" || got.Observation != nil {
		t.Fatal(got)
	}
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "skipped_busy" {
		t.Fatal("uncertain cancel admitted another probe", got)
	}
	conn.transportMu.Lock()
	conn.writer = bufio.NewWriter(wire)
	conn.transportMu.Unlock()
	if err := conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567"); err != nil {
		t.Fatal("ordinary capacity starved", err)
	}
	if err := conn.RequestAccountSummaryForAccount(101, "NetLiquidation", "DU1234567"); !errors.Is(err, ErrAccountSummaryProbeCancelUncertain) {
		t.Fatal("third potential broker subscription not held", err)
	}
	if err := conn.CancelAccountSummary(100); err != nil {
		t.Fatal(err)
	}
	if err := conn.RequestAccountSummaryForAccount(102, "NetLiquidation", "DU1234567"); err != nil {
		t.Fatal("ordinary capacity did not recover", err)
	}
}

func TestSettlementProbeSendBudgetAndLateDispatchQuarantine(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.transportMu.Lock()
	done := startSettlementProbe(c, t.Context(), 20*time.Millisecond)
	var got AccountSettlementProbe
	select {
	case got = <-done:
	case <-time.After(1300 * time.Millisecond):
		conn.transportMu.Unlock()
		t.Fatal("probe ignored bounded send budget")
	}
	conn.transportMu.Unlock()
	if got.Status != "timeout" || (got.CancelStatus != "not_needed" && got.CancelStatus != "failed") || got.Observation != nil {
		t.Fatal(got)
	}
	// A limiter worker that was already queued cannot emit later.
	time.Sleep(20 * time.Millisecond)
	if len(wire.frames(conn)) != 0 {
		t.Fatal("expired unsent diagnostic later escaped to wire")
	}
	probeCallback(conn, 1, "DU1234567", "NetLiquidation", "7654321", "EUR")
	if len(conn.GetAccountSummary()) != 0 {
		t.Fatal("unsent retired probe seeded cache")
	}
}

func TestSettlementProbeNormalCancellationRetiresBeforeDiagnostic(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	normal := make(chan error, 1)
	go func() { _, _, err := c.RequestAccountSummaryWithProvenance(ctx, time.Minute); normal <- err }()
	probeFrames(t, wire, conn, 1)
	cancel()
	if err := <-normal; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	conn.accountMu.RLock()
	n := len(conn.summarySnapshots)
	conn.accountMu.RUnlock()
	if n != 0 {
		t.Fatal("cancelled ordinary await left accumulator in flight")
	}
	done := startSettlementProbe(c, t.Context(), time.Second)
	frames := probeFrames(t, wire, conn, 3)
	id, _ := strconv.Atoi(frames[2][2])
	probeEnd(conn, id)
	if got := <-done; got.Status != "completed_empty" {
		t.Fatal("cancelled ordinary lease retained", got)
	}
}

func TestSettlementProbeQuarantineLimitAndDuplicateOrdinaryLease(t *testing.T) {
	c, conn, wire := settlementConnectorFixture(t)
	conn.accountMu.Lock()
	conn.retiredSummaryProbes = make(map[int]uint64)
	for i := range 256 {
		conn.retiredSummaryProbes[i] = conn.BrokerSessionEpoch()
	}
	conn.accountMu.Unlock()
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "skipped_quarantine_limit" || len(wire.frames(conn)) != 0 {
		t.Fatal("unbounded quarantine", got)
	}
	conn.accountMu.Lock()
	clear(conn.retiredSummaryProbes)
	conn.accountMu.Unlock()
	if err := conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567"); err != nil {
		t.Fatal(err)
	}
	if err := conn.RequestAccountSummaryForAccount(100, "NetLiquidation", "DU1234567"); err == nil {
		t.Fatal("duplicate ordinary accepted")
	}
	if got := c.RequestSettlementCashProbe(t.Context(), time.Second); got.Status != "skipped_busy" {
		t.Fatal("duplicate removed original subscription lease", got)
	}
}
