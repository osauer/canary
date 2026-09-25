package daemon

import (
	"context"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDataHealthUnknownCoverageAndPassiveRead(t *testing.T) {
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	r, err := s.handleDataHealth(rpc.DataHealthParams{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Summary.Unverified < 11 || r.Summary.Current != 6 || r.ScopeState != "unknown" {
		t.Fatalf("cold coverage: %+v", r.Summary)
	}
	if r.Summary.Problems != 2 {
		t.Fatalf("connection and reporting causes should remain distinct: %+v", r.Summary)
	}
	if s.dataHealth.wake != nil || s.connector != nil || s.marketData.interest != nil {
		t.Fatal("passive report started acquisition")
	}
	first := r
	now = now.Add(10 * time.Second)
	for r.NextOffset != nil {
		r, err = s.handleDataHealth(rpc.DataHealthParams{Offset: *r.NextOffset, Revision: first.Revision, Limit: 3})
		if err != nil {
			t.Fatal(err)
		}
		if r.Revision != first.Revision || r.AsOf != first.AsOf || r.Summary != first.Summary {
			t.Fatal("mixed report revisions")
		}
	}
	_, err = s.handleDataHealth(rpc.DataHealthParams{Revision: "missing", Offset: 2})
	if err == nil {
		t.Fatal("unknown revision accepted")
	}
	now = now.Add(3 * time.Minute)
	if _, err := s.handleDataHealth(rpc.DataHealthParams{Revision: first.Revision}); err == nil {
		t.Fatal("expired revision accepted without an intervening fresh report")
	}
	if result := s.runDataHealthCheck(context.Background()); result.Checked != 0 || result.Deferred != 1 {
		t.Fatalf("offline check: %+v", result)
	}
}

func TestDataHealthKeepsNativeCadenceAndPublicationFailureSeparate(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC) // Saturday
	r := &rpc.RegimeSnapshotResult{AsOf: now}
	r.VIXTermStructure.Status, r.VolOfVol.Status = "unavailable", "unavailable"
	r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status = "unavailable", "unavailable", "unavailable"
	r.GammaZero.Status, r.Breadth.Status, r.USDJPY.Status = "unavailable", "unavailable", "stale"
	r.USDJPY.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
	r.Fingerprint = rpc.BuildRegimeFingerprint(r)
	raw, _ := json.Marshal(r)
	s := &Server{regimeSnapshots: &regimeSnapshotCache{raw: raw, revision: 1, lastSuccessAt: now, freshFor: time.Minute, now: func() time.Time { return now }, failureCode: rpc.RegimeAuthorityFailureRefreshFailed}}
	find := func(id string) rpc.DataSourceHealth {
		t.Helper()
		for _, row := range s.regimeDataHealth(now) {
			if row.ID == id {
				return row
			}
		}
		t.Fatalf("missing source %s", id)
		return rpc.DataSourceHealth{}
	}
	if row := find("regime:fx"); row.State != "not_due" {
		t.Fatalf("closed FX schedule became a failure: %+v", row)
	}
	if row := find("regime_publication"); row.State != "limited" || row.Detail != "refresh_failed" {
		t.Fatalf("schedule hid failed publication: %+v", row)
	}
	now = time.Date(2026, 9, 13, 22, 0, 0, 0, time.UTC) // Sunday after IDEALPRO opens
	if row := find("regime:fx"); row.State != "limited" {
		t.Fatalf("old closed-market verdict survived market open: %+v", row)
	}
}

func TestDataHealthVerdictPreservesOperationalConcerns(t *testing.T) {
	healthy := rpc.HealthResult{Connected: true, DataHealth: &rpc.DataHealthResult{Summary: rpc.DataHealthSummary{State: "current"}}}
	if got := authoritativeHealthVerdict(&healthy); got.State != "READY" {
		t.Fatal(got)
	}
	for _, mutate := range []func(*rpc.HealthResult){
		func(h *rpc.HealthResult) { h.GatewayTLS = true },
		func(h *rpc.HealthResult) { h.Trading = rpc.TradingStatus{Mode: "paper", Blocked: true} },
		func(h *rpc.HealthResult) { h.Members = rpc.MembersHealth{Source: "synthetic", RefreshState: "failed"} },
		func(h *rpc.HealthResult) { h.Subsystems = []rpc.SubsystemHealth{{Name: "synthetic", Status: "error"}} },
	} {
		h := healthy
		mutate(&h)
		if got := authoritativeHealthVerdict(&h); got.State != "ATTENTION" {
			t.Fatalf("operational warning lost: %+v", got)
		}
	}
}

func TestDataHealthDelayedQuoteRetainsOnlyActualClocks(t *testing.T) {
	now := time.Date(2026, 9, 15, 6, 0, 0, 0, time.UTC)
	q := &rpc.Quote{Price: new(100.0), PriceSource: "mark", DataType: rpc.MarketDataDelayedFrozen, PriceAt: now, AsOf: now, ReceivedAt: now.Add(-time.Second)}
	row := projectQuoteSource(q, nil, now)
	if row.State != "limited" || row.DataType != "delayed-frozen" || !row.SourceAt.IsZero() || row.SourceTimeKind != "" || row.ReceivedAt != q.ReceivedAt {
		t.Fatalf("invented price clock or lost delay: %+v", row)
	}
	q.PriceSource, q.TradeAt = "last", now.Add(-15*time.Minute)
	row = projectQuoteSource(q, nil, now)
	if !row.SourceAt.IsZero() || q.TradeAt != now.Add(-15*time.Minute) {
		t.Fatal("source adopted an instrument clock or changed quote evidence")
	}
	q.DataType = rpc.MarketDataUnknown
	if row = projectQuoteSource(q, nil, now); row.State == "current" {
		t.Fatal("untyped data became healthy")
	}
}

func TestDataHealthCauseDedupDoesNotHideIndependentFailure(t *testing.T) {
	now := time.Now()
	rows := []rpc.DataSourceHealth{
		{ID: "broker", Required: true, State: "unavailable", ProblemIDs: []string{"connection"}},
		{ID: "derived", Required: true, State: "unavailable", ProblemIDs: []string{"connection"}, DerivedFrom: []string{"broker"}},
		{ID: "contract", Required: true, State: "unavailable", ProblemIDs: []string{"contract"}},
		{ID: "healthy", Required: true, State: "current"},
		{ID: "unexplained", Required: true, State: "unavailable", ProblemIDs: []string{"independent"}, DerivedFrom: []string{"healthy"}},
	}
	r, err := finalizeDataHealth(rows, "current", now, rpc.DataHealthParams{})
	if err != nil || r.Summary.Problems != 3 {
		t.Fatalf("dedup: %+v %v", r.Summary, err)
	}
}

// A limited row whose cause is expected, such as Cboe's pending VIX3M close
// on regime:vix_term, was counted as a source concern: the summary read "1
// source concerns · 0 unknown coverage" and Desk mirrored it as a gap. The
// row keeps its state and stays in the limited count; the summary counts it
// as an expected delay and the ranked concerns leave it out. A failure, any
// other cause, an unavailable row and unknown coverage still count, and a
// not_due row is on schedule, neither a concern nor a delay.
func TestDataHealthSummaryCountsExpectedDelaysApartFromConcerns(t *testing.T) {
	now := time.Date(2026, 9, 25, 13, 0, 0, 0, time.UTC)
	row := func(id, state, cause string) rpc.DataSourceHealth {
		r := rpc.DataSourceHealth{ID: id, Name: id, Required: true, State: state, Receiving: state, Cause: cause}
		if state == "limited" || state == "unavailable" {
			r.ProblemIDs = []string{id}
		}
		return r
	}
	pending := rpc.DataHealthCauseUpstreamPublicationPending
	failed := row("regime:credit", "limited", "")
	failed.Failure = &rpc.SourceFailure{Code: rpc.SourceFailureTimeout}
	for _, tc := range []struct {
		name     string
		rows     []rpc.DataSourceHealth
		want     rpc.DataHealthSummary
		concerns []string
	}{
		{"expected_delay_only", []rpc.DataSourceHealth{row("market_quotes", "current", ""), row("regime:fx", "not_due", ""), row("regime:vix_term", "limited", pending)},
			rpc.DataHealthSummary{State: "limited", Label: "0 source concerns · 1 expected delay · 0 unknown coverage", Total: 3, Required: 3, Current: 2, Limited: 1, ExpectedDelays: 1}, nil},
		{"on_schedule", []rpc.DataSourceHealth{row("market_quotes", "current", ""), row("regime:fx", "not_due", "")},
			rpc.DataHealthSummary{State: "current", Label: "Required sources are available", Total: 2, Required: 2, Current: 2}, nil},
		{"faults_stay_concerns", []rpc.DataSourceHealth{row("regime:vix_term", "limited", pending), row("regime:vvix", "limited", pending), failed, row("events:borrow_fee", "limited", "unclassified_publisher_delay"), row("gateway", "unavailable", pending), row("calendar", "unknown", "")},
			rpc.DataHealthSummary{State: "unavailable", Label: "3 source concerns · 2 expected delays · 1 unknown coverage", Total: 6, Required: 6, Limited: 4, Unavailable: 1, Unverified: 1, Problems: 3, ExpectedDelays: 2}, []string{"calendar", "events:borrow_fee", "gateway", "regime:credit"}},
		{"one_concern", []rpc.DataSourceHealth{row("market_quotes", "current", ""), failed},
			rpc.DataHealthSummary{State: "limited", Label: "1 source concern · 0 expected delays · 0 unknown coverage", Total: 2, Required: 2, Current: 1, Limited: 1, Problems: 1}, []string{"regime:credit"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := finalizeDataHealth(slices.Clone(tc.rows), "current", now, rpc.DataHealthParams{})
			if err != nil {
				t.Fatal(err)
			}
			if r.Summary != tc.want {
				t.Fatalf("summary:\n got %+v\nwant %+v", r.Summary, tc.want)
			}
			if s := r.Summary; s.Required != s.Current+s.Limited+s.Unavailable+s.Unverified {
				t.Fatalf("row states no longer partition the required rows: %+v", s)
			}
			var concerns []string
			for _, c := range r.Concerns {
				concerns = append(concerns, c.SourceID)
			}
			slices.Sort(concerns)
			if !slices.Equal(concerns, tc.concerns) {
				t.Fatalf("ranked concerns: got %v want %v", concerns, tc.concerns)
			}
			for _, in := range tc.rows {
				i := slices.IndexFunc(r.Sources, func(out rpc.DataSourceHealth) bool { return out.ID == in.ID })
				if i < 0 {
					t.Fatalf("row %s missing from the report", in.ID)
				}
				if out := r.Sources[i]; out.State != in.State || out.Cause != in.Cause || !slices.Equal(out.ProblemIDs, in.ProblemIDs) {
					t.Fatalf("row %s changed: %+v", in.ID, out)
				}
			}
		})
	}
}

func TestDataHealthBoundedPageAndDiagnosticExpiry(t *testing.T) {
	now := time.Now()
	var rows []rpc.DataSourceHealth
	for i := range 64 {
		rows = append(rows, rpc.DataSourceHealth{ID: strings.Repeat("x", i+1), Required: true, State: "unknown", Detail: strings.Repeat("d", 900)})
	}
	r, err := finalizeDataHealth(rows, "current", now, rpc.DataHealthParams{Limit: 64})
	raw, _ := json.Marshal(r)
	if err != nil || len(raw) > 32*1024 || r.NextOffset == nil || r.Summary.Total != 64 {
		t.Fatalf("page budget: %d %+v %v", len(raw), r.Summary, err)
	}
	s := &Server{}
	row := rpc.DataSourceHealth{ID: "quote", State: "limited", DataType: "delayed", CheckedAt: now, ValidUntil: now.Add(time.Minute)}
	s.recordDataHealth(row, nil, ibkr.ConnectorSessionBinding{}, false)
	expired, ok := s.observedDataHealth("quote", now.Add(2*time.Minute))
	if !ok || expired.State != "unknown" || len(expired.History) != 1 || expired.DataType != "delayed" || len(expired.ProblemIDs) != 0 {
		t.Fatalf("expiry lost history or fabricated recovery: %+v", expired)
	}
}

func TestDataHealthHistoryRestoresNoCurrentAccess(t *testing.T) {
	s, _, _, now, _ := historyFixture(t)
	s.now = func() time.Time { return now }
	row := rpc.DataSourceHealth{ID: quoteSourceID, Name: "IBKR quote feed", State: "limited", DataType: "delayed-frozen", CheckedAt: now, ReceivedAt: now, ValidUntil: now.Add(time.Minute), Access: &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now}}
	s.recordDataHealth(row, nil, ibkr.ConnectorSessionBinding{}, false)
	s.recordDataHealth(rpc.DataSourceHealth{ID: "account", Name: "Account fields", Required: true, State: "current", CheckedAt: now}, nil, ibkr.ConnectorSessionBinding{}, false)
	s.persistDataHealthHistory(t.Context())
	restarted := &Server{coreStore: s.coreStore, now: func() time.Time { return now.Add(time.Minute) }}
	restarted.loadDataHealthHistory()
	recovered, ok := restarted.observedDataHealth(row.ID, now.Add(time.Minute))
	if !ok || recovered.State != "unknown" || recovered.DataType != "" || recovered.Access != nil || recovered.LastSuccess != now || len(recovered.History) != 2 || recovered.History[1].Reason != "daemon_restart_observation_gap" {
		t.Fatalf("historical access restored as authority: %+v", recovered)
	}
	if recovered.History[0].Reason != "not_subscribed" || recovered.History[0].DataType != "delayed-frozen" {
		t.Fatal("diagnostic history lost the denied live attempt alongside successful delayed data")
	}
	rows, _ := restarted.collectDataHealth(now.Add(time.Minute))
	for _, source := range rows {
		if source.ID == "account" && (!source.Required || source.State != "unknown") {
			t.Fatal("restored chronology removed a required account capability from coverage")
		}
	}
}
