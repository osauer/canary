package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// Each held name's earnings row carries the rulebook's own window verdict and
// session distance, so a reader shows a name reporting inside the window
// without re-deriving the rule. A name without a published date gets no
// verdict, never "outside".
func TestAssembleEarningsCarriesTheWindowVerdict(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 30, 16, 37, 0, 0, berlin) // Friday, a US session
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateWithAudit(context.Background(), now, rpc.OrderOriginHumanTTY, []string{"features.rulebook.earnings_overrides"}, func(next *platformSettingsData) error {
		next.Features.Rulebook.EarningsOverrides = map[string]string{"NEAR": "2026-11-02Tbmo", "FAR": "2026-11-20Tamc"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil, nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["UNDATED"] = earningsSymbolState{
		Resolution: earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished},
		Providers: map[string]earningsProviderState{earningsNasdaqProvider: {
			LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, ParserContractVersion: earningsNasdaqParserContract},
		}},
		UpdatedAt: now,
	}
	s := &Server{earnings: cache, platformSettings: store}
	pol := risk.DefaultRulebookPolicy()
	if pol.EarningsFreezeSessions != 3 {
		t.Fatalf("test assumes the default three-session window, got %d", pol.EarningsFreezeSessions)
	}

	_, infos := s.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "NEAR"}, {Symbol: "FAR"}, {Symbol: "UNDATED"}}, pol, marketcal.NewWithClock(func() time.Time { return now }), now, false)
	byName := map[string]rpc.EarningsInfo{}
	for _, info := range infos {
		byName[info.Symbol] = info
	}
	if info := byName["NEAR"]; info.Date != "2026-11-02" || info.TimeOfDay != "bmo" || info.Window != risk.EarningsWindowInside || info.SessionsUntil == nil || *info.SessionsUntil != 2 {
		t.Fatalf("near info = %+v, want inside the window two sessions out", info)
	}
	if info := byName["FAR"]; info.Window != risk.EarningsWindowOutside || info.SessionsUntil == nil || *info.SessionsUntil <= pol.EarningsFreezeSessions {
		t.Fatalf("far info = %+v, want outside the window", info)
	}
	if info := byName["UNDATED"]; info.Status != rpc.EarningsStatusNoDatePublished || info.Window != "" || info.SessionsUntil != nil {
		t.Fatalf("undated info = %+v, want no window verdict", info)
	}
}
