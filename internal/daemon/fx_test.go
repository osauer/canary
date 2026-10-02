package daemon

import (
	"context"
	"crypto/sha256"
	"github.com/osauer/canary/v2/internal/config"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFXEvidenceCacheRestatementAndRetraction(t *testing.T) {
	file := func(raw []byte) statementProjectionFile {
		return statementProjectionFile{data: raw, digest: sha256.Sum256(raw)}
	}
	raw := reportingFlexFixture("20260820", "20260820")
	cache := &fxEvidenceCache{}
	rows, err := cache.parse(t.Context(), []statementProjectionFile{file(raw)})
	if err != nil || len(rows) != 1 || len(cache.rows) != 1 {
		t.Fatal("initial evidence", err)
	}
	repeated, err := cache.parse(t.Context(), []statementProjectionFile{file(raw)})
	if err != nil || repeated[0].FX != rows[0].FX {
		t.Fatal("exact bytes were reparsed", err)
	}
	// An equal-length source restatement must replace the parsed book.
	restated := []byte(strings.Replace(string(raw), `total="100"`, `total="101"`, 1))
	if string(restated) == string(raw) {
		t.Fatal("fixture did not change")
	}
	updated, err := cache.parse(t.Context(), []statementProjectionFile{file(restated)})
	if err != nil || updated[0].Equity[0].TotalBase == rows[0].Equity[0].TotalBase || len(cache.rows) != 1 {
		t.Fatal("restatement reused old evidence", err)
	}
	empty, err := cache.parse(t.Context(), nil)
	if err != nil || len(empty) != 0 || len(cache.rows) != 0 {
		t.Fatal("removed evidence retained", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.parse(ctx, []statementProjectionFile{file(raw)}); err != context.Canceled {
		t.Fatal("cancelled read parsed evidence", err)
	}
}

func fxTestSnapshot(day, previous string, eur, usd, rate float64) *flexstmt.FXSnapshot {
	return &flexstmt.FXSnapshot{Day: day, PreviousDay: previous, BaseCurrency: "EUR", SingleDay: true, Foreign: usd != 0, ClosingForeign: usd != 0, Book: map[string]float64{"EUR": eur, "USD": usd}, Rates: map[string]float64{"EUR": 1, "USD": rate}, NAV: eur + usd*rate, Conversion: map[string]float64{}, External: map[string]float64{}}
}

func TestFXAccountingWitnesses(t *testing.T) {
	tests := []struct {
		name                   string
		eur0, usd0, eur1, usd1 float64
		conversion, external   map[string]float64
		externalBase, want     float64
	}{
		{name: "buy foreign cash", eur0: 1000, eur1: 150, usd1: 1000, conversion: map[string]float64{"EUR": -850, "USD": 1000}, want: -50},
		{name: "sell foreign cash", usd0: 1000, eur1: 850, conversion: map[string]float64{"EUR": 850, "USD": -1000}, want: -50},
		{name: "flat intraday conversion", eur0: 1000, eur1: 950, conversion: map[string]float64{"EUR": -50}, want: -50},
		{name: "price interaction", usd0: 1000, usd1: 1100, want: -110},
		{name: "income closing convention", usd0: 1000, usd1: 1100, want: -110},
		{name: "foreign deposit at actual value", usd1: 1000, external: map[string]float64{"USD": 1000}, externalBase: 850, want: -50},
		{name: "base deposit", eur1: 1000, external: map[string]float64{"EUR": 1000}, externalBase: 1000, want: 0},
		{name: "foreign commission counted once", usd0: 1000, usd1: 990, want: -99},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start := fxTestSnapshot("2026-09-30", "2026-09-29", tt.eur0, tt.usd0, .9)
			end := fxTestSnapshot("2026-10-01", start.Day, tt.eur1, tt.usd1, .8)
			end.Conversion = tt.conversion
			end.External = tt.external
			end.ExternalBase = tt.externalBase
			if len(tt.conversion) > 0 {
				end.Foreign = true
			}
			got := attributeFX(end, start)
			if got.Contribution == nil || math.Abs(*got.Contribution-tt.want) > 1e-8 {
				t.Fatalf("FX %v reason %s, want %g", got.Contribution, got.Reason, tt.want)
			}
			if got.ReconciliationResidual == nil || math.Abs(*got.ReconciliationResidual) > 1e-8 {
				t.Fatal("NAV bridge failed")
			}
		})
	}
}

func TestFXBoundaryFailuresNeverZero(t *testing.T) {
	start := fxTestSnapshot("2026-09-30", "2026-09-29", 100, 1000, .9)
	end := fxTestSnapshot("2026-10-01", start.Day, 100, 1000, .8)
	t.Run("missing opening", func(t *testing.T) {
		if attributeFX(end, nil).Contribution != nil {
			t.Fatal("missing opening certified")
		}
	})
	t.Run("cash discontinuity", func(t *testing.T) {
		a, b := *start, *end
		a.CashEnd = map[string]float64{"USD": 2}
		b.CashStart = map[string]float64{"USD": 3}
		if attributeFX(&b, &a).Contribution != nil {
			t.Fatal("cash gap certified")
		}
	})
	t.Run("accrual discontinuity", func(t *testing.T) {
		a, b := *start, *end
		a.InterestEnd = map[string]float64{"USD": 2}
		b.InterestStart = map[string]float64{"USD": 3}
		if attributeFX(&b, &a).Contribution != nil {
			t.Fatal("accrual gap certified")
		}
	})
	t.Run("lending collateral discontinuity", func(t *testing.T) {
		a, b := *start, *end
		a.CollateralEnd = map[string]float64{"USD": 100}
		b.CollateralStart = map[string]float64{"USD": 101}
		if attributeFX(&b, &a).Contribution != nil {
			t.Fatal("collateral gap certified")
		}
	})
	t.Run("missing rate", func(t *testing.T) {
		a := *start
		a.Rates = map[string]float64{"EUR": 1}
		if attributeFX(end, &a).Contribution != nil {
			t.Fatal("missing rate certified")
		}
	})
	t.Run("NAV mismatch", func(t *testing.T) {
		a := *start
		a.NAV++
		if attributeFX(end, &a).Contribution != nil {
			t.Fatal("NAV mismatch certified")
		}
	})
}

func TestFXPeriodsAndHistoricalExposure(t *testing.T) {
	now := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	dates := []string{"2025-12-31", "2026-01-01", "2026-01-02", "2026-01-05"}
	rows := []flexstmt.Statement{}
	equity := []flexstmt.EquityRow{}
	for i, d := range dates {
		day, _ := time.Parse(performanceDayFormat, d)
		f := fxTestSnapshot(d, "2025-12-30", 1000, 0, .9)
		if i > 0 {
			f.PreviousDay = dates[i-1]
		}
		if i == 1 {
			f.Foreign = true
		} // Intraday foreign exposure, closing book flat.
		rows = append(rows, flexstmt.Statement{FromDate: day, ToDate: day, WhenGenerated: now, FX: f})
		equity = append(equity, flexstmt.EquityRow{ReportDate: day, TotalBase: 1000})
	}
	rows = append(rows, flexstmt.Statement{FromDate: time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC), ToDate: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), WhenGenerated: now, Equity: equity})
	r := buildFX(rows, now)
	if err := rpc.ValidateFXResult(r); err != nil {
		t.Fatal(err)
	}
	if r.Periods[0].State != "no_exposure" || r.Periods[3].State != "available" || r.Periods[3].Contribution == nil {
		t.Fatalf("historical exposure lost: %+v", r.Periods)
	}
	rows[2].FX = nil
	r = buildFX(rows, now)
	if r.Periods[3].Contribution != nil || r.Periods[3].State != "partial" || len(r.Periods[3].MissingDays) != 2 {
		t.Fatalf("gap certified: %+v", r.Periods[3])
	}
}

func TestFXHistoricalRetentionScopesGeneration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	selection := flexEvidenceSelection{ActiveQueryFingerprint: flexQueryFingerprint("synthetic-query")}
	annual := reportingFlexFixture("20260101", "20260824")
	if _, err := retainFlexStatementWithGenerationPolicy(t.Context(), annual, selection, false); err != nil {
		t.Fatal(err)
	}
	cached := []byte(strings.ReplaceAll(string(reportingFlexFixture("20260820", "20260820")), "20260824;120000", "20260821;120000"))
	if _, err := retainFlexStatementWithGenerationPolicy(t.Context(), cached, selection, false); err == nil {
		t.Fatal("ordinary freshness guard bypassed")
	}
	if _, err := retainFlexStatementWithGenerationPolicy(t.Context(), cached, selection, true); err != nil {
		t.Fatal("cached historic day refused", err)
	}
	older := []byte(strings.ReplaceAll(string(cached), "20260821;120000", "20260820;120000"))
	if _, err := retainFlexStatementWithGenerationPolicy(t.Context(), older, selection, true); err == nil {
		t.Fatal("older generation of same daily scope accepted")
	}
	otherQuery := flexEvidenceSelection{ActiveQueryFingerprint: flexQueryFingerprint("other-synthetic-query")}
	if _, err := retainFlexStatementWithGenerationPolicy(t.Context(), older, otherQuery, true); err != nil {
		t.Fatal("other query's generation contaminated scope", err)
	}
}

func TestFXAcquisitionRequiresRequestedRangeAndSharedLane(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	day := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	srv := &Server{cfg: &config.Resolved{Flex: config.Flex{Enabled: true, QueryID: "synthetic-query"}}}
	srv.flexRawDateRangeLockedFn = func(_ context.Context, from, to time.Time, _ int, query, token string) ([]byte, error) {
		if srv.flexBrokerMu.TryLock() {
			srv.flexBrokerMu.Unlock()
			t.Fatal("historical fetch outside shared lane")
		}
		return reportingFlexFixture("20260819", "20260819"), nil
	}
	if _, err := srv.fetchFXStatement(t.Context(), day, day); err == nil {
		t.Fatal("off-day broker report retained as requested day")
	}
}

func TestFXLeapYearSeedStaysWithinBrokerBound(t *testing.T) {
	from := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	a, b, complete := fxNextSeed(nil, from, "2024-12-31")
	if complete || a != from || int(b.Sub(a).Hours()/24)+1 > 365 || b.Weekday() == time.Saturday || b.Weekday() == time.Sunday {
		t.Fatal("invalid primary calendar window", a, b, complete)
	}
	prior := from.AddDate(0, 0, -1)
	seed := flexstmt.Statement{FromDate: from, ToDate: b, Equity: []flexstmt.EquityRow{{ReportDate: prior}, {ReportDate: b}}}
	a, b, complete = fxNextSeed([]flexstmt.Statement{seed}, from, "2024-12-31")
	if complete || a.Format(performanceDayFormat) != "2024-12-31" || b.Format(performanceDayFormat) != "2024-12-31" {
		t.Fatal("tail calendar not requested", a, b)
	}
	tail := flexstmt.Statement{FromDate: a, ToDate: b, Equity: []flexstmt.EquityRow{{ReportDate: seed.ToDate}, {ReportDate: b}}}
	_, _, complete = fxNextSeed([]flexstmt.Statement{seed, tail}, from, "2024-12-31")
	if !complete {
		t.Fatal("reconciled two-window calendar refused")
	}
}

func TestFXSharedPacingCancelsBeforeSending(t *testing.T) {
	srv := &Server{flexNextRequest: time.Now().Add(time.Hour)}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	before := srv.flexNextRequest
	if err := srv.paceFlexRequestLocked(ctx); err != context.Canceled {
		t.Fatal("pacing ignored lifecycle cancellation", err)
	}
	if srv.flexNextRequest != before {
		t.Fatal("cancelled request consumed quota")
	}
	srv.flexNextRequest = time.Time{}
	start := time.Now()
	if err := srv.paceFlexRequestLocked(t.Context()); err != nil {
		t.Fatal(err)
	}
	if srv.flexNextRequest.Before(start.Add(10 * time.Second)) {
		t.Fatal("shared lane permits more than six requests per minute")
	}
}

func TestFXCanonicalNAVAndCurrencyFailures(t *testing.T) {
	now := time.Date(2026, 1, 6, 12, 0, 0, 0, time.UTC)
	fixture := func() []flexstmt.Statement {
		dates := []string{"2025-12-31", "2026-01-01", "2026-01-02", "2026-01-05"}
		rows := []flexstmt.Statement{}
		equity := []flexstmt.EquityRow{}
		for i, d := range dates {
			day, _ := time.Parse(performanceDayFormat, d)
			f := fxTestSnapshot(d, "2025-12-30", 1000, 0, .9)
			if i > 0 {
				f.PreviousDay = dates[i-1]
			}
			rows = append(rows, flexstmt.Statement{FromDate: day, ToDate: day, WhenGenerated: now, FX: f})
			equity = append(equity, flexstmt.EquityRow{ReportDate: day, TotalBase: 1000})
		}
		return append(rows, flexstmt.Statement{FromDate: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), ToDate: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC), WhenGenerated: now, Equity: equity})
	}
	for _, opening := range []bool{false, true} {
		rows := fixture()
		index := 3
		want := "restated_nav_requires_daily_snapshot"
		if opening {
			index = 2
			want = "restated_opening_nav_requires_daily_snapshot"
		}
		rows[4].Equity[index].TotalBase++
		r := buildFX(rows, now)
		last := r.Days[len(r.Days)-1]
		if last.Contribution != nil || last.Reason != want {
			t.Fatalf("restatement certified: %+v", last)
		}
	}
	t.Run("equal generation NAV conflict", func(t *testing.T) {
		rows := fixture()
		conflict := rows[4]
		conflict.Equity = append([]flexstmt.EquityRow{}, conflict.Equity...)
		conflict.Equity[3].TotalBase++
		r := buildFX(append(rows, conflict), now)
		if r.Days[len(r.Days)-1].Contribution != nil || r.Periods[3].Contribution != nil {
			t.Fatal("conflicting NAV certified")
		}
	})
	t.Run("base currency change", func(t *testing.T) {
		rows := fixture()
		rows[2].FX.BaseCurrency = "USD"
		r := buildFX(rows, now)
		if r.BaseCurrency != "" || r.Reason != "base_currency_changed_within_year" {
			t.Fatal("mixed currency labelled", r)
		}
		for _, p := range r.Periods {
			if p.Contribution != nil {
				t.Fatal("mixed currency total certified")
			}
		}
		if err := rpc.ValidateFXResult(r); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("short calendar cannot certify YTD", func(t *testing.T) {
		rows := fixture()
		rows[4].FromDate = time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
		r := buildFX(rows, now)
		if r.Periods[3].Contribution != nil || r.Reason != "year_to_date_calendar_backfill_required" {
			t.Fatal("short calendar certified YTD")
		}
	})
}
