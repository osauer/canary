package ibkr

import (
	"strings"
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
