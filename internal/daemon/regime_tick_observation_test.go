package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// Every tick-derived quality used to be stamped with snapshot-build time, so a
// served row reported about a second old whatever its true vintage and a live
// subscription that had stopped delivering ticks was indistinguishable from one
// that was ticking. The stamp is now the instant the tick arrived.
func TestFirmTickQualityStampsTickArrival(t *testing.T) {
	t.Parallel()
	now := regimeTestNow
	observed := now.Add(-8 * time.Minute)

	got := firmTickQuality(observed, now, rpc.MarketDataLive, "VIX tick")

	if !got.AsOf.Equal(observed) {
		t.Fatalf("as_of=%s, want the tick arrival %s (not build time %s)", got.AsOf, observed, now)
	}
	if got.FreshnessClass != rpc.FreshnessLive {
		t.Errorf("freshness_class=%q, want live — a quiet feed is visible in the age, not by relabelling the subscription mode", got.FreshnessClass)
	}
	if class, ok := regimeTickQualityClass(got, now); !ok || class != rpc.FreshnessLive {
		t.Errorf("regimeTickQualityClass=(%q, %v), want (live, true): serving a real age must not change what is classifiable", class, ok)
	}
}

// Fail closed on both ways the observation instant can be unusable. Broker-
// adjacent timestamps are untrusted, and an absent one must never be invented:
// either way the quality carries no as_of, which classifies as unusable — the
// conservative reading, never "just observed".
func TestFirmTickQualityRefusesAbsentAndFutureStamps(t *testing.T) {
	t.Parallel()
	now := regimeTestNow

	for _, tc := range []struct {
		name       string
		observedAt time.Time
	}{
		{"absent", time.Time{}},
		{"ahead_of_the_daemon_clock", now.Add(2 * time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := firmTickQuality(tc.observedAt, now, rpc.MarketDataLive, "VIX tick")
			if !got.AsOf.IsZero() {
				t.Fatalf("as_of=%s, want zero so absent provenance cannot read as a fresh observation", got.AsOf)
			}
			if _, ok := regimeTickQualityClass(got, now); ok {
				t.Error("quality classified as usable with no as_of; absent evidence must keep the conservative treatment")
			}
		})
	}
}

// End-to-end: the connector's tick-arrival instant reaches every per-scalar
// quality a regime row publishes, through the snapshot closures and the
// bounded wrappers. Each leg is given a different age so a fetcher that
// stamped one leg with another's clock — or with its own — is caught.
func TestRegimeRowsPublishTickArrivalAsOf(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := regimeTestNow
	vixAt := now.Add(-30 * time.Second)
	hygAt := now.Add(-4 * time.Minute)
	spyAt := now.Add(-90 * time.Second)
	fxAt := now.Add(-2 * time.Minute)

	fake := &fakeDeps{
		snapshots: map[string]fakeQuote{
			"VIX":     {price: 18.0, dataType: rpc.MarketDataLive, observedAt: vixAt},
			"VIX3M":   {price: 21.0, dataType: rpc.MarketDataLive, observedAt: now.Add(-45 * time.Second)},
			"HYG":     {price: 79.0, dataType: rpc.MarketDataLive, observedAt: hygAt},
			"USD.JPY": {price: 147.5, dataType: rpc.MarketDataLive, observedAt: fxAt},
		},
		rich: map[string]fakeRichQuote{
			"SPY": {price: 620.0, week52High: 640.0, dataType: rpc.MarketDataLive, observedAt: spyAt},
		},
		bars: map[string]fakeHistory{
			"HYG":     {bars: makeBars(60, 78.0)},
			"USD.JPY": {bars: makeBars(20, 146.0)},
		},
	}

	vixTerm := fetchRegimeVIXTerm(ctx, fake.build())
	hygSpy := fetchRegimeHYGSPY(ctx, fake.build())
	usdJpy := fetchRegimeUSDJPY(ctx, fake.build())

	for _, tc := range []struct {
		field string
		got   *rpc.Quality
		want  time.Time
	}{
		{"vix_quality", vixTerm.VIXQuality, vixAt},
		{"hyg_quality", hygSpy.HYGQuality, hygAt},
		{"spy_quality", hygSpy.SPYQuality, spyAt},
		{"spy_52w_high_quality", hygSpy.SPY52WHighQuality, spyAt},
		{"usdjpy_last_quality", usdJpy.LastQuality, fxAt},
	} {
		if tc.got == nil {
			t.Errorf("%s missing", tc.field)
			continue
		}
		if !tc.got.AsOf.Equal(tc.want) {
			t.Errorf("%s.as_of=%s, want the tick arrival %s", tc.field, tc.got.AsOf, tc.want)
		}
		if tc.got.AsOf.Equal(now) {
			t.Errorf("%s.as_of is snapshot-build time; the row cannot show a quiet feed", tc.field)
		}
	}
}

// A fetcher reads its clock, then spends several seconds subscribing and
// waiting, so every tick it collects arrives after that read. Stamping against
// the fetcher's opening clock therefore made each live observation look
// future-dated, and the future-stamp refusal zeroed every served as_of — which
// is what the daemon did on the first live run of this change. The clock the
// refusal compares against has to be read when the quality is built.
func TestRegimeStampsTicksThatArriveAfterTheFetcherStarted(t *testing.T) {
	t.Parallel()
	start := regimeTestNow
	arrived := start.Add(3 * time.Second)

	// An advancing clock: the fetcher's opening read, then later reads once
	// the subscribe window has elapsed. A frozen clock cannot see this.
	reads := 0
	fake := &fakeDeps{
		snapshots: map[string]fakeQuote{
			"VIX":   {price: 18.0, dataType: rpc.MarketDataLive, observedAt: arrived},
			"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive, observedAt: arrived},
		},
	}
	deps := fake.build()
	deps.now = func() time.Time {
		reads++
		if reads == 1 {
			return start
		}
		return arrived.Add(time.Second)
	}

	got := fetchRegimeVIXTerm(context.Background(), deps)

	if got.VIXQuality == nil || got.VIXQuality.AsOf.IsZero() {
		t.Fatalf("vix_quality as_of=%v; a tick that arrived during the subscribe window is not future-dated", got.VIXQuality)
	}
	if !got.VIXQuality.AsOf.Equal(arrived) {
		t.Errorf("vix_quality.as_of=%s, want %s", got.VIXQuality.AsOf, arrived)
	}
	if got.VIX3MQuality == nil || !got.VIX3MQuality.AsOf.Equal(arrived) {
		t.Errorf("vix3m_quality as_of=%v, want %s", got.VIX3MQuality, arrived)
	}
}

// The point of the change: a subscription the gateway still labels live, but
// which stopped delivering ticks, now reads as a growing age. The row's own
// status is deliberately untouched — this task serves the honest number and
// leaves any staleness rule built on it to a separate decision.
func TestRegimeRowShowsAgeOfAQuietLiveSubscription(t *testing.T) {
	t.Parallel()
	now := regimeTestNow
	quiet := now.Add(-11 * time.Minute)

	got := fetchRegimeVIXTerm(context.Background(), (&fakeDeps{
		snapshots: map[string]fakeQuote{
			"VIX":   {price: 18.0, dataType: rpc.MarketDataLive, observedAt: quiet},
			"VIX3M": {price: 21.0, dataType: rpc.MarketDataLive, observedAt: now},
		},
	}).build())

	if got.VIXQuality == nil {
		t.Fatal("vix_quality missing")
	}
	if age := now.Sub(got.VIXQuality.AsOf); age != 11*time.Minute {
		t.Errorf("vix_quality age=%s, want 11m0s", age)
	}
	if got.Status != rpc.RegimeStatusOK {
		t.Errorf("status=%q, want ok: serving a real age must not start blocking on its own", got.Status)
	}
	if class, ok := regimeTickQualityClass(got.VIXQuality, now); !ok || class != rpc.FreshnessLive {
		t.Errorf("regimeTickQualityClass=(%q, %v), want (live, true): confirmation authority is unchanged by this task", class, ok)
	}
}
