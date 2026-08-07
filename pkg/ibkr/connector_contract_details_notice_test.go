package ibkr

import (
	"bufio"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests pin the contract-details half of the system-notice recovery
// path. A reqContractDetails reqID lives in neither historicalReqs nor
// reqIDMap, so before contractDetailsReqs existed a code-200 answering one was
// invisible: the caller burned its whole budget and reported
// ErrContractDetailsTimeout, and nothing counted the rejection. A held defunct
// stock therefore re-resolved forever — 778 abort cycles in one 4-hour
// afternoon, each preceded by the broker's definitive answer.

func TestSystemNoticeFailsPendingContractDetails(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	req, release := c.registerContractDetailsRequest(41, "HGENQ")
	defer release()

	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: 41,
		code:     200,
		message:  "No security definition has been found for the request",
	})

	select {
	case err := <-req.fail:
		if !errors.Is(err, ErrContractNoDefinition) {
			t.Fatalf("expected ErrContractNoDefinition, got %v", err)
		}
	default:
		t.Fatal("contract-details request not failed by system notice")
	}
}

// A definition rejection must stay distinguishable from silence: callers and
// the daemon log branch on which one they got.
func TestContractDetailsRejectionIsNotATimeout(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	req, release := c.registerContractDetailsRequest(42, "HGENQ")
	defer release()

	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: 42,
		code:     200,
		message:  "No security definition has been found for the request",
	})

	err := <-req.fail
	if errors.Is(err, ErrContractDetailsTimeout) {
		t.Fatal("a definition rejection must not read as a timeout")
	}
}

func TestInformationalNoticeLeavesContractDetailsPending(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	req, release := c.registerContractDetailsRequest(43, "SPY")
	defer release()

	c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "STK"}, &systemNotification{
		tickerID: 43,
		code:     2106,
		message:  "HMDS data farm connection is OK",
	})

	select {
	case err := <-req.fail:
		t.Fatalf("informational notice must not fail the request, got %v", err)
	default:
	}
}

// A released request must not be failed by a late notice reusing its reqID.
func TestContractDetailsNoticeIgnoredAfterRelease(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	req, release := c.registerContractDetailsRequest(44, "HGENQ")
	release()

	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: 44,
		code:     200,
		message:  "No security definition has been found for the request",
	})

	select {
	case err := <-req.fail:
		t.Fatalf("released request must not be failed, got %v", err)
	default:
	}
}

// Contract-details rejections are the only definition evidence a name that
// never reaches a market-data subscription produces, so they must feed the
// guarded candidate — under the same two-in-ten-minutes rule as subscription
// rejections, and under a key the resolve paths check.
func TestContractDetailsDefinitionErrorNeedsTwoConfirmations(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	reject := func(reqID int) {
		req, release := c.registerContractDetailsRequest(reqID, "HGENQ")
		defer release()
		c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
			tickerID: int64(reqID),
			code:     200,
			message:  "No security definition has been found for the request",
		})
		<-req.fail
	}

	reject(51)
	if c.IsSymbolInactive("HGENQ") {
		t.Fatal("a single definition rejection must not mark; transients are routine")
	}

	reject(52)
	if !c.IsSymbolInactive("HGENQ") {
		t.Fatal("two rejections inside the window must mark")
	}

	if _, err := c.FetchContractDetails("HGENQ", time.Second); !errors.Is(err, ErrSymbolInactive) {
		t.Fatalf("resolve path must short-circuit on the mark, got %v", err)
	}
}

// Wedge replay: while the gateway answers "no security definition" for
// everything because its backend link is down, nothing may be marked. This is
// the 2026-07-08 failure (held AMD/BB/IBM and VIX marked) reaching the
// contract-details path, which previously could not mark at all and so was
// never covered.
func TestContractDetailsDefinitionErrorIgnoredWhileFarmImpaired(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	now := time.Now()

	c.processSystemNotice(reqAliasEntry{}, &systemNotification{
		tickerID: -1, code: 1100,
		message:   "Connectivity between IBKR and Trader Workstation has been lost.",
		timestamp: now,
	})

	for _, reqID := range []int{61, 62, 63} {
		req, release := c.registerContractDetailsRequest(reqID, "IBM")
		c.processSystemNotice(reqAliasEntry{symbol: "IBM", secType: "STK"}, &systemNotification{
			tickerID: int64(reqID), code: 200,
			message:   "No security definition has been found for the request",
			timestamp: now,
		})
		// Failing fast is still correct here — the caller should not wait on
		// an answer that already arrived. Only the verdict is withheld.
		if err := <-req.fail; !errors.Is(err, ErrContractNoDefinition) {
			t.Fatalf("expected ErrContractNoDefinition, got %v", err)
		}
		release()
	}

	if c.IsSymbolInactive("IBM") {
		reason, _ := c.InactiveReason("IBM")
		t.Fatalf("held name marked inactive during connectivity-lost window; reason=%q", reason)
	}
}

// Derivative probes legitimately request unlisted contracts: option fan-outs
// walk strike supersets and FX resolution quotes both pair directions. Their
// rejections are routine and must not accumulate toward a mark.
func TestContractDetailsDerivativeRejectionDoesNotMark(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)

	for _, secType := range []string{"OPT", "CASH"} {
		key := resolutionKeyForSecType("SPY", secType)
		if key != "" {
			t.Fatalf("secType %s must not carry a resolution key, got %q", secType, key)
		}
	}

	for _, reqID := range []int{71, 72, 73} {
		req, release := c.registerContractDetailsRequest(reqID, resolutionKeyForSecType("SPY", "OPT"))
		c.processSystemNotice(reqAliasEntry{symbol: "SPY", secType: "OPT"}, &systemNotification{
			tickerID: int64(reqID), code: 200,
			message: "No security definition has been found for the request",
		})
		<-req.fail
		release()
	}

	if c.IsSymbolInactive("SPY") {
		t.Fatal("option-probe rejections must not mark the underlying")
	}
}

func TestConcurrentContractResolutionSharesTerminalHGENQResultWithoutSlots(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	var out safeBuffer
	c.conn.writer = bufio.NewWriter(&out)
	setServerVersionReady(c.conn, minServerVersionRequired)

	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			<-start
			_, err := c.FetchContractDetails("HGENQ", time.Second)
			results <- err
		}()
	}
	close(start)
	reqID := waitForSharedContractDetailsFlight(t, c, "symbol\x00HGENQ", 2)
	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: int64(reqID), code: 200, message: "No security definition has been found for the request",
	})
	for range 2 {
		if err := <-results; !errors.Is(err, ErrContractNoDefinition) {
			t.Fatalf("shared terminal result=%v", err)
		}
	}
	if c.IsSymbolInactive("HGENQ") {
		t.Fatal("one shared broker answer counted as multiple inactive confirmations")
	}
	if got := c.conn.rateLimiter.marketDataSubs.Count(); got != 0 {
		t.Fatalf("contract resolution consumed market-data slots: %d", got)
	}

	second := make(chan error, 1)
	go func() {
		_, err := c.FetchContractDetails("HGENQ", time.Second)
		second <- err
	}()
	secondReqID := waitForSharedContractDetailsFlight(t, c, "symbol\x00HGENQ", 1)
	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: int64(secondReqID), code: 200, message: "No security definition has been found for the request",
	})
	if err := <-second; !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("second terminal result=%v", err)
	}
	if !c.IsSymbolInactive("HGENQ") {
		t.Fatal("two independent healthy-farm broker answers did not mark HGENQ inactive")
	}
}

func TestContractResolutionLeaderTimeoutDoesNotEndSharedWireFlight(t *testing.T) {
	c := readyBrokerEvidenceTestConnector(t)
	var out safeBuffer
	c.conn.writer = bufio.NewWriter(&out)
	setServerVersionReady(c.conn, minServerVersionRequired)

	leader := make(chan error, 1)
	go func() {
		_, err := c.FetchContractDetails("HGENQ", 20*time.Millisecond)
		leader <- err
	}()
	reqID := waitForSharedContractDetailsFlight(t, c, "symbol\x00HGENQ", 1)

	follower := make(chan error, 1)
	go func() {
		_, err := c.FetchContractDetails("HGENQ", time.Second)
		follower <- err
	}()
	waitForSharedContractDetailsFlight(t, c, "symbol\x00HGENQ", 2)
	if err := <-leader; !errors.Is(err, ErrContractDetailsTimeout) {
		t.Fatalf("short-budget leader error=%v, want timeout", err)
	}

	c.processSystemNotice(reqAliasEntry{symbol: "HGENQ", secType: "STK"}, &systemNotification{
		tickerID: int64(reqID), code: 200, message: "No security definition has been found for the request",
	})
	if err := <-follower; !errors.Is(err, ErrContractNoDefinition) {
		t.Fatalf("long-budget follower error=%v, want shared terminal result", err)
	}
	if c.IsSymbolInactive("HGENQ") {
		t.Fatal("one shared terminal broker answer counted as multiple inactive confirmations")
	}
	if got := c.conn.rateLimiter.marketDataSubs.Count(); got != 0 {
		t.Fatalf("contract resolution consumed market-data slots: %d", got)
	}
}

func waitForSharedContractDetailsFlight(t *testing.T, c *Connector, key string, waiters int) int {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.contractDetailsFlightMu.Lock()
		flight := c.contractDetailsFlights[key]
		joined := flight != nil && flight.waiters >= waiters
		c.contractDetailsFlightMu.Unlock()
		if joined {
			c.contractDetailsMu.Lock()
			for reqID := range c.contractDetailsReqs {
				c.contractDetailsMu.Unlock()
				return reqID
			}
			c.contractDetailsMu.Unlock()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("contract-details flight %q did not reach %d waiters", key, waiters)
	return 0
}

func TestContractWarningCoalescerSummarizesRepeats(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { c.conn.rateLimiter.Stop() })
	now := time.Date(2026, time.August, 7, 8, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	c.contractWarningNow = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	if got, emit := c.coalesceContractWarning("HGENQ", time.Minute, "unresolved HGENQ"); !emit || got != "unresolved HGENQ" {
		t.Fatalf("first warning=(%q,%v)", got, emit)
	}
	for range 3 {
		if got, emit := c.coalesceContractWarning("HGENQ", time.Minute, "unresolved HGENQ"); emit || got != "" {
			t.Fatalf("repeat warning=(%q,%v)", got, emit)
		}
	}
	mu.Lock()
	now = now.Add(time.Minute)
	mu.Unlock()
	got, emit := c.coalesceContractWarning("HGENQ", time.Minute, "unresolved HGENQ")
	if !emit || !strings.Contains(got, "3 identical warnings suppressed") {
		t.Fatalf("summary warning=(%q,%v)", got, emit)
	}
}
