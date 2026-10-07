package ibkr

import (
	"testing"
	"time"
)

// Only a bar farm whose break has drawn its WARN counts as announced: a short
// flap, a fundamentals farm and a recovered farm do not.
func TestHistoricalBarFarmOutageAnnouncedOnlyAfterTheWarning(t *testing.T) {
	_ = captureConnectorLogs(t)
	c := &Connector{config: &ConnectorConfig{PreferredClientID: 15}}
	t0 := time.Date(2026, 10, 5, 7, 0, 0, 0, time.UTC)
	if (*Connector)(nil).HistoricalBarFarmOutageAnnounced() {
		t.Fatal("nil connector reported an outage")
	}
	c.recordDataFarmNotice(2105, "HMDS data farm connection is broken:fundfarm", t0)
	c.recordDataFarmNotice(2105, "HMDS data farm connection is broken:ushmds", t0)
	if c.HistoricalBarFarmOutageAnnounced() {
		t.Fatal("a break that has not lasted counted as announced")
	}
	c.CheckBackendLogRelevance(t0.Add(dataFarmBreakAttention + time.Minute))
	if !c.HistoricalBarFarmOutageAnnounced() {
		t.Fatal("a warned ushmds break was not reported")
	}
	c.recordDataFarmNotice(2106, "HMDS data farm connection is OK:ushmds", t0.Add(10*time.Minute))
	if c.HistoricalBarFarmOutageAnnounced() {
		t.Fatal("a recovered farm, or the still-broken fundfarm, counted as a bar outage")
	}
}
