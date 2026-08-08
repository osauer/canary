package live

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

var liveAlertTestAuthorityScope = func() string {
	scope, err := rpc.BuildAlertAuthorityScope("LIVE-SERVICE-TEST", "paper")
	if err != nil {
		panic(err)
	}
	return scope
}()

func TestPollOnceCachesSnapshotAndPublishesEvents(t *testing.T) {
	t.Parallel()
	client := &fakeClient{
		status:    &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:  &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: "STK"}}},
		quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: new(500.0), ChangePct: new(0.4), DataType: rpc.MarketDataLive},
			"QQQ": {Symbol: "QQQ", Price: new(420.0), ChangePct: new(0.5), DataType: rpc.MarketDataLive},
			"IWM": {Symbol: "IWM", Price: new(210.0), ChangePct: new(0.2), DataType: rpc.MarketDataLive},
			"VIX": {Symbol: "VIX", Price: new(18.0), ChangePct: new(-2.0), DataType: rpc.MarketDataLive},
			"HYG": {Symbol: "HYG", Price: new(78.0), ChangePct: new(0.1), DataType: rpc.MarketDataLive},
			"TLT": {Symbol: "TLT", Price: new(92.0), ChangePct: new(-0.1), DataType: rpc.MarketDataLive},
		},
		regime:       &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}, Composite: rpc.RegimeComposite{Verdict: "Stress signal present", ClusterRedCount: 1, ClusterRankedCount: 6}},
		stress:       &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}, Severity: risk.SeverityWatch, Action: "watch"},
		brief:        &rpc.BriefResult{BriefFingerprint: "brief-1"},
		marketEvents: &rpc.MarketEventsResult{Kind: rpc.MarketEventsKind, SchemaVersion: rpc.MarketEventsSchemaVersion, Fingerprint: rpc.Fingerprint{Key: "market-events-1"}},
		trading:      &rpc.TradingStatus{CanPreview: true},
	}
	svc := New(client, time.Minute, time.Minute)
	ch, release := svc.Subscribe()
	defer release()

	snap := svc.PollOnce(context.Background())
	if snap.Version != 2 {
		t.Fatalf("snapshot version=%d, want 2", snap.Version)
	}
	if snap.Status == nil || !snap.Status.Connected {
		t.Fatalf("status missing from snapshot: %#v", snap.Status)
	}
	if snap.Calendar == nil || snap.Calendar.Session.State != "regular" {
		t.Fatalf("calendar missing from snapshot: %#v", snap.Calendar)
	}
	if snap.Account == nil || snap.Account.BaseCurrency != "USD" {
		t.Fatalf("account missing from snapshot: %#v", snap.Account)
	}
	if snap.Stress == nil || snap.Stress.Fingerprint.Key != "fp-1" {
		t.Fatalf("stress missing from snapshot: %#v", snap.Stress)
	}
	if snap.Quotes == nil || len(snap.Quotes.Quotes) != 6 || snap.Quotes.Quotes["QQQ"].Symbol != "QQQ" || snap.Quotes.Quotes["TLT"].Symbol != "TLT" {
		t.Fatalf("market quotes missing from snapshot: %#v", snap.Quotes)
	}
	if snap.MarketEvents == nil || snap.MarketEvents.Fingerprint.Key != "market-events-1" {
		t.Fatalf("market events missing from snapshot: %#v", snap.MarketEvents)
	}
	if snap.Regime == nil || snap.Regime.Fingerprint.Key != "regime-1" {
		t.Fatalf("regime missing from snapshot: %#v", snap.Regime)
	}
	if snap.Trading == nil || !snap.Trading.CanPreview {
		t.Fatalf("trading missing from snapshot: %#v", snap.Trading)
	}
	if snap.Settings == nil {
		t.Fatalf("settings missing from snapshot: %#v", snap.Settings)
	}
	if got := snap.Settings.MarketData.Quality.Status; got != "unknown" {
		t.Fatalf("settings market-data quality status = %q, want daemon-owned unknown", got)
	}

	seen := map[string]bool{}
	for range 16 {
		select {
		case ev := <-ch:
			seen[ev.Type] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for live events; seen=%v", seen)
		}
	}
	for _, want := range []string{"status", "market_calendar", "account", "positions", "market_events", "market_quotes", "trading", "auto_trade", "proposals", "opportunities", "settings", "regime", "stress", "rules", "brief", "snapshot"} {
		if !seen[want] {
			t.Fatalf("missing event %q; seen=%v", want, seen)
		}
	}
	diag := svc.Diagnostics()
	if diag.Subscribers != 1 {
		t.Fatalf("subscribers=%d, want 1", diag.Subscribers)
	}
	if diag.LastEventAt["snapshot"].IsZero() {
		t.Fatalf("snapshot event timestamp missing: %#v", diag.LastEventAt)
	}
}

func TestPublishedSnapshotDoesNotAliasLaterPollWrites(t *testing.T) {
	t.Parallel()
	svc := New(midPollFakeClient(nil), time.Minute, time.Minute)
	ch, release := svc.Subscribe()
	defer release()

	svc.PollOnce(context.Background())

	published, ok := firstPublishedSnapshot(ch)
	if !ok {
		t.Fatal("no snapshot event published")
	}
	if _, ok := published.Sources["status"]; !ok {
		t.Fatalf("mid-poll snapshot is missing an already-observed source: %+v", published.Sources)
	}
	for _, name := range []string{"market_quotes", "trading", "settings"} {
		if _, ok := published.Sources[name]; ok {
			t.Fatalf("mid-poll snapshot carries %q, written only after publication: %+v", name, published.Sources)
		}
	}
}

func TestAccountSourceRetainsLastGoodAndNamesTransportState(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	client := &fakeClient{account: &rpc.AccountResult{AccountID: "SYNTHETIC", BaseCurrency: "EUR", NetLiquidation: 0}}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }

	first := service.PollOnce(t.Context())
	if first.Account == nil || first.Account.AccountID != "SYNTHETIC" {
		t.Fatalf("initial account snapshot=%+v", first.Account)
	}
	if source := first.Sources["account"]; source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) || source.Error != "" {
		t.Fatalf("initial account source=%+v, want current", source)
	}

	client.accountErr = errors.New("private account transport sentinel")
	now = now.Add(time.Minute)
	failed := service.PollOnce(t.Context())
	if failed.Account == nil || failed.Account.AccountID != "SYNTHETIC" {
		t.Fatalf("transport failure discarded last-good account: %+v", failed.Account)
	}
	if source := failed.Sources["account"]; source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) || source.Error != "" {
		t.Fatalf("failed account source=%+v, want allowlisted unavailable state", source)
	}
	for _, item := range failed.Errors {
		if strings.Contains(item.Message, "private account transport sentinel") {
			t.Fatalf("raw account error leaked into public snapshot: %+v", item)
		}
	}

	client.accountErr = nil
	client.account = nil
	now = now.Add(time.Minute)
	nilResult := service.PollOnce(t.Context())
	if nilResult.Account == nil || nilResult.Account.AccountID != "SYNTHETIC" {
		t.Fatalf("nil success discarded last-good account: %+v", nilResult.Account)
	}
	if source := nilResult.Sources["account"]; source.State != SourceStateUnavailable || source.Reason != SourceReasonProducerUnavailable || !source.LastSuccessAt.Equal(now.Add(-2*time.Minute)) {
		t.Fatalf("nil account source=%+v, want producer unavailable", source)
	}
}

func TestPositionsSourceRetainsLastGoodAndDoesNotTurnFailureIntoAnEmptyBook(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 10, 15, 0, 0, time.UTC)
	client := &fakeClient{positions: &rpc.PositionsResult{
		AccountID: "SYNTHETIC", Stocks: []rpc.PositionView{{Symbol: "SYN", SecType: "STK"}},
	}}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	first := service.PollOnce(t.Context())
	if first.Positions == nil || len(first.Positions.Stocks) != 1 || first.Sources["positions"].State != SourceStateCurrent {
		t.Fatalf("initial positions snapshot=%+v source=%+v", first.Positions, first.Sources["positions"])
	}

	client.positionsErr = errors.New("private positions transport sentinel")
	now = now.Add(time.Minute)
	failed := service.PollOnce(t.Context())
	if failed.Positions == nil || len(failed.Positions.Stocks) != 1 {
		t.Fatalf("positions failure became an empty book: %+v", failed.Positions)
	}
	if source := failed.Sources["positions"]; source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) || source.Error != "" {
		t.Fatalf("failed positions source=%+v", source)
	}
}

func TestPublicSnapshotCarriesAccountAuthorityAndKeepsItImmutable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 10, 30, 0, 0, time.UTC)
	daily := 0.0
	client := &fakeClient{account: &rpc.AccountResult{
		AccountID: "SYNTHETIC", BaseCurrency: "EUR", NetLiquidation: 0, DailyPnL: &daily, AsOf: now,
		Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "SYNTHETIC", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourceAccountSummaryRequest, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now,
			Fields: &rpc.AccountFieldAvailability{BaseCurrency: true, NetLiquidation: true, DailyPnL: true},
		},
	}, positions: &rpc.PositionsResult{
		AccountID: "SYNTHETIC", Authority: &rpc.AccountDataAuthority{
			Scope:  rpc.AccountDataScope{AccountID: "SYNTHETIC", AccountMode: rpc.AccountModePaper},
			Source: rpc.AccountDataSourcePortfolioStream, Availability: rpc.AccountDataAvailable,
			Freshness: rpc.AccountDataFreshnessCurrent, AsOf: now,
		},
	}}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	got := service.PollOnce(t.Context())

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		Account struct {
			Authority struct {
				Scope        rpc.AccountDataScope        `json:"scope"`
				Availability rpc.AccountDataAvailability `json:"availability"`
				Freshness    rpc.AccountDataFreshness    `json:"freshness"`
				Fields       map[string]bool             `json:"fields"`
			} `json:"authority"`
		} `json:"account"`
		Positions struct {
			Authority struct {
				Scope        rpc.AccountDataScope        `json:"scope"`
				Availability rpc.AccountDataAvailability `json:"availability"`
				Freshness    rpc.AccountDataFreshness    `json:"freshness"`
			} `json:"authority"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(encoded, &public); err != nil {
		t.Fatal(err)
	}
	if public.Account.Authority.Scope.AccountID != "SYNTHETIC" ||
		public.Account.Authority.Availability != rpc.AccountDataAvailable ||
		public.Account.Authority.Freshness != rpc.AccountDataFreshnessCurrent {
		t.Fatalf("public account authority=%+v", public.Account.Authority)
	}
	if !public.Account.Authority.Fields["net_liquidation"] || public.Account.Authority.Fields["buying_power"] {
		t.Fatalf("public account field presence=%+v, want true zero and explicit missing zero", public.Account.Authority.Fields)
	}
	if public.Positions.Authority.Scope.AccountID != "SYNTHETIC" ||
		public.Positions.Authority.Availability != rpc.AccountDataAvailable ||
		public.Positions.Authority.Freshness != rpc.AccountDataFreshnessCurrent {
		t.Fatalf("public positions authority=%+v", public.Positions.Authority)
	}

	got.Account.Authority.Scope.AccountID = "MUTATED"
	got.Account.Authority.Fields.NetLiquidation = false
	got.Positions.Authority.Scope.AccountID = "MUTATED"
	current := service.Snapshot()
	if current.Account.Authority.Scope.AccountID != "SYNTHETIC" || !current.Account.Authority.Fields.NetLiquidation {
		t.Fatalf("returned snapshot aliased service account authority: %+v", current.Account.Authority)
	}
	if current.Positions.Authority.Scope.AccountID != "SYNTHETIC" {
		t.Fatalf("returned snapshot aliased service positions authority: %+v", current.Positions.Authority)
	}
}

// A published snapshot is handed to SSE subscribers that marshal it while the
// same poll keeps recording source metadata. Sharing either side's maps is a
// fatal concurrent map access, not a stale read.
func TestPublishedSnapshotIsSafeToMarshalDuringPoll(t *testing.T) {
	t.Parallel()
	quoteRelease := make(chan struct{})
	svc := New(midPollFakeClient(quoteRelease), time.Minute, time.Minute)
	ch, release := svc.Subscribe()
	defer release()

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.PollOnce(context.Background())
	}()

	published := waitForPublishedSnapshot(t, ch)
	close(quoteRelease)
	for range 500 {
		if _, err := json.Marshal(published); err != nil {
			t.Fatalf("marshal published snapshot: %v", err)
		}
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the poll to finish")
	}
}

func midPollFakeClient(quoteBlock <-chan struct{}) *fakeClient {
	return &fakeClient{
		status:     &rpc.HealthResult{Connected: true},
		calendar:   &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:    &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions:  &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "XYZ", SecType: "STK"}}},
		trading:    &rpc.TradingStatus{CanPreview: true},
		quoteBlock: quoteBlock,
	}
}

func firstPublishedSnapshot(ch <-chan Event) (Snapshot, bool) {
	for {
		select {
		case ev := <-ch:
			if snap, ok := ev.Data.(Snapshot); ok && ev.Type == "snapshot" {
				return snap, true
			}
		default:
			return Snapshot{}, false
		}
	}
}

func waitForPublishedSnapshot(t *testing.T, ch <-chan Event) Snapshot {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatal("live event channel closed before a snapshot event")
			}
			if ev.Type != "snapshot" {
				continue
			}
			snap, ok := ev.Data.(Snapshot)
			if !ok {
				t.Fatalf("snapshot event carries %T", ev.Data)
			}
			return snap
		case <-deadline:
			t.Fatal("timed out waiting for a snapshot event")
		}
	}
}

func TestBriefPollsOnStressCadenceWithoutAcknowledging(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	client := &fakeClient{brief: &rpc.BriefResult{BriefFingerprint: "brief-1"}}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }

	svc.PollOnce(context.Background())
	briefCalls, ackCalls := client.BriefCounts()
	if briefCalls != 1 || ackCalls != 0 {
		t.Fatalf("after first poll: brief=%d ack=%d, want 1/0", briefCalls, ackCalls)
	}

	now = now.Add(30 * time.Second)
	svc.PollOnce(context.Background())
	briefCalls, ackCalls = client.BriefCounts()
	if briefCalls != 1 || ackCalls != 0 {
		t.Fatalf("before cadence: brief=%d ack=%d, want 1/0", briefCalls, ackCalls)
	}

	now = now.Add(31 * time.Second)
	svc.PollOnce(context.Background())
	briefCalls, ackCalls = client.BriefCounts()
	if briefCalls != 2 || ackCalls != 0 {
		t.Fatalf("after cadence: brief=%d ack=%d, want 2/0", briefCalls, ackCalls)
	}
}

func TestBriefPollErrorKeepsLastGoodSnapshotAndSetsSourceMeta(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 8, 0, 0, 0, time.UTC)
	client := &fakeClient{
		status: &rpc.HealthResult{Connected: true},
		brief:  &rpc.BriefResult{BriefFingerprint: "brief-last-good"},
	}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }
	first := svc.PollOnce(context.Background())
	if first.Brief == nil || first.Brief.BriefFingerprint != "brief-last-good" {
		t.Fatalf("first brief=%#v", first.Brief)
	}

	client.briefErr = errors.New("brief composition unavailable")
	now = now.Add(time.Minute)
	got := svc.PollOnce(context.Background())
	if got.Brief == nil || got.Brief.BriefFingerprint != "brief-last-good" {
		t.Fatalf("last good brief was not retained: %#v", got.Brief)
	}
	if got.Status == nil || !got.Status.Connected {
		t.Fatalf("rest of snapshot was not retained: %#v", got.Status)
	}
	briefSource := got.Sources["brief"]
	if briefSource.State != SourceStateUnavailable || briefSource.Reason != SourceReasonTransportUnavailable || briefSource.Error != "" || !briefSource.LastSuccessAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("brief source meta=%#v", got.Sources["brief"])
	}
	found := false
	for _, sourceErr := range got.Errors {
		if sourceErr.Source == "brief" && sourceErr.Message == "Source temporarily unavailable." {
			found = true
		}
	}
	if !found {
		t.Fatalf("brief source error missing: %#v", got.Errors)
	}
}

func TestCadenceSourcesStartNotObserved(t *testing.T) {
	t.Parallel()
	svc := New(&fakeClient{}, 5*time.Second, time.Minute)
	for _, name := range []string{"stress", "regime", "alert_candidates", "rules", "brief"} {
		source, ok := svc.Snapshot().Sources[name]
		if !ok {
			t.Fatalf("source %q missing at startup", name)
		}
		if source.State != SourceStateNotObserved || source.Reason != SourceReasonNotObserved || !source.UpdatedAt.IsZero() || !source.LastSuccessAt.IsZero() || source.Error != "" {
			t.Fatalf("startup source %q=%+v, want allowlisted not_observed", name, source)
		}
	}
}

func TestRegimeAuthorityHealthControlsSourceMetaWithoutPollAgeDrift(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	oldPublish := now.Add(-4 * time.Minute)
	age := int64((4 * time.Minute) / time.Second)
	fresh := &rpc.RegimeMonitorResult{AuthorityHealth: &rpc.RegimeAuthorityHealth{
		Status: rpc.RegimeAuthorityFresh, LastSuccessAt: &oldPublish, LastSuccessAgeSeconds: &age,
	}}
	meta := regimeSourceMeta(SourceMeta{}, now, fresh)
	if meta.State != SourceStateCurrent || !meta.LastSuccessAt.Equal(now) {
		t.Fatalf("fresh authority source meta = %#v, want successful poll time", meta)
	}

	stale := cloneRegimeMonitor(fresh)
	stale.AuthorityHealth.Status = rpc.RegimeAuthorityStale
	stale.AuthorityHealth.FailureCode = rpc.RegimeAuthorityFailureRefreshFailed
	meta = regimeSourceMeta(meta, now.Add(time.Minute), stale)
	if meta.State != SourceStateStale || meta.Reason != SourceReasonPollStale || !meta.LastSuccessAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("stale authority source meta = %#v", meta)
	}
	if fresh.AuthorityHealth.Status != rpc.RegimeAuthorityFresh {
		t.Fatal("cloneRegimeMonitor aliased authority health")
	}
}

func TestStressRegimePollIsAtomicOnFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 19, 0, 0, 0, time.UTC)
	client := &fakeClient{
		stress: &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "stress-last-good"}},
		regime: &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-last-good"}},
		brief:  &rpc.BriefResult{BriefFingerprint: "brief-ready"},
	}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }

	first := svc.PollOnce(t.Context())
	for _, name := range []string{"stress", "regime"} {
		source := first.Sources[name]
		if source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) || source.Error != "" {
			t.Fatalf("initial source %q=%+v, want current", name, source)
		}
	}

	client.stress = &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "must-not-publish"}}
	client.regime = &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "must-not-publish"}}
	client.stressErr = errors.New("regime: private transport sentinel")
	now = now.Add(time.Minute)
	got := svc.PollOnce(t.Context())
	if got.Stress == nil || got.Stress.Fingerprint.Key != "stress-last-good" || got.Regime == nil || got.Regime.Fingerprint.Key != "regime-last-good" {
		t.Fatalf("atomic failure replaced last-good pair: stress=%#v regime=%#v", got.Stress, got.Regime)
	}
	for _, name := range []string{"stress", "regime"} {
		source := got.Sources[name]
		if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) || source.Error != "" {
			t.Fatalf("failed source %q=%+v, want allowlisted unavailable with retained success", name, source)
		}
	}
	seenErrors := map[string]bool{}
	for _, sourceErr := range got.Errors {
		if sourceErr.Message == "Source temporarily unavailable." {
			seenErrors[sourceErr.Source] = true
		}
		if strings.Contains(sourceErr.Message, "private transport sentinel") {
			t.Fatalf("raw transport error leaked through SourceError: %#v", sourceErr)
		}
	}
	if !seenErrors["stress"] || !seenErrors["regime"] {
		t.Fatalf("legacy stress/regime SourceError attribution was not retained: %#v", got.Errors)
	}
}

func TestStressRegimeNilSuccessIsUnavailableAndNeverPublishes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		stress        *rpc.StressResult
		regime        *rpc.RegimeMonitorResult
		missingSource []string
	}{
		{name: "nil stress", regime: &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime"}}, missingSource: []string{"stress"}},
		{name: "nil regime", stress: &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "stress"}}, missingSource: []string{"regime"}},
		{name: "nil pair", missingSource: []string{"stress", "regime"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 7, 20, 19, 30, 0, 0, time.UTC)
			client := &fakeClient{stress: tt.stress, regime: tt.regime, brief: &rpc.BriefResult{BriefFingerprint: "brief-ready"}}
			svc := New(client, 5*time.Second, time.Minute)
			svc.now = func() time.Time { return now }

			got := svc.PollOnce(t.Context())
			if got.Stress != nil || got.Regime != nil {
				t.Fatalf("partial pair was published: stress=%#v regime=%#v", got.Stress, got.Regime)
			}
			for _, name := range []string{"stress", "regime"} {
				source := got.Sources[name]
				if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.IsZero() || source.Error != "" {
					t.Fatalf("source %q=%+v, want cold unavailable", name, source)
				}
			}
			seen := map[string]bool{}
			for _, sourceErr := range got.Errors {
				seen[sourceErr.Source] = true
			}
			for _, name := range tt.missingSource {
				if !seen[name] {
					t.Fatalf("missing nil-result SourceError attribution for %q: %#v", name, got.Errors)
				}
			}
		})
	}
}

func TestRulesAndBriefFailuresRetainLastGoodAndRecover(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 20, 0, 0, 0, time.UTC)
	client := &fakeClient{
		stress: &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "stress"}},
		regime: &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime"}},
		rules:  &rpc.RulesResult{Enabled: true, Status: "first"},
		brief:  &rpc.BriefResult{BriefFingerprint: "brief-first"},
	}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }
	svc.PollOnce(t.Context())

	client.rules = &rpc.RulesResult{Enabled: true, Status: "must-not-publish"}
	client.rulesErr = errors.New("private rules transport sentinel")
	client.brief = &rpc.BriefResult{BriefFingerprint: "must-not-publish"}
	client.briefErr = errors.New("private brief transport sentinel")
	now = now.Add(time.Minute)
	failed := svc.PollOnce(t.Context())
	if failed.Rules == nil || failed.Rules.Status != "first" || failed.Brief == nil || failed.Brief.BriefFingerprint != "brief-first" {
		t.Fatalf("error discarded last-good payloads: rules=%#v brief=%#v", failed.Rules, failed.Brief)
	}
	for _, name := range []string{"rules", "brief"} {
		source := failed.Sources[name]
		if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) || source.Error != "" {
			t.Fatalf("failed source %q=%+v", name, source)
		}
	}

	client.rulesErr = nil
	client.rules = &rpc.RulesResult{Enabled: true, Status: "recovered"}
	client.briefErr = nil
	client.brief = &rpc.BriefResult{BriefFingerprint: "brief-recovered"}
	now = now.Add(time.Minute)
	recovered := svc.PollOnce(t.Context())
	if recovered.Rules == nil || recovered.Rules.Status != "recovered" || recovered.Brief == nil || recovered.Brief.BriefFingerprint != "brief-recovered" {
		t.Fatalf("recovery payloads missing: rules=%#v brief=%#v", recovered.Rules, recovered.Brief)
	}
	for _, name := range []string{"rules", "brief"} {
		source := recovered.Sources[name]
		if source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) || source.Error != "" {
			t.Fatalf("recovered source %q=%+v", name, source)
		}
	}

	client.rulesNil = true
	client.brief = nil
	now = now.Add(time.Minute)
	nilResult := svc.PollOnce(t.Context())
	if nilResult.Rules == nil || nilResult.Rules.Status != "recovered" || nilResult.Brief == nil || nilResult.Brief.BriefFingerprint != "brief-recovered" {
		t.Fatalf("nil success discarded last-good payloads: rules=%#v brief=%#v", nilResult.Rules, nilResult.Brief)
	}
	for _, name := range []string{"rules", "brief"} {
		source := nilResult.Sources[name]
		if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) || source.Error != "" {
			t.Fatalf("nil source %q=%+v", name, source)
		}
	}
	seen := map[string]string{}
	for _, sourceErr := range nilResult.Errors {
		seen[sourceErr.Source] = sourceErr.Message
	}
	if seen["rules"] != "Source temporarily unavailable." || seen["brief"] != "Source temporarily unavailable." {
		t.Fatalf("nil-result SourceErrors=%#v", nilResult.Errors)
	}
}

func TestCadenceSourceFreshnessAgesCurrentButNotUnavailable(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 20, 30, 0, 0, time.UTC)
	client := &fakeClient{
		stress: &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "stress"}},
		regime: &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime"}},
		rules:  &rpc.RulesResult{Enabled: true, Status: "ok"},
		brief:  &rpc.BriefResult{BriefFingerprint: "brief"},
	}
	svc := New(client, 5*time.Second, time.Minute)
	svc.now = func() time.Time { return now }
	svc.PollOnce(t.Context())
	lastSuccess := now

	now = now.Add(time.Minute + time.Nanosecond)
	aged := svc.Snapshot()
	for _, name := range []string{"stress", "regime", "rules", "brief"} {
		source := aged.Sources[name]
		if source.State != SourceStateStale || source.Reason != SourceReasonPollStale || !source.LastSuccessAt.Equal(lastSuccess) {
			t.Fatalf("aged source %q=%+v", name, source)
		}
	}

	client.stressErr = errors.New("transport down")
	client.rulesErr = errors.New("transport down")
	client.briefErr = errors.New("transport down")
	svc.PollOnce(t.Context())
	now = now.Add(2 * time.Minute)
	unavailable := svc.Snapshot()
	for _, name := range []string{"stress", "regime", "rules", "brief"} {
		source := unavailable.Sources[name]
		if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || !source.LastSuccessAt.Equal(lastSuccess) {
			t.Fatalf("explicit outage source %q was reclassified by aging: %+v", name, source)
		}
	}

	client.stressErr = nil
	client.rulesErr = nil
	client.briefErr = nil
	svc.PollOnce(t.Context())
	recovered := svc.Snapshot()
	for _, name := range []string{"stress", "regime", "rules", "brief"} {
		source := recovered.Sources[name]
		if source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) {
			t.Fatalf("recovered source %q=%+v", name, source)
		}
	}
}

func TestSnapshotBriefCloneIsIndependent(t *testing.T) {
	t.Parallel()
	brief := &rpc.BriefResult{
		Review: rpc.BriefReviewSection{
			SessionPnL:    rpc.BriefAccountRow{EquityBase: new(100.0)},
			Attribution:   rpc.BriefMoversRow{Rows: []rpc.BriefMover{{Symbol: "AAA"}}},
			RulesDelta:    rpc.BriefRulesDeltaRow{Transitions: []rpc.BriefRuleTransition{{RuleID: "r1"}}, Added: []string{"r2"}, Removed: []string{"r3"}},
			Overrides:     rpc.BriefOverridesRow{Rows: []rpc.BriefOverride{{Control: "limit"}}},
			CapitalEvents: rpc.BriefCapitalEventsRow{LatchAgeDays: new(2)},
			Reconcile:     rpc.BriefReconcileRow{DaysRemaining: new(3)},
			OneTap:        rpc.BriefOneTapRow{Blockers: []string{"blocked"}},
			WorkingOrders: rpc.BriefCountRow{Count: new(1)},
		},
		Ready: rpc.BriefReadySection{
			Breadth:       rpc.BriefBreadthRow{PctAbove50DMA: new(51.0)},
			Gamma:         rpc.BriefGammaRow{Spot: new(6000.0)},
			MarketEvents:  []rpc.BriefMarketEventRow{{Symbols: []string{"AAA"}}},
			Capital:       rpc.BriefCapitalRow{ConsumedPct: new(20.0)},
			Latch:         rpc.BriefLatchRow{AgeDays: new(2)},
			PremiumAtRisk: rpc.BriefMoneyCoverageRow{AmountBase: new(10.0)},
			PolicyDrift:   rpc.BriefPolicyDriftRow{Rows: []rpc.PolicyPinStatus{{Policy: "rules"}}},
			Artefacts:     rpc.BriefArtefactsRow{Rows: []rpc.BriefArtefact{{Kind: "morning"}}},
			MonthlyPulse:  &rpc.BriefMonthlyPulseRow{Status: "due", Month: "2026-08"},
		},
	}
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	svc.snapshot = Snapshot{Brief: brief}
	got := svc.Snapshot()

	*got.Brief.Ready.Breadth.PctAbove50DMA = 0
	*got.Brief.Ready.Gamma.Spot = 0
	got.Brief.Ready.MarketEvents[0].Symbols[0] = "MUTATED"
	*got.Brief.Review.SessionPnL.EquityBase = 0
	got.Brief.Review.Attribution.Rows[0].Symbol = "MUTATED"
	*got.Brief.Ready.PremiumAtRisk.AmountBase = 0
	*got.Brief.Review.WorkingOrders.Count = 0
	*got.Brief.Ready.Capital.ConsumedPct = 0
	*got.Brief.Ready.Latch.AgeDays = 0
	*got.Brief.Review.CapitalEvents.LatchAgeDays = 0
	got.Brief.Review.Overrides.Rows[0].Control = "MUTATED"
	got.Brief.Ready.PolicyDrift.Rows[0].Policy = "MUTATED"
	*got.Brief.Review.Reconcile.DaysRemaining = 0
	got.Brief.Review.OneTap.Blockers[0] = "MUTATED"
	got.Brief.Review.RulesDelta.Transitions[0].RuleID = "MUTATED"
	got.Brief.Review.RulesDelta.Added[0] = "MUTATED"
	got.Brief.Review.RulesDelta.Removed[0] = "MUTATED"
	got.Brief.Ready.Artefacts.Rows[0].Kind = "MUTATED"
	got.Brief.Ready.MonthlyPulse.Status = "MUTATED"

	current := svc.Snapshot().Brief
	if *current.Ready.Breadth.PctAbove50DMA != 51 || *current.Ready.Gamma.Spot != 6000 ||
		current.Ready.MarketEvents[0].Symbols[0] != "AAA" || *current.Review.SessionPnL.EquityBase != 100 ||
		current.Review.Attribution.Rows[0].Symbol != "AAA" || *current.Ready.PremiumAtRisk.AmountBase != 10 ||
		*current.Review.WorkingOrders.Count != 1 || *current.Ready.Capital.ConsumedPct != 20 ||
		*current.Ready.Latch.AgeDays != 2 || *current.Review.CapitalEvents.LatchAgeDays != 2 ||
		current.Review.Overrides.Rows[0].Control != "limit" ||
		current.Ready.PolicyDrift.Rows[0].Policy != "rules" || *current.Review.Reconcile.DaysRemaining != 3 ||
		current.Review.OneTap.Blockers[0] != "blocked" || current.Review.RulesDelta.Transitions[0].RuleID != "r1" ||
		current.Review.RulesDelta.Added[0] != "r2" || current.Review.RulesDelta.Removed[0] != "r3" ||
		current.Ready.Artefacts.Rows[0].Kind != "morning" || current.Ready.MonthlyPulse.Status != "due" {
		t.Fatalf("mutating returned brief changed service snapshot: %#v", current)
	}
}

func TestSnapshotDeepCopiesNudgeContextAndConsumedPercentage(t *testing.T) {
	t.Parallel()
	consumed := 31.25
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	svc.snapshot = Snapshot{Nudges: &rpc.NudgesSnapshotResult{Context: &rpc.NudgeSnapshotContext{
		Shadow: &rpc.NudgeShadowSummary{Count: 7},
		Drawdown: &rpc.NudgeDrawdownSummary{
			Tier: rpc.NudgeDrawdownTierBlock, ConsumedPct: &consumed,
		},
	}}}

	got := svc.Snapshot()
	got.Nudges.Context.Shadow.Count = 99
	got.Nudges.Context.Drawdown.Tier = "HOSTILE"
	*got.Nudges.Context.Drawdown.ConsumedPct = 0
	current := svc.Snapshot()
	if current.Nudges.Context.Shadow.Count != 7 || current.Nudges.Context.Drawdown.Tier != rpc.NudgeDrawdownTierBlock || current.Nudges.Context.Drawdown.ConsumedPct == nil || *current.Nudges.Context.Drawdown.ConsumedPct != 31.25 {
		t.Fatalf("mutating returned nudge context changed cached authority state: %+v", current.Nudges.Context)
	}
}

func TestStartPublishesStatusBeforeFullPollCompletes(t *testing.T) {
	t.Parallel()
	stressBlock := make(chan struct{})
	client := &fakeClient{
		status:      &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:    &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:     &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions:   &rpc.PositionsResult{Stocks: []rpc.PositionView{}},
		quotes:      map[string]rpc.Quote{"SPY": {Symbol: "SPY", Price: new(500.0)}},
		regime:      &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		stress:      &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading:     &rpc.TradingStatus{CanPreview: true},
		stressBlock: stressBlock,
	}
	svc := New(client, time.Hour, time.Hour)
	ch, release := svc.Subscribe()
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		svc.Start(ctx)
		close(done)
	}()
	defer func() {
		close(stressBlock)
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("live service did not stop")
		}
	}()

	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("subscription closed before status event")
			}
			if ev.Type == "status" {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for startup status event")
		}
	}
}

func TestPollOncePublishesPositionsBeforeMarketQuotesComplete(t *testing.T) {
	t.Parallel()
	quoteBlock := make(chan struct{})
	client := &fakeClient{
		status:     &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:   &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:    &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions:  &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "SAP"}}},
		quotes:     map[string]rpc.Quote{"SPY": {Symbol: "SPY", Price: new(500.0)}},
		regime:     &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		stress:     &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading:    &rpc.TradingStatus{CanPreview: true},
		quoteBlock: quoteBlock,
	}
	svc := New(client, time.Hour, time.Hour)
	ch, release := svc.Subscribe()
	defer release()

	done := make(chan struct{})
	go func() {
		svc.PollOnce(context.Background())
		close(done)
	}()
	defer func() {
		close(quoteBlock)
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("PollOnce did not stop")
		}
	}()

	for {
		select {
		case ev := <-ch:
			if ev.Type != "snapshot" {
				continue
			}
			snap, ok := ev.Data.(Snapshot)
			if !ok {
				t.Fatalf("snapshot event data type %T, want Snapshot", ev.Data)
			}
			if snap.Positions != nil && len(snap.Positions.Stocks) == 1 {
				return
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for early positions snapshot")
		}
	}
}

func TestMarketQuoteStreamFrameKeepsChangeAnchor(t *testing.T) {
	t.Parallel()
	svc := New(&fakeClient{}, time.Minute, time.Minute)
	prev := 500.0
	svc.snapshot.Quotes = &MarketQuotes{
		Quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", PrevClose: new(prev)},
		},
	}

	last := 505.0
	svc.applyMarketQuoteFrame("SPY", rpc.Frame{T: time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC), Last: new(last), DataType: rpc.MarketDataLive})
	got := svc.Snapshot().Quotes.Quotes["SPY"]
	if got.Price == nil || *got.Price != 505.0 {
		t.Fatalf("stream frame price=%v, want 505", got.Price)
	}
	if got.ChangePct == nil || *got.ChangePct != 1.0 {
		t.Fatalf("stream frame change_pct=%v, want 1.0", got.ChangePct)
	}
	if got.PriceSource != "last" || got.DataType != rpc.MarketDataLive {
		t.Fatalf("stream frame metadata source=%q data_type=%q", got.PriceSource, got.DataType)
	}
}

func TestMergeMarketQuotesPreservesLastGoodStreamQuote(t *testing.T) {
	t.Parallel()
	oldSPY := 500.0
	newQQQ := 420.0
	existing := &MarketQuotes{
		AsOf: time.Date(2026, 6, 4, 15, 30, 0, 0, time.UTC),
		Quotes: map[string]rpc.Quote{
			"SPY": {Symbol: "SPY", Price: &oldSPY},
		},
	}
	update := &MarketQuotes{
		AsOf: time.Date(2026, 6, 4, 15, 31, 0, 0, time.UTC),
		Quotes: map[string]rpc.Quote{
			"QQQ": {Symbol: "QQQ", Price: &newQQQ},
		},
		Errors: map[string]string{"SPY": "snapshot timeout"},
	}

	got := mergeMarketQuotes(existing, update)
	if got.Quotes["SPY"].Price == nil || *got.Quotes["SPY"].Price != oldSPY {
		t.Fatalf("SPY last-good quote lost: %#v", got.Quotes["SPY"])
	}
	if got.Quotes["QQQ"].Price == nil || *got.Quotes["QQQ"].Price != newQQQ {
		t.Fatalf("QQQ update missing: %#v", got.Quotes["QQQ"])
	}
	if got.Errors["SPY"] != "snapshot timeout" {
		t.Fatalf("SPY error=%q, want snapshot timeout", got.Errors["SPY"])
	}
}

func TestPollOnceIncludesHeldUnderlyingQuotes(t *testing.T) {
	t.Parallel()
	aaplPrice := 207.42
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1}
	client := &fakeClient{
		status:    &rpc.HealthResult{Connected: true, GatewayHost: "127.0.0.1", GatewayPort: 7497},
		calendar:  &rpc.MarketCalendarResult{Market: "us_equity", Session: rpc.MarketSession{State: "regular", IsOpen: true}},
		account:   &rpc.AccountResult{BaseCurrency: "USD", NetLiquidation: 100000},
		positions: &rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Underlying: "AAPL", Stock: &stock}}, Portfolio: &rpc.PositionsPortfolio{}},
		quotes: map[string]rpc.Quote{
			"AAPL": {Symbol: "AAPL", Price: &aaplPrice, DataType: rpc.MarketDataLive},
		},
		regime:  &rpc.RegimeMonitorResult{Fingerprint: rpc.Fingerprint{Key: "regime-1"}},
		stress:  &rpc.StressResult{Fingerprint: rpc.Fingerprint{Key: "fp-1"}},
		trading: &rpc.TradingStatus{CanPreview: true},
	}
	svc := New(client, time.Minute, time.Minute)

	snap := svc.PollOnce(context.Background())
	got := snap.Quotes.Quotes["AAPL"]
	if got.Price == nil || *got.Price != aaplPrice {
		t.Fatalf("AAPL quote missing from market_quotes: %#v", snap.Quotes)
	}
	var routed rpc.ContractParams
	for _, call := range client.QuoteCalls() {
		if call.Symbol == "AAPL" {
			routed = call
			break
		}
	}
	if routed.Symbol != "AAPL" || routed.SecType != "STK" || routed.Currency != "USD" {
		t.Fatalf("AAPL quote routed as %#v, want STK/USD", routed)
	}
}

func TestMarketQuoteContractsSkipFreshStreamedBaselines(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 9, 10, 0, 0, 0, time.UTC)
	existing := &MarketQuotes{
		AsOf:   now.Add(-time.Second),
		Quotes: map[string]rpc.Quote{},
	}
	for _, item := range marketQuoteContracts {
		label := normalizeQuoteLabel(item.label)
		existing.Quotes[label] = rpc.Quote{
			Symbol:       label,
			QuotePriceAt: now.Add(-time.Second),
			AsOf:         now.Add(-time.Second),
		}
	}
	stock := rpc.PositionView{Symbol: "AAPL", SecType: rpc.SecTypeStock, Currency: "USD", Multiplier: 1}
	positions := &rpc.PositionsResult{
		ByUnderlying: []rpc.PositionGroup{{Underlying: "AAPL", Stock: &stock}},
	}

	got := marketQuoteContractsFor(positions, existing, now, 15*time.Second)
	if len(got) != 1 || got[0].label != "AAPL" {
		t.Fatalf("contracts=%#v, want only held AAPL after fresh baselines", got)
	}
}

func TestMarketQuoteErrorIncludesDynamicSymbols(t *testing.T) {
	t.Parallel()
	got := marketQuoteError(map[string]string{
		"aapl": "snapshot timeout",
		"SPY":  "farm disconnected",
	})
	want := "SPY: farm disconnected | AAPL: snapshot timeout"
	if got != want {
		t.Fatalf("marketQuoteError()=%q, want %q", got, want)
	}
}

func TestNudgesPollSeparatesTransportFromDaemonSourceHealth(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	healthAt := now.Add(-time.Minute)
	client := &fakeClient{nudges: &rpc.NudgesSnapshotResult{
		AsOf:                  now,
		ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: healthAt},
		SourceHealth: rpc.NudgeSourceHealth{
			Policy:         rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Reconciliation: rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusUnavailable, Reason: rpc.NudgeHealthReasonSourceUnavailable, AsOf: healthAt},
			Capital:        rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Pins:           rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			Cadence:        rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
			ConfirmedFlow:  rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: healthAt},
		},
	}}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	service.PollNudgesOnce(t.Context())
	snap := service.Snapshot()
	if snap.Nudges == nil || snap.Nudges.SourceHealth.Aggregate != rpc.NudgeAggregateSuppressed {
		t.Fatalf("nudges=%+v, want daemon-suppressed snapshot", snap.Nudges)
	}
	source := snap.Sources["nudges"]
	if source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) {
		t.Fatalf("poll source=%+v, want current separate from daemon suppression", source)
	}

	client.nudgesErr = errors.New("socket path /private/sentinel token")
	now = now.Add(time.Minute)
	service.PollNudgesOnce(t.Context())
	snap = service.Snapshot()
	if snap.Nudges == nil {
		t.Fatal("last successful nudge snapshot was discarded")
	}
	source = snap.Sources["nudges"]
	if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable || source.Error != "" || !source.LastSuccessAt.Equal(now.Add(-time.Minute)) {
		t.Fatalf("poll source=%+v, want allowlisted unavailable state without raw error", source)
	}
}

func TestPollOnceAndForcedNudgePollSerializeWholeSnapshotReplacement(t *testing.T) {
	t.Run("normal poll owns writer first", func(t *testing.T) {
		now := time.Date(2026, 7, 19, 14, 0, 0, 0, time.UTC)
		statusStarted := make(chan struct{})
		releaseStatus := make(chan struct{})
		client := &pollOverlapClient{
			fakeClient:    &fakeClient{status: &rpc.HealthResult{Connected: true}},
			statusStarted: statusStarted,
			statusRelease: releaseStatus,
			nudgeResults:  []*rpc.NudgesSnapshotResult{readyNudges(now), readyNudges(now.Add(time.Minute))},
		}
		service := New(client, time.Hour, time.Hour)
		service.now = func() time.Time { return now }
		normalDone := make(chan Snapshot, 1)
		go func() { normalDone <- service.PollOnce(t.Context()) }()
		<-statusStarted
		forcedStarted := make(chan struct{})
		forcedDone := make(chan Snapshot, 1)
		go func() {
			close(forcedStarted)
			forcedDone <- service.PollNudgesOnce(t.Context())
		}()
		<-forcedStarted
		close(releaseStatus)
		<-normalDone
		<-forcedDone
		final := service.Snapshot()
		if final.Status == nil || !final.Status.Connected || final.Nudges == nil || !final.Nudges.AsOf.Equal(now.Add(time.Minute)) {
			t.Fatalf("final snapshot lost serialized fields: %+v", final)
		}
	})

	t.Run("forced nudge poll owns writer first", func(t *testing.T) {
		now := time.Date(2026, 7, 19, 14, 30, 0, 0, time.UTC)
		nudgeStarted := make(chan struct{})
		releaseNudge := make(chan struct{})
		statusStarted := make(chan struct{})
		client := &pollOverlapClient{
			fakeClient:        &fakeClient{status: &rpc.HealthResult{Connected: true}},
			statusStarted:     statusStarted,
			firstNudgeStarted: nudgeStarted,
			firstNudgeRelease: releaseNudge,
			nudgeResults:      []*rpc.NudgesSnapshotResult{readyNudges(now.Add(time.Minute)), readyNudges(now)},
		}
		service := New(client, time.Hour, time.Hour)
		service.now = func() time.Time { return now }
		forcedDone := make(chan Snapshot, 1)
		go func() { forcedDone <- service.PollNudgesOnce(t.Context()) }()
		<-nudgeStarted
		normalStarted := make(chan struct{})
		normalDone := make(chan Snapshot, 1)
		go func() {
			close(normalStarted)
			normalDone <- service.PollOnce(t.Context())
		}()
		<-normalStarted
		close(releaseNudge)
		<-forcedDone
		<-statusStarted
		<-normalDone
		final := service.Snapshot()
		if final.Status == nil || !final.Status.Connected || final.Nudges == nil || !final.Nudges.AsOf.Equal(now.Add(time.Minute)) {
			t.Fatalf("final snapshot lost serialized fields: %+v", final)
		}
	})
}

func TestStartupStatusAndForcedNudgePollSerializeWholeSnapshotReplacement(t *testing.T) {
	now := time.Date(2026, 7, 19, 15, 0, 0, 0, time.UTC)
	statusStarted := make(chan struct{})
	releaseStatus := make(chan struct{})
	client := &pollOverlapClient{
		fakeClient:    &fakeClient{status: &rpc.HealthResult{Connected: true}},
		statusStarted: statusStarted,
		statusRelease: releaseStatus,
		nudgeResults:  []*rpc.NudgesSnapshotResult{readyNudges(now.Add(time.Minute))},
	}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	statusDone := make(chan Snapshot, 1)
	go func() { statusDone <- service.pollStatus(t.Context()) }()
	<-statusStarted
	if service.pollMu.TryLock() {
		service.pollMu.Unlock()
		close(releaseStatus)
		<-statusDone
		t.Fatal("pollStatus did not own the full-snapshot writer lock while status I/O was blocked")
	}
	forcedStarted := make(chan struct{})
	forcedDone := make(chan Snapshot, 1)
	go func() {
		close(forcedStarted)
		forcedDone <- service.PollNudgesOnce(t.Context())
	}()
	<-forcedStarted
	close(releaseStatus)
	<-statusDone
	<-forcedDone
	final := service.Snapshot()
	if final.Status == nil || !final.Status.Connected || final.Nudges == nil || !final.Nudges.AsOf.Equal(now.Add(time.Minute)) {
		t.Fatalf("final snapshot lost startup status or forced nudges: %+v", final)
	}
}

func TestNudgesPollUsesDedicatedOneMinuteCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Second, time.Hour)
	service.now = func() time.Time { return now }
	service.PollOnce(t.Context())
	now = now.Add(59 * time.Second)
	service.PollOnce(t.Context())
	now = now.Add(time.Second)
	service.PollOnce(t.Context())
	client.quoteMu.Lock()
	calls := client.nudgeCalls
	client.quoteMu.Unlock()
	if calls != 2 {
		t.Fatalf("nudges calls=%d, want initial and one-minute poll", calls)
	}
}

func TestNudgesSourceStartsNotObservedAndAgesToStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 18, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	startup := service.Snapshot().Sources["nudges"]
	if startup.State != SourceStateNotObserved || startup.Reason != SourceReasonNotObserved || !startup.LastSuccessAt.IsZero() {
		t.Fatalf("startup source=%+v", startup)
	}
	service.PollNudgesOnce(t.Context())
	now = now.Add(nudgesPollEvery + time.Nanosecond)
	aged := service.Snapshot().Sources["nudges"]
	if aged.State != SourceStateStale || aged.Reason != SourceReasonPollStale || aged.LastSuccessAt.IsZero() {
		t.Fatalf("aged source=%+v", aged)
	}
}

func TestNudgesExplicitOutageRemainsUnavailableAfterFreshnessBudget(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 19, 9, 0, 0, 0, time.UTC)
	client := &fakeClient{nudges: readyNudges(now)}
	service := New(client, time.Hour, time.Hour)
	service.now = func() time.Time { return now }
	service.PollNudgesOnce(t.Context())
	client.nudgesErr = errors.New("transport sentinel")
	now = now.Add(2 * nudgesPollEvery)
	service.PollNudgesOnce(t.Context())
	source := service.Snapshot().Sources["nudges"]
	if source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable {
		t.Fatalf("explicit outage was aged to stale: %+v", source)
	}
}

func TestAlertCandidatesPollOnStressCadenceThroughAuthority(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	candidate := liveAlertCandidate(t, now)
	client := &alertCandidateFakeClient{
		fakeClient: &fakeClient{},
		snapshot:   liveAlertSnapshot(now, candidate),
	}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	authority := setTestAlertAuthority(t, service, store)

	first := service.PollOnce(t.Context())
	if first.AlertCandidates == nil || first.AlertCandidates.CurrentState != rpc.AlertSnapshotActive {
		t.Fatalf("alert candidate snapshot=%+v, want active", first.AlertCandidates)
	}
	if source := first.Sources["alert_candidates"]; source.State != SourceStateCurrent || source.Reason != SourceReasonNone || !source.LastSuccessAt.Equal(now) {
		t.Fatalf("alert candidate source=%+v, want current", source)
	}
	publicJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var publicSnapshot map[string]json.RawMessage
	if err := json.Unmarshal(publicJSON, &publicSnapshot); err != nil {
		t.Fatal(err)
	}
	_, producerSnapshotExposed := publicSnapshot["alert_candidates"]
	if producerSnapshotExposed || strings.Contains(string(publicJSON), candidate.EpisodeKey) || strings.Contains(string(publicJSON), candidate.OccurrenceKey) {
		t.Fatalf("private producer snapshot escaped into live HTTP/SSE JSON: %s", publicJSON)
	}
	view := store.AlertDelivery(now)
	if !view.Initialized || view.CurrentState != rpc.AlertSnapshotActive || len(view.Occurrences) != 1 {
		t.Fatalf("authoritative alert delivery view=%+v", view)
	}
	if view.Occurrences[0].Disposition != state.AlertDispositionCutoverExisting || len(store.AlertDeliveriesDue(now)) != 0 {
		t.Fatalf("first active cutover occurrence became transport due: occurrence=%+v due=%d", view.Occurrences[0], len(store.AlertDeliveriesDue(now)))
	}
	if authority.observeCount() != 1 {
		t.Fatalf("normal candidate poll bypassed the sole observer: %d observations", authority.observeCount())
	}

	// Returned snapshots cannot mutate the service-owned typed contract.
	first.AlertCandidates.Coverage.ExpectedSources[0] = rpc.AlertSourceRegime
	if got := service.Snapshot().AlertCandidates.Coverage.ExpectedSources[0]; got != rpc.AlertSourceStress {
		t.Fatalf("snapshot clone aliased expected sources: %q", got)
	}

	now = now.Add(30 * time.Second)
	service.PollOnce(t.Context())
	if calls := client.Calls(); calls != 1 {
		t.Fatalf("alert candidate calls before stress cadence=%d, want 1", calls)
	}
	now = now.Add(31 * time.Second)
	service.PollOnce(t.Context())
	if calls := client.Calls(); calls != 2 {
		t.Fatalf("alert candidate calls after stress cadence=%d, want 2", calls)
	}
}

func TestAlertCandidatesPollAfterCurrentStressProducerInSameCadence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 30, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{
		fakeClient: &fakeClient{stress: &rpc.StressResult{}, regime: &rpc.RegimeMonitorResult{}},
		snapshot:   liveAlertSnapshot(now.Add(-time.Minute)),
	}
	current := liveAlertCandidate(t, now)
	client.producerHook = func() { client.Set(liveAlertSnapshot(now, current), nil) }
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	setTestAlertAuthority(t, service, store)

	got := service.PollOnce(t.Context())
	if got.AlertCandidates == nil || got.AlertCandidates.CurrentState != rpc.AlertSnapshotActive || len(got.AlertCandidates.Candidates) != 1 {
		t.Fatalf("same-cycle producer snapshot was not ingested: %+v", got.AlertCandidates)
	}
	if stressCalls, alertCalls := client.StressCalls(), client.Calls(); stressCalls != 1 || alertCalls != 1 {
		t.Fatalf("same-cycle refresh duplicated work: stress=%d alert_snapshot=%d", stressCalls, alertCalls)
	}
	if view := store.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotActive || len(view.Occurrences) != 1 {
		t.Fatalf("same-cycle candidate was not persisted: %+v", view)
	}
}

func TestAlertCandidatePollFollowsAllSameCycleProducers(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 45, 0, 0, time.UTC)
	client := &alertCandidateFakeClient{
		fakeClient: &fakeClient{
			stress: &rpc.StressResult{AsOf: now}, regime: &rpc.RegimeMonitorResult{AsOf: now},
			rules: &rpc.RulesResult{AsOf: now, Enabled: true, Status: "ok"},
		},
		orders:   &rpc.OrdersOpenResult{AsOf: now, Orders: []rpc.OrderView{}},
		snapshot: liveAlertSnapshot(now),
	}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }

	service.PollOnce(t.Context())
	if got, want := strings.Join(client.CallOrder(), ","), "stress,rules,orders_open,alert_candidates"; got != want {
		t.Fatalf("same-cycle producer order=%q, want %q", got, want)
	}
}

func TestRepeatedOldAlertSnapshotCannotResetAuthoritativeFreshness(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 21, 13, 0, 0, 0, time.UTC)
	now := base
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(base)}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	setTestAlertAuthority(t, service, store)
	first := service.PollOnce(t.Context())
	if source := first.Sources["alert_candidates"]; source.State != SourceStateCurrent || !source.LastSuccessAt.Equal(base) {
		t.Fatalf("initial source freshness=%+v", source)
	}

	now = base.Add(time.Minute + time.Nanosecond)
	second := service.PollOnce(t.Context())
	if second.AlertCandidates == nil || second.AlertCandidates.CurrentState != rpc.AlertSnapshotUnknown || second.AlertCandidates.Coverage.Freshness != rpc.AlertCoverageStale {
		t.Fatalf("identical old clear snapshot refreshed itself: %+v", second.AlertCandidates)
	}
	if source := second.Sources["alert_candidates"]; source.State != SourceStateStale || source.Reason != SourceReasonPollStale || !source.LastSuccessAt.Equal(base) || !source.UpdatedAt.Equal(now) {
		t.Fatalf("authoritative freshness was reset by poll time: %+v", source)
	}
	if view := store.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.Freshness != rpc.AlertCoverageStale || !view.Coverage.AsOf.Equal(base) {
		t.Fatalf("durable ledger retained a false-fresh clear: %+v", view)
	}
}

func TestInFlightAlertCandidatePollPrecedesFreshnessExpiry(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 21, 13, 30, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := New(&fakeClient{}, 5*time.Second, time.Minute)
	service.now = func() time.Time { return base }
	setTestAlertAuthority(t, service, store)
	seed := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(base)}
	if _, _, err := service.pollAlertCandidates(t.Context(), seed, base); err != nil {
		t.Fatalf("seed alert candidates: %v", err)
	}

	replyAt := base.Add(30 * time.Second)
	started := make(chan struct{})
	release := make(chan struct{})
	client := &blockingAlertCandidateClient{
		snapshot: liveAlertSnapshot(replyAt),
		started:  started,
		release:  release,
	}
	type pollResult struct {
		snapshot *rpc.AlertCandidateSnapshot
		source   SourceMeta
		err      error
	}
	pollDone := make(chan pollResult, 1)
	go func() {
		snapshot, source, err := service.pollAlertCandidates(t.Context(), client, replyAt)
		pollDone <- pollResult{snapshot: snapshot, source: source, err: err}
	}()
	<-started

	// Reproduce the freshness guard at the prior snapshot's expiry boundary.
	// If the in-flight poll did not reserve alert ordering before its RPC, the
	// guard can persist a later synthetic snapshot and make the valid reply old.
	if service.alertMu.TryLock() {
		service.alertMu.Unlock()
		close(release)
		<-pollDone
		t.Fatal("in-flight alert poll did not reserve freshness ordering before its RPC")
	}
	expiryDone := make(chan struct{})
	go func() {
		service.expireAlertSnapshot(base.Add(time.Minute + time.Nanosecond))
		close(expiryDone)
	}()
	select {
	case <-expiryDone:
		t.Fatal("freshness expiry completed ahead of an in-flight producer reply")
	default:
	}
	close(release)
	result := <-pollDone
	<-expiryDone
	if result.err != nil {
		t.Fatalf("valid in-flight alert snapshot was rejected after expiry: %v", result.err)
	}
	if result.snapshot == nil || !result.snapshot.AsOf.Equal(replyAt) || result.snapshot.Coverage.Freshness != rpc.AlertCoverageCurrent {
		t.Fatalf("in-flight result=%+v, want current producer snapshot", result.snapshot)
	}
	if result.source.State != SourceStateCurrent || result.source.Reason != SourceReasonNone || !result.source.LastSuccessAt.Equal(replyAt) {
		t.Fatalf("in-flight source=%+v, want current", result.source)
	}
	if view := store.AlertDelivery(replyAt); !view.AsOf.Equal(replyAt) || view.CurrentState != rpc.AlertSnapshotClear || view.Coverage.Freshness != rpc.AlertCoverageCurrent {
		t.Fatalf("valid in-flight snapshot was not authoritative: %+v", view)
	}

	// Serialization must not suppress normal fail-closed aging after the poll.
	expiredAt := replyAt.Add(time.Minute + time.Nanosecond)
	service.expireAlertSnapshot(expiredAt)
	aged := service.Snapshot()
	if aged.AlertCandidates == nil || aged.AlertCandidates.CurrentState != rpc.AlertSnapshotUnknown || aged.AlertCandidates.Coverage.Freshness != rpc.AlertCoverageStale {
		t.Fatalf("completed poll did not age fail closed: %+v", aged.AlertCandidates)
	}
	if view := store.AlertDelivery(expiredAt); !view.AsOf.Equal(expiredAt) || view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.Freshness != rpc.AlertCoverageStale {
		t.Fatalf("aged in-flight snapshot was not persisted fail closed: %+v", view)
	}
}

func TestAlertCandidateOutageReplacesPriorClearWithPersistedUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(now)}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	authority := setTestAlertAuthority(t, service, store)
	if first := service.PollOnce(t.Context()); first.AlertCandidates == nil || first.AlertCandidates.CurrentState != rpc.AlertSnapshotClear {
		t.Fatalf("initial alert snapshot=%+v, want trusted clear", first.AlertCandidates)
	}

	now = now.Add(time.Minute)
	client.Set(nil, errors.New("daemon unavailable"))
	failed := service.PollOnce(t.Context())
	if failed.AlertCandidates == nil || failed.AlertCandidates.CurrentState != rpc.AlertSnapshotUnknown || failed.AlertCandidates.Coverage.State != rpc.AlertCoverageUnavailable {
		t.Fatalf("outage snapshot=%+v, want unknown/unavailable", failed.AlertCandidates)
	}
	if source := failed.Sources["alert_candidates"]; source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable {
		t.Fatalf("outage source=%+v", source)
	}
	view := store.AlertDelivery(now)
	if !view.Initialized || view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.State != rpc.AlertCoverageUnavailable {
		t.Fatalf("persisted outage view=%+v, want unknown/unavailable", view)
	}
	if authority.observeCount() != 2 {
		t.Fatalf("normal and outage observations did not share one observer: %d", authority.observeCount())
	}
	if err := rpc.ValidateAlertCandidateSnapshot(*failed.AlertCandidates); err != nil {
		t.Fatalf("typed outage snapshot invalid: %v", err)
	}
	if len(failed.AlertCandidates.Sources) != 1 || failed.AlertCandidates.Sources[0].Covered || failed.AlertCandidates.Sources[0].EvidenceHealth != rpc.AlertEvidenceUnavailable {
		t.Fatalf("outage source row is not explicit and unavailable: %+v", failed.AlertCandidates.Sources)
	}
}

func TestAlertCandidateDispatchFailureKeepsCommittedProducerEvidence(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 15, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(now)}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	authority := setTestAlertAuthority(t, service, store)
	authority.failAfterPersist(errors.New("push completion unavailable"))

	got := service.PollOnce(t.Context())
	if got.AlertCandidates == nil || got.AlertCandidates.CurrentState != rpc.AlertSnapshotClear || got.AlertCandidates.Coverage.Freshness != rpc.AlertCoverageCurrent {
		t.Fatalf("delivery failure destroyed committed producer evidence: %+v", got.AlertCandidates)
	}
	if source := got.Sources["alert_candidates"]; source.State != SourceStateCurrent || !source.LastSuccessAt.Equal(now) {
		t.Fatalf("delivery failure mislabeled current producer source: %+v", source)
	}
	if view := store.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotClear || !view.AsOf.Equal(now) {
		t.Fatalf("committed producer evidence was overwritten: %+v", view)
	}
	if authority.observeCount() != 1 {
		t.Fatalf("delivery failure triggered an unavailable overwrite: %d observations", authority.observeCount())
	}
	foundDeliveryError := false
	for _, sourceErr := range got.Errors {
		if sourceErr.Source == "alert_candidates" {
			t.Fatalf("delivery failure was attributed to producer evidence: %+v", sourceErr)
		}
		if sourceErr.Source == "alert_delivery" {
			foundDeliveryError = true
		}
	}
	if !foundDeliveryError {
		t.Fatalf("delivery failure was not surfaced separately: %+v", got.Errors)
	}
}

func TestAlertCandidateRestartOutageUsesPersistedCoverageAndNeverClears(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	firstClient := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(now)}
	firstService := New(firstClient, 5*time.Second, time.Minute)
	firstService.now = func() time.Time { return now }
	setTestAlertAuthority(t, firstService, store)
	firstService.PollOnce(t.Context())
	if view := store.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotClear {
		t.Fatalf("seed view=%+v, want clear", view)
	}

	restarted, err := state.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	failingClient := &alertCandidateFakeClient{fakeClient: &fakeClient{}, err: errors.New("method unavailable")}
	restartedService := New(failingClient, 5*time.Second, time.Minute)
	restartedService.now = func() time.Time { return now }
	restartAuthority := setTestAlertAuthority(t, restartedService, restarted)
	if primed := restarted.AlertDelivery(now); primed.CurrentState != rpc.AlertSnapshotUnknown || primed.Coverage.State != rpc.AlertCoverageUnavailable {
		t.Fatalf("restart served prior clear before first daemon poll: %+v", primed)
	}
	if restartAuthority.observeCount() != 1 {
		t.Fatalf("restart priming bypassed the sole observer: %d", restartAuthority.observeCount())
	}
	got := restartedService.PollOnce(t.Context())
	if got.AlertCandidates == nil || got.AlertCandidates.CurrentState != rpc.AlertSnapshotUnknown || len(got.AlertCandidates.Coverage.ExpectedSources) != 1 {
		t.Fatalf("restart outage snapshot=%+v", got.AlertCandidates)
	}
	if view := restarted.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.State != rpc.AlertCoverageUnavailable {
		t.Fatalf("restart outage persisted false clear: %+v", view)
	}
}

func TestAlertCandidateColdOutageRemainsUninitialized(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, err: errors.New("method unavailable")}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	setTestAlertAuthority(t, service, store)
	got := service.PollOnce(t.Context())
	if got.AlertCandidates != nil {
		t.Fatalf("cold outage invented coverage: %+v", got.AlertCandidates)
	}
	if source := got.Sources["alert_candidates"]; source.State != SourceStateUnavailable || source.Reason != SourceReasonTransportUnavailable {
		t.Fatalf("cold outage source=%+v", source)
	}
	if view := store.AlertDelivery(now); view.Initialized || view.CurrentState == rpc.AlertSnapshotClear {
		t.Fatalf("cold outage initialized a false clear: %+v", view)
	}
}

func TestAlertCandidateSnapshotAgeProjectsClearToTypedStaleUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(now)}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	authority := setTestAlertAuthority(t, service, store)
	service.PollOnce(t.Context())
	now = now.Add(time.Minute + time.Nanosecond)
	aged := service.Snapshot()
	if aged.AlertCandidates == nil || aged.AlertCandidates.CurrentState != rpc.AlertSnapshotUnknown || aged.AlertCandidates.Coverage.Freshness != rpc.AlertCoverageStale {
		t.Fatalf("aged snapshot=%+v, want stale unknown", aged.AlertCandidates)
	}
	if err := rpc.ValidateAlertCandidateSnapshot(*aged.AlertCandidates); err != nil {
		t.Fatalf("aged typed snapshot invalid: %v", err)
	}
	if source := aged.Sources["alert_candidates"]; source.State != SourceStateStale || source.Reason != SourceReasonPollStale {
		t.Fatalf("aged source=%+v", source)
	}
	if view := store.AlertDelivery(now); view.CurrentState != rpc.AlertSnapshotUnknown || view.Coverage.Freshness != rpc.AlertCoverageStale {
		t.Fatalf("aged clear remained durable: %+v", view)
	}
	if authority.observeCount() != 2 {
		t.Fatalf("poll and expiry did not share one observer: %d", authority.observeCount())
	}
	if len(aged.AlertCandidates.Sources) != 1 || aged.AlertCandidates.Sources[0].Status != "stale" || aged.AlertCandidates.Sources[0].Reason != "freshness_expired" || aged.AlertCandidates.Sources[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("expiry did not age the source row consistently: %+v", aged.AlertCandidates.Sources)
	}
}

func TestAlertCandidateExpiryAgesActiveEvidenceInLedger(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 7, 21, 12, 30, 0, 0, time.UTC)
	now := base
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	client := &alertCandidateFakeClient{fakeClient: &fakeClient{}, snapshot: liveAlertSnapshot(now)}
	service := New(client, 5*time.Second, time.Minute)
	service.now = func() time.Time { return now }
	authority := setTestAlertAuthority(t, service, store)
	service.PollOnce(t.Context())

	now = now.Add(time.Minute)
	candidate := liveAlertCandidate(t, now)
	client.Set(liveAlertSnapshot(now, candidate), nil)
	service.PollOnce(t.Context())

	now = now.Add(time.Minute + time.Nanosecond)
	authority.failAfterPersist(errors.New("push completion unavailable during expiry"))
	aged := service.Snapshot()
	if aged.AlertCandidates == nil || len(aged.AlertCandidates.Candidates) != 1 || aged.AlertCandidates.Candidates[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("active candidate evidence did not age: %+v", aged.AlertCandidates)
	}
	view := store.AlertDelivery(now)
	if len(view.Sources) != 1 || view.Sources[0].EvidenceHealth != rpc.AlertEvidenceStale || len(view.Occurrences) != 1 || view.Occurrences[0].EvidenceHealth != rpc.AlertEvidenceStale {
		t.Fatalf("ledger retained current evidence under stale coverage: sources=%+v occurrences=%+v", view.Sources, view.Occurrences)
	}
	if authority.observeCount() != 3 {
		t.Fatalf("baseline, active, and expiry did not share one observer: %d", authority.observeCount())
	}
}

func TestClientWithoutAlertCapabilityLeavesSourceNotObserved(t *testing.T) {
	t.Parallel()
	service := New(&fakeClient{}, 5*time.Second, time.Minute)
	got := service.PollOnce(t.Context())
	if got.AlertCandidates != nil {
		t.Fatalf("client without capability produced snapshot: %+v", got.AlertCandidates)
	}
	if source := got.Sources["alert_candidates"]; source.State != SourceStateNotObserved || source.Reason != SourceReasonNotObserved {
		t.Fatalf("client without capability source=%+v", source)
	}
}

func liveAlertSnapshot(at time.Time, candidates ...rpc.AlertCandidate) *rpc.AlertCandidateSnapshot {
	if candidates == nil {
		candidates = []rpc.AlertCandidate{}
	}
	current := rpc.AlertSnapshotClear
	if len(candidates) > 0 {
		current = rpc.AlertSnapshotActive
	}
	return &rpc.AlertCandidateSnapshot{
		SchemaVersion:  rpc.AlertCandidateSnapshotVersion,
		AuthorityScope: liveAlertTestAuthorityScope,
		AsOf:           at,
		CurrentState:   current,
		Coverage: rpc.AlertCoverage{
			State:           rpc.AlertCoverageComplete,
			Freshness:       rpc.AlertCoverageCurrent,
			AsOf:            at,
			ExpectedSources: []rpc.AlertSource{rpc.AlertSourceStress},
			CoveredSources:  []rpc.AlertSource{rpc.AlertSourceStress},
		},
		Sources: []rpc.AlertSourceCoverage{{
			Source: rpc.AlertSourceStress, Status: "current", Reason: "current", EvidenceHealth: rpc.AlertEvidenceCurrent,
			InputAsOf: at, ObservedAt: at, EvidenceAsOf: at, FreshUntil: at.Add(time.Hour), Covered: true,
		}},
		Candidates: append([]rpc.AlertCandidate{}, candidates...),
	}
}

func liveAlertCandidate(t *testing.T, at time.Time) rpc.AlertCandidate {
	t.Helper()
	episode, err := rpc.BuildAlertEpisodeKey(rpc.AlertSourceStress, rpc.AlertKindMarketState, "live-service-test")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := rpc.BuildAlertOccurrenceKey(episode, "opening")
	if err != nil {
		t.Fatal(err)
	}
	return rpc.AlertCandidate{
		EpisodeKey: episode, OccurrenceKey: occurrence,
		EvidenceFingerprint: "sha256:" + strings.Repeat("a", 64),
		Source:              rpc.AlertSourceStress, Kind: rpc.AlertKindMarketState, PresentationCode: rpc.AlertPresentationPortfolioStress,
		State: rpc.AlertEpisodeOpen, Severity: rpc.AlertSeverityWatch,
		EvidenceHealth: rpc.AlertEvidenceCurrent, Destination: rpc.AlertDestinationAlerts,
		EvidenceAsOf: at, StateChangedAt: at, ObservedAt: at,
	}
}

func readyNudges(at time.Time) *rpc.NudgesSnapshotResult {
	ok := rpc.NudgeInputHealth{Status: rpc.NudgeInputStatusOK, AsOf: at}
	return &rpc.NudgesSnapshotResult{AsOf: at, SourceHealth: rpc.NudgeSourceHealth{
		Policy: ok, Reconciliation: ok, Capital: ok, Pins: ok, Cadence: ok, ConfirmedFlow: ok,
	}, ConfirmedFlowCoverage: &rpc.NudgeConfirmedFlowCoverage{CoverageFrom: at}}
}

type testAlertAuthority struct {
	store *state.Store
	mu    sync.Mutex
	seen  []rpc.AlertCandidateSnapshot
	after error
}

func setTestAlertAuthority(t *testing.T, service *Service, store *state.Store) *testAlertAuthority {
	t.Helper()
	authority := &testAlertAuthority{store: store}
	if err := service.SetAlertSnapshotAuthority(authority); err != nil {
		t.Fatal(err)
	}
	return authority
}

func (a *testAlertAuthority) Observe(_ context.Context, snapshot rpc.AlertCandidateSnapshot) (state.AlertDeliveryView, error) {
	view, err := a.store.ObserveAlertSnapshot(snapshot)
	a.mu.Lock()
	a.seen = append(a.seen, snapshot)
	after := a.after
	a.mu.Unlock()
	if err != nil {
		return view, err
	}
	return view, after
}

func (a *testAlertAuthority) Current(now time.Time) state.AlertDeliveryView {
	return a.store.AlertDelivery(now)
}

func (a *testAlertAuthority) observeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.seen)
}

func (a *testAlertAuthority) failAfterPersist(err error) {
	a.mu.Lock()
	a.after = err
	a.mu.Unlock()
}

type fakeClient struct {
	status       *rpc.HealthResult
	calendar     *rpc.MarketCalendarResult
	account      *rpc.AccountResult
	accountErr   error
	positions    *rpc.PositionsResult
	positionsErr error
	quotes       map[string]rpc.Quote
	quoteErrs    map[string]error
	regime       *rpc.RegimeMonitorResult
	stress       *rpc.StressResult
	stressErr    error
	rules        *rpc.RulesResult
	rulesErr     error
	rulesNil     bool
	brief        *rpc.BriefResult
	briefErr     error
	nudges       *rpc.NudgesSnapshotResult
	nudgesErr    error
	marketEvents *rpc.MarketEventsResult
	trading      *rpc.TradingStatus

	stressBlock <-chan struct{}
	quoteBlock  <-chan struct{}
	quoteMu     sync.Mutex
	quoteCalls  []rpc.ContractParams
	briefCalls  int
	briefAcks   int
	nudgeCalls  int
}

type pollOverlapClient struct {
	*fakeClient
	mu                sync.Mutex
	statusOnce        sync.Once
	statusStarted     chan struct{}
	statusRelease     <-chan struct{}
	firstNudgeOnce    sync.Once
	firstNudgeStarted chan struct{}
	firstNudgeRelease <-chan struct{}
	nudgeResults      []*rpc.NudgesSnapshotResult
	nudgeCalls        int
}

type alertCandidateFakeClient struct {
	*fakeClient
	mu           sync.Mutex
	snapshot     *rpc.AlertCandidateSnapshot
	orders       *rpc.OrdersOpenResult
	err          error
	calls        int
	stressCalls  int
	callOrder    []string
	producerHook func()
}

type blockingAlertCandidateClient struct {
	snapshot *rpc.AlertCandidateSnapshot
	started  chan<- struct{}
	release  <-chan struct{}
}

func (c *blockingAlertCandidateClient) AlertCandidates(ctx context.Context) (*rpc.AlertCandidateSnapshot, error) {
	close(c.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return cloneAlertCandidateSnapshot(c.snapshot), nil
	}
}

func (c *alertCandidateFakeClient) StressWithRegime(ctx context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	c.mu.Lock()
	c.stressCalls++
	c.callOrder = append(c.callOrder, "stress")
	hook := c.producerHook
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return c.fakeClient.StressWithRegime(ctx)
}

func (c *alertCandidateFakeClient) AlertCandidates(context.Context) (*rpc.AlertCandidateSnapshot, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.callOrder = append(c.callOrder, "alert_candidates")
	if c.err != nil {
		return nil, c.err
	}
	return cloneAlertCandidateSnapshot(c.snapshot), nil
}

func (c *alertCandidateFakeClient) Rules(ctx context.Context) (*rpc.RulesResult, error) {
	c.recordCall("rules")
	return c.fakeClient.Rules(ctx)
}

func (c *alertCandidateFakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callOrder = append(c.callOrder, "orders_open")
	return c.orders, nil
}

func (c *alertCandidateFakeClient) recordCall(name string) {
	c.mu.Lock()
	c.callOrder = append(c.callOrder, name)
	c.mu.Unlock()
}

func (c *alertCandidateFakeClient) CallOrder() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.callOrder...)
}

func (c *alertCandidateFakeClient) Set(snapshot *rpc.AlertCandidateSnapshot, err error) {
	c.mu.Lock()
	c.snapshot = cloneAlertCandidateSnapshot(snapshot)
	c.err = err
	c.mu.Unlock()
}

func (c *alertCandidateFakeClient) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *alertCandidateFakeClient) StressCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stressCalls
}

func (c *pollOverlapClient) Status(context.Context) (*rpc.HealthResult, error) {
	c.statusOnce.Do(func() {
		if c.statusStarted != nil {
			close(c.statusStarted)
		}
		if c.statusRelease != nil {
			<-c.statusRelease
		}
	})
	return c.fakeClient.status, nil
}

func (c *pollOverlapClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	c.mu.Lock()
	call := c.nudgeCalls
	c.nudgeCalls++
	c.mu.Unlock()
	if call == 0 {
		c.firstNudgeOnce.Do(func() {
			if c.firstNudgeStarted != nil {
				close(c.firstNudgeStarted)
			}
			if c.firstNudgeRelease != nil {
				<-c.firstNudgeRelease
			}
		})
	}
	if call >= len(c.nudgeResults) {
		return nil, nil
	}
	return c.nudgeResults[call], nil
}

func (c *fakeClient) Status(context.Context) (*rpc.HealthResult, error) {
	return c.status, nil
}

func (c *fakeClient) MarketCalendar(context.Context) (*rpc.MarketCalendarResult, error) {
	return c.calendar, nil
}

func (c *fakeClient) MarketCalendarFor(context.Context, string) (*rpc.MarketCalendarResult, error) {
	return c.calendar, nil
}

func (c *fakeClient) Account(context.Context) (*rpc.AccountResult, error) {
	return c.account, c.accountErr
}

func (c *fakeClient) Positions(context.Context) (*rpc.PositionsResult, error) {
	return c.positions, c.positionsErr
}

func (c *fakeClient) Quote(_ context.Context, contract rpc.ContractParams) (*rpc.Quote, error) {
	if c.quoteBlock != nil {
		<-c.quoteBlock
	}
	c.quoteMu.Lock()
	c.quoteCalls = append(c.quoteCalls, contract)
	c.quoteMu.Unlock()
	if err := c.quoteErrs[contract.Symbol]; err != nil {
		return nil, err
	}
	q, ok := c.quotes[contract.Symbol]
	if !ok {
		q = rpc.Quote{Symbol: contract.Symbol, Contract: contract, DataType: rpc.MarketDataLive}
	}
	return &q, nil
}

func (c *fakeClient) QuoteCalls() []rpc.ContractParams {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	return append([]rpc.ContractParams(nil), c.quoteCalls...)
}

func (c *fakeClient) StreamQuote(context.Context, rpc.ContractParams, func(rpc.Frame) error) error {
	return nil
}

func (c *fakeClient) MarketEvents(context.Context, rpc.MarketEventsParams) (*rpc.MarketEventsResult, error) {
	return c.marketEvents, nil
}

func (c *fakeClient) Stress(context.Context) (*rpc.StressResult, error) {
	return c.stress, nil
}

func (c *fakeClient) StressWithRegime(context.Context) (*rpc.StressResult, *rpc.RegimeMonitorResult, error) {
	if c.stressBlock != nil {
		<-c.stressBlock
	}
	return c.stress, c.regime, c.stressErr
}

func (c *fakeClient) Rules(context.Context) (*rpc.RulesResult, error) {
	if c.rulesErr != nil || c.rulesNil {
		return nil, c.rulesErr
	}
	if c.rules != nil {
		return c.rules, nil
	}
	return &rpc.RulesResult{Enabled: true, Status: "ok"}, nil
}

func (c *fakeClient) Brief(context.Context) (*rpc.BriefResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.briefCalls++
	return c.brief, c.briefErr
}

func (c *fakeClient) NudgesSnapshot(context.Context) (*rpc.NudgesSnapshotResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.nudgeCalls++
	return c.nudges, c.nudgesErr
}

func (c *fakeClient) BriefAck(context.Context, rpc.BriefAckParams) (*rpc.BriefAckResult, error) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	c.briefAcks++
	return &rpc.BriefAckResult{OK: true}, nil
}

func (c *fakeClient) BriefCounts() (brief, ack int) {
	c.quoteMu.Lock()
	defer c.quoteMu.Unlock()
	return c.briefCalls, c.briefAcks
}

func (c *fakeClient) ReconcileSignoff(context.Context, rpc.CapitalEventParams) (*rpc.RiskPolicyWriteResult, error) {
	return &rpc.RiskPolicyWriteResult{OK: true}, nil
}

func (c *fakeClient) TradingStatus(context.Context) (*rpc.TradingStatus, error) {
	return c.trading, nil
}

func (c *fakeClient) AutoTradeStatus(context.Context) (*rpc.AutoTradeStatus, error) {
	return &rpc.AutoTradeStatus{ProposalsEnabled: true, FastPathEnabled: true}, nil
}

func (c *fakeClient) OpportunitiesStatus(context.Context) (*rpc.OpportunityStatus, error) {
	return &rpc.OpportunityStatus{Enabled: true}, nil
}

func (c *fakeClient) OpportunitiesSnapshot(context.Context, rpc.OpportunitySnapshotParams) (*rpc.OpportunitySnapshot, error) {
	return &rpc.OpportunitySnapshot{Kind: rpc.OpportunitySnapshotKind, SchemaVersion: rpc.OpportunitySnapshotSchemaVersion, Revision: "empty", Opportunities: []rpc.Opportunity{}}, nil
}

func (c *fakeClient) OpportunitiesRefresh(context.Context, rpc.OpportunityRefreshParams) (*rpc.OpportunitySnapshot, error) {
	return c.OpportunitiesSnapshot(context.Background(), rpc.OpportunitySnapshotParams{})
}

func (c *fakeClient) OpportunitiesPreviewExercise(context.Context, rpc.OpportunityExercisePreviewParams) (*rpc.OpportunityExercisePreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) OpportunitiesSubmitExercise(context.Context, rpc.OpportunityExerciseSubmitParams) (*rpc.OpportunityExerciseSubmitResult, error) {
	return nil, nil
}

func (c *fakeClient) OpportunitiesIgnore(context.Context, rpc.OpportunityIgnoreParams) (*rpc.OpportunityIgnoreResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsSnapshot(context.Context, rpc.TradeProposalSnapshotParams) (*rpc.TradeProposalSnapshot, error) {
	return &rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, Revision: "empty", Proposals: []rpc.TradeProposal{}}, nil
}

func (c *fakeClient) TradeProposalsRefresh(context.Context, rpc.TradeProposalRefreshParams) (*rpc.TradeProposalSnapshot, error) {
	return c.TradeProposalsSnapshot(context.Background(), rpc.TradeProposalSnapshotParams{})
}

func (c *fakeClient) TradeProposalsPreview(context.Context, rpc.TradeProposalPreviewParams) (*rpc.TradeProposalPreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsSubmit(context.Context, rpc.TradeProposalSubmitParams) (*rpc.TradeProposalSubmitResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePreview(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReduceSubmit(context.Context, rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePortfolioPreview(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsReducePortfolioSubmit(context.Context, rpc.TradeProposalReducePortfolioParams) (*rpc.TradeProposalReducePortfolioResult, error) {
	return nil, nil
}

func (c *fakeClient) TradeProposalsIgnore(context.Context, rpc.TradeProposalIgnoreParams) (*rpc.TradeProposalIgnoreResult, error) {
	return nil, nil
}

func (c *fakeClient) Settings(context.Context) (*rpc.PlatformSettings, error) {
	return &rpc.PlatformSettings{
		Kind: "ibkr.platform_settings",
		MarketData: rpc.PlatformMarketDataSetting{
			Quality: rpc.PlatformMarketDataQuality{Status: "unknown", Access: rpc.SettingsAccessRead, Source: rpc.SettingsSourceObserved},
		},
	}, nil
}

func (c *fakeClient) UpdateSettings(context.Context, json.RawMessage) (*rpc.PlatformSettings, error) {
	return c.Settings(context.Background())
}

func (c *fakeClient) OrderPreview(context.Context, rpc.OrderPreviewParams) (*rpc.OrderPreviewResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderPlace(context.Context, rpc.OrderPlaceParams) (*rpc.OrderPlaceResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderModify(context.Context, rpc.OrderModifyParams) (*rpc.OrderModifyResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderCancel(context.Context, rpc.OrderCancelParams) (*rpc.OrderCancelResult, error) {
	return nil, nil
}

func (c *fakeClient) OrdersOpen(context.Context, rpc.OrdersOpenParams) (*rpc.OrdersOpenResult, error) {
	return nil, nil
}

func (c *fakeClient) OrderStatus(context.Context, rpc.OrderStatusParams) (*rpc.OrderStatusResult, error) {
	return nil, nil
}
