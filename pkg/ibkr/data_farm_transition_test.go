package ibkr

import (
	"strings"
	"testing"
	"time"
)

func warnLines(buf *safeBuffer, substr string) []string {
	var out []string
	for _, line := range logLines(buf, substr) {
		if strings.Contains(line, "level=WARN") {
			out = append(out, line)
		}
	}
	return out
}

// Farm notices log at INFO, below the WARN-only production log. A farm that
// stops answering must still leave one WARN when it breaks and one when it
// recovers, while routine connect-time notices stay quiet.
func TestDataFarmTransitionsWarnOncePerBreakAndRecovery(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := &Connector{config: &ConnectorConfig{PreferredClientID: 15}}
	t0 := time.Date(2026, 9, 28, 4, 26, 0, 0, time.UTC)

	c.recordDataFarmNotice(2104, "Market data farm connection is OK:usfarm", t0)
	c.recordDataFarmNotice(2106, "HMDS data farm connection is OK:ushmds", t0)
	c.recordDataFarmNotice(2107, "HMDS data farm connection is inactive but should be available upon demand.fundfarm", t0)
	if lines := warnLines(buf, "data farm"); len(lines) != 0 {
		t.Fatalf("routine farm notices warned: %q", lines)
	}

	c.recordDataFarmNotice(2105, "HMDS data farm connection is broken:ushmds", t0.Add(time.Minute))
	c.recordDataFarmNotice(2105, "HMDS data farm connection is broken:ushmds", t0.Add(2*time.Minute))
	if lines := warnLines(buf, "historical data farm ushmds is disconnected (code 2105)"); len(lines) != 1 {
		t.Fatalf("break warnings = %q, want exactly one", lines)
	}

	c.recordDataFarmNotice(2106, "HMDS data farm connection is OK:ushmds", t0.Add(7*time.Minute))
	c.recordDataFarmNotice(2106, "HMDS data farm connection is OK:ushmds", t0.Add(8*time.Minute))
	recovered := warnLines(buf, "historical data farm ushmds recovered")
	if len(recovered) != 1 || !strings.Contains(recovered[0], "after 6m0s") {
		t.Fatalf("recovery warnings = %q, want one after 6m0s", recovered)
	}

	c.recordDataFarmNotice(2157, "Sec-def data farm connection is broken:secdefil", t0.Add(9*time.Minute))
	if lines := warnLines(buf, "security definition data farm secdefil is disconnected"); len(lines) != 1 {
		t.Fatalf("sec-def break warnings = %q, want one", lines)
	}
}

// 2110 had no WARN of its own; 1100/1101/1102 warn through the backend link
// tracker and must not gain a second line here.
func TestConnectivityBreakWarnsOnlyForCode2110(t *testing.T) {
	buf := captureConnectorLogs(t)
	c := &Connector{config: &ConnectorConfig{PreferredClientID: 15}}
	t0 := time.Date(2026, 9, 28, 4, 26, 0, 0, time.UTC)

	c.recordDataFarmNotice(1100, "Connectivity between IBKR and Trader Workstation has been lost.", t0)
	c.recordDataFarmNotice(1102, "Connectivity between IBKR and Trader Workstation has been restored - data maintained.", t0.Add(time.Minute))
	if lines := warnLines(buf, "IBKR"); len(lines) != 0 {
		t.Fatalf("1100/1102 warned twice: %q", lines)
	}
	c.recordDataFarmNotice(2110, "Connectivity between Trader Workstation and server is broken. It will be restored automatically.", t0.Add(2*time.Minute))
	c.recordDataFarmNotice(2110, "Connectivity between Trader Workstation and server is broken. It will be restored automatically.", t0.Add(3*time.Minute))
	if lines := warnLines(buf, "code 2110"); len(lines) != 1 {
		t.Fatalf("2110 warnings = %q, want one", lines)
	}
}
