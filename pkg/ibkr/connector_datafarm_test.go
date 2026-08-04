package ibkr

import (
	"testing"
	"time"
)

func connectivityFarm(t *testing.T, c *Connector) (DataFarmStatus, bool) {
	t.Helper()
	for _, farm := range c.DataFarmStatuses() {
		if farm.Type == "connectivity" {
			return farm, true
		}
	}
	return DataFarmStatus{}, false
}

// A market-data farm reporting OK says nothing about the TWS<->IBKR link.
// recordDataFarmNotice used to delete the connectivity entry on ANY ok
// notice, so a routine 2104 erased a 1100 break — and an impaired
// connectivity row is what short-circuits every farm type to unavailable in
// `canary status`, so the erasure turned a live outage green.
func TestFarmOKNoticeDoesNotClearTheConnectivityBreak(t *testing.T) {
	now := time.Now()
	c := NewConnector(&ConnectorConfig{})
	c.recordDataFarmNotice(1100, "Connectivity between IB and Trader Workstation has been lost.", now)

	for _, notice := range []struct {
		code    int
		message string
	}{
		{2104, "Market data farm connection is OK:usfarm"},
		{2106, "HMDS data farm connection is OK:ushmds"},
		{2158, "Sec-def data farm connection is OK:secdefnj"},
	} {
		c.recordDataFarmNotice(notice.code, notice.message, now)
		farm, ok := connectivityFarm(t, c)
		if !ok {
			t.Fatalf("notice %d erased the connectivity entry", notice.code)
		}
		if !farmStatusImpaired(farm.Status) {
			t.Fatalf("notice %d left connectivity status %q, want impaired", notice.code, farm.Status)
		}
	}
}

// The codes that actually name the link still clear it, through the same
// key the break was written under.
func TestConnectivityRestoreNoticeClearsTheBreak(t *testing.T) {
	for _, code := range []int{1101, 1102} {
		now := time.Now()
		c := NewConnector(&ConnectorConfig{})
		c.recordDataFarmNotice(1100, "Connectivity between IB and Trader Workstation has been lost.", now)
		c.recordDataFarmNotice(code, "Connectivity between IB and Trader Workstation has been restored - data maintained.", now)

		farm, ok := connectivityFarm(t, c)
		if !ok {
			t.Fatalf("code %d: connectivity row disappeared instead of turning ok", code)
		}
		if farmStatusImpaired(farm.Status) {
			t.Errorf("code %d: connectivity status = %q, want ok", code, farm.Status)
		}
		c.dataFarmMu.RLock()
		recovered := !c.farmRecoveryAt.IsZero()
		c.dataFarmMu.RUnlock()
		if !recovered {
			t.Errorf("code %d: impaired->ok transition was not stamped", code)
		}
	}
}
