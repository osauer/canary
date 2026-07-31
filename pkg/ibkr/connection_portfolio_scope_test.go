package ibkr

import (
	"bufio"
	"testing"
	"time"
)

const (
	scopeTestJoint      = "U1111111"
	scopeTestIndividual = "U6666666"
	scopeTestManaged    = scopeTestJoint + "," + scopeTestIndividual
)

// portfolioFrame builds a 20-field msgPortfolioValue payload. The field
// layout matches the comment block on handlePortfolioValue; field 9 is
// primaryExchange, and field 19 is the account the row belongs to.
func portfolioFrame(conID, symbol, secType, expiry, strike, right, multiplier, account string) []string {
	return []string{
		"7", "8",
		conID, symbol, secType, expiry, strike, right, multiplier,
		"SMART", "USD", symbol, symbol,
		"10", "24.00", "240.00", "23.00", "10.00", "0.00", account,
	}
}

func newPortfolioScopeTestConnection(t *testing.T, managed string, boundAccount string, at time.Time) *Connection {
	t.Helper()
	conn := NewConnection(DefaultConfig())
	if conn == nil {
		t.Fatal("NewConnection returned nil")
	}
	t.Cleanup(func() { conn.rateLimiter.Stop() })
	if managed != "" {
		conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", managed))
	}
	conn.resetPortfolioStreamHealth(boundAccount, at)
	return conn
}

// TestHandlePortfolioValueMultiplierBySecType pins which secTypes may fail a
// staged portfolio generation over an absent or zero contract multiplier.
// IB omits the field entirely on cash, bond, bill, fund and CFD frames, and
// requiring it there discarded the whole download over one treasury holding
// (issue #14). Genuine derivatives keep the hard requirement, because
// normalizing a real multiplier to 1 would understate the row.
func TestHandlePortfolioValueMultiplierBySecType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		secType    string
		expiry     string
		strike     string
		right      string
		multiplier string
		published  bool
		want       int
	}{
		{name: "stock blank multiplier", secType: "STK", published: true, want: 1},
		{name: "stock zero multiplier", secType: "STK", multiplier: "0", published: true, want: 1},
		{name: "stock explicit multiplier", secType: "STK", multiplier: "1", published: true, want: 1},
		{name: "bond blank multiplier", secType: "BOND", published: true, want: 1},
		{name: "bond zero multiplier", secType: "BOND", multiplier: "0", published: true, want: 1},
		{name: "bill blank multiplier", secType: "BILL", published: true, want: 1},
		{name: "cash blank multiplier", secType: "CASH", published: true, want: 1},
		{name: "fund blank multiplier", secType: "FUND", published: true, want: 1},
		{name: "cfd blank multiplier", secType: "CFD", published: true, want: 1},
		{name: "index blank multiplier", secType: "IND", published: true, want: 1},
		{name: "commodity blank multiplier", secType: "CMDTY", published: true, want: 1},
		{
			name: "option explicit multiplier", secType: "OPT",
			expiry: "20260821", strike: "305", right: "C", multiplier: "100",
			published: true, want: 100,
		},
		{
			name: "option blank multiplier", secType: "OPT",
			expiry: "20260821", strike: "305", right: "C",
			published: false,
		},
		{
			name: "option zero multiplier", secType: "OPT",
			expiry: "20260821", strike: "305", right: "C", multiplier: "0",
			published: false,
		},
		{
			name: "future option blank multiplier", secType: "FOP",
			expiry: "20260918", strike: "6500", right: "P",
			published: false,
		},
		{
			name: "warrant blank multiplier", secType: "WAR",
			expiry: "20281231", strike: "12",
			published: false,
		},
		{name: "future blank multiplier", secType: "FUT", expiry: "20260918", published: false},
		{name: "future explicit multiplier", secType: "FUT", expiry: "20260918", multiplier: "50", published: true, want: 50},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
			conn := newPortfolioScopeTestConnection(t, "", scopeTestIndividual, now)
			conn.handlePortfolioValue(portfolioFrame(
				"555001", "TST", tc.secType, tc.expiry, tc.strike, tc.right, tc.multiplier, scopeTestIndividual,
			))

			published := conn.completePortfolioDownload(scopeTestIndividual, now.Add(time.Second))
			rows, health := conn.GetPositionsWithPortfolioHealth()
			if published != tc.published {
				t.Fatalf("published = %v, want %v (health=%+v)", published, tc.published, health)
			}
			if !tc.published {
				if health.InvalidPayloadAt.IsZero() {
					t.Fatalf("%s generation was not marked invalid: health=%+v", tc.secType, health)
				}
				if len(rows) != 0 {
					t.Fatalf("invalid generation published %d rows", len(rows))
				}
				return
			}
			if !health.InvalidPayloadAt.IsZero() {
				t.Fatalf("valid %s generation marked invalid: health=%+v", tc.secType, health)
			}
			row := rows[cachedPositionKey(scopeTestIndividual, Contract{ConID: 555001, SecType: tc.secType})]
			if row == nil {
				t.Fatalf("%s row missing from published generation: %+v", tc.secType, rows)
			}
			if row.Contract.Multiplier != tc.want {
				t.Fatalf("%s multiplier = %d, want %d", tc.secType, row.Contract.Multiplier, tc.want)
			}
		})
	}
}

// TestHandlePortfolioValueBondDoesNotDiscardOtherHoldings is the reporter's
// scenario in miniature: a single treasury row without a multiplier used to
// invalidate the whole staged generation, so every stock holding vanished
// from the positions array too.
func TestHandlePortfolioValueBondDoesNotDiscardOtherHoldings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	conn := newPortfolioScopeTestConnection(t, "", scopeTestIndividual, now)

	conn.handlePortfolioValue(portfolioFrame("4391", "AMD", "STK", "", "", "", "", scopeTestIndividual))
	conn.handlePortfolioValue(portfolioFrame("770001", "T 4 1/8 08/31/26", "BILL", "20260831", "", "", "", scopeTestIndividual))
	conn.handlePortfolioValue(portfolioFrame("265598", "AAPL", "STK", "", "", "", "", scopeTestIndividual))

	if !conn.completePortfolioDownload(scopeTestIndividual, now.Add(time.Second)) {
		t.Fatal("a treasury holding without a multiplier discarded the whole download")
	}
	rows, health := conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 3 {
		t.Fatalf("published %d rows, want 3: %+v", len(rows), rows)
	}
	if !health.InvalidPayloadAt.IsZero() || !health.ScopeConflictAt.IsZero() {
		t.Fatalf("healthy generation reported a fault: %+v", health)
	}
}

// TestAcceptPortfolioAccountFrameDropsManagedSiblings pins the multi-account
// steady state. One TWS login can hold several unlinked accounts, and the
// account-updates service streams all of them over the single subscription.
// A sibling's rows belong to another scope and must be dropped without
// latching a scope conflict, which previously rejected every later frame —
// including the pinned account's own — until a resubscribe (issue #14).
func TestAcceptPortfolioAccountFrameDropsManagedSiblings(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	conn := newPortfolioScopeTestConnection(t, scopeTestManaged, scopeTestIndividual, now)

	conn.handlePortfolioValue(portfolioFrame("111001", "JNT", "STK", "", "", "", "", scopeTestJoint))
	if _, health := conn.GetPositionsWithPortfolioHealth(); !health.ScopeConflictAt.IsZero() {
		t.Fatalf("sibling frame latched a scope conflict: %+v", health)
	}

	// A sibling's end marker says nothing about the bound account's download.
	if conn.completePortfolioDownload(scopeTestJoint, now.Add(time.Second)) {
		t.Fatal("sibling accountDownloadEnd published the bound account's generation")
	}
	if _, health := conn.GetPositionsWithPortfolioHealth(); !health.ScopeConflictAt.IsZero() {
		t.Fatalf("sibling end marker latched a scope conflict: %+v", health)
	}

	conn.handlePortfolioValue(portfolioFrame("666001", "IND", "STK", "", "", "", "", scopeTestIndividual))
	if !conn.completePortfolioDownload(scopeTestIndividual, now.Add(2*time.Second)) {
		t.Fatal("bound-account generation did not complete after sibling traffic")
	}

	rows, health := conn.GetPositionsWithPortfolioHealth()
	if len(rows) != 1 {
		t.Fatalf("published %d rows, want only the bound account's: %+v", len(rows), rows)
	}
	if rows[cachedPositionKey(scopeTestIndividual, Contract{ConID: 666001, SecType: "STK"})] == nil {
		t.Fatalf("bound-account row missing: %+v", rows)
	}
	if health.Account != scopeTestIndividual || !health.ScopeConflictAt.IsZero() || health.InitialCompletedAt.IsZero() {
		t.Fatalf("stream health after sibling traffic = %+v", health)
	}
}

// TestAcceptPortfolioAccountFrameLatchesUnknownAccount keeps the conflict
// latch for an account the gateway never listed — the case it exists for.
func TestAcceptPortfolioAccountFrameLatchesUnknownAccount(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	conn := newPortfolioScopeTestConnection(t, scopeTestManaged, scopeTestIndividual, now)

	conn.handlePortfolioValue(portfolioFrame("999001", "FRN", "STK", "", "", "", "", "U9999999"))
	_, health := conn.GetPositionsWithPortfolioHealth()
	if health.ScopeConflictAt.IsZero() {
		t.Fatalf("unlisted account did not latch a scope conflict: %+v", health)
	}
}

// TestHandleAccountValueDropsManagedSiblingRows covers the summary half of
// the same failure. c.account holds the raw managedAccounts value, which is
// a comma-separated list for a multi-account login and so is never a
// concrete code — the issue #12 guard compared against it and was therefore
// inert for exactly those logins, letting a sibling's zeroed batch and its
// JOINT AccountType overwrite the pinned account's summary.
func TestHandleAccountValueDropsManagedSiblingRows(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	conn := newPortfolioScopeTestConnection(t, scopeTestManaged, scopeTestIndividual, now)

	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "250000.00", "USD", scopeTestIndividual})
	conn.handleAccountValue([]string{"6", "2", "AccountType", "INDIVIDUAL", "", scopeTestIndividual})

	conn.handleAccountValue([]string{"6", "2", "NetLiquidation", "0.00", "USD", scopeTestJoint})
	conn.handleAccountValue([]string{"6", "2", "AccountType", "JOINT", "", scopeTestJoint})

	summary := conn.GetAccountSummary()
	if got := summary["NetLiquidation_USD"]; got != "250000.00" {
		t.Fatalf("sibling batch zeroed the bound account: NetLiquidation_USD = %q", got)
	}
	if got := summary["AccountType"]; got != "INDIVIDUAL" {
		t.Fatalf("sibling batch overwrote account type: AccountType = %q", got)
	}
}

// TestRequestAccountUpdatesDoesNotBindToManagedAccountsList keeps the raw
// list out of PortfolioStreamHealth.Account, where a comma-separated value
// silently disables every concrete-account check that reads it.
func TestRequestAccountUpdatesDoesNotBindToManagedAccountsList(t *testing.T) {
	t.Parallel()
	conn, _ := newReadyWireTestConnection(t)
	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", scopeTestManaged))

	if err := conn.RequestAccountUpdates(""); err != nil {
		t.Fatalf("RequestAccountUpdates: %v", err)
	}
	if _, health := conn.GetPositionsWithPortfolioHealth(); health.Account != "" {
		t.Fatalf("stream bound to a non-concrete account: %q", health.Account)
	}
}

// TestResubscribeAccountUpdatesKeepsPinnedAccount is the defect-3 regression.
// A scope-conflict or 1101 self-heal that resubscribes with "" resolves
// through the raw managedAccounts list to its positionally FIRST entry. With
// the operator's pin on the second entry that silently rebinds the stream to
// the wrong account, whereupon the pinned account's own frames look foreign
// and latch the conflict again — a loop the daemon never escapes.
func TestResubscribeAccountUpdatesKeepsPinnedAccount(t *testing.T) {
	t.Parallel()
	connector := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { connector.conn.rateLimiter.Stop() })
	conn := connector.conn
	conn.setStatus(StatusConnected)
	setServerVersionReady(conn, maxClientVersion)
	out := &safeBuffer{}
	conn.writer = bufio.NewWriter(out)
	connector.mu.Lock()
	connector.ready = true
	connector.mu.Unlock()

	// Two unlinked accounts on one login; the config pin is the second.
	conn.processMessage(conn.encodeMsg(msgManagedAccts, "1", scopeTestManaged))

	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	connector.acctUpdatesNow = func() time.Time { return now }
	if err := connector.RequestAccountUpdates(scopeTestIndividual); err != nil {
		t.Fatalf("initial RequestAccountUpdates: %v", err)
	}

	// Past the resubscribe throttle, drive the scope-conflict self-heal.
	now = now.Add(2 * acctUpdatesResubscribeThrottle)
	connector.maybeResubscribeAccountUpdatesForScopeConflict()

	frames := decodeOutboundFrames(t, conn, out.Bytes())
	if len(frames) != 2 {
		t.Fatalf("outbound reqAcctData frames = %d, want 2: %#v", len(frames), frames)
	}
	for i, frame := range frames {
		// reqAcctData payload is [msgID, version, subscribe, acctCode].
		if len(frame) < 4 {
			t.Fatalf("frame %d = %#v, want at least 4 fields", i, frame)
		}
		if frame[3] != scopeTestIndividual {
			t.Fatalf("frame %d subscribed account = %q, want the pinned %q", i, frame[3], scopeTestIndividual)
		}
	}
}
