package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// After a print the vendor answers "no date" until it publishes the next
// quarter. The kept event makes the name read reported on the print's date:
// the rules stay unknown for want of a next date, the source is not degraded,
// and no surface says "no date published" or asks for a hand-typed override.
func TestAssembleEarningsReadsAKeptPrintAsReported(t *testing.T) {
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 30, 16, 37, 0, 0, berlin)
	cache := newEarningsCacheMemory(nil, nil)
	cache.clock = func() time.Time { return now }
	cache.symbols["SYNTH"] = earningsSymbolState{
		Resolution: earningsResolution{Status: rpc.EarningsStatusNoDatePublished, Reason: rpc.EarningsStatusNoDatePublished},
		Providers: map[string]earningsProviderState{earningsNasdaqProvider: {
			LastAttempt: earningsProviderAttempt{Status: rpc.EarningsStatusNoDatePublished, AttemptedAt: now, CompletedAt: now, ParserContractVersion: earningsNasdaqParserContract},
			LastGood:    &earningsEntry{Date: "2026-10-29", ObservedAt: now.Add(-48 * time.Hour)},
		}},
		UpdatedAt: now,
	}
	cache.events["SYNTH"] = []earningsEvent{{Date: "2026-10-29", Source: earningsNasdaqProvider, FirstSeen: now.Add(-72 * time.Hour), LastSeen: now.Add(-48 * time.Hour)}}
	s := &Server{earnings: cache}

	earnings, infos := s.assembleEarnings(t.Context(), []risk.NameInput{{Symbol: "SYNTH"}}, risk.DefaultRulebookPolicy(), nil, now, false)
	e := earnings["SYNTH"]
	if e.Known || e.Source != "fetched" || e.Reason != earningsReasonReported || e.ReportedOn.Format(time.DateOnly) != "2026-10-29" {
		t.Fatalf("rule input = %+v, want an unknown next date reported on 2026-10-29", e)
	}
	if len(infos) != 1 || infos[0].Status != rpc.EarningsStatusReported || infos[0].ReportedOn != "2026-10-29" || infos[0].Date != "" || infos[0].Source != "fetched" {
		t.Fatalf("info = %+v", infos[0])
	}
	health, degraded := rulesEarningsSourceHealth(infos, now)
	if degraded || health.Status != rpc.SourceStatusOK {
		t.Fatalf("a kept print is a definitive vendor answer, not a degraded source: %+v", health)
	}
	rules := &rpc.RulesResult{Earnings: infos}
	if hint := briefEarningsOverrideHint(rules); hint != "" {
		t.Fatalf("a reported name must not ask for an override: %q", hint)
	}
	unresolved := briefUnresolvedEarnings(rules)
	if len(unresolved) != 1 || unresolved[0] != "SYNTH (reported on 2026-10-29; next date not yet published)" {
		t.Fatalf("brief wording = %q", unresolved)
	}
	for _, line := range unresolved {
		if strings.Contains(line, "no date published") {
			t.Fatalf("the brief must never say no date published for a kept print: %q", line)
		}
	}
}
