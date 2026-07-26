package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestParseNasdaqRegSHOThresholdList(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"CRWV|CoreWeave Inc.|Q|Y|N",
		"MSFT|Microsoft Corp.|Q|N|N",
		"20260605180000",
		"",
	}, "\n"))
	got, err := parseNasdaqRegSHO(raw)
	if err != nil {
		t.Fatalf("parse Reg SHO: %v", err)
	}
	if _, ok := got.Symbols["CRWV"]; !ok {
		t.Fatalf("CRWV threshold row missing: %+v", got.Symbols)
	}
	if _, ok := got.Symbols["MSFT"]; ok {
		t.Fatalf("non-threshold row should not be flagged: %+v", got.Symbols)
	}
	if got.AsOf.IsZero() {
		t.Fatalf("as_of not parsed")
	}
}

func TestParseNasdaqRegSHOAllowsValidEmptyThresholdList(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"MSFT|Microsoft Corp.|Q|N|N",
		"20260605180000",
		"",
	}, "\n"))
	got, err := parseNasdaqRegSHO(raw)
	if err != nil {
		t.Fatalf("parse empty threshold list: %v", err)
	}
	if len(got.Symbols) != 0 {
		t.Fatalf("threshold symbols=%+v, want empty", got.Symbols)
	}
}

func TestParseNasdaqTradeHaltsClassifiesActiveAndRecent(t *testing.T) {
	t.Parallel()
	raw := strings.NewReader(`<?xml version="1.0" encoding="UTF-8"?>
<rss><channel>
<pubDate>Fri, 05 Jun 2026 14:30:00 GMT</pubDate>
<item>
	<IssueSymbol>CRWV</IssueSymbol>
	<IssueName>CoreWeave Inc.</IssueName>
	<Market>NASDAQ</Market>
	<ReasonCode>T7</ReasonCode>
	<HaltDate>06/05/2026</HaltDate>
	<HaltTime>10:15:00.000</HaltTime>
	<PauseThresholdPrice>123.45</PauseThresholdPrice>
</item>
<item>
	<IssueSymbol>MSFT</IssueSymbol>
	<IssueName>Microsoft Corp.</IssueName>
	<Market>NASDAQ</Market>
	<ReasonCode>T1</ReasonCode>
	<HaltDate>06/05/2026</HaltDate>
	<HaltTime>09:15:00.000</HaltTime>
	<ResumptionDate>06/05/2026</ResumptionDate>
	<ResumptionTradeTime>09:45:00.000</ResumptionTradeTime>
</item>
</channel></rss>`)
	entry, err := parseNasdaqTradeHalts(raw)
	if err != nil {
		t.Fatalf("parse halts: %v", err)
	}
	if len(entry.Records) != 2 {
		t.Fatalf("records=%d, want 2", len(entry.Records))
	}
	now := time.Date(2026, 6, 5, 16, 0, 0, 0, time.UTC)
	luld, ok := marketEventHaltFlag("CRWV", entry.Records[0], entry, now)
	if !ok || luld.ID != rpc.MarketEventLULDRecent || luld.Status != rpc.MarketEventStatusActive || luld.Severity != rpc.MarketEventSeverityBlock {
		t.Fatalf("active LULD flag = %+v ok=%v", luld, ok)
	}
	halt, ok := marketEventHaltFlag("MSFT", entry.Records[1], entry, now)
	if !ok || halt.ID != rpc.MarketEventHaltRegulatoryOrNews || halt.Status != rpc.MarketEventStatusRecent || halt.Severity != rpc.MarketEventSeverityWatch {
		t.Fatalf("recent halt flag = %+v ok=%v", halt, ok)
	}
}

func TestMarketEventBorrowInventoryFlagThresholds(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	if _, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableShares: 500}, now); ok {
		t.Fatal("unobserved shortable shares should be unknown, not false-active")
	}
	flag, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 500}, now)
	if !ok || flag.Severity != rpc.MarketEventSeverityAct || flag.Label != "Borrow scarce" {
		t.Fatalf("scarce flag = %+v ok=%v", flag, ok)
	}
	flag, ok = marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 5_000}, now)
	if !ok || flag.Severity != rpc.MarketEventSeverityWatch || flag.Label != "Borrow tight" {
		t.Fatalf("tight flag = %+v ok=%v", flag, ok)
	}
	if _, ok := marketEventBorrowInventoryFlag("CRWV", ibkrlib.MarketData{ShortableObserved: true, ShortableShares: 50_000}, now); ok {
		t.Fatal("ample borrow inventory should not emit an inactive false flag")
	}
}

func TestParseIBKRBorrowFeesAndEmitExtremeFlag(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	raw := strings.NewReader(strings.Join([]string{
		"#BOF|2026.06.06|11:45:03",
		"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
		"CRWV|USD|COREWEAVE INC|123456789|US0000000001|-70.2500|75.2500|1500|",
		"MSFT|USD|MICROSOFT CORP|272093|US5949181045|4.7500|0.2500|8000000|",
		"",
	}, "\n"))
	entry, err := parseIBKRBorrowFees(raw)
	if err != nil {
		t.Fatalf("parse IBKR borrow fees: %v", err)
	}
	if entry.AsOf.IsZero() {
		t.Fatal("as_of not parsed")
	}
	flag, ok := marketEventBorrowFeeFlag("CRWV", entry.Symbols["CRWV"], entry, now)
	if !ok {
		t.Fatal("expected borrow_fee_extreme flag")
	}
	if flag.ID != rpc.MarketEventBorrowFeeExtreme || flag.Severity != rpc.MarketEventSeverityAct || flag.Role != rpc.MarketEventRoleProposalModifier {
		t.Fatalf("borrow fee flag classification = %+v", flag)
	}
	if flag.Unit != "pct_annualized" || flag.Value == nil || *flag.Value != 75.25 {
		t.Fatalf("borrow fee value/unit = value %v unit %q", flag.Value, flag.Unit)
	}
	if flag.AsOf.IsZero() || flag.Source == "" {
		t.Fatalf("borrow fee source/as_of missing: %+v", flag)
	}
	if _, ok := marketEventBorrowFeeFlag("MSFT", entry.Symbols["MSFT"], entry, now); ok {
		t.Fatal("low borrow fee should not emit an inactive false flag")
	}
}

func TestParseIBKRBorrowFeesRejectsMalformedEnvelopes(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"missing BOF": strings.Join([]string{
			"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
			"CRWV|USD|COREWEAVE INC|123|US0000000001|-70|75|1500|",
		}, "\n"),
		"invalid BOF": strings.Join([]string{
			"#BOF|not-a-date|not-a-time",
			"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
			"CRWV|USD|COREWEAVE INC|123|US0000000001|-70|75|1500|",
		}, "\n"),
		"missing header": strings.Join([]string{
			"#BOF|2026.06.06|11:45:03",
			"CRWV|USD|COREWEAVE INC|123|US0000000001|-70|75|1500|",
		}, "\n"),
		"invalid header": strings.Join([]string{
			"#BOF|2026.06.06|11:45:03",
			"#SYM|CUR|NAME|CON|ISIN|REBATERATE|CHANGED|AVAILABLE|",
			"CRWV|USD|COREWEAVE INC|123|US0000000001|-70|75|1500|",
		}, "\n"),
		"zero usable rows": strings.Join([]string{
			"#BOF|2026.06.06|11:45:03",
			"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
			"CRWV|USD|COREWEAVE INC|123|US0000000001|-70|not-a-fee|1500|",
		}, "\n"),
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := parseIBKRBorrowFees(strings.NewReader(raw))
			var sourceErr *borrowFeeFetchError
			if !errors.As(err, &sourceErr) {
				t.Fatalf("error=%v, want typed borrow-fee failure", err)
			}
			if sourceErr.code != rpc.SourceFailureInvalidPayload || sourceErr.stage != rpc.SourceFailureStageBorrowParse {
				t.Fatalf("typed failure=%+v", sourceErr)
			}
		})
	}
}

func TestFTPPassiveAddrRejectsOutOfRangeParts(t *testing.T) {
	t.Parallel()

	got, err := ftpPassiveAddr("227 Entering Passive Mode (127,0,0,1,195,80)")
	if err != nil {
		t.Fatalf("valid PASV address: %v", err)
	}
	if got != "127.0.0.1:50000" {
		t.Fatalf("valid PASV address = %q, want 127.0.0.1:50000", got)
	}

	for _, line := range []string{
		"227 Entering Passive Mode (256,0,0,1,1,2)",
		"227 Entering Passive Mode (127,0,0,999,1,2)",
		"227 Entering Passive Mode (127,0,0,1,1,999)",
	} {
		if _, err := ftpPassiveAddr(line); err == nil {
			t.Fatalf("ftpPassiveAddr(%q) succeeded; want byte-range error", line)
		}
	}
}

func TestMarketEventBorrowFeesSnapshotIndexesBySymbol(t *testing.T) {
	now := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.regSHO = marketEventRegSHOEntry{FetchedAt: now, AsOf: now, Symbols: map[string]marketEventRegSHORecord{}}
	cache.halts = marketEventHaltsEntry{FetchedAt: now, AsOf: now}
	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: now,
		AsOf:      now.Add(-5 * time.Minute),
		SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols: map[string]marketEventBorrowFeeRecord{
			"CRWV": {Symbol: "CRWV", Currency: "USD", Name: "COREWEAVE INC", FeeRate: 55.5, Available: 10_000},
		},
	}

	res := cache.snapshot(context.Background(), []string{"CRWV"}, nil, nil)
	flags := res.BySymbol["CRWV"]
	if len(flags) == 0 {
		t.Fatalf("by_symbol missing CRWV flag: %+v", res)
	}
	found := false
	for _, flag := range flags {
		if flag.ID == rpc.MarketEventBorrowFeeExtreme {
			found = true
			if flag.Value == nil || *flag.Value != 55.5 {
				t.Fatalf("borrow fee flag value = %v, want 55.5", flag.Value)
			}
		}
	}
	if !found {
		t.Fatalf("borrow_fee_extreme flag missing from by_symbol: %+v", flags)
	}
}

func TestMarketEventBorrowFeeStaleCacheFallback(t *testing.T) {
	now := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: now.Add(-marketEventsBorrowFeeFreshFor - time.Minute),
		AsOf:      now.Add(-marketEventsBorrowFeeFreshFor - time.Minute),
		SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols:   map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
	}
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		return marketEventBorrowFeeEntry{}, errors.New("network down")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	entry, health, err := cache.loadBorrowFees(context.Background(), now)
	if err != nil {
		t.Fatalf("stale borrow-fee cache fallback should not fail: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("cached symbol missing: %+v", entry.Symbols)
	}
	if health.Status != rpc.SourceStatusStale || health.RefreshState != rpc.SourceRefreshFetchFailed || health.NextAttempt == nil {
		t.Fatalf("health=%+v, want stale first-fetch failure with retry", health)
	}
}

func TestMarketEventBorrowFeeNotDueSkipsFetch(t *testing.T) {
	now := time.Date(2026, 7, 20, 5, 5, 0, 0, time.UTC) // Monday 01:05 ET.
	cache := newMarketEventCache(func() time.Time { return now })
	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, errors.New("must not fetch before the regular session")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	entry, health, err := cache.loadBorrowFees(context.Background(), now)
	if err != nil || len(entry.Symbols) != 0 || fetchCalls != 0 {
		t.Fatalf("not-due read entry=%+v health=%+v calls=%d err=%v", entry, health, fetchCalls, err)
	}
	if health.Status != rpc.SourceStatusUnknown || health.RefreshState != rpc.SourceRefreshNotDue || health.NextAttempt != nil {
		t.Fatalf("not-due health=%+v", health)
	}

	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC), AsOf: time.Date(2026, 7, 17, 18, 0, 0, 0, time.UTC),
		Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
	}
	entry, health, err = cache.loadBorrowFees(context.Background(), now)
	if err != nil || len(entry.Symbols) != 1 || fetchCalls != 0 || health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue {
		t.Fatalf("not-due last-good entry=%+v health=%+v calls=%d err=%v", entry, health, fetchCalls, err)
	}
	tuesdayPreopen := time.Date(2026, 7, 21, 5, 5, 0, 0, time.UTC)
	entry, health, err = cache.loadBorrowFees(context.Background(), tuesdayPreopen)
	if err != nil || len(entry.Symbols) != 1 || fetchCalls != 0 || health.Status != rpc.SourceStatusStale || health.RefreshState != rpc.SourceRefreshNotDue {
		t.Fatalf("missed-session last-good entry=%+v health=%+v calls=%d err=%v", entry, health, fetchCalls, err)
	}
}

func TestMarketEventFingerprintIgnoresBorrowFeeTimestampChurn(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	build := func(asOf time.Time) rpc.MarketEventsResult {
		value := 75.25
		res := rpc.MarketEventsResult{
			Kind:          rpc.MarketEventsKind,
			SchemaVersion: rpc.MarketEventsSchemaVersion,
			AsOf:          now,
			Symbols:       []string{"CRWV"},
			Flags: []rpc.MarketEventFlag{{
				ID:         rpc.MarketEventBorrowFeeExtreme,
				Symbol:     "CRWV",
				Label:      "Fee extreme",
				Status:     rpc.MarketEventStatusActive,
				Severity:   rpc.MarketEventSeverityAct,
				Role:       rpc.MarketEventRoleProposalModifier,
				Source:     "IBKR short stock availability",
				AsOf:       asOf,
				ObservedAt: now,
				Value:      &value,
				Unit:       "pct_annualized",
			}},
		}
		res.Fingerprint = rpc.BuildMarketEventsFingerprint(&res)
		return res
	}
	a := build(now.Add(-time.Minute))
	b := build(now.Add(-2 * time.Minute))
	if !reflect.DeepEqual(a.Flags[0].Value, b.Flags[0].Value) {
		t.Fatal("test setup value mismatch")
	}
	if a.Fingerprint.Key != b.Fingerprint.Key {
		t.Fatalf("fingerprint churned on timestamp only: %q != %q", a.Fingerprint.Key, b.Fingerprint.Key)
	}
}

func TestMarketEventRegSHOStaleCacheFallback(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.regSHO = marketEventRegSHOEntry{
		FetchedAt: now.Add(-13 * time.Hour),
		AsOf:      now.Add(-13 * time.Hour),
		Symbols:   map[string]marketEventRegSHORecord{"CRWV": {Symbol: "CRWV"}},
	}
	orig := marketEventsHTTPClient
	marketEventsHTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	t.Cleanup(func() { marketEventsHTTPClient = orig })

	entry, health, err := cache.loadRegSHO(context.Background(), now)
	if err != nil {
		t.Fatalf("stale cache fallback should not fail: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("cached symbol missing: %+v", entry.Symbols)
	}
	if health.Status != rpc.SourceStatusStale {
		t.Fatalf("health status=%q, want stale", health.Status)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestMarketEventCacheShortableAbsence pins the TTL'd negative cache
// behind the borrow-inventory polls: a symbol observed absent is skipped
// for marketEventsShortableAbsentRetry, then re-probed (a pre-market
// absence must not blind borrow inventory for the whole trading day),
// and a gateway reconnect (clearShortableAbsence) re-arms the probe
// immediately. Without this memory, every market-events snapshot
// re-burned the full poll budget per non-US name whose shortable tick
// never arrives.
func TestMarketEventCacheShortableAbsence(t *testing.T) {
	t.Parallel()
	cache := newMarketEventCache(nil)
	observedAt := time.Date(2026, 6, 11, 8, 30, 0, 0, time.UTC)
	withinTTL := observedAt.Add(marketEventsShortableAbsentRetry - time.Minute)
	pastTTL := observedAt.Add(marketEventsShortableAbsentRetry)

	if cache.shortableAbsentRecently("DTE", observedAt) {
		t.Fatal("fresh cache should not report absence")
	}
	cache.rememberShortableAbsent("DTE", observedAt)
	if !cache.shortableAbsentRecently("DTE", withinTTL) {
		t.Error("absence within the retry TTL should skip the probe")
	}
	if cache.shortableAbsentRecently("DTE", pastTTL) {
		t.Error("absence past the retry TTL must re-arm the probe")
	}
	if cache.shortableAbsentRecently("SAP", withinTTL) {
		t.Error("absence is per-symbol")
	}

	cache.rememberShortableAbsent("DTE", observedAt)
	cache.clearShortableAbsence()
	if cache.shortableAbsentRecently("DTE", withinTTL) {
		t.Error("clearShortableAbsence (reconnect) should re-arm the probe")
	}
}

// TestMarketEventBorrowFeeFailureMemory pins the retry-suppression
// window: after a failed fetch (observed live: ftp3.interactivebrokers
// port 21 filtered → full dial-timeout hang), snapshots within
// marketEventsBorrowFeeRetryAfter must NOT re-attempt the fetch — the
// hang was being re-paid on every canary run. Past the window, exactly
// one retry fires; success clears the memory.
func TestMarketEventBorrowFeeFailureMemory(t *testing.T) {
	now := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })

	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, errors.New("dial tcp: i/o timeout")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	if _, health, err := cache.loadBorrowFees(context.Background(), now); err == nil || health.Status != rpc.SourceStatusUnknown || health.RefreshState != rpc.SourceRefreshFetchFailed || health.NextAttempt == nil {
		t.Fatalf("first failure: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 1 {
		t.Fatalf("first call: fetch ran %d times, want 1", fetchCalls)
	}

	// Within the retry window: no fetch attempt, same degraded health.
	within := now.Add(marketEventsBorrowFeeRetryAfter - time.Minute)
	if _, health, err := cache.loadBorrowFees(context.Background(), within); err == nil || health.Status != rpc.SourceStatusUnknown || health.RefreshState != rpc.SourceRefreshFetchFailedBackoff || health.NextAttempt == nil {
		t.Fatalf("suppressed call: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 1 {
		t.Fatalf("suppressed call must not re-fetch: fetch ran %d times", fetchCalls)
	}

	// Past the window: one retry, now succeeding, clears the memory.
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{
			AsOf:      now.Add(marketEventsBorrowFeeRetryAfter + time.Minute),
			SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
			Symbols:   map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
		}, nil
	}
	past := now.Add(marketEventsBorrowFeeRetryAfter + time.Minute)
	if _, health, err := cache.loadBorrowFees(context.Background(), past); err != nil || health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshCurrent || health.NextAttempt != nil {
		t.Fatalf("recovery call: err=%v status=%s", err, health.Status)
	}
	if fetchCalls != 2 {
		t.Fatalf("recovery: fetch ran %d times, want 2", fetchCalls)
	}
	if cache.borrowFeesLastAttempt == nil || cache.borrowFeesLastAttempt.Outcome != marketEventBorrowFeeOutcomeSuccess || cache.borrowFeesLastAttempt.Failure != nil {
		t.Fatalf("success should supersede failure memory: %+v", cache.borrowFeesLastAttempt)
	}

	// Stale-cache variant: a later failure serves the cached list
	// without retrying inside the window.
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, errors.New("network down again")
	}
	expired := past.Add(marketEventsBorrowFeeFreshFor + time.Minute)
	if entry, health, err := cache.loadBorrowFees(context.Background(), expired); err != nil || health.Status != rpc.SourceStatusStale || health.RefreshState != rpc.SourceRefreshFetchFailed || health.NextAttempt == nil || len(entry.Symbols) == 0 {
		t.Fatalf("stale fallback after failure: err=%v status=%s symbols=%d", err, health.Status, len(entry.Symbols))
	}
	if fetchCalls != 3 {
		t.Fatalf("stale-fallback failure: fetch ran %d times, want 3", fetchCalls)
	}
	if entry, health, err := cache.loadBorrowFees(context.Background(), expired.Add(time.Minute)); err != nil || health.Status != rpc.SourceStatusStale || health.RefreshState != rpc.SourceRefreshFetchFailedBackoff || health.NextAttempt == nil || len(entry.Symbols) == 0 {
		t.Fatalf("suppressed stale fallback: err=%v status=%s symbols=%d", err, health.Status, len(entry.Symbols))
	}
	if fetchCalls != 3 {
		t.Fatalf("suppressed stale fallback must not re-fetch: fetch ran %d times", fetchCalls)
	}
}

func TestMarketEventBorrowFeeFreshnessUsesProviderAsOf(t *testing.T) {
	now := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	cache.borrowFees = marketEventBorrowFeeEntry{
		FetchedAt: now,
		AsOf:      now.Add(-marketEventsBorrowFeeFreshFor - time.Minute),
		SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols:   map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
	}
	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{
			AsOf: now, SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
			Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65}},
		}, nil
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	_, health, err := cache.loadBorrowFees(context.Background(), now)
	if err != nil || fetchCalls != 1 || health.Status != rpc.SourceStatusOK {
		t.Fatalf("provider-age refresh calls=%d health=%+v err=%v", fetchCalls, health, err)
	}
}

func TestMarketEventBorrowFeeFailurePersistsAcrossRestartAndSuccessSupersedes(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	failedAt := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return failedAt })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}

	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{}, newBorrowFeeFetchError(rpc.SourceFailureTimeout, rpc.SourceFailureStageFTPControlConnect, true)
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	_, health, err := cache.loadBorrowFees(context.Background(), failedAt)
	if err == nil || fetchCalls != 1 || health.LastFailure == nil {
		t.Fatalf("first failure calls=%d health=%+v err=%v", fetchCalls, health, err)
	}
	if health.LastFailure.Code != rpc.SourceFailureTimeout || health.LastFailure.Stage != rpc.SourceFailureStageFTPControlConnect || !health.LastFailure.Retryable {
		t.Fatalf("typed failure=%+v", health.LastFailure)
	}
	if strings.Contains(strings.Join(health.Notes, " "), "dial tcp") {
		t.Fatalf("raw transport text crossed health boundary: %+v", health.Notes)
	}

	failureObservation, ok, err := authority.LatestObservation(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind)
	if err != nil || !ok {
		t.Fatalf("failure observation ok=%v err=%v", ok, err)
	}
	if strings.Contains(string(failureObservation.Payload), "dial tcp") {
		t.Fatalf("raw transport text persisted: %s", failureObservation.Payload)
	}
	var outcome marketEventBorrowFeeOutcome
	if err := json.Unmarshal(failureObservation.Payload, &outcome); err != nil {
		t.Fatalf("decode failure outcome: %v", err)
	}
	if outcome.Attempt.Failure == nil || outcome.Attempt.Failure.Code != rpc.SourceFailureTimeout {
		t.Fatalf("persisted failure outcome=%+v", outcome)
	}

	within := failedAt.Add(time.Minute)
	restarted := newMarketEventCache(func() time.Time { return within })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	_, health, err = restarted.loadBorrowFees(context.Background(), within)
	if err == nil || fetchCalls != 1 || health.RefreshState != rpc.SourceRefreshFetchFailedBackoff || health.LastFailure == nil {
		t.Fatalf("restart backoff calls=%d health=%+v err=%v", fetchCalls, health, err)
	}

	recoveredAt := failedAt.Add(marketEventsBorrowFeeRetryAfter + time.Minute)
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return marketEventBorrowFeeEntry{
			AsOf: recoveredAt, SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
			Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65, Available: 1500}},
		}, nil
	}
	entry, health, err := restarted.loadBorrowFees(context.Background(), recoveredAt)
	if err != nil || fetchCalls != 2 || len(entry.Symbols) != 1 || health.LastFailure != nil || health.Status != rpc.SourceStatusOK {
		t.Fatalf("recovery calls=%d entry=%+v health=%+v err=%v", fetchCalls, entry, health, err)
	}

	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: marketEventBorrowFeesScope, Source: marketEventBorrowFeesSource, Kind: marketEventBorrowFeesObservationKind,
	})
	if err != nil || len(observations) != 2 {
		t.Fatalf("fetch outcome observations=%d err=%v", len(observations), err)
	}
	doc, ok, err := authority.GetStateDocument(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
	if err != nil || !ok {
		t.Fatalf("borrow-fee state ok=%v err=%v", ok, err)
	}
	var state marketEventBorrowFeesState
	if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
		t.Fatalf("decode recovered state: %v", err)
	}
	if state.Version != marketEventBorrowFeesStateVersion || state.LastAttempt == nil || state.LastAttempt.Outcome != marketEventBorrowFeeOutcomeSuccess || state.LastAttempt.Failure != nil {
		t.Fatalf("success did not supersede failure: %+v", state)
	}

	afterRestart := newMarketEventCache(func() time.Time { return recoveredAt.Add(time.Minute) })
	if err := afterRestart.UseCoreStore(authority); err != nil {
		t.Fatalf("post-recovery restart UseCoreStore: %v", err)
	}
	_, health, err = afterRestart.loadBorrowFees(context.Background(), recoveredAt.Add(time.Minute))
	if err != nil || fetchCalls != 2 || health.LastFailure != nil || health.Status != rpc.SourceStatusOK {
		t.Fatalf("post-recovery restart calls=%d health=%+v err=%v", fetchCalls, health, err)
	}
}

func TestMarketEventBorrowFeeDownloadParseSQLiteReopenRecovery(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure test corestore dir: %v", err)
	}
	path := filepath.Join(dir, "daemon.db")
	authority, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("open corestore: %v", err)
	}
	authorityOpen := true
	t.Cleanup(func() {
		if authorityOpen {
			_ = authority.Close()
		}
	})

	now := time.Date(2026, 6, 8, 16, 1, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	raw := strings.Join([]string{
		"#BOF|2026.06.08|12:00:00",
		"#SYM|CUR|NAME|CON|ISIN|REBATERATE|FEERATE|AVAILABLE|",
		"CRWV|USD|COREWEAVE INC|123456789|US0000000001|-70.2500|75.2500|1500|",
		"MSFT|USD|MICROSOFT CORP|272093|US5949181045|4.7500|0.2500|8000000|",
	}, "\n")
	var fetchCalls int
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		fetchCalls++
		return parseIBKRBorrowFeeDownload(raw, "ftp://ftp3.interactivebrokers.com/usa.txt")
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })

	entry, health, err := cache.loadBorrowFees(context.Background(), now)
	if err != nil || fetchCalls != 1 || len(entry.Symbols) != 2 || health.Status != rpc.SourceStatusOK {
		t.Fatalf("download/parse/persist calls=%d entry=%+v health=%+v err=%v", fetchCalls, entry, health, err)
	}
	if entry.AsOf != time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC) {
		t.Fatalf("provider as_of=%s", entry.AsOf)
	}
	if _, ok, err := authority.LatestObservation(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind); err != nil || !ok {
		t.Fatalf("fetch outcome observation ok=%v err=%v", ok, err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("close corestore: %v", err)
	}
	authorityOpen = false

	reopened, err := corestore.Open(context.Background(), corestore.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen corestore: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	afterRestart := newMarketEventCache(func() time.Time { return now.Add(2 * time.Minute) })
	if err := afterRestart.UseCoreStore(reopened); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	recovered, health, err := afterRestart.loadBorrowFees(context.Background(), now.Add(2*time.Minute))
	if err != nil || fetchCalls != 1 || len(recovered.Symbols) != 2 || health.Status != rpc.SourceStatusOK || health.LastFailure != nil {
		t.Fatalf("restart recovery calls=%d entry=%+v health=%+v err=%v", fetchCalls, recovered, health, err)
	}
}

func TestMarketEventBorrowFeeDoesNotPublishMemoryWhenAuthorityCommitFails(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	now := time.Date(2026, 6, 8, 16, 0, 0, 0, time.UTC)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	old := marketEventBorrowFeeEntry{
		FetchedAt: now.Add(-time.Hour), AsOf: now.Add(-time.Hour), SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols: map[string]marketEventBorrowFeeRecord{"OLD": {Symbol: "OLD", FeeRate: 65, Available: 10}},
	}
	if err := cache.persistBorrowFees(context.Background(), old); err != nil {
		t.Fatalf("persist initial last-good: %v", err)
	}
	orig := fetchIBKRBorrowFees
	fetchIBKRBorrowFees = func(context.Context) (marketEventBorrowFeeEntry, error) {
		return marketEventBorrowFeeEntry{
			AsOf: now, SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
			Symbols: map[string]marketEventBorrowFeeRecord{"NEW": {Symbol: "NEW", FeeRate: 75, Available: 20}},
		}, nil
	}
	t.Cleanup(func() { fetchIBKRBorrowFees = orig })
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	entry, health, err := cache.loadBorrowFees(canceled, now)
	if err != nil {
		t.Fatalf("stale last-good fallback should remain usable: %v", err)
	}
	if health.LastFailure == nil || health.LastFailure.Code != rpc.SourceFailureAuthorityWriteFailed || health.LastFailure.Stage != rpc.SourceFailureStageAuthorityPersist {
		t.Fatalf("authority failure health=%+v", health)
	}
	if _, ok := entry.Symbols["OLD"]; !ok {
		t.Fatalf("returned last-good was replaced before commit: %+v", entry.Symbols)
	}
	if _, ok := cache.borrowFees.Symbols["OLD"]; !ok {
		t.Fatalf("memory was replaced before commit: %+v", cache.borrowFees.Symbols)
	}
	if _, ok := cache.borrowFees.Symbols["NEW"]; ok {
		t.Fatalf("uncommitted entry leaked into memory: %+v", cache.borrowFees.Symbols)
	}
	observations, err := authority.ListObservations(context.Background(), corestore.ObservationQuery{
		ScopeKey: marketEventBorrowFeesScope, Source: marketEventBorrowFeesSource, Kind: marketEventBorrowFeesObservationKind,
	})
	if err != nil || len(observations) != 1 {
		t.Fatalf("failed commit appended an observation: count=%d err=%v", len(observations), err)
	}
}

func TestMarketEventBorrowFeeAuthorityMigratesV1InPlace(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	legacy := marketEventBorrowFeesStateV1{Version: marketEventStateVersion, Entry: marketEventBorrowFeeEntry{
		FetchedAt: now, AsOf: now.Add(-time.Minute), SourceURL: "ftp://ftp3.interactivebrokers.com/usa.txt",
		Symbols: map[string]marketEventBorrowFeeRecord{"CRWV": {Symbol: "CRWV", FeeRate: 65, Available: 1500}},
	}}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	writeMalformedMarketState(t, authority, marketEventBorrowFeesScope, marketEventBorrowFeesStateKind, raw)

	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("migrate v1 borrow-fee authority: %v", err)
	}
	if _, ok := cache.borrowFees.Symbols["CRWV"]; !ok {
		t.Fatalf("migrated last-good missing: %+v", cache.borrowFees)
	}
	doc, ok, err := authority.GetStateDocument(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesStateKind)
	if err != nil || !ok {
		t.Fatalf("migrated document ok=%v err=%v", ok, err)
	}
	var state marketEventBorrowFeesState
	if err := decodeStrictMarketEventJSON(doc.JSON, &state); err != nil {
		t.Fatalf("decode migrated state: %v", err)
	}
	if state.Version != marketEventBorrowFeesStateVersion || state.LastGood == nil || state.LastAttempt != nil {
		t.Fatalf("migrated state=%+v", state)
	}
	if _, ok, err := authority.LatestObservation(context.Background(), marketEventBorrowFeesScope, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind); err != nil || ok {
		t.Fatalf("schema-only migration must not invent a fetch observation: ok=%v err=%v", ok, err)
	}
}

// TestRegSHODatedWalkSkipsRedirect pins the 302 handling: Nasdaq
// redirects missing dated threshold files to an HTML error page, and
// following it parsed as an EMPTY success — caching "no threshold
// symbols" for 12h and never reaching the newest real file. The
// no-redirect policy turns the 302 into a status error so the walk
// proceeds to the prior day.
func TestRegSHODatedWalkSkipsRedirect(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	body := strings.Join([]string{
		"Symbol|Security Name|Market Category|Reg SHO Threshold Flag|Rule3210",
		"CRWV|CoreWeave Inc.|Q|Y|N",
		"20260609180000",
		"",
	}, "\n")
	orig := marketEventsHTTPClient
	marketEventsHTTPClient = &http.Client{
		CheckRedirect: marketEventsNoRedirect,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.Contains(req.URL.Path, "20260610") {
				return &http.Response{
					StatusCode: http.StatusFound,
					Header:     http.Header{"Location": []string{"https://www.nasdaqtrader.com/Error.aspx"}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			}
			if strings.Contains(req.URL.Path, "20260609") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			}
			return nil, errors.New("unexpected URL " + req.URL.String())
		}),
	}
	t.Cleanup(func() { marketEventsHTTPClient = orig })

	entry, err := fetchLatestNasdaqRegSHO(context.Background(), now)
	if err != nil {
		t.Fatalf("dated walk should land on the prior day's file: %v", err)
	}
	if _, ok := entry.Symbols["CRWV"]; !ok {
		t.Fatalf("expected the 2026-06-09 file's symbols, got %+v", entry.Symbols)
	}
}

func TestMarketEventSQLitePersistsNormalizedSnapshotsAndRestarts(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	cache := newMarketEventCache(func() time.Time { return now })
	if err := cache.UseCoreStore(authority); err != nil {
		t.Fatalf("UseCoreStore: %v", err)
	}
	regSHO := marketEventRegSHOEntry{
		FetchedAt: now, AsOf: now.Add(-time.Hour), SourceURL: "https://example.test/regsho.txt",
		Symbols: map[string]marketEventRegSHORecord{
			"CRWV": {Symbol: "CRWV", SecurityName: "CoreWeave"},
		},
	}
	halts := marketEventHaltsEntry{
		FetchedAt: now, AsOf: now, SourceURL: "https://example.test/halts.xml",
		Records: []marketEventHaltRecord{{Symbol: "CRWV", ReasonCode: "T1", HaltedAt: now.Add(-time.Minute)}},
	}
	fees := marketEventBorrowFeeEntry{
		FetchedAt: now, AsOf: now.Add(-time.Minute), SourceURL: "ftp://example.test/usa.txt",
		Symbols: map[string]marketEventBorrowFeeRecord{
			"CRWV": {Symbol: "CRWV", Currency: "USD", FeeRate: 55, Available: 1000},
		},
	}
	inventory := map[string]marketEventBorrowInventoryRecord{
		"CRWV": {Symbol: "CRWV", ShortableShares: 500, AsOf: now, DataType: "live"},
	}
	if err := cache.persistRegSHO(context.Background(), regSHO); err != nil {
		t.Fatalf("persist Reg SHO: %v", err)
	}
	if err := cache.persistHalts(context.Background(), halts); err != nil {
		t.Fatalf("persist halts: %v", err)
	}
	if err := cache.persistBorrowFees(context.Background(), fees); err != nil {
		t.Fatalf("persist borrow fees: %v", err)
	}
	if err := cache.persistBorrowInventory(context.Background(), now, inventory); err != nil {
		t.Fatalf("persist borrow inventory: %v", err)
	}
	assertStateAndObservation(t, authority, marketEventRegSHOScope, marketEventRegSHOStateKind, marketEventRegSHOSource, marketEventRegSHOObservationKind)
	assertStateAndObservation(t, authority, marketEventHaltsScope, marketEventHaltsStateKind, marketEventHaltsSource, marketEventHaltsObservationKind)
	assertStateAndObservation(t, authority, marketEventBorrowFeesScope, marketEventBorrowFeesStateKind, marketEventBorrowFeesSource, marketEventBorrowFeesObservationKind)
	assertStateAndObservation(t, authority, marketEventBorrowInventoryScope, marketEventBorrowInventoryStateKind, marketEventBorrowInventorySource, marketEventBorrowInventoryObservationKind)

	restarted := newMarketEventCache(func() time.Time { return now.Add(time.Hour) })
	if err := restarted.UseCoreStore(authority); err != nil {
		t.Fatalf("restart UseCoreStore: %v", err)
	}
	if _, ok := restarted.regSHO.Symbols["CRWV"]; !ok {
		t.Fatalf("Reg SHO state did not survive restart: %+v", restarted.regSHO)
	}
	if len(restarted.halts.Records) != 1 || restarted.halts.Records[0].Symbol != "CRWV" {
		t.Fatalf("halts state did not survive restart: %+v", restarted.halts)
	}
	if _, ok := restarted.borrowFees.Symbols["CRWV"]; !ok {
		t.Fatalf("borrow-fee state did not survive restart: %+v", restarted.borrowFees)
	}
	if restarted.shortableAbsent != nil || !restarted.regSHOFailedAt.IsZero() || !restarted.haltsFailedAt.IsZero() {
		t.Fatal("ephemeral retry/absence state should not persist")
	}
	if restarted.borrowFeesLastAttempt == nil || restarted.borrowFeesLastAttempt.Outcome != marketEventBorrowFeeOutcomeSuccess {
		t.Fatalf("borrow-fee success attempt did not survive restart: %+v", restarted.borrowFeesLastAttempt)
	}
}

func TestMarketEventSQLiteRejectsMalformedRowWithoutReplacingCache(t *testing.T) {
	authority := openMarketTestCoreStore(t)
	writeMalformedMarketState(t, authority, marketEventRegSHOScope, marketEventRegSHOStateKind, []byte(`{"version":1,"entry":{"fetched_at":"2026-07-20T12:00:00Z","as_of":"2026-07-20T11:00:00Z","source_url":"https://example.test","symbols":{"CRWV":{"symbol":"WRONG"}}}}`))
	cache := newMarketEventCache(nil)
	cache.regSHO = marketEventRegSHOEntry{Symbols: map[string]marketEventRegSHORecord{"KEEP": {Symbol: "KEEP"}}}
	if err := cache.UseCoreStore(authority); err == nil {
		t.Fatal("malformed market-event row attached")
	}
	if _, ok := cache.regSHO.Symbols["KEEP"]; !ok {
		t.Fatal("failed attachment mutated existing cache projection")
	}
}
