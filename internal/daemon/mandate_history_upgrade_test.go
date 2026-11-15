package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v3/internal/daemon/corestore"
	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/internal/rpc"
	"testing"
	"time"
)

func TestRetainedIntradayHistoryUpgrade(t *testing.T) {
	cases := []struct {
		name                          string
		market                        marketcal.Market
		fetched, after, previous, day string
	}{
		{"old-default-US", "", "2026-10-19T15:00:00Z", "2026-10-19T21:00:00Z", "2026-10-16", "2026-10-19"},
		{"Xetra", marketcal.MarketDEXetra, "2026-10-19T14:00:00Z", "2026-10-19T16:00:00Z", "2026-10-16", "2026-10-19"},
		{"LSE-Christmas-early-close", marketcal.MarketUKLSE, "2026-12-24T12:00:00Z", "2026-12-24T13:00:00Z", "2026-12-23", "2026-12-24"},
		{"HK-Christmas-early-close", marketcal.MarketHKHKEX, "2026-12-24T03:00:00Z", "2026-12-24T05:00:00Z", "2026-12-23", "2026-12-24"},
	}
	for _, tc := range cases {
		for _, onlyIncomplete := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/mixed", true: "/only-incomplete"}[onlyIncomplete], func(t *testing.T) {
				parse := func(s string) time.Time {
					v, e := time.Parse(time.RFC3339, s)
					if e != nil {
						t.Fatal(e)
					}
					return v
				}
				store := openMarketTestCoreStore(t)
				now := parse(tc.after)
				old := mandateHistorySeries{Version: 1, Key: "u:SYNX", Contract: rpc.ContractParams{Symbol: "SYNX", SecType: "STK", Currency: "USD"}, Dates: []string{tc.previous, tc.day}, Closes: []float64{99, 100}, FetchedAt: parse(tc.fetched), Market: tc.market}
				if onlyIncomplete {
					old.Dates = old.Dates[1:]
					old.Closes = old.Closes[1:]
				}
				raw, e := json.Marshal(old)
				if e != nil {
					t.Fatal(e)
				}
				doc, e := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: mandateHistoryScope(old.Key), Kind: stateKindMandateHistory, ExpectedRevision: 0, JSON: raw})
				if e != nil {
					t.Fatal(e)
				}
				srv := &Server{coreStore: store, now: func() time.Time { return now }}
				got := srv.mandateHistorySeries(t.Context(), old.Key)
				if onlyIncomplete {
					if got != nil {
						t.Fatalf("retained incomplete-only accepted: %+v", got)
					}
				} else if got == nil || len(got.Dates) != 1 || got.Dates[0] != tc.previous || got.Closes[0] != 99 {
					t.Fatalf("load did not sanitize retained series: %+v", got)
				}
				if srv.mandateHistory.revisions[old.Key] != doc.Revision {
					t.Fatal("CAS revision lost")
				}
				unchanged, ok, e := store.GetStateDocument(t.Context(), mandateHistoryScope(old.Key), stateKindMandateHistory)
				if e != nil || !ok || string(unchanged.JSON) != string(raw) {
					t.Fatal("sanitization overwrote original observation")
				}
				reads := 0
				srv.mandateHistoryFetchForTest = func(_ context.Context, p rpc.MarketHistoryParams, _ int, at time.Time) (*rpc.MarketHistoryResult, error) {
					reads++
					day, _ := time.Parse(time.DateOnly, tc.day)
					return &rpc.MarketHistoryResult{Contract: p.Contract, Interval: "1 day", AsOf: at, Source: "synthetic", Points: []rpc.MarketHistoryPoint{{At: day, Value: 120}}}, nil
				}
				need := mandateHistoryNeed{Key: old.Key, Contract: old.Contract, Market: tc.market}
				srv.refreshMandateSeries(t.Context(), &fakeSetupSource{}, need)
				fresh := srv.mandateHistoryForNeed(t.Context(), need)
				if reads != 1 || fresh == nil || len(fresh.Closes) != 1 || fresh.Closes[0] != 120 {
					t.Fatalf("post-close upgrade refresh failed reads=%d series=%+v", reads, fresh)
				}
				again := &Server{coreStore: store, now: func() time.Time { return now }}
				retained := again.mandateHistoryForNeed(t.Context(), need)
				boundKey := bindMandateHistoryNeed(need).Key
				if retained == nil || retained.Closes[0] != 120 || again.mandateHistory.revisions[boundKey] == 0 {
					t.Fatalf("fresh result not durable: %+v", retained)
				}
				unchanged, ok, e = store.GetStateDocument(t.Context(), mandateHistoryScope(old.Key), stateKindMandateHistory)
				if e != nil || !ok || unchanged.Revision != doc.Revision || string(unchanged.JSON) != string(raw) {
					t.Fatal("identity-bound refresh overwrote the legacy observation")
				}
				t.Log("old partial bar discarded; completed refresh survives restart under its bound identity")
			})
		}
	}
}
