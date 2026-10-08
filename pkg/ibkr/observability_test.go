package ibkr

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestManagedConnectionDiagnosticsKeepStandaloneWarnings(t *testing.T) {
	buf := captureConnectorLogs(t)
	SetLogLevel("debug")
	defer SetLogLevel("info")
	c := &Connection{config: &ConnectionConfig{ManagedConnectionLogging: true}}
	c.logConnectAttempt("synthetic managed failure")
	c.config.ManagedConnectionLogging = false
	c.logConnectAttempt("synthetic standalone failure")
	managed := logLines(buf, "synthetic managed")
	standalone := logLines(buf, "synthetic standalone")
	if len(managed) != 1 || !strings.Contains(managed[0], "level=DEBUG") || len(standalone) != 1 || !strings.Contains(standalone[0], "level=WARN") {
		t.Fatal(string(buf.Bytes()))
	}
}

func TestPnLSilenceRecoveryRequiresCurrentSubscriptionFrame(t *testing.T) {
	buf := captureConnectorLogs(t)
	now := time.Now()
	c := &Connector{pnl: newPnLCache(), pnlResubNow: func() time.Time { return now }}
	c.pnl.accountReqID = 12
	c.pnlSilenceLog.Observe(now.Add(-time.Hour))
	c.handlePnL([]string{"94", "11", "1", "1", "1"})
	if len(logLines(buf, "stream resumed")) != 0 {
		t.Fatal("old subscription reported recovery")
	}
	c.handlePnL([]string{"94", "12", "1", "1", "1"})
	c.handlePnL([]string{"94", "12", "2", "2", "2"})
	if lines := logLines(buf, "stream resumed"); len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
		t.Fatal(string(buf.Bytes()))
	}
}

// readerLoss runs a connected reader against a loopback Gateway, lets end
// finish the Gateway's side, and returns the log once the reader exits.
func readerLoss(t *testing.T, managed bool, end func(gateway net.Conn)) string {
	t.Helper()
	buf := captureConnectorLogs(t)
	SetLogLevel("debug")
	t.Cleanup(func() { SetLogLevel("info") })
	conn, gateway := heartbeatPair(t, time.Hour)
	conn.config.ManagedConnectionLogging = managed
	conn.wg.Add(1)
	go conn.readMessages()
	end(gateway)
	deadline := time.Now().Add(5 * time.Second)
	for conn.IsConnected() {
		if time.Now().After(deadline) {
			t.Fatal("reader did not observe the session's end")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	return string(buf.Bytes())
}

func closeGateway(gateway net.Conn) { _ = gateway.Close() }

func resetGateway(gateway net.Conn) {
	_ = gateway.(*net.TCPConn).SetLinger(0)
	_ = gateway.Close()
}

// A lost session on a managed connection is its owner's single incident
// (the daemon's gateway log): the reader's EOF, reset and disconnect lines
// stay in the debug log. Standalone callers keep their warnings and the
// reset's error, and an oversized frame is a protocol fault no owner
// explains, so it stays an error either way.
func TestManagedSessionLossLogsAtDebug(t *testing.T) {
	for _, tc := range []struct {
		name    string
		end     func(net.Conn)
		managed bool
		want    map[string]string // line -> level
	}{
		{"managed EOF", closeGateway, true, map[string]string{"Failed to read length": "DEBUG", "Connection closed by server": "DEBUG", "Disconnection detected": "DEBUG"}},
		{"managed reset", resetGateway, true, map[string]string{"Failed to read length": "DEBUG", "Error reading message": "DEBUG", "Disconnection detected": "DEBUG"}},
		{"standalone EOF", closeGateway, false, map[string]string{"Failed to read length": "WARN", "Connection closed by server": "WARN", "Disconnection detected": "WARN"}},
		{"standalone reset", resetGateway, false, map[string]string{"Failed to read length": "WARN", "Error reading message": "ERROR", "Disconnection detected": "WARN"}},
		{"managed oversized frame", func(g net.Conn) { _, _ = g.Write([]byte{0x7f, 0xff, 0xff, 0xff}) }, true, map[string]string{"Error reading message: message too large": "ERROR", "Disconnection detected": "DEBUG"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logged := readerLoss(t, tc.managed, tc.end)
			for substr, level := range tc.want {
				var lines []string
				for line := range strings.SplitSeq(logged, "\n") {
					if strings.Contains(line, substr) {
						lines = append(lines, line)
					}
				}
				if len(lines) != 1 || !strings.Contains(lines[0], "level="+level) {
					t.Errorf("%q: want one %s line, got %q", substr, level, lines)
				}
			}
		})
	}
}

// The startAPI retry narration of a managed connection is its owner's
// connect attempt; the daemon reports an attempt that never succeeds.
// Standalone callers keep the warnings.
func TestManagedStartAPIRetriesLogAtDebug(t *testing.T) {
	buf := captureConnectorLogs(t)
	SetLogLevel("debug")
	t.Cleanup(func() { SetLogLevel("info") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// A Gateway that completes the version handshake but never answers
	// startAPI, as TWS did for 17 s after its restart on 2026-10-07.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var head [8]byte // "API\0" and the descriptor's length
				if _, err := io.ReadFull(c, head[:]); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, binary.BigEndian.Uint32(head[4:]))); err != nil {
					return
				}
				reply := []byte("176\x0020261007 23:45:17 CET\x00")
				_, _ = c.Write(append(binary.BigEndian.AppendUint32(nil, uint32(len(reply))), reply...))
				_, _ = io.Copy(io.Discard, c)
			}(c)
		}
	}()
	// One attempt each (startAPI waits 1 s, then backs off 2 s), side by side.
	clients := map[int]bool{15: true, 25: false}
	var wg sync.WaitGroup
	for clientID, managed := range clients {
		wg.Go(func() {
			conn := NewConnection(&ConnectionConfig{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, ClientID: clientID, ConnectTimeout: 2 * time.Second, MaxClientIDRetries: 1, ManagedConnectionLogging: managed})
			_ = conn.Connect(t.Context())
			_ = conn.Disconnect()
		})
	}
	wg.Wait()
	for clientID, managed := range clients {
		want := "WARN"
		if managed {
			want = "DEBUG"
		}
		for _, substr := range []string{fmt.Sprintf("Client %d: Failed to start API", clientID), fmt.Sprintf("startAPI failed for Client ID %d; retrying", clientID)} {
			lines := logLines(buf, substr)
			if len(lines) != 1 || !strings.Contains(lines[0], "level="+want) {
				t.Errorf("managed=%t: want one %s line for %q, got %q", managed, want, substr, lines)
			}
		}
	}
}
