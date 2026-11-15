package daemon

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

// Exercise the actual Add failure boundary, including the broker decoder. A
// routine option-definition rejection is debug-only in the library, so the
// existing normal log cannot explain this failed horizon without Add's warning.
func TestAddInventoryFailureLogsSanitizedBrokerVerdict(t *testing.T) {
	runAddInventoryDiagnostic(t, "broker-rejection")
}

// A complete exact inventory arriving after the old eight-second budget must
// still reach selection on the same request, without retries or partial data.
func TestAddInventoryRetainsSlowCompleteReply(t *testing.T) {
	runAddInventoryDiagnostic(t, "slow-complete")
}

func runAddInventoryDiagnostic(t *testing.T, mode string) {
	const privateText = "private-marker-do-not-log"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	expiry := time.Now().AddDate(0, 0, 30).Format("20060102")
	done := make(chan error, 1)
	var inventoryRequests atomic.Int64
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(25 * time.Second))
		r := bufio.NewReader(conn)
		prefix := make([]byte, 4)
		if _, err := io.ReadFull(r, prefix); err != nil {
			done <- err
			return
		}
		if _, err := readTWSFrame(r); err != nil {
			done <- err
			return
		}
		if err := writeTWSFrame(conn, []byte("203\x0020261022 12:00:00 UTC\x00")); err != nil {
			done <- err
			return
		}
		if _, err := readTWSFrame(r); err != nil {
			done <- err
			return
		}
		var writeErr error
		send := func(id uint32, fields ...string) {
			if writeErr == nil {
				body := binary.BigEndian.AppendUint32(nil, id)
				writeErr = writeTWSFrame(conn, append(body, strings.Join(fields, "\x00")+"\x00"...))
			}
		}
		sendError := func(idText string, code int, message string) {
			id, _ := strconv.Atoi(idText)
			body := binary.BigEndian.AppendUint32(nil, 204)
			body = binary.AppendUvarint(append(body, 8), uint64(id))
			body = binary.AppendUvarint(append(body, 24), uint64(code))
			body = binary.AppendUvarint(append(body, 34), uint64(len(message)))
			if writeErr == nil {
				writeErr = writeTWSFrame(conn, append(body, message...))
			}
		}
		send(9, "1", "1")
		send(15, "1", "DU1234567") // managedAccounts: no one-second startup grace
		for writeErr == nil {
			frame, err := readTWSFrame(r)
			if err != nil {
				done <- nil
				return
			}
			if len(frame) < 4 {
				continue
			}
			f := append([]string{strconv.Itoa(int(binary.BigEndian.Uint32(frame[:4])))}, strings.Split(string(frame[4:]), "\x00")...)
			switch f[0] {
			case "9":
				if f[5] == "STK" {
					send(10, addSnapshotContractFields(f[2], "STK", "", "", "17")...)
					send(52, "1", f[2])
				} else if mode != "broker-rejection" {
					// Only a lookup without contract id and strike lists the expiry.
					if f[3] == "0" && (f[7] == "" || f[7] == "0") {
						inventoryRequests.Add(1)
						if mode == "slow-complete" {
							time.Sleep(9 * time.Second)
						}
					}
					send(10, addSnapshotContractFields(f[2], "OPT", expiry, "110", "10110")...)
					send(52, "1", f[2])
				} else {
					sendError(f[2], 200, "No security definition has been found for the request: "+privateText)
				}
			case "91":
				send(93, f[1], "1", "0", "0.01")
			case "1":
				if f[5] == "OPT" {
					send(58, "1", f[2], "1")
					send(1, "6", f[2], "1", "1.00", "100", "0")
					send(1, "6", f[2], "2", "1.02", "100", "0")
					// The 30-day fixture must stay inside the owner's 0.35–0.50
					// delta band so this test isolates slow inventory completion.
					send(21, f[2], "13", "0", "0.2", "0.45", "1.01", "0", "0.01", "0.1", "-0.1", "110")
					continue
				}
				send(58, "1", f[2], "1")
				send(1, "6", f[2], "1", "109.99", "100", "0")
				send(1, "6", f[2], "2", "110.01", "100", "0")
				send(1, "6", f[2], "4", "110", "100", "0")
				send(1, "6", f[2], "9", "109", "100", "0")
			case "78":
				send(75, f[1], "SMART", "17", "SYNTH", "100", "1", expiry, "1", "110")
				send(76, f[1])
			case "20":
				send(17, f[1], "", "", "0")
			}
		}
		done <- writeErr
	}()
	cfg := ibkr.DefaultConfig()
	cfg.Host, cfg.Port, cfg.ClientID, cfg.AutoReconnect = "127.0.0.1", ln.Addr().(*net.TCPAddr).Port, 72, false
	c := ibkr.NewConnector(&ibkr.ConnectorConfig{BaseConfig: cfg})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil || !c.IsReady() {
		t.Fatalf("fake broker not ready: %v %s", err, c.LastError())
	}
	t.Cleanup(func() {
		_ = c.Stop()
		if err := <-done; err != nil {
			t.Errorf("fake broker: %v", err)
		}
	})
	s := newTestServer(t)
	logs := &lockedBuffer{}
	s.connector, s.logger = c, NewLogger(logs, "warn")
	if mode == "broker-rejection" {
		s.now = func() time.Time { return time.Date(2026, 10, 22, 16, 0, 0, 0, time.UTC) }
	}
	if mode != "broker-rejection" {
		// Selection remains planning-only even when this transport regression
		// runs during market hours or beyond the embedded calendar's coverage.
		s.now = func() time.Time { return time.Date(2026, 10, 22, 23, 0, 0, 0, time.UTC) }
	}
	params, _ := json.Marshal(rpc.AddMarketParams{Underlying: rpc.ContractParams{ConID: 17, Symbol: "SYNTH", SecType: "STK", Exchange: "SMART", Currency: "USD"}, Discover: true})
	got, err := s.handleAddMarket(ctx, &rpc.Request{Params: params})
	if mode != "broker-rejection" {
		if err != nil || got == nil || len(got.Calls) != 4 {
			t.Fatalf("Add read failed: got=%+v err=%v", got, err)
		}
		row := got.Calls[1]
		if row.Contract == nil || row.Contract.ConID != 10110 || inventoryRequests.Load() != 1 {
			t.Fatalf("complete inventory was discarded or retried: row=%+v requests=%d log=%s", row, inventoryRequests.Load(), logs.String())
		}
		if strings.Contains(logs.String(), "add.inventory") {
			t.Fatalf("complete inventory was reported failed: %s", logs.String())
		}
		return
	}
	if err != nil || got == nil || len(got.Calls) != 4 || got.Calls[1].Unavailable != "Call contracts unavailable" {
		t.Fatalf("fixture did not reach the inventory error boundary: result=%+v err=%v", got, err)
	}
	logged := logs.String()
	for _, want := range []string{"level=WARN", "add.inventory", "symbol=SYNTH", "expiry=" + expiry, "horizon=30", "elapsed_ms=", "reason=no_definition"} {
		if !strings.Contains(logged, want) {
			t.Fatalf("missing normal-log inventory evidence %q: %s", want, logged)
		}
	}
	if strings.Contains(logged, privateText) || strings.Contains(logged, "No security definition") || strings.Count(logged, "add.inventory") != 1 {
		t.Fatalf("inventory diagnostic leaked raw text or repeated: %s", logged)
	}
}

func TestAddInventoryDiagnosticNeverLogsRawErrors(t *testing.T) {
	const privateText = "private-marker-do-not-log"
	for _, tc := range []struct {
		name, reason string
		err          error
	}{
		{"timeout", "timeout", fmt.Errorf(privateText+": %w", context.DeadlineExceeded)},
		{"cancel", "canceled", fmt.Errorf(privateText+": %w", context.Canceled)},
		{"definition", "no_definition", fmt.Errorf(privateText+": %w", ibkr.ErrContractNoDefinition)},
		{"mismatch", "identity_mismatch", fmt.Errorf(privateText+": %w", errors.New("expiry call inventory contains a mismatched contract"))},
		{"conflict", "identity_conflict", errors.New("expiry call inventory contains conflicting identities")},
		{"response bound", "response_limit", errors.New("expiry call inventory exceeds 2048 responses")},
		{"retired session", "session_changed", errors.New("broker session changed during expiry call discovery")},
		{"typed broker", "broker_321", &ibkr.ContractDetailsRejection{Code: 321, Message: privateText}},
		{"ordinary broker", "broker_321", fmt.Errorf(privateText+": %w", errors.New("contract details request failed (IBKR 321)"))},
		{"untrusted broker suffix", "read_failed", errors.New("contract details request failed (IBKR 321) " + privateText)},
		{"untrusted broker code", "read_failed", errors.New("contract details request failed (IBKR 321\n" + privateText + ")")},
		{"unknown", "read_failed", errors.New(privateText)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedBuffer{}
			s := &Server{logger: NewLogger(logs, "warn")}
			s.warnAddInventoryFailure("SYNTH", "20270219", 120, 1250*time.Millisecond, tc.err)
			logged := logs.String()
			if !strings.Contains(logged, "level=WARN") || !strings.Contains(logged, "elapsed_ms=1250 reason="+tc.reason+"\"") || strings.Contains(logged, privateText) || strings.Count(logged, "add.inventory") != 1 {
				t.Fatalf("diagnostic did not retain safe context and classification: %s", logged)
			}
		})
	}
}
