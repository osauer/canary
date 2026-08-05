package daemon

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestParseNasdaqEarnings(t *testing.T) {
	now := time.Date(2026, 7, 7, 8, 0, 0, 0, time.UTC)
	providerSymbol := "TESTQ"
	body := nasdaqTestPayload(t, map[string]any{
		"announcement": nasdaqAnnouncementPrefix(providerSymbol) + " Jul 22, 2026",
	}, 200)
	entry, err := parseNasdaqEarnings(body, providerSymbol, now)
	if err != nil {
		t.Fatalf("parse synthetic fixture: %v", err)
	}
	if entry.Date != "2026-07-22" || entry.TimeOfDay != "" || entry.Estimated {
		t.Fatal("synthetic fixture did not produce only the typed date")
	}

	if _, err := parseNasdaqEarnings(nasdaqTestPayload(t, map[string]any{"announcement": "untrusted text"}, 200), providerSymbol, now); err == nil {
		t.Fatal("missing date must be an error, never a guessed date")
	}
	if _, err := parseNasdaqEarnings([]byte(`not json`), providerSymbol, now); err == nil {
		t.Fatal("malformed payload must be an error")
	}
	badDate := nasdaqAnnouncementPrefix(providerSymbol) + " Julember 40, 2026"
	if _, err := parseNasdaqEarnings(nasdaqTestPayload(t, map[string]any{"announcement": badDate}, 200), providerSymbol, now); err == nil {
		t.Fatal("unparseable date must be an error")
	}
}

func TestNasdaqSymbolMapping(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "TESTQ", "TESTQ"},
		{"trim and uppercase", " testq ", "TESTQ"},
		{"broker class space", "TEST Q", "TEST.Q"},
		{"interior dot", "TEST.Q", "TEST.Q"},
		{"interior hyphen", "TEST-Q", "TEST-Q"},
		{"digit", "TEST2", "TEST2"},
		{"empty", "", ""},
		{"slash", "TEST/Q", ""},
		{"backslash", `TEST\Q`, ""},
		{"question mark", "TEST?Q", ""},
		{"percent", "TEST%Q", ""},
		{"quote", `TEST"Q`, ""},
		{"colon", "TEST:Q", ""},
		{"wildcard", "TEST*Q", ""},
		{"interior control", "TEST\nQ", ""},
		{"leading control", "\tTESTQ", ""},
		{"trailing control", "TESTQ\x7f", ""},
		{"underscore", "TEST_Q", ""},
		{"leading dot", ".TESTQ", ""},
		{"leading hyphen", "-TESTQ", ""},
		{"trailing dot", "TESTQ.", ""},
		{"trailing hyphen", "TESTQ-", ""},
		{"repeated punctuation", "TEST.-Q", ""},
		{"repeated broker spaces", "TEST  Q", ""},
		{"non ascii", "TESTÄ", ""},
		{"non ascii padding", "\u00a0TESTQ", ""},
		{"too long", strings.Repeat("A", nasdaqProviderSymbolMaxLen+1), ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := nasdaqSymbol(test.in); got != test.want {
				t.Fatal("symbol mapping mismatch")
			}
		})
	}
}

func TestCloneRulesResultDeepCopiesEarningsIdentity(t *testing.T) {
	now := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	next := now.Add(5 * time.Minute)
	in := &rpc.RulesResult{Earnings: []rpc.EarningsInfo{{Identity: &rpc.EarningsIdentityInfo{
		Outcome: earningsIdentityUnknown, NotApplicable: true, AttemptedAt: now, NextAttempt: &next,
		LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureContractUnavailable, Stage: rpc.SourceFailureStageWSHContractResolve, FailedAt: now},
	}}}}
	out := cloneRulesResult(in)
	out.Earnings[0].Identity.Outcome = earningsIdentityIssuer
	*out.Earnings[0].Identity.NextAttempt = next.Add(time.Hour)
	out.Earnings[0].Identity.LastFailure.Code = rpc.SourceFailureInvalidPayload
	if in.Earnings[0].Identity.Outcome != earningsIdentityUnknown || !in.Earnings[0].Identity.NextAttempt.Equal(next) ||
		in.Earnings[0].Identity.LastFailure.Code != rpc.SourceFailureContractUnavailable {
		t.Fatal("rules clone aliased nested earnings identity state")
	}
}

func TestParseEarningsOverride(t *testing.T) {
	loc, _ := time.LoadLocation("America/New_York")
	e, ok := parseEarningsOverride("2026-07-22Tamc", loc)
	if !ok || e.Date.Format("2006-01-02") != "2026-07-22" || e.TimeOfDay != "amc" {
		t.Fatalf("override parse = %+v ok=%v", e, ok)
	}
	if _, ok := parseEarningsOverride("July 22", loc); ok {
		t.Fatal("bad override must not parse")
	}
	if e, ok := parseEarningsOverride("2026-07-22", loc); !ok || e.TimeOfDay != "" {
		t.Fatalf("date-only override = %+v ok=%v, want empty time_of_day", e, ok)
	}
}

func TestAssembleEarningsPropagatesTypedUnknownAndSourceHealth(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	next := now.Add(earningsFreshWindow)
	provider := earningsProviderState{LastAttempt: earningsProviderAttempt{
		Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, NextAttempt: &next,
	}}
	providers := map[string]earningsProviderState{earningsNasdaqProvider: provider}
	cache := newEarningsCacheMemory(nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["NOW"] = earningsSymbolState{
		Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now,
	}
	srv := &Server{earnings: cache}
	earnings, infos := srv.assembleEarnings(context.Background(), []risk.NameInput{{Symbol: "NOW"}}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusNoDatePublished || infos[0].Reason != rpc.EarningsStatusNoDatePublished || infos[0].Source != "fetched" {
		t.Fatalf("typed earnings info = %+v", infos)
	}
	if got := earnings["NOW"]; got.Known || got.Reason != rpc.EarningsStatusNoDatePublished {
		t.Fatalf("risk earnings input = %+v", got)
	}
	// Operator policy (2026-08-03): the sole configured vendor's definitive
	// no-date verdict is accepted — the gap is disclosed in the notes and the
	// name's own rules stay unknown, but the source is not degraded.
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded || health.Status != rpc.SourceStatusOK || len(health.Notes) != 1 ||
		!strings.Contains(health.Notes[0], rpc.EarningsStatusNoDatePublished) || !strings.Contains(health.Notes[0], "vendor definitive") {
		t.Fatalf("earnings source health = %+v degraded=%v", health, degraded)
	}
}

func TestAssembleEarningsRetriesDueProviderBehindFreshAggregate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	primaryNext := now.Add(earningsFreshWindow)
	secondaryDue := now.Add(-time.Minute)
	entry := earningsEntry{Date: "2026-07-30", TimeOfDay: "amc", ObservedAt: now}
	providers := map[string]earningsProviderState{
		earningsNasdaqProvider: {
			LastAttempt: earningsProviderAttempt{
				Status: rpc.EarningsStatusDate, Entry: &entry,
				AttemptedAt: now, CompletedAt: now, NextAttempt: &primaryNext,
			},
			LastGood: &entry,
		},
		earningsWSHProvider: {
			LastAttempt: earningsProviderAttempt{
				Status:      rpc.EarningsStatusTransportFailure,
				AttemptedAt: now.Add(-earningsFailureRetry), CompletedAt: now.Add(-earningsFailureRetry),
				NextAttempt: &secondaryDue,
				LastFailure: &rpc.SourceFailure{
					Code: rpc.SourceFailureTimeout, Stage: rpc.SourceFailureStageWSHEvent,
					FailedAt: now.Add(-earningsFailureRetry), Retryable: true,
				},
			},
		},
	}
	cache := newEarningsCache(t.TempDir(), nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["AAPL"] = earningsSymbolState{
		Resolution: resolveEarningsProviders(providers, now),
		Providers:  providers,
		UpdatedAt:  now,
	}
	called := make(chan struct{}, 1)
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		called <- struct{}{}
		return earningsProviderFetchResult{Status: rpc.EarningsStatusDate, Entry: entry}, nil
	}); err != nil {
		t.Fatal(err)
	}

	srv := &Server{earnings: cache}
	srv.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "AAPL"}}, risk.DefaultRulebookPolicy(), marketcal.New(), now, true)
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("due WSH retry was suppressed by the fresh single-source aggregate")
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		view, ok := cache.resolution("AAPL")
		if ok && view.Reason == earningsReasonConsensus {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("due WSH retry did not commit provider agreement: %+v", view)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAssembleEarningsUsesPersistedExactContractTerminalEvidence(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	terminal := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now)))
	if err := terminal.UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	srv := &Server{earningsTerminal: terminal}
	name := risk.NameInput{Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK"}
	earnings, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{name}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusTerminalNonReporting ||
		infos[0].Source != "verified_terminal" || infos[0].Terminal == nil ||
		infos[0].Terminal.AuthorityRevision != 1 {
		t.Fatalf("terminal earnings info = %+v", infos)
	}
	if got := earnings["ACMEQ"]; !got.TerminalNonReporting || got.Known || got.Source != "verified_terminal" {
		t.Fatalf("terminal risk input = %+v", got)
	}
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded || health.Status != rpc.SourceStatusOK {
		t.Fatalf("terminal source health = %+v degraded=%v", health, degraded)
	}
}

func TestAssembleEarningsTerminalConflictsFailClosed(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	authority := openMarketTestCoreStore(t)
	terminal := newEarningsTerminalStore(writeTerminalImport(t, syntheticTerminalDocument(now)))
	if err := terminal.UseCoreStore(t.Context(), authority, now); err != nil {
		t.Fatal(err)
	}
	name := risk.NameInput{Symbol: "ACMEQ", StockConID: 1001, StockSecType: "STK"}

	t.Run("provider publishes date", func(t *testing.T) {
		cache := newEarningsCacheMemory(nil)
		cache.clock = func() time.Time { return now }
		entry := earningsEntry{Date: "2026-08-01", ObservedAt: now}
		next := now.Add(earningsFreshWindow)
		providers := map[string]earningsProviderState{earningsNasdaqProvider: {
			LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusDate, Entry: &entry, AttemptedAt: now, CompletedAt: now, NextAttempt: &next},
			LastGood:    &entry,
		}}
		cache.symbols["ACMEQ"] = earningsSymbolState{Resolution: resolveEarningsProviders(providers, now), Providers: providers, UpdatedAt: now}
		srv := &Server{earnings: cache, earningsTerminal: terminal}
		earnings, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{name}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
		if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusConflictingSources || infos[0].Reason != earningsTerminalReasonSourceConflict {
			t.Fatalf("provider conflict info = %+v", infos)
		}
		if got := earnings["ACMEQ"]; got.Known || got.TerminalNonReporting {
			t.Fatalf("provider conflict became usable = %+v", got)
		}
	})

	t.Run("operator override publishes date", func(t *testing.T) {
		srv := &Server{
			earningsTerminal: terminal,
			platformSettings: &platformSettingsStore{data: platformSettingsData{Version: platformSettingsDocVersion, Features: platformFeatureSettingsData{
				Rulebook: platformRulebookSettingsData{EarningsOverrides: map[string]string{"ACMEQ": "2026-08-01"}},
			}}},
		}
		earnings, infos := srv.assembleEarnings(t.Context(), []risk.NameInput{name}, risk.DefaultRulebookPolicy(), marketcal.New(), now, false)
		if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusConflictingSources || infos[0].Reason != earningsTerminalReasonSourceConflict {
			t.Fatalf("override conflict info = %+v", infos)
		}
		if got := earnings["ACMEQ"]; got.Known || got.TerminalNonReporting {
			t.Fatalf("override conflict became usable = %+v", got)
		}
	})
}

func TestSessionsUntilCountsTradingDays(t *testing.T) {
	cal := marketcal.New()
	loc, _ := time.LoadLocation("America/New_York")
	// Tue 2026-07-07 → Thu 2026-07-09: Tue+Wed+Thu = 3 sessions.
	got := sessionsUntil(cal, time.Date(2026, 7, 7, 9, 0, 0, 0, loc), time.Date(2026, 7, 9, 0, 0, 0, 0, loc))
	if got == nil || *got != 3 {
		t.Fatalf("sessionsUntil Tue→Thu = %v, want 3", got)
	}
	// Fri 2026-07-10 → Mon 2026-07-13 skips the weekend: 2 sessions.
	got = sessionsUntil(cal, time.Date(2026, 7, 10, 9, 0, 0, 0, loc), time.Date(2026, 7, 13, 0, 0, 0, 0, loc))
	if got == nil || *got != 2 {
		t.Fatalf("sessionsUntil Fri→Mon = %v, want 2", got)
	}
	if got := sessionsUntil(cal, time.Date(2026, 7, 10, 9, 0, 0, 0, loc), time.Date(2026, 7, 1, 0, 0, 0, 0, loc)); got != nil {
		t.Fatalf("past target = %v, want nil", got)
	}
}

func TestRulebookPortfolioSourceHealthRequiresCurrentScopedReceipt(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	tests := []struct {
		name       string
		receipt    ibkrlib.PortfolioStreamHealth
		wantStatus string
		wantReason string
		wantOK     bool
	}{
		{
			name: "current empty snapshot is trustworthy",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Minute),
				InitialCompletedAt: now.Add(-30 * time.Second)},
			wantStatus: rpc.SourceStatusOK, wantOK: true,
		},
		{
			name: "receipt completed after evaluation start but before read completion",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Second),
				InitialCompletedAt: now.Add(-100 * time.Millisecond)},
			wantStatus: rpc.SourceStatusOK, wantOK: true,
		},
		{
			name: "silent cached snapshot is stale",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Hour),
				InitialCompletedAt: now.Add(-portfolioStreamReceiptMaxAge - time.Second)},
			wantStatus: rpc.SourceStatusStale, wantReason: "positions_stale",
		},
		{
			name: "reconnect account mismatch is unavailable",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU999", RequestedAt: now.Add(-time.Minute),
				InitialCompletedAt: now.Add(-30 * time.Second)},
			wantStatus: "unavailable", wantReason: "positions_unavailable",
		},
		{
			name: "latched account scope conflict is unavailable",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Minute),
				InitialCompletedAt: now.Add(-30 * time.Second), ScopeConflictAt: now},
			wantStatus: "unavailable", wantReason: "positions_unavailable",
		},
		{
			name:       "initial snapshot in flight is pending",
			receipt:    ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-30 * time.Second)},
			wantStatus: "pending", wantReason: "positions_pending",
		},
		{
			name: "future receipt is unavailable",
			receipt: ibkrlib.PortfolioStreamHealth{Account: "DU123", RequestedAt: now.Add(-time.Minute),
				InitialCompletedAt: now.Add(time.Second)},
			wantStatus: "unavailable", wantReason: "positions_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, health := rulebookPortfolioSourceHealth(scope, test.receipt, now)
			if state.Healthy != test.wantOK || state.Reason != test.wantReason || health.Status != test.wantStatus {
				t.Fatalf("state=%+v health=%+v", state, health)
			}
			if health.MaxAgeSeconds != int64(portfolioStreamReceiptMaxAge/time.Second) {
				t.Fatalf("max age = %d", health.MaxAgeSeconds)
			}
		})
	}
}

func TestRulebookPortfolioProjectionRejectsForeignAccountRows(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	health := ibkrlib.PortfolioStreamHealth{
		Account: "DU123", RequestedAt: now.Add(-time.Minute), InitialCompletedAt: now.Add(-30 * time.Second),
	}
	rows := []*ibkrlib.RawPosition{{
		Account: "DU999", Contract: ibkrlib.Contract{ConID: 1001, Symbol: "FOREIGN", SecType: "STK"}, Position: 1,
	}}

	health, scoped := scopedPortfolioStreamHealth(rows, health, scope, now)
	if scoped || health.ScopeConflictAt.IsZero() {
		t.Fatalf("foreign-account Rulebook projection scoped=%t health=%+v, want latched conflict", scoped, health)
	}
	state, source := rulebookPortfolioSourceHealth(scope, health, now)
	if state.Healthy || state.Reason != "positions_unavailable" || source.Status != "unavailable" {
		t.Fatalf("foreign-account Rulebook health state=%+v source=%+v, want unavailable", state, source)
	}
}

func TestRulebookTapeSourceHealthRTHAndOffHours(t *testing.T) {
	now := time.Date(2026, 7, 21, 15, 0, 0, 0, time.UTC)
	dayChange := -1.25
	tests := []struct {
		name             string
		includeTape      bool
		sessionOpen      bool
		positionsHealthy bool
		dayChange        *float64
		wantStatus       string
		wantRefresh      string
	}{
		{name: "RTH current quote", includeTape: true, sessionOpen: true, positionsHealthy: true, dayChange: &dayChange, wantStatus: rpc.SourceStatusOK},
		{name: "RTH quote unavailable", includeTape: true, sessionOpen: true, positionsHealthy: true, wantStatus: "unavailable"},
		{name: "RTH portfolio unavailable", includeTape: true, sessionOpen: true, wantStatus: "unavailable"},
		{name: "off-hours typed not due", includeTape: true, positionsHealthy: true, wantStatus: rpc.SourceStatusOK, wantRefresh: rpc.SourceRefreshNotDue},
		{name: "noncanonical read", sessionOpen: true, positionsHealthy: true, dayChange: &dayChange, wantStatus: "unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			health := rulebookTapeSourceHealth(test.includeTape, test.sessionOpen, test.positionsHealthy, test.dayChange, now)
			if health.Source != "tape" || health.Status != test.wantStatus || health.RefreshState != test.wantRefresh {
				t.Fatalf("tape health=%+v", health)
			}
		})
	}
}

func TestRulebookUnavailableResultCoversFiveInputClasses(t *testing.T) {
	result := (&Server{}).rulebookUnavailableResult("canonical_cache_missing_or_stale")
	seen := map[string]bool{}
	for _, health := range result.InputHealth {
		seen[health.Source] = health.Status == "unavailable"
	}
	for _, source := range []string{"account", "positions", "earnings", "regime_stage", "tape"} {
		if !seen[source] {
			t.Fatalf("unavailable result missing %s: %+v", source, result.InputHealth)
		}
	}
	if result.Status != "degraded" || len(result.Rules) != 0 {
		t.Fatalf("unavailable result=%+v", result)
	}
}

func TestRulebookCacheInvalidatesAcrossConnectorEpoch(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, func() time.Time { return now })
	binding := server.currentRulebookBinding()
	result := alertShadowTestRulebook(now, risk.RuleStatusPass)
	if !server.cacheRulebookResult(&result, binding, now) {
		t.Fatal("failed to cache scoped result")
	}
	if _, ok := server.cachedRulebookResult(binding, rulesPreviewTTL, now); !ok {
		t.Fatal("same-generation cache unexpectedly unavailable")
	}
	server.mu.Lock()
	server.connectorEpoch++
	server.mu.Unlock()
	if _, ok := server.cachedRulebookResult(server.currentRulebookBinding(), rulesPreviewTTL, now); ok {
		t.Fatal("pre-reconnect Rulebook cache survived connector generation change")
	}
}

func TestRulebookAccountSourceHealthRequiresFreshCompleteOneShot(t *testing.T) {
	completedAt := time.Date(2026, 7, 21, 15, 0, 2, 0, time.UTC)
	requestAt := completedAt.Add(-time.Second)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	complete := &rpc.AccountResult{AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(0.0)}
	completeAuthority := accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: requestAt,
		NetLiquidationAvailable: true, TotalCashAvailable: true, BaseCurrencyAvailable: true,
	}

	tests := []struct {
		name       string
		account    *rpc.AccountResult
		authority  accountSummaryAuthority
		wantOK     bool
		wantStatus string
		wantReason string
	}{
		{name: "fresh complete request", account: complete, authority: completeAuthority, wantOK: true, wantStatus: rpc.SourceStatusOK},
		{
			name: "fresh complete request permits negative finite cash",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: -2500, DailyPnL: new(-125.0),
			},
			authority: completeAuthority, wantOK: true, wantStatus: rpc.SourceStatusOK,
		},
		{
			name: "unstamped cached fallback", account: complete,
			authority: accountSummaryAuthority{
				Provenance: ibkrlib.AccountSummaryProvenanceCachedFallback, AsOf: requestAt,
				NetLiquidationAvailable: true, TotalCashAvailable: true, BaseCurrencyAvailable: true,
			},
			wantStatus: rpc.SourceStatusDegraded, wantReason: "account_cached_fallback",
		},
		{
			name: "fresh response missing net liquidation", account: complete,
			authority: func() accountSummaryAuthority {
				partial := completeAuthority
				partial.NetLiquidationAvailable = false
				return partial
			}(),
			wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name:    "fresh response missing base currency",
			account: &rpc.AccountResult{AccountID: "DU123", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(0.0)},
			authority: func() accountSummaryAuthority {
				partial := completeAuthority
				partial.BaseCurrencyAvailable = false
				return partial
			}(),
			wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "fresh response missing cash presence", account: complete,
			authority: func() accountSummaryAuthority {
				partial := completeAuthority
				partial.TotalCashAvailable = false
				return partial
			}(),
			wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "non-finite net liquidation",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: math.NaN(), TotalCash: 0, DailyPnL: new(0.0),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "non-positive net liquidation",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 0, TotalCash: 0, DailyPnL: new(0.0),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "non-finite cash",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: math.Inf(1), DailyPnL: new(0.0),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "missing daily P&L",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 0,
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "non-finite daily P&L",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(math.NaN()),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "infinite daily P&L",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(math.Inf(1)),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "malformed base currency",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "EU1", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(0.0),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "placeholder base currency",
			account: &rpc.AccountResult{
				AccountID: "DU123", BaseCurrency: "BASE", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(0.0),
			},
			authority: completeAuthority, wantStatus: rpc.SourceStatusDegraded, wantReason: "account_incomplete",
		},
		{
			name: "future request receipt", account: complete,
			authority: func() accountSummaryAuthority {
				future := completeAuthority
				future.AsOf = completedAt.Add(time.Second)
				return future
			}(),
			wantStatus: "unavailable", wantReason: "account_unavailable",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, health := rulebookAccountSourceHealth(scope, test.account, test.authority, true, completedAt)
			if state.Healthy != test.wantOK || state.Reason != test.wantReason || health.Status != test.wantStatus {
				t.Fatalf("state=%+v health=%+v", state, health)
			}
			if test.wantStatus == rpc.SourceStatusDegraded && !rulebookInputHealthDegraded([]rpc.SourceHealth{health}) {
				t.Fatalf("degraded account health would not degrade the Rulebook envelope: %+v", health)
			}
		})
	}
}

// Issue #14: a multi-account login's snapshot was stamped with the comma-joined
// managedAccounts aggregate. That is not a concrete account, so account health
// failed closed on every evaluation — all account-dependent rules came back
// unknown behind a note about another broker account, while the account surface
// still answered. The aggregate must keep failing closed, and a snapshot
// carrying the pinned account it actually describes must not.
func TestRulebookAccountSourceHealthAcceptsPinnedMultiAccountLabel(t *testing.T) {
	completedAt := time.Date(2026, 7, 21, 15, 0, 2, 0, time.UTC)
	scope := brokerStateScope{Account: "DU2222222", Mode: rpc.AccountModePaper}
	authority := accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: completedAt.Add(-time.Second),
		NetLiquidationAvailable: true, TotalCashAvailable: true, BaseCurrencyAvailable: true,
	}
	account := func(id string) *rpc.AccountResult {
		return &rpc.AccountResult{AccountID: id, BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 0, DailyPnL: new(0.0)}
	}

	state, health := rulebookAccountSourceHealth(scope, account("DU1111111,DU2222222"), authority, true, completedAt)
	if state.Healthy || state.Reason != "account_unavailable" || health.Status != "unavailable" {
		t.Fatalf("managed-account aggregate state=%+v health=%+v, want fail-closed account authority", state, health)
	}

	state, health = rulebookAccountSourceHealth(scope, account(scope.Account), authority, true, completedAt)
	if !state.Healthy || state.Reason != "" || health.Status != rpc.SourceStatusOK {
		t.Fatalf("pinned account state=%+v health=%+v, want a healthy account source", state, health)
	}

	cached := authority
	cached.Provenance = ibkrlib.AccountSummaryProvenanceCachedFallback
	state, health = rulebookAccountSourceHealth(scope, account(scope.Account), cached, true, completedAt)
	if state.Healthy || state.Reason != "account_cached_fallback" || health.Status != rpc.SourceStatusDegraded {
		t.Fatalf("cached fallback state=%+v health=%+v, want the provenance note rather than a foreign-account refusal", state, health)
	}
}

func TestRulebookAccountSourceHealthMissingDailyPnLPostCloseIsNotDue(t *testing.T) {
	completedAt := time.Date(2026, 7, 21, 21, 0, 2, 0, time.UTC) // Tuesday 17:00 ET.
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	account := &rpc.AccountResult{AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 10000}
	account.DailyPnLObservation = &rpc.DailyPnLObservation{
		Status: rpc.DailyPnLObservationNotDue, SessionKey: "2026-07-21", AsOf: completedAt,
	}
	authority := accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: completedAt.Add(-time.Second),
		NetLiquidationAvailable: true, TotalCashAvailable: true, BaseCurrencyAvailable: true,
	}
	state, health := rulebookAccountSourceHealth(scope, account, authority, false, completedAt)
	if !state.Healthy || state.Reason != "" || health.Status != rpc.SourceStatusOK || health.RefreshState != rpc.SourceRefreshNotDue {
		t.Fatalf("state=%+v health=%+v, want healthy account with daily P&L not due", state, health)
	}

	evaluation := risk.EvaluateRulebook(risk.RuleInputs{
		AsOf: completedAt, Account: state, Positions: risk.SourceState{Healthy: true},
		NLVBase: new(100000.0), CashBase: new(10000.0), DailyPnLBase: nil,
	}, risk.DefaultRulebookPolicy())
	var concentration, greenDay *risk.RuleRow
	for i := range evaluation.Rows {
		switch evaluation.Rows[i].ID {
		case risk.RuleSingleNameExposure:
			concentration = &evaluation.Rows[i]
		case risk.RuleGreenDayAction:
			greenDay = &evaluation.Rows[i]
		}
	}
	if concentration == nil || concentration.Status != risk.RuleStatusPass {
		t.Fatalf("non-P&L concentration rule = %+v, want evaluated pass", concentration)
	}
	if greenDay == nil || greenDay.Status != risk.RuleStatusNotEvaluated || greenDay.Reason != risk.RuleReasonPnLUnavailable {
		t.Fatalf("P&L-only rule = %+v, want localized not_evaluated/%s", greenDay, risk.RuleReasonPnLUnavailable)
	}
}

func TestRulebookAccountSourceHealthRetainsDailyPnLFailurePostClose(t *testing.T) {
	completedAt := time.Date(2026, 7, 21, 21, 0, 2, 0, time.UTC)
	scope := brokerStateScope{Account: "DU123", Mode: rpc.AccountModePaper}
	account := &rpc.AccountResult{
		AccountID: "DU123", BaseCurrency: "EUR", NetLiquidation: 100000, TotalCash: 10000,
		DailyPnLObservation: &rpc.DailyPnLObservation{
			Status: rpc.DailyPnLObservationMissing, SessionKey: "2026-07-21", AsOf: completedAt.Add(-time.Hour),
		},
	}
	authority := accountSummaryAuthority{
		Provenance: ibkrlib.AccountSummaryProvenanceRequest, AsOf: completedAt.Add(-time.Second),
		NetLiquidationAvailable: true, TotalCashAvailable: true, BaseCurrencyAvailable: true,
	}
	state, health := rulebookAccountSourceHealth(scope, account, authority, false, completedAt)
	if state.Healthy || state.Reason != "account_incomplete" || health.Status != rpc.SourceStatusDegraded || health.RefreshState == rpc.SourceRefreshNotDue {
		t.Fatalf("state=%+v health=%+v, want retained degraded Daily P&L failure", state, health)
	}
}

func TestRulesForPreviewReturnsTypedUnavailableWhenBusyUntilDeadline(t *testing.T) {
	server := &Server{}
	attachAlertShadowCadenceTestAuthority(t, server, time.Now)
	binding := server.currentRulebookBinding()
	stale := &rpc.RulesResult{Enabled: true, Status: "ok"}
	if !server.cacheRulebookResult(stale, binding, time.Now().Add(-rulesPreviewTTL-time.Second)) {
		t.Fatal("failed to seed stale scoped cache")
	}

	server.rulesEvaluationMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	got := server.rulesForPreview(ctx)
	cancel()
	server.rulesEvaluationMu.Unlock()
	if got == nil || got.Status != "degraded" || len(got.Rules) != 0 {
		t.Fatalf("busy preview result=%+v, want typed unavailable advisory input", got)
	}
	warnings := rulebookPreviewWarnings(got, rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "NOW", SecType: "STK"}}, rpc.OrderPositionImpact{Effect: "increase"})
	if codes := warningCodes(warnings); !codes["rulebook_unavailable"] {
		t.Fatalf("busy preview silently omitted Rulebook warning: %+v", warnings)
	}

	fresh := &rpc.RulesResult{Enabled: true, Status: "degraded"}
	if !server.cacheRulebookResult(fresh, binding, time.Now()) {
		t.Fatal("failed to seed fresh scoped cache")
	}
	if got := server.rulesForPreview(t.Context()); got == nil || got.Status != fresh.Status {
		t.Fatalf("fresh preview cache = %+v, want status %q", got, fresh.Status)
	}
}

// TestMapRuleNamesExposureMatchesGroups is the aggregation-consistency
// gate: rule 1 must read the identical delta-dollar exposure the canary's
// concentration check reads (GroupDollarDeltaBase). Bars may differ across
// surfaces; observations may not.
func TestMapRuleNamesExposureMatchesGroups(t *testing.T) {
	pos := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:           "NOW",
				GroupDollarDeltaBase: new(380000.0),
				GroupMarketValueBase: new(120000.0),
				Stock:                &rpc.PositionView{Symbol: "NOW", SecType: "STK", ConID: 1234, Quantity: 500, DayChangePct: new(1.6)},
				Options: []rpc.PositionView{{
					Symbol: "NOW", SecType: "OPTION", Quantity: 50, Multiplier: 100,
					Expiry: "20260821", Strike: 115, Right: "C", Mark: 7.86,
					Underlying: new(108.0), Delta: new(0.46),
					MarketValue: 39300, MarketValueBase: new(36000.0),
				}},
			},
			{
				Underlying:           "GAPPY",
				GroupDollarDeltaBase: new(10000.0),
				Options: []rpc.PositionView{{
					Symbol: "GAPPY", Quantity: 10, Multiplier: 100, Expiry: "20260821",
					Strike: 10, Right: "C", Mark: 2, MarketValue: 2000, MarketValueBase: new(1800.0),
				}},
			},
		},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
	if len(names) != 2 {
		t.Fatalf("names = %d, want 2", len(names))
	}
	bysym := map[string]risk.NameInput{}
	for _, n := range names {
		bysym[n.Symbol] = n
	}
	if bysym["NOW"].ExposureBase != 380000 {
		t.Fatalf("NOW exposure = %v, want the group's GroupDollarDeltaBase 380000", bysym["NOW"].ExposureBase)
	}
	if bysym["NOW"].StockConID != 1234 || bysym["NOW"].StockSecType != "STK" {
		t.Fatalf("NOW exact stock identity = conid %d type %q", bysym["NOW"].StockConID, bysym["NOW"].StockSecType)
	}
	if bysym["NOW"].Legs[0].ExtrinsicBase == nil {
		t.Fatal("computable leg extrinsic must be set")
	}
	// The GAPPY leg has no delta: its notional must land in the greeks gap,
	// never silently shrink the exposure.
	if bysym["GAPPY"].GreeksGapNotionalBase != 1800 {
		t.Fatalf("GAPPY greeks gap = %v, want 1800", bysym["GAPPY"].GreeksGapNotionalBase)
	}
	if bysym["NOW"].Legs[0].DTE <= 0 {
		t.Fatalf("DTE = %d, want positive", bysym["NOW"].Legs[0].DTE)
	}
}

func TestMapRuleNamesExactStockIdentityFailsClosedOnSameTickerListings(t *testing.T) {
	groupStock := rpc.PositionView{Symbol: "DUAL", SecType: "STK", ConID: 1001, Quantity: 10, Mark: 20}
	pos := &rpc.PositionsResult{
		Stocks: []rpc.PositionView{
			groupStock,
			{Symbol: "DUAL", SecType: "STK", ConID: 2002, Quantity: 5, Mark: 21},
		},
		ByUnderlying: []rpc.PositionGroup{{Underlying: "DUAL", Stock: &groupStock}},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "USD")
	if len(names) != 1 || names[0].StockConID != 0 || names[0].StockSecType != "" {
		t.Fatalf("same-ticker listings leaked one exact identity: %+v", names)
	}

	pos.Stocks = pos.Stocks[:1]
	names = mapRuleNames(pos, risk.DefaultRulebookPolicy(), "USD")
	if len(names) != 1 || names[0].StockConID != 1001 || names[0].StockSecType != "STK" {
		t.Fatalf("single exact stock identity was not preserved: %+v", names)
	}
}

func TestMapRuleNamesCanonicalizesBrokerStockAliasesForIdentity(t *testing.T) {
	for _, secType := range []string{"STOCK", "ETF"} {
		stock := rpc.PositionView{Symbol: "SYNTH1", SecType: secType, ConID: 7001, Quantity: 10, Mark: 20}
		pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{stock}, ByUnderlying: []rpc.PositionGroup{{Underlying: "SYNTH1", Stock: &stock}}}
		names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "USD")
		if len(names) != 1 || names[0].StockConID != 7001 || names[0].StockSecType != "STK" {
			t.Fatal("broker stock alias did not produce a canonical exact identity")
		}
	}
}

// TestMapRuleNamesCostBasisJoinAndCompleteness covers the v2 input assembly:
// multiplier-inclusive cost basis with the same-currency FX path (a
// zero-marked line must keep its cost basis — that is rule 13's −100% case),
// the stock-leg underlying join with its quality gate, and the exposure
// completeness signal that guards rule 1's lower bound.
func TestMapRuleNamesCostBasisJoinAndCompleteness(t *testing.T) {
	pos := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{
			{
				Underlying:           "AAA",
				GroupDollarDeltaBase: new(50000.0),
				Stock:                &rpc.PositionView{Symbol: "AAA", Quantity: 100, Mark: 100, Currency: "USD"},
				Options: []rpc.PositionView{
					// FXRate present: cost = 250 × 2 × 0.9, no ×multiplier.
					{Symbol: "AAA", Quantity: 2, Multiplier: 100, Expiry: "20260821", Strike: 110, Right: "C",
						AvgCost: 250, Mark: 3, Currency: "USD", FXRate: new(0.9),
						MarketValue: 600, MarketValueBase: new(540.0)},
					// No greeks-tick spot: joins the stock mark, disclosed —
					// and a delta WITHOUT a spot marks the sum incomplete.
					{Symbol: "AAA", Quantity: 1, Multiplier: 100, Expiry: "20260918", Strike: 120, Right: "C",
						AvgCost: 100, Mark: 1, Currency: "USD", FXRate: new(0.9), Delta: new(0.3),
						MarketValue: 100, MarketValueBase: new(90.0)},
				},
			},
			{
				Underlying:           "BBB",
				GroupDollarDeltaBase: new(0.0),
				Options: []rpc.PositionView{
					// Same-currency book, marked to zero: the MV ratio is
					// undefined but positionBaseRate resolves fx=1, so the
					// −100% line keeps its cost basis for rule 13.
					{Symbol: "BBB", Quantity: 1, Multiplier: 100, Expiry: "20260821", Strike: 10, Right: "C",
						AvgCost: 500, Mark: 0, Currency: "EUR", MarketValue: 0},
				},
			},
			{
				Underlying:           "STALE",
				GroupDollarDeltaBase: new(1000.0),
				Stock:                &rpc.PositionView{Symbol: "STALE", Quantity: 10, Mark: 50, Stale: true, Currency: "EUR"},
				Options: []rpc.PositionView{
					{Symbol: "STALE", Quantity: 1, Multiplier: 100, Expiry: "20260821", Strike: 60, Right: "C",
						AvgCost: 100, Mark: 1, Currency: "EUR", MarketValue: 100},
				},
			},
		},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
	bysym := map[string]risk.NameInput{}
	for _, n := range names {
		bysym[n.Symbol] = n
	}

	aaa := bysym["AAA"]
	if got := aaa.Legs[0].CostBasisBase; got == nil || *got != 450 {
		t.Errorf("FXRate leg cost basis = %v, want 450 (AvgCost×|qty|×fx, no multiplier)", got)
	}
	if aaa.Legs[1].Underlying == nil || *aaa.Legs[1].Underlying != 100 ||
		aaa.Legs[1].UnderlyingSource != risk.UnderlyingSourceStockLegMark {
		t.Errorf("spotless leg must join the stock mark with disclosure, got %+v/%q",
			aaa.Legs[1].Underlying, aaa.Legs[1].UnderlyingSource)
	}
	if aaa.ExposureBaseComplete {
		t.Error("delta-without-spot leg must mark the exposure sum incomplete")
	}

	bbb := bysym["BBB"]
	if got := bbb.Legs[0].CostBasisBase; got == nil || *got != 500 {
		t.Errorf("same-currency zero-marked leg cost basis = %v, want 500 — the -100%% line must stay visible to rule 13", got)
	}

	if got := bysym["STALE"].Legs[0].Underlying; got != nil {
		t.Errorf("stale stock mark must not join as underlying, got %v", *got)
	}
}

// TestNonBaseExposure pins rule 14's corroboration: an empty currency report
// is only trusted as base-only when a healthy positions snapshot shows no
// non-base holdings — an unprimed or absent snapshot must stay unknown
// (never-false-pass; a $LEDGER flake must not pass a 90%-USD book).
func TestNonBaseExposure(t *testing.T) {
	acct := func(rows ...rpc.CurrencyExposure) *rpc.AccountResult {
		return &rpc.AccountResult{BaseCurrency: "EUR", CurrencyExposure: rows}
	}
	// Production shape: ByUnderlying is derived from Stocks by
	// groupByUnderlying, so a leg is never in the group view and absent from the
	// flat list. Populating both is what the daemon actually serves.
	posWith := func(ccy string) *rpc.PositionsResult {
		stock := rpc.PositionView{Symbol: "AAA", SecType: "STK", Quantity: 1, Currency: ccy}
		return &rpc.PositionsResult{
			Stocks: []rpc.PositionView{stock},
			ByUnderlying: []rpc.PositionGroup{{
				Underlying: "AAA",
				Stock:      &stock,
			}},
		}
	}
	// A bond is held in the Stocks slice but cannot anchor an underlying group,
	// so it never reaches ByUnderlying at all.
	posWithBond := func(ccy string) *rpc.PositionsResult {
		return &rpc.PositionsResult{
			Stocks: []rpc.PositionView{{Symbol: "T", SecType: "BOND", Quantity: 10000, Currency: ccy}},
		}
	}

	if got, _ := nonBaseExposure(acct(), nil); got != nil {
		t.Errorf("empty report with no corroborating snapshot = %v, want nil (unknown)", *got)
	}
	// The finding this pins: a foreign bond is invisible to the grouped view, so
	// corroborating from ByUnderlying alone asserted a clean 0.0% on a book that
	// was never measured — a false pass on a risk rule.
	if got, _ := nonBaseExposure(acct(), posWithBond("USD")); got != nil {
		t.Errorf("empty report with a non-base BOND leg = %v, want nil (ungrouped legs still contradict)", *got)
	}
	if got, _ := nonBaseExposure(acct(), posWithBond("EUR")); got == nil || *got != 0 {
		t.Errorf("empty report with a base-currency BOND leg = %v, want 0 (corroborated)", got)
	}
	if got, _ := nonBaseExposure(acct(), posWith("USD")); got != nil {
		t.Errorf("empty report with a USD leg = %v, want nil (report contradicted)", *got)
	}
	if got, _ := nonBaseExposure(acct(), posWith("EUR")); got == nil || *got != 0 {
		t.Errorf("empty report corroborated base-only = %v, want 0 (legitimate pass)", got)
	}
	got, ccys := nonBaseExposure(acct(
		rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 100000, ExchangeRate: 0.9, NetLiquidationBase: 90000},
		rpc.CurrencyExposure{Currency: "EUR", NetLiquidationCcy: 5000, ExchangeRate: 1, NetLiquidationBase: 5000},
	), posWith("USD"))
	if got == nil || *got != 90000 || len(ccys) != 1 || ccys[0] != "USD" {
		t.Errorf("non-base sum = %v/%v, want 90000/[USD] (base row excluded)", got, ccys)
	}
	if got, _ := nonBaseExposure(acct(
		rpc.CurrencyExposure{Currency: "USD", NetLiquidationCcy: 100000, ExchangeRate: 0},
	), posWith("USD")); got != nil {
		t.Errorf("missing exchange rate = %v, want nil (conversion unavailable)", *got)
	}
}

func TestBucketRegimeStage(t *testing.T) {
	cases := map[string]string{
		rpc.LifecycleQuiet:           risk.RegimeBucketCalm,
		rpc.LifecycleOpportunity:     risk.RegimeBucketCalm,
		rpc.LifecycleEarlyWarning:    risk.RegimeBucketEarlyWarning,
		rpc.LifecycleStabilization:   risk.RegimeBucketEarlyWarning,
		rpc.LifecycleConfirmedStress: risk.RegimeBucketConfirmed,
		rpc.LifecyclePanic:           risk.RegimeBucketConfirmed,
		rpc.LifecycleDataQuality:     "", // hold the previous latch
		"":                           "",
		"some_future_stage":          risk.RegimeBucketEarlyWarning, // middle, never silently calm
	}
	for stage, want := range cases {
		if got := bucketRegimeStage(stage); got != want {
			t.Errorf("bucketRegimeStage(%q) = %q, want %q", stage, got, want)
		}
	}
}

// TestRulesRegimeStagePersistence pins restart-mid-stress: a latched stage
// survives into a fresh Server via the state file, and a skewed stored
// bucket is re-derived from the stage rather than trusted.
func TestRulesRegimeStagePersistence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	s := &Server{}
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleConfirmedStress}})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Fatalf("latched bucket = %q, want confirmed", st.Bucket)
	}

	fresh := &Server{}
	st := fresh.rulesRegimeStageSnapshot()
	if st.Bucket != risk.RegimeBucketConfirmed || st.Stage != rpc.LifecycleConfirmedStress {
		t.Fatalf("restart lost the stage: %+v", st)
	}

	// data_quality must hold the previous latch, not clear it.
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{Lifecycle: rpc.LifecycleState{Stage: rpc.LifecycleDataQuality}})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Errorf("data_quality stage cleared the latch: %+v", st)
	}

	// Closed-date snapshots hold the previous latch too: a weekend
	// cluster-only "quiet" must not re-freshen or relax the bucket; the last
	// trading-date stage ages into the carried worse-of path instead.
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{
		TapeSessionState: rpc.TapeSessionClosedDate,
		Lifecycle:        rpc.LifecycleState{Stage: rpc.LifecycleQuiet},
	})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Errorf("closed-date snapshot moved the latch: %+v", st)
	}

	// The first live trading-date snapshot re-latches fresh.
	s.latchRulesRegimeStage(&rpc.RegimeSnapshotResult{
		TapeSessionState: rpc.TapeSessionTradingDate,
		Lifecycle:        rpc.LifecycleState{Stage: rpc.LifecycleQuiet},
	})
	if st := s.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketCalm {
		t.Errorf("trading-date snapshot did not re-latch: %+v", st)
	}

	// A skewed stored bucket is re-derived from the stage on load.
	path, err := defaultTradingStatePath(rulesRegimeStageFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateStateAtomic(path, []byte(`{"version":1,"bucket":"calm","stage":"panic","as_of":"2026-07-08T10:00:00Z"}`)); err != nil {
		t.Fatal(err)
	}
	skewed := &Server{}
	if st := skewed.rulesRegimeStageSnapshot(); st.Bucket != risk.RegimeBucketConfirmed {
		t.Errorf("stored bucket was trusted over the stage: %+v", st)
	}
}

func TestRulebookPreviewWarnings(t *testing.T) {
	res := &rpc.RulesResult{
		Enabled: true,
		AsOf:    time.Now(),
		Status:  "ok",
		Rules: []risk.RuleRow{
			{ID: risk.RuleSingleNameExposure, Number: 1, Status: risk.RuleStatusAct,
				Offenders: []risk.RuleOffender{{Symbol: "NOW"}}},
			{ID: risk.RuleCashSellOnly, Number: 3, Status: risk.RuleStatusAct},
			{ID: risk.RuleExtrinsicBudget, Number: 4, Status: risk.RuleStatusWatch},
		},
	}
	buyStock := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "NOW", SecType: "STK"}}
	buyOpt := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{Symbol: "NOW", SecType: "OPT"}}
	sellOther := rpc.OrderDraft{Action: "SELL", Contract: rpc.ContractParams{Symbol: "IBM", SecType: "STK"}}

	warns := rulebookPreviewWarnings(res, buyOpt, rpc.OrderPositionImpact{Effect: "increase"})
	codes := warningCodes(warns)
	for _, want := range []string{"rule_single_name_exposure", "rule_cash_sell_only", "rule_extrinsic_budget"} {
		if !codes[want] {
			t.Errorf("buy option on offender: missing %s (got %v)", want, codes)
		}
	}
	for _, w := range warns {
		if w.Scope != "rulebook" || (w.Severity != risk.RuleStatusAct && w.Severity != risk.RuleStatusWatch) {
			t.Errorf("warning %s carries scope=%q severity=%q; wants rulebook scope and the rule's own severity", w.Code, w.Scope, w.Severity)
		}
	}

	if warns := rulebookPreviewWarnings(res, buyStock, rpc.OrderPositionImpact{Effect: "reduce"}); len(warns) != 0 {
		t.Errorf("reduce effect must never warn, got %v", warningCodes(warns))
	}
	warns = rulebookPreviewWarnings(res, sellOther, rpc.OrderPositionImpact{Effect: "open_short"})
	if codes := warningCodes(warns); codes["rule_single_name_exposure"] || codes["rule_cash_sell_only"] {
		t.Errorf("sell on a non-offender must not inherit NOW/cash warnings, got %v", codes)
	}
	if warns := rulebookPreviewWarnings(nil, buyStock, rpc.OrderPositionImpact{Effect: "increase"}); warns != nil {
		t.Error("nil rules result must be silent")
	}

	// Rule 13: only averaging down into the SAME flagged line warns — a
	// different strike (a roll) must not.
	res.Rules = append(res.Rules, risk.RuleRow{ID: risk.RuleExitDiscipline, Number: 13, Status: risk.RuleStatusWatch,
		Offenders: []risk.RuleOffender{{Symbol: "NOW", Leg: "NOW 20260821 C 115"}}})
	sameLeg := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{
		Symbol: "NOW", SecType: "OPT", Expiry: "2026-08-21", Right: "c", Strike: 115}}
	if codes := warningCodes(rulebookPreviewWarnings(res, sameLeg, rpc.OrderPositionImpact{Effect: "increase"})); !codes["rule_exit_discipline"] {
		t.Errorf("averaging down into a flagged line must warn (dash expiry + lowercase right normalized), got %v", codes)
	}
	otherStrike := rpc.OrderDraft{Action: "BUY", Contract: rpc.ContractParams{
		Symbol: "NOW", SecType: "OPT", Expiry: "20260821", Right: "C", Strike: 120}}
	if codes := warningCodes(rulebookPreviewWarnings(res, otherStrike, rpc.OrderPositionImpact{Effect: "increase"})); codes["rule_exit_discipline"] {
		t.Error("rolling to a different strike must not inherit the exit-discipline warning")
	}
}

func TestJournalRuleTransitionsCarriesPolicyAndTerminalAuthorityFingerprints(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	pol := risk.DefaultRulebookPolicy()
	key := pol.FingerprintKey()
	at := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	terminalKey := "sha256:" + strings.Repeat("a", 64)
	identityKey := "sha256:" + strings.Repeat("c", 64)
	identityObservationID := opaqueEarningsIdentityObservationID(81, identityKey)
	identityAttemptedAt := at.Add(-2 * time.Minute)
	identityFailedAt := at.Add(-time.Minute)
	identityNextAttempt := identityFailedAt.Add(earningsContractResolutionRetry)
	identity := &rpc.EarningsIdentityInfo{
		Outcome: earningsIdentityUnknown, NotApplicable: true, AttemptedAt: identityAttemptedAt,
		ProofObservedAt: at.Add(-30 * time.Minute), ProofOutcome: rpc.EarningsStatusNotApplicable,
		AuthorityRevision: 8, AuthorityFingerprint: identityKey, ObservationID: identityObservationID,
		NextAttempt: &identityNextAttempt,
		LastFailure: &rpc.SourceFailure{
			Code: rpc.SourceFailureContractUnavailable, Stage: rpc.SourceFailureStageWSHContractResolve,
			FailedAt: identityFailedAt, Retryable: true,
		},
	}
	identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding("SYNTH1", *identity)
	terminal := &rpc.EarningsTerminalInfo{
		ContractConID: 42, Issuer: "private issuer must not enter transitions", CIK: "0000000042",
		Classification: earningsTerminalClassEquityCancelled,
		EffectiveDate:  at.AddDate(0, -1, 0).Format(time.DateOnly),
		VerifiedAt:     at.Add(-2 * time.Hour), RevalidateAfter: at.Add(30 * 24 * time.Hour),
		AuthorityRevision: 7, AuthorityReviewedAt: at.Add(-time.Hour), AuthorityFingerprint: terminalKey,
		Evidence: []rpc.EarningsEvidenceReference{{Authority: "SEC", Document: "private document", URL: "https://www.sec.gov/Archives/edgar/data/42/private"}},
	}
	terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding("PRIVATE", *terminal)
	server := &Server{}
	server.journalRuleTransitions(&rpc.RulesResult{
		AsOf:          at,
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		PolicyFingerprint: &rpc.Fingerprint{
			Version: rpc.RulebookPolicyFingerprintVersion,
			Key:     key,
		},
		Earnings: []rpc.EarningsInfo{{
			Symbol: "PRIVATE", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
			Terminal: terminal,
		}, {
			Symbol: "SYNTH1", Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable,
			Identity: identity,
		}},
		Rules: []risk.RuleRow{{ID: risk.RuleSingleNameExposure, Status: risk.RuleStatusWatch}},
	})
	(&Server{}).journalRuleTransitions(&rpc.RulesResult{
		AsOf:          time.Now(),
		PolicyID:      pol.ID,
		PolicyVersion: pol.Version,
		Rules:         []risk.RuleRow{{ID: risk.RuleCashSellOnly, Status: risk.RuleStatusAct}},
	})

	path, err := defaultTradingStatePath("rules-decisions.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal entries = %d, want 2", len(lines))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("journal entry is not JSON: %v", err)
	}
	if got, _ := entry["policy_fingerprint"].(string); got == "" || got != key {
		t.Fatalf("policy_fingerprint = %q, want %q", got, key)
	}
	authorities, ok := entry["terminal_authorities"].([]any)
	if !ok || len(authorities) != 1 {
		t.Fatalf("terminal_authorities = %#v, want one accepted authority", entry["terminal_authorities"])
	}
	authority, ok := authorities[0].(map[string]any)
	if !ok {
		t.Fatalf("terminal authority = %#v, want object", authorities[0])
	}
	if got := int(authority["contract_con_id"].(float64)); got != 42 {
		t.Fatalf("contract_con_id = %d, want 42", got)
	}
	if got, _ := authority["authority_fingerprint"].(string); got != terminalKey {
		t.Fatalf("authority_fingerprint = %q, want %q", got, terminalKey)
	}
	if got, _ := authority["authority_binding"].(string); got != terminal.AuthorityBinding {
		t.Fatalf("authority_binding = %q, want %q", got, terminal.AuthorityBinding)
	}
	allowed := map[string]bool{
		"contract_con_id": true, "authority_revision": true, "authority_fingerprint": true,
		"authority_binding":     true,
		"authority_reviewed_at": true, "verified_at": true, "revalidate_after": true, "classification": true,
	}
	for field := range authority {
		if !allowed[field] {
			t.Fatalf("terminal authority leaked non-audit field %q: %#v", field, authority[field])
		}
	}
	if len(authority) != len(allowed) {
		t.Fatalf("terminal authority fields = %#v, want exactly %#v", authority, allowed)
	}
	identityAuthorities, ok := entry["identity_authorities"].([]any)
	if !ok || len(identityAuthorities) != 1 {
		t.Fatalf("identity_authorities = %#v, want one accepted authority", entry["identity_authorities"])
	}
	identityAuthority, ok := identityAuthorities[0].(map[string]any)
	if !ok || identityAuthority["authority_fingerprint"] != identityKey || int(identityAuthority["authority_revision"].(float64)) != 8 ||
		identityAuthority["observation_id"] != identityObservationID ||
		identityAuthority["authority_binding"] != identity.AuthorityBinding ||
		identityAuthority["outcome"] != earningsIdentityNotApplicable {
		t.Fatalf("identity authority = %#v", identityAuthorities[0])
	}
	identityAllowed := map[string]bool{
		"authority_revision": true, "authority_fingerprint": true, "observation_id": true,
		"authority_binding": true, "observed_at": true, "outcome": true,
	}
	for field := range identityAuthority {
		if !identityAllowed[field] {
			t.Fatalf("identity authority leaked non-audit field %q", field)
		}
	}
	if err := json.Unmarshal([]byte(lines[1]), &entry); err != nil {
		t.Fatalf("nil-fingerprint journal entry is not JSON: %v", err)
	}
	if got, ok := entry["policy_fingerprint"].(string); !ok || got != "" {
		t.Fatalf("nil policy_fingerprint = %#v, want empty string", entry["policy_fingerprint"])
	}
	if authorities, ok := entry["terminal_authorities"].([]any); !ok || len(authorities) != 0 {
		t.Fatalf("empty terminal_authorities = %#v, want []", entry["terminal_authorities"])
	}
	if authorities, ok := entry["identity_authorities"].([]any); !ok || len(authorities) != 0 {
		t.Fatalf("empty identity_authorities = %#v, want []", entry["identity_authorities"])
	}
}

func TestJournalRuleTransitionsDoesNotRetainSQLiteCalibrationHistory(t *testing.T) {
	store := openMarketTestCoreStore(t)
	at := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	terminalKey := "sha256:" + strings.Repeat("b", 64)
	terminal := &rpc.EarningsTerminalInfo{
		ContractConID: 84, Classification: earningsTerminalClassIssuerDissolved,
		EffectiveDate: at.AddDate(0, -1, 0).Format(time.DateOnly),
		VerifiedAt:    at.Add(-2 * time.Hour), RevalidateAfter: at.Add(24 * time.Hour),
		AuthorityRevision: 9, AuthorityReviewedAt: at.Add(-time.Hour), AuthorityFingerprint: terminalKey,
	}
	terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding("PRIVATE2", *terminal)
	server := &Server{coreStore: store}
	server.journalRuleTransitions(&rpc.RulesResult{
		AsOf: at, PolicyID: "rulebook", PolicyVersion: 3,
		Earnings: []rpc.EarningsInfo{{
			Symbol: "PRIVATE2", Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
			Terminal: terminal,
		}},
		Rules: []risk.RuleRow{{ID: risk.RuleEarningsSizeFreeze, Status: risk.RuleStatusInfo}},
	})
	events, err := loadAllCoreEvents(t.Context(), store, coreEventRuleTransition)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("rule transition events = %d, want none", len(events))
	}
}

func TestAcceptedRuleTransitionTerminalAuthoritiesSortsDedupesAndRejectsEquivocation(t *testing.T) {
	at := time.Date(2026, 7, 21, 14, 0, 0, 0, time.UTC)
	makeInfo := func(symbol string, conID int, fingerprint string) rpc.EarningsInfo {
		info := rpc.EarningsInfo{
			Symbol: symbol, Source: "verified_terminal", Status: rpc.EarningsStatusTerminalNonReporting,
			Terminal: &rpc.EarningsTerminalInfo{
				ContractConID: conID, Classification: earningsTerminalClassEquityCancelled,
				EffectiveDate: at.AddDate(0, -1, 0).Format(time.DateOnly),
				VerifiedAt:    at.Add(-2 * time.Hour), RevalidateAfter: at.Add(time.Hour),
				AuthorityRevision: 3, AuthorityReviewedAt: at.Add(-time.Hour), AuthorityFingerprint: fingerprint,
			},
		}
		info.Terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(symbol, *info.Terminal)
		return info
	}
	one := "sha256:" + strings.Repeat("1", 64)
	two := "sha256:" + strings.Repeat("2", 64)
	conflictA := "sha256:" + strings.Repeat("3", 64)
	conflictB := "sha256:" + strings.Repeat("4", 64)
	info200 := makeInfo("TERM200", 200, two)
	info100 := makeInfo("TERM100", 100, one)
	conflictedA := makeInfo("TERM300A", 300, conflictA)
	conflictedB := makeInfo("TERM300B", 300, conflictB)
	invalid := makeInfo("TERM400", 400, "raw-free-text")
	expired := makeInfo("TERM500", 500, one)
	expired.Terminal.RevalidateAfter = at
	expired.Terminal.AuthorityBinding = rpc.BuildEarningsTerminalAuthorityBinding(expired.Symbol, *expired.Terminal)
	wrongSource := makeInfo("TERM600", 600, one)
	wrongSource.Source = "fetched"
	got := acceptedRuleTransitionTerminalAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{
		info200, info100, info100, conflictedA, conflictedB, invalid, expired, wrongSource,
	}})
	if len(got) != 2 || got[0].ContractConID != 100 || got[1].ContractConID != 200 {
		t.Fatalf("accepted authorities = %+v, want sorted deduped ConIDs 100, 200", got)
	}
	duplicateA := makeInfo("TERM700A", 700, one)
	duplicateB := makeInfo("TERM700B", 700, one)
	if got := acceptedRuleTransitionTerminalAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{duplicateA, duplicateB}}); len(got) != 0 {
		t.Fatalf("terminal transition accepted one ConID for multiple symbols: %+v", got)
	}
}

func TestAcceptedRuleTransitionIdentityAuthoritiesRequireOpaqueObservationLink(t *testing.T) {
	at := time.Date(2026, 7, 23, 14, 0, 0, 0, time.UTC)
	fingerprint := "sha256:" + strings.Repeat("7", 64)
	proofObservedAt := at.Add(-time.Hour)
	nextAttempt := proofObservedAt.Add(earningsFreshWindow)
	info := rpc.EarningsInfo{
		Symbol: "SYNTH1",
		Source: "broker_identity", Status: rpc.EarningsStatusNotApplicable,
		Identity: &rpc.EarningsIdentityInfo{
			Outcome: earningsIdentityNotApplicable, NotApplicable: true,
			AttemptedAt: proofObservedAt.Add(-time.Minute), ProofObservedAt: proofObservedAt,
			ProofOutcome: rpc.EarningsStatusNotApplicable, AuthorityRevision: 4, AuthorityFingerprint: fingerprint,
			NextAttempt: &nextAttempt,
		},
	}
	if got := acceptedRuleTransitionIdentityAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{info}}); len(got) != 0 {
		t.Fatal("identity transition accepted authority without an observation link")
	}
	info.Identity.ObservationID = opaqueEarningsIdentityObservationID(41, fingerprint)
	info.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(info.Symbol, *info.Identity)
	got := acceptedRuleTransitionIdentityAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{info}})
	if len(got) != 1 || got[0].ObservationID != info.Identity.ObservationID || got[0].AuthorityBinding != info.Identity.AuthorityBinding {
		t.Fatal("identity transition dropped a valid opaque observation link")
	}
	info.Identity.ObservationID = opaqueEarningsIdentityObservationID(42, fingerprint)
	if got := acceptedRuleTransitionIdentityAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{info}}); len(got) != 0 {
		t.Fatal("identity transition accepted a valid-shaped receipt that did not match its authority binding")
	}
	info.Identity.ObservationID = opaqueEarningsIdentityObservationID(41, fingerprint)
	info.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(info.Symbol, *info.Identity)
	duplicate := info
	duplicate.Symbol = "SYNTH2"
	identityCopy := *info.Identity
	duplicate.Identity = &identityCopy
	duplicate.Identity.AuthorityRevision = 5
	duplicate.Identity.AuthorityBinding = rpc.BuildEarningsIdentityAuthorityBinding(duplicate.Symbol, *duplicate.Identity)
	if got := acceptedRuleTransitionIdentityAuthorities(&rpc.RulesResult{AsOf: at, Earnings: []rpc.EarningsInfo{info, duplicate}}); len(got) != 0 {
		t.Fatal("identity transition accepted one opaque receipt for multiple symbols")
	}
}

func warningCodes(warns []rpc.DataWarning) map[string]bool {
	out := map[string]bool{}
	for _, w := range warns {
		out[w.Code] = true
	}
	return out
}

func TestRulebookSettingsPatch(t *testing.T) {
	applyFeatureSettingsPatch := func(next *platformSettingsData, featurePatch json.RawMessage) error {
		patch := map[string]json.RawMessage{"features": featurePatch}
		flat, err := flattenSettingsPatch(patch)
		if err != nil {
			return err
		}
		for key, raw := range flat {
			if err := applySettingsKey(next, key, raw); err != nil {
				return err
			}
		}
		return nil
	}
	next := &platformSettingsData{Version: platformSettingsDocVersion}
	patch := json.RawMessage(`{"rulebook":{"enabled":false,"earnings_overrides":{"now":"2026-07-22Tamc","BB":null}}}`)
	if err := applyFeatureSettingsPatch(next, patch); err != nil {
		t.Fatalf("patch: %v", err)
	}
	if next.Features.Rulebook.Enabled == nil || *next.Features.Rulebook.Enabled {
		t.Fatal("enabled=false not applied")
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("override = %q, want normalized NOW key", got)
	}
	if _, exists := next.Features.Rulebook.EarningsOverrides["BB"]; exists {
		t.Fatal("null override value must clear the symbol")
	}
	bad := json.RawMessage(`{"rulebook":{"earnings_overrides":{"NOW":"soon"}}}`)
	if err := applyFeatureSettingsPatch(next, bad); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Fatalf("bad override date must fail loudly, got %v", err)
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("failed patch must leave prior overrides intact, got %q", got)
	}

	// Patches merge per symbol: touching AMD must not drop NOW, a null AMD
	// clears only AMD, and a null map clears everything.
	upsert := json.RawMessage(`{"rulebook":{"earnings_overrides":{"amd":"2026-08-04Tbmo"}}}`)
	if err := applyFeatureSettingsPatch(next, upsert); err != nil {
		t.Fatalf("upsert patch: %v", err)
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("unmentioned symbol must survive a per-symbol patch, got %q", got)
	}
	if got := next.Features.Rulebook.EarningsOverrides["AMD"]; got != "2026-08-04Tbmo" {
		t.Fatalf("AMD override = %q, want normalized upsert", got)
	}
	clearOne := json.RawMessage(`{"rulebook":{"earnings_overrides":{"AMD":null}}}`)
	if err := applyFeatureSettingsPatch(next, clearOne); err != nil {
		t.Fatalf("clear-one patch: %v", err)
	}
	if _, exists := next.Features.Rulebook.EarningsOverrides["AMD"]; exists {
		t.Fatal("per-symbol null must clear only that symbol")
	}
	if got := next.Features.Rulebook.EarningsOverrides["NOW"]; got != "2026-07-22Tamc" {
		t.Fatalf("clearing one symbol must not touch the others, got %q", got)
	}
	clearAll := json.RawMessage(`{"rulebook":{"earnings_overrides":null}}`)
	if err := applyFeatureSettingsPatch(next, clearAll); err != nil {
		t.Fatalf("clear-all patch: %v", err)
	}
	if next.Features.Rulebook.EarningsOverrides != nil {
		t.Fatal("null map must clear all overrides")
	}
}

// A substituted leg (no FX rate, MarketValueBase nil) can still have a
// computable extrinsic — mark and spot need no conversion — but that figure
// is in the leg's own currency. Serving it as ExtrinsicBase fed rule 4's
// base budget a number wrong by the exchange rate, and the rule's
// substitution guard sits inside its ExtrinsicBase == nil branch, unreachable
// for such a leg. The daemon must leave ExtrinsicBase nil so the leg routes
// into that guarded branch and is disclosed, not summed.
func TestMapRuleNamesSubstitutedLegLeavesExtrinsicNil(t *testing.T) {
	pos := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{{
			Underlying: "CHFY",
			// No GroupDollarDeltaBase: the aggregator had no FX rate either.
			Options: []rpc.PositionView{{
				Symbol: "CHFY", SecType: "OPTION", Quantity: 1, Multiplier: 100,
				Expiry: "20260918", Strike: 55, Right: "C", Mark: 5,
				Underlying: new(50.0), Delta: new(0.4), Currency: "CHF",
				MarketValue: 500, // raw contract currency; MarketValueBase deliberately nil
			}},
		}},
	}
	names := mapRuleNames(pos, risk.DefaultRulebookPolicy(), "EUR")
	if len(names) != 1 || len(names[0].Legs) != 1 {
		t.Fatalf("names = %+v, want one name with one leg", names)
	}
	leg := names[0].Legs[0]
	if leg.MarketValueBaseSource != risk.MarketValueBaseSourceSubstituted {
		t.Fatalf("leg source = %q, want substituted", leg.MarketValueBaseSource)
	}
	if leg.ExtrinsicBase != nil {
		t.Fatalf("substituted leg ExtrinsicBase = %v, want nil — a raw contract-currency extrinsic must not enter the base budget", *leg.ExtrinsicBase)
	}

	// End to end: the leg lands in rule 4's unknowns, not its measured total.
	in := risk.RuleInputs{
		AsOf:      time.Now(),
		Positions: risk.SourceState{Healthy: true},
		Account:   risk.SourceState{Healthy: true},
		NLVBase:   new(100000.0),
		Names:     names,
	}
	ev := risk.EvaluateRulebook(in, risk.DefaultRulebookPolicy())
	var row risk.RuleRow
	found := false
	for _, r := range ev.Rows {
		if r.ID == risk.RuleExtrinsicBudget {
			row, found = r, true
		}
	}
	if !found {
		t.Fatal("extrinsic_budget row missing")
	}
	if row.Status != risk.RuleStatusUnknown {
		t.Fatalf("rule 4 = %s, want unknown (evidence: %s)", row.Status, row.Evidence)
	}
	var disclosed bool
	for _, o := range row.Offenders {
		if o.Symbol == "CHFY" && strings.Contains(o.Note, "extrinsic uncomputable") {
			disclosed = true
		}
	}
	if !disclosed {
		t.Fatalf("substituted leg must be disclosed in rule 4's unknowns, offenders = %+v", row.Offenders)
	}
	if row.Observed == nil || *row.Observed != 0 || row.ImpactBase != 0 {
		t.Fatalf("rule 4 observed/impact = %v/%v, want 0/0 — the unconverted figure may not enter the measured total", row.Observed, row.ImpactBase)
	}
}
