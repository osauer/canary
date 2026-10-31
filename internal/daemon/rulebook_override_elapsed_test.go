package daemon

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// An earnings override whose date has passed is a print that happened, not
// the next date: it no longer reads Known, the provider path is consulted,
// and the name discloses the elapsed pin in the rules result. The day itself
// stays Known: an amc print is still ahead, one session inside the window.
func TestAssembleEarningsReadsAnElapsedOverrideAsElapsed(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 30, 16, 37, 0, 0, berlin) // 2026-10-30 in ET as well
	store, err := newPlatformSettingsStore(filepath.Join(t.TempDir(), "platform-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.updateWithAudit(context.Background(), now, rpc.OrderOriginHumanTTY, []string{"features.rulebook.earnings_overrides"}, func(next *platformSettingsData) error {
		next.Features.Rulebook.EarningsOverrides = map[string]string{"ELAPSED": "2026-10-29Tamc", "TODAY": "2026-10-30Tamc"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	cache := newEarningsCacheMemory(nil, nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["ELAPSED"] = earningsSymbolState{
		Resolution: earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished},
		Providers: map[string]earningsProviderState{earningsNasdaqProvider: {
			LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, ParserContractVersion: earningsNasdaqParserContract},
			LastGood:    &earningsEntry{Date: "2026-10-29", ObservedAt: now.Add(-48 * time.Hour)},
		}},
		UpdatedAt: now,
	}
	cache.events["ELAPSED"] = []earningsEvent{{Date: "2026-10-29", Source: earningsNasdaqProvider, FirstSeen: now.Add(-72 * time.Hour), LastSeen: now.Add(-48 * time.Hour)}}
	s := &Server{earnings: cache, platformSettings: store}

	earnings, infos := s.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "ELAPSED"}, {Symbol: "TODAY"}}, risk.DefaultRulebookPolicy(), marketcal.NewWithClock(func() time.Time { return now }), now, false)
	e := earnings["ELAPSED"]
	if e.Known || e.Source != "fetched" || e.Reason != earningsReasonReported || e.ReportedOn.Format(time.DateOnly) != "2026-10-29" {
		t.Fatalf("elapsed override rule input = %+v, want the provider path (reported on 2026-10-29), not a Known pin", e)
	}
	if d := earnings["TODAY"]; !d.Known || d.Source != "override" || d.Date.Format(time.DateOnly) != "2026-10-30" || d.SessionsUntil == nil || *d.SessionsUntil != 1 {
		t.Fatalf("an override for today must stay Known: %+v", d)
	}
	byName := map[string]rpc.EarningsInfo{}
	for _, info := range infos {
		byName[info.Symbol] = info
	}
	if info := byName["ELAPSED"]; info.Source != "fetched" || info.Status != rpc.EarningsStatusReported || info.Date != "" || info.OverrideElapsed != "2026-10-29" {
		t.Fatalf("elapsed info = %+v", info)
	}
	if info := byName["TODAY"]; info.Source != "override" || info.Date != "2026-10-30" || info.OverrideElapsed != "" {
		t.Fatalf("today info = %+v", info)
	}
}
