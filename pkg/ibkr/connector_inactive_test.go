package ibkr

import (
	"testing"
	"time"
)

// TestInactiveMarkExpiresAfterTTL pins the lazy-expiry contract: an inactive
// mark is a cache, not a verdict. After inactiveMarkTTL the mark is deleted
// on read and the symbol earns a fresh probe; re-marking requires a fresh
// 2-in-10-min confirmation.
func TestInactiveMarkExpiresAfterTTL(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.markSymbolInactive("ZVZZT", "No security definition has been found for the request")
	if !c.IsSymbolInactive("ZVZZT") {
		t.Fatal("fresh mark must suppress")
	}

	c.inactiveMu.Lock()
	state := c.inactiveSymbols["ZVZZT"]
	state.markedAt = time.Now().Add(-inactiveMarkTTL - time.Minute)
	c.inactiveSymbols["ZVZZT"] = state
	c.inactiveMu.Unlock()

	if c.IsSymbolInactive("ZVZZT") {
		t.Fatal("expired mark must not suppress")
	}
	c.inactiveMu.RLock()
	_, still := c.inactiveSymbols["ZVZZT"]
	c.inactiveMu.RUnlock()
	if still {
		t.Fatal("expired mark must be deleted on read, not just ignored")
	}
	// One error after expiry is a transient, not a confirmation.
	if c.registerInactiveCandidate("ZVZZT", "No security definition has been found for the request"); c.IsSymbolInactive("ZVZZT") {
		t.Fatal("re-marking after expiry must require fresh confirmation")
	}
}

// TestRegisterInactiveCandidateSuppressedWhileFarmImpaired pins the
// choke-point guard: while any tracked farm is impaired, definition errors
// are a session verdict, not a contract verdict — no candidate counting on
// EITHER write path (subscription notices and historical failures both
// converge on registerInactiveCandidate). Regression: the 2026-07-08
// nightly-reset wedge marked held AMD/BB/IBM and VIX inactive and (then)
// persisted them into every daemon's boot state.
func TestRegisterInactiveCandidateSuppressedWhileFarmImpaired(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.recordDataFarmNotice(2105, "HMDS data farm connection is broken:ushmds", time.Now())
	if !c.marketDataFarmImpaired() {
		t.Fatal("historical farm broken must count as impaired")
	}

	reason := "No security definition has been found for the request"
	for range 4 {
		if c.registerInactiveCandidate("AMD", reason) {
			t.Fatal("must not mark while a farm is impaired")
		}
	}
	if c.IsSymbolInactive("AMD") {
		t.Fatal("no mark may form while a farm is impaired")
	}
	c.inactiveMu.RLock()
	_, candidate := c.inactiveCandidates["AMD"]
	c.inactiveMu.RUnlock()
	if candidate {
		t.Fatal("impaired-window errors must not accumulate as candidates")
	}

	// Farm recovers: the settle window still counts as impaired until it
	// ages out (queued outage-era answers flush around the transition),
	// then normal confirmation applies again.
	c.recordDataFarmNotice(2106, "HMDS data farm connection is OK:ushmds", time.Now())
	if !c.marketDataFarmImpaired() {
		t.Fatal("fresh recovery must stay impaired for the settle window")
	}
	c.dataFarmMu.Lock()
	c.farmRecoveryAt = time.Now().Add(-farmRecoverySettleWindow - time.Second)
	c.dataFarmMu.Unlock()
	if c.marketDataFarmImpaired() {
		t.Fatal("recovered farm must clear impairment after the settle window")
	}
	c.registerInactiveCandidate("ZVZZT", reason)
	if !c.registerInactiveCandidate("ZVZZT", reason) {
		t.Fatal("second confirmation after recovery must mark")
	}
}

// TestSecurityDefinitionFarmCountsAsImpaired pins the widened farm-type
// filter: secdef (2157/2158) and historical (2105/2106) farms gate marking,
// not just market-data and connectivity farms.
func TestSecurityDefinitionFarmCountsAsImpaired(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.recordDataFarmNotice(2157, "Sec-def data farm connection is broken:secdefnj", time.Now())
	if !c.marketDataFarmImpaired() {
		t.Fatal("broken secdef farm must count as impaired")
	}
	c.recordDataFarmNotice(2158, "Sec-def data farm connection is OK:secdefnj", time.Now())
	c.dataFarmMu.Lock()
	c.farmRecoveryAt = time.Now().Add(-farmRecoverySettleWindow - time.Second)
	c.dataFarmMu.Unlock()
	if c.marketDataFarmImpaired() {
		t.Fatal("recovered secdef farm must clear impairment after the settle window")
	}
}

// TestConnectivityLostCountsAsImpaired pins the 1100/1101/1102 mapping.
// Regression: 2026-07-29 16:24:17 — TWS flushed queued code=200 answers 1ms
// after an untracked 1100 and 4s before the per-farm break notices, so the
// farm map read healthy and IWM/TLT/QQQ were marked inactive for the 12h TTL.
func TestConnectivityLostCountsAsImpaired(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.recordDataFarmNotice(1100, "Connectivity between IBKR and Trader Workstation has been lost.", time.Now())
	if !c.marketDataFarmImpaired() {
		t.Fatal("1100 connectivity-lost must count as impaired")
	}

	reason := "No security definition has been found for the request"
	for range 4 {
		if c.registerInactiveCandidate("IWM", reason) {
			t.Fatal("must not mark while connectivity is lost")
		}
	}
	if c.IsSymbolInactive("IWM") {
		t.Fatal("no mark may form while connectivity is lost")
	}

	// 1102 restores connectivity; its trailing farm list must not mint a
	// second connectivity key that the broken mark never matches.
	c.recordDataFarmNotice(1102, "Connectivity between IBKR and Trader Workstation has been restored - data maintained. All data farms are connected: usfarm; ushmds; secdefeu.", time.Now())
	for _, farm := range c.DataFarmStatuses() {
		if farm.Type == "connectivity" && farm.Name != "tws-server" {
			t.Fatalf("connectivity entry name = %q, want tws-server", farm.Name)
		}
		if farm.Type == "connectivity" && farm.Status != "ok" {
			t.Fatalf("connectivity entry status = %q after 1102, want ok", farm.Status)
		}
	}
	if !c.marketDataFarmImpaired() {
		t.Fatal("fresh 1102 recovery must stay impaired for the settle window")
	}
	c.dataFarmMu.Lock()
	c.farmRecoveryAt = time.Now().Add(-farmRecoverySettleWindow - time.Second)
	c.dataFarmMu.Unlock()
	if c.marketDataFarmImpaired() {
		t.Fatal("restored connectivity must clear impairment after the settle window")
	}
	c.registerInactiveCandidate("ZVZZT", reason)
	if !c.registerInactiveCandidate("ZVZZT", reason) {
		t.Fatal("second confirmation after recovery must mark")
	}
}

// TestFarmRecoverySettleWindowSuppressesDefinitionErrors pins the settle
// window on the plain farm break/heal path: answers to requests queued
// during the break flush around the OK notice and still carry outage-era
// verdicts.
func TestFarmRecoverySettleWindowSuppressesDefinitionErrors(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	c.recordDataFarmNotice(2103, "Market data farm connection is broken:usfarm", time.Now())
	c.recordDataFarmNotice(2104, "Market data farm connection is OK:usfarm", time.Now())
	if !c.marketDataFarmImpaired() {
		t.Fatal("settle window after farm recovery must count as impaired")
	}

	reason := "No security definition has been found for the request"
	c.registerInactiveCandidate("TLT", reason)
	if c.registerInactiveCandidate("TLT", reason) || c.IsSymbolInactive("TLT") {
		t.Fatal("definition errors inside the settle window must not mark")
	}

	c.dataFarmMu.Lock()
	c.farmRecoveryAt = time.Now().Add(-farmRecoverySettleWindow - time.Second)
	c.dataFarmMu.Unlock()
	if c.marketDataFarmImpaired() {
		t.Fatal("settle window must age out")
	}
}

// TestCachedPositionsNeverHidesHeldRowsOnInactiveMark pins the
// consequence-surface fix: an inactive mark must never hide a held stock
// row. For a true delisting the row is zero-value and was always kept; the
// removed skip branch fired almost exclusively on FALSE marks, silently
// hiding healthy holdings during gateway-wide degradation.
func TestCachedPositionsNeverHidesHeldRowsOnInactiveMark(t *testing.T) {
	c, conn, _ := newAcctResubscribeRig(t)
	c.markSymbolInactive("AMD", "No security definition has been found for the request")

	conn.positionsMu.Lock()
	conn.positions = map[string]*RawPosition{
		"AMD": {
			Contract:    Contract{ConID: 4391, Symbol: "AMD", SecType: "STK", Currency: "USD"},
			Position:    100,
			MarketPrice: 200,
			MarketValue: 20000,
		},
	}
	conn.positionsMu.Unlock()

	positions, err := c.CachedPositions()
	if err != nil {
		t.Fatalf("CachedPositions: %v", err)
	}
	if len(positions) != 1 || positions[0].Contract.Symbol != "AMD" {
		t.Fatalf("held marked stock must remain visible, got %+v", positions)
	}
}
