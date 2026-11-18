package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// currentRegimeRows is a regime read with all eight rows current and green
// on a flat tape. Each case changes only the rows it is about.
func currentRegimeRows() *rpc.RegimeSnapshotResult {
	asOf := time.Date(2026, 9, 10, 16, 0, 0, 0, time.UTC)
	meta := func() rpc.RegimeIndicatorMeta {
		return rpc.RegimeIndicatorMeta{Band: "green", Freshness: &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}, AsOf: &rpc.RegimeAsOfSummary{Time: asOf}}
	}
	r := &rpc.RegimeSnapshotResult{AsOf: asOf, TapeSessionState: rpc.TapeSessionTradingDate}
	r.VIXTermStructure = rpc.RegimeVIXTerm{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK, VIX: new(16.0), VIX3M: new(18.0), Ratio: new(16.0 / 18.0), VIXChangePct: new(0.0)}
	r.VolOfVol = rpc.RegimeVolOfVol{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK, Last: new(85.0)}
	r.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{
		RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK, SPYChangePct: new(0.0),
		SPYQuality: &rpc.Quality{AsOf: asOf, FreshnessClass: rpc.FreshnessLive},
	}
	r.CreditSpreads = rpc.RegimeCreditSpreads{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK, HYOAS: new(3.0), HY20DChange: new(0.1)}
	r.FundingStress = rpc.RegimeFundingStress{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK}
	r.USDJPY = rpc.RegimeUSDJPY{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK, Last: new(150.0)}
	r.GammaZero = rpc.RegimeGammaZero{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}}
	r.Breadth = rpc.RegimeBreadth{RegimeIndicatorMeta: meta(), Status: rpc.RegimeStatusOK}
	r.Breadth.Envelope = rpc.BreadthSPXResult{State: rpc.BreadthStateReady, SessionKey: asOf.Format("2006-01-02")}
	return r
}

func redRow(meta *rpc.RegimeIndicatorMeta) {
	meta.Band = "red"
	meta.Eligibility = &rpc.RegimeEligibility{Eligible: true}
}

func lostTermRow(r *rpc.RegimeSnapshotResult) {
	r.VIXTermStructure.Status = rpc.RegimeStatusError
	r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
	r.VIXTermStructure.Ratio = nil
}

// assembleRegimeRead finishes a classified read in the order the snapshot
// handler does (acquireRegimeSnapshot after its policy pass) and returns the
// warning openers as the summary saw them, before data quality and source
// health are stamped.
func assembleRegimeRead(r *rpc.RegimeSnapshotResult) (early []string) {
	r.Composite = buildRegimeComposite(r)
	r.Summary = buildRegimeSummary(r)
	early = rpc.RegimeWarningOpeners(r)
	r.DataQuality = regimeSnapshotDataQuality(r)
	r.SourceHealth = rpc.BuildRegimeSourceHealth(r, r.AsOf)
	r.Lifecycle = rpc.BuildRegimeLifecycle(r)
	r.Composite.Verdict = rpc.RegimeHeadline(r.Composite, r.Lifecycle.Stage)
	r.Summary.Label = r.Composite.Verdict
	r.Posture = rpc.BuildRegimePosture(r)
	return early
}

// The rulebook follows the dark-witness warning into its early-warning
// bucket (owner ruling 2026-11-18 14:47 CET, "Early warning"), through the
// real stage-to-bucket map. A healthy panel with co-signs red stays calm, two
// dark clusters still hold the latch, a witness judged by its own row keeps
// its stage, and panic without a current tape is confirmed stress. The
// opener list the summary reads before source health is stamped is the list
// the stress read serves after.
func TestDarkWitnessWarningMovesTheRulebookBucket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		shape  func(r *rpc.RegimeSnapshotResult)
		stage  string
		bucket string
	}{
		{"term row lost; proxy, breadth and yen yellow; SPY -1.4%", func(r *rpc.RegimeSnapshotResult) {
			lostTermRow(r)
			r.HYGSPYDivergence.Band, r.Breadth.Band, r.USDJPY.Band = "yellow", "yellow", "yellow"
			r.HYGSPYDivergence.SPYChangePct = new(-1.4)
		}, rpc.LifecycleEarlyWarning, risk.RegimeBucketEarlyWarning},
		{"term row lost; proxy, breadth and yen red", func(r *rpc.RegimeSnapshotResult) {
			lostTermRow(r)
			redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
			redRow(&r.Breadth.RegimeIndicatorMeta)
			redRow(&r.USDJPY.RegimeIndicatorMeta)
		}, rpc.LifecycleEarlyWarning, risk.RegimeBucketEarlyWarning},
		{"cash credit unavailable; proxy red", func(r *rpc.RegimeSnapshotResult) {
			redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
			r.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusUnavailable}
		}, rpc.LifecycleEarlyWarning, risk.RegimeBucketEarlyWarning},
		{"funding overdue; breadth yellow", func(r *rpc.RegimeSnapshotResult) {
			r.Breadth.Band = "yellow"
			r.FundingStress.Status = rpc.RegimeStatusStale
			r.FundingStress.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
		}, rpc.LifecycleEarlyWarning, risk.RegimeBucketEarlyWarning},
		{"control: witnesses current and green, co-signs red", func(r *rpc.RegimeSnapshotResult) {
			redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
			redRow(&r.Breadth.RegimeIndicatorMeta)
			redRow(&r.USDJPY.RegimeIndicatorMeta)
		}, rpc.LifecycleQuiet, risk.RegimeBucketCalm},
		{"control: term row lost, every co-sign green", lostTermRow, rpc.LifecycleQuiet, risk.RegimeBucketCalm},
		{"two dark clusters, proxy red: the latch holds", func(r *rpc.RegimeSnapshotResult) {
			lostTermRow(r)
			r.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusUnavailable}
			redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
		}, rpc.LifecycleDataQuality, ""},
		{"cash red, proxy row unavailable", func(r *rpc.RegimeSnapshotResult) {
			redRow(&r.CreditSpreads.RegimeIndicatorMeta)
			r.CreditSpreads.HYOAS = new(5.8)
			r.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusUnavailable}
		}, rpc.LifecycleEarlyWarning, risk.RegimeBucketEarlyWarning},
		{"cash red and funding red, proxy row unavailable", func(r *rpc.RegimeSnapshotResult) {
			redRow(&r.CreditSpreads.RegimeIndicatorMeta)
			r.CreditSpreads.HYOAS = new(5.8)
			redRow(&r.FundingStress.RegimeIndicatorMeta)
			r.HYGSPYDivergence = rpc.RegimeHYGSPYDivergence{Status: rpc.RegimeStatusUnavailable}
		}, rpc.LifecycleConfirmedStress, risk.RegimeBucketConfirmed},
		{"SPY -4.5% not current; term, funding and breadth red", func(r *rpc.RegimeSnapshotResult) {
			redRow(&r.VIXTermStructure.RegimeIndicatorMeta)
			redRow(&r.FundingStress.RegimeIndicatorMeta)
			redRow(&r.Breadth.RegimeIndicatorMeta)
			r.HYGSPYDivergence.SPYChangePct = new(-4.5)
			r.HYGSPYDivergence.Status = rpc.RegimeStatusStale
			r.HYGSPYDivergence.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessStale}
			r.HYGSPYDivergence.SPYQuality.AsOf = r.AsOf.AddDate(0, 0, -1) // a prior session's print
		}, rpc.LifecycleConfirmedStress, risk.RegimeBucketConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := currentRegimeRows()
			tc.shape(r)
			early := assembleRegimeRead(r)
			if r.Lifecycle.Stage != tc.stage || bucketRegimeStage(r.Lifecycle.Stage) != tc.bucket {
				t.Fatalf("stage %s (readiness %s), bucket %q; want %s, bucket %q",
					r.Lifecycle.Stage, r.Lifecycle.Readiness, bucketRegimeStage(r.Lifecycle.Stage), tc.stage, tc.bucket)
			}
			if late := rpc.RegimeWarningOpeners(r); !slices.Equal(early, late) {
				t.Fatalf("openers %v before source health, %v after; want one list", early, late)
			}
		})
	}
}

// The regime summary says why the dark-witness warning flashes: the reading
// that is unavailable, and the co-sign beside it as weak, not as context.
func TestRegimeSummaryNamesTheDarkWitnessCause(t *testing.T) {
	r := coSignRedSnapshot()
	r.VIXTermStructure = rpc.RegimeVIXTerm{Status: rpc.RegimeStatusError}
	annotateCurrentRows(r)
	punch := buildRegimeSummary(r).PunchLine
	if !strings.Contains(punch, "ETF credit proxy is weak") ||
		!strings.Contains(punch, "volatility term structure and dealer gamma are unavailable") || strings.Contains(punch, "Context") {
		t.Fatalf("punch line %q; want the unavailable term structure and the weak proxy named", punch)
	}
}

// annotateCurrentRows classifies the rows as a direct test caller does, with
// no policy pass, and stamps each row current, as the policy pass does for a
// row read on its cadence. A row without a currency is overdue.
func annotateCurrentRows(r *rpc.RegimeSnapshotResult) {
	annotateRegimeMetadata(r, nil)
	for _, meta := range []*rpc.RegimeIndicatorMeta{
		&r.VIXTermStructure.RegimeIndicatorMeta, &r.VolOfVol.RegimeIndicatorMeta,
		&r.HYGSPYDivergence.RegimeIndicatorMeta, &r.CreditSpreads.RegimeIndicatorMeta,
		&r.FundingStress.RegimeIndicatorMeta, &r.USDJPY.RegimeIndicatorMeta,
		&r.GammaZero.RegimeIndicatorMeta, &r.Breadth.RegimeIndicatorMeta,
	} {
		meta.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh}
	}
}

// The hours a stress witness reads dark on a normal week. A reading that is
// not published yet is not dark: VVIX holds the last session's close as
// not_due until the next session's close has passed, and the VIX/VIX3M row
// holds the prior close for a five-minute grace from 09:31 until VIX3M's
// first tick. Dark are the real gaps: a close missing past its allowance,
// no VIX3M tick after the grace, a carried tick past 15 minutes, an
// off-window leg nothing vouches for.
func TestWitnessDarkHoursOnANormalWeek(t *testing.T) {
	ny := newYorkLocation()
	at := func(day string, hour, minute int) time.Time {
		d, err := time.ParseInLocation("2006-01-02", day, ny)
		if err != nil {
			t.Fatal(err)
		}
		return d.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	}
	vvix := func(date string, now time.Time) string {
		return volOfVolCadenceClass(&rpc.RegimeSnapshotResult{VolOfVol: rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, AsOfDate: date, Last: new(90.0)}}, now)
	}
	for _, tc := range []struct {
		name, date string
		now        time.Time
		want       string
	}{
		{"Monday 10:00, Friday's close", "2026-11-13", at("2026-11-16", 10, 0), rpc.RegimeFreshnessFresh},
		{"Monday 19:30, Friday's close", "2026-11-13", at("2026-11-16", 19, 30), rpc.RegimeFreshnessNotDue},
		{"Tuesday 09:00, Friday's close", "2026-11-13", at("2026-11-17", 9, 0), rpc.RegimeFreshnessNotDue},
		{"Tuesday 10:00, Friday's close", "2026-11-13", at("2026-11-17", 10, 0), rpc.RegimeFreshnessOverdue},
		{"Tuesday 19:30, Friday's close", "2026-11-13", at("2026-11-17", 19, 30), rpc.RegimeFreshnessOverdue},
		{"Tuesday 10:00, Monday's close", "2026-11-16", at("2026-11-17", 10, 0), rpc.RegimeFreshnessFresh},
		{"Thursday 20:00, Wednesday's close", "2026-11-18", at("2026-11-19", 20, 0), rpc.RegimeFreshnessFresh},
		{"Tuesday noon after a Monday holiday, Friday's close", "2026-01-16", at("2026-01-20", 12, 0), rpc.RegimeFreshnessNotDue},
		{"Tuesday 19:30 after a Monday holiday, Friday's close", "2026-01-16", at("2026-01-20", 19, 30), rpc.RegimeFreshnessNotDue},
		{"Wednesday 19:30 after a Monday holiday, Friday's close", "2026-01-16", at("2026-01-21", 19, 30), rpc.RegimeFreshnessOverdue},
	} {
		if got := vvix(tc.date, tc.now); got != tc.want {
			t.Errorf("VVIX %s: %s, want %s", tc.name, got, tc.want)
		}
	}

	prevWindowEnd := at("2026-11-16", 16, 15)
	term := func(now time.Time, status string, vix, vix3m *rpc.Quality, crossCheck string) string {
		row := rpc.RegimeVIXTerm{
			Status: status, VIX: new(16.0), VIX3M: new(18.0), Ratio: new(16.0 / 18.0), VIX3MAnchorVIX: new(16.0),
			VIXQuality: vix, VIX3MQuality: vix3m, VIX3MCrossCheck: crossCheck,
		}
		return vixTermCadenceClass(&rpc.RegimeSnapshotResult{VIXTermStructure: row}, now)
	}
	q := func(asOf time.Time, class string) *rpc.Quality {
		return &rpc.Quality{AsOf: asOf, FreshnessClass: class, Confidence: rpc.ConfidenceFirm}
	}
	tick := at("2026-11-17", 9, 50)
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"09:32, VIX3M still yesterday's last print", term(at("2026-11-17", 9, 32), rpc.RegimeStatusOK, q(at("2026-11-17", 9, 32), rpc.FreshnessLive), q(prevWindowEnd, rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessNotDue},
		{"09:40, still no VIX3M tick", term(at("2026-11-17", 9, 40), rpc.RegimeStatusOK, q(at("2026-11-17", 9, 40), rpc.FreshnessLive), q(prevWindowEnd, rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessOverdue},
		{"09:32, VIX3M leg two sessions old", term(at("2026-11-17", 9, 32), rpc.RegimeStatusOK, q(at("2026-11-17", 9, 32), rpc.FreshnessLive), q(at("2026-11-13", 16, 15), rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessOverdue},
		{"09:45, both legs live", term(at("2026-11-17", 9, 45), rpc.RegimeStatusOK, q(at("2026-11-17", 9, 45), rpc.FreshnessLive), q(at("2026-11-17", 9, 45), rpc.FreshnessLive), ""), rpc.RegimeFreshnessFresh},
		{"10:00, VIX3M carried 10 minutes", term(at("2026-11-17", 10, 0), rpc.RegimeStatusOK, q(at("2026-11-17", 10, 0), rpc.FreshnessLive), q(tick, rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessStale},
		{"10:20, VIX3M carried 30 minutes", term(at("2026-11-17", 10, 20), rpc.RegimeStatusOK, q(at("2026-11-17", 10, 20), rpc.FreshnessLive), q(tick, rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessOverdue},
		{"08:30, Cboe's close vouches", term(at("2026-11-17", 8, 30), rpc.RegimeStatusStale, q(prevWindowEnd, rpc.FreshnessFrozen), q(prevWindowEnd, rpc.FreshnessFrozen), rpc.VIX3MCrossCheckAgree), rpc.RegimeFreshnessNotDue},
		{"08:30, nothing vouches", term(at("2026-11-17", 8, 30), rpc.RegimeStatusStale, q(prevWindowEnd, rpc.FreshnessFrozen), q(prevWindowEnd, rpc.FreshnessFrozen), ""), rpc.RegimeFreshnessOverdue},
	} {
		if tc.got != tc.want {
			t.Errorf("VIX/VIX3M %s: %s, want %s", tc.name, tc.got, tc.want)
		}
	}

	// Monday 19:30 with Friday's VVIX close is quiet beside red co-signs.
	now := at("2026-11-16", 19, 30)
	r := currentRegimeRows()
	r.AsOf = now
	r.VolOfVol.AsOfDate = "2026-11-13"
	r.VolOfVol.Freshness = &rpc.RegimeFreshness{Class: volOfVolCadenceClass(r, now), NextDueAt: volOfVolNextDue(r, now)}
	r.VolOfVol.AsOf = &rpc.RegimeAsOfSummary{Time: at("2026-11-13", 16, 15)}
	redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
	redRow(&r.Breadth.RegimeIndicatorMeta)
	for _, meta := range []*rpc.RegimeIndicatorMeta{&r.VIXTermStructure.RegimeIndicatorMeta, &r.HYGSPYDivergence.RegimeIndicatorMeta, &r.CreditSpreads.RegimeIndicatorMeta, &r.FundingStress.RegimeIndicatorMeta, &r.USDJPY.RegimeIndicatorMeta, &r.GammaZero.RegimeIndicatorMeta, &r.Breadth.RegimeIndicatorMeta} {
		meta.AsOf = &rpc.RegimeAsOfSummary{Time: now}
	}
	r.HYGSPYDivergence.SPYQuality.AsOf = now
	assembleRegimeRead(r)
	if r.Lifecycle.Stage != rpc.LifecycleQuiet || slices.Contains(rpc.RegimeWarningOpeners(r), rpc.RegimeWarningWitnessUnavailable) {
		t.Fatalf("Monday 19:30 with Friday's VVIX close (%s): stage %s, openers %v; want quiet", r.VolOfVol.Freshness.Class, r.Lifecycle.Stage, rpc.RegimeWarningOpeners(r))
	}
}

// The overnight cases of the fourth review, through the real stage-to-bucket
// map: a crash-day evening is panic, a latched term inversion beside an
// unrelated error keeps the confirmed bucket, a prior session's SPY print
// picks no stress, and a live crash is panic beside two outages.
func TestOvernightStressKeepsItsBucket(t *testing.T) {
	latched := func(r *rpc.RegimeSnapshotResult) {
		redRow(&r.VIXTermStructure.RegimeIndicatorMeta)
		r.VIXTermStructure.Eligibility.Latched = true
		r.VIXTermStructure.Status = rpc.RegimeStatusStale
		r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
		r.VIXTermStructure.VIX3MCrossCheck = rpc.VIX3MCrossCheckAgree
	}
	spy := func(r *rpc.RegimeSnapshotResult, pct float64, class string, at time.Time) {
		r.HYGSPYDivergence.SPYChangePct = new(pct)
		r.HYGSPYDivergence.SPYQuality = &rpc.Quality{AsOf: at, FreshnessClass: class}
	}
	for _, tc := range []struct {
		name   string
		shape  func(r *rpc.RegimeSnapshotResult)
		stage  string
		bucket string
	}{
		{"crash-day evening, breadth overdue", func(r *rpc.RegimeSnapshotResult) {
			spy(r, -7.5, rpc.FreshnessFrozen, r.AsOf)
			latched(r)
			redRow(&r.VolOfVol.RegimeIndicatorMeta)
			r.VolOfVol.Last = new(150.0)
			r.Breadth.Status = rpc.RegimeStatusStale
			r.Breadth.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
		}, rpc.LifecyclePanic, risk.RegimeBucketConfirmed},
		{"latched term inversion, proxy and breadth red, yen in error", func(r *rpc.RegimeSnapshotResult) {
			latched(r)
			redRow(&r.HYGSPYDivergence.RegimeIndicatorMeta)
			redRow(&r.Breadth.RegimeIndicatorMeta)
			r.USDJPY = rpc.RegimeUSDJPY{Status: rpc.RegimeStatusError}
		}, rpc.LifecycleConfirmedStress, risk.RegimeBucketConfirmed},
		{"yesterday's SPY print at -7.5%", func(r *rpc.RegimeSnapshotResult) {
			spy(r, -7.5, rpc.FreshnessFrozen, r.AsOf.AddDate(0, 0, -1))
		}, rpc.LifecycleQuiet, risk.RegimeBucketCalm},
		{"live -7.5%, cash unavailable, breadth overdue", func(r *rpc.RegimeSnapshotResult) {
			spy(r, -7.5, rpc.FreshnessLive, r.AsOf)
			r.CreditSpreads = rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusUnavailable}
			r.Breadth.Status = rpc.RegimeStatusStale
			r.Breadth.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessOverdue}
		}, rpc.LifecyclePanic, risk.RegimeBucketConfirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := currentRegimeRows()
			tc.shape(r)
			assembleRegimeRead(r)
			if r.Lifecycle.Stage != tc.stage || bucketRegimeStage(r.Lifecycle.Stage) != tc.bucket {
				t.Fatalf("stage %s (readiness %s), bucket %q; want %s, bucket %q", r.Lifecycle.Stage, r.Lifecycle.Readiness, bucketRegimeStage(r.Lifecycle.Stage), tc.stage, tc.bucket)
			}
		})
	}
}

// The page the running build sends for three standing stress witnesses is
// kept (owner ruling 2026-11-18 16:56 CET, "Yes, keep the page"): the
// composer's own page rank pages the three-witness read and not the read of
// two witnesses and a co-sign.
func TestThreeStandingWitnessesPage(t *testing.T) {
	read := func(witnesses int) rpc.LifecycleState {
		r := currentRegimeRows()
		rows := []struct {
			meta      *rpc.RegimeIndicatorMeta
			indicator string
		}{
			{&r.VolOfVol.RegimeIndicatorMeta, rpc.RegimeIndicatorVolOfVol},
			{&r.FundingStress.RegimeIndicatorMeta, rpc.RegimeIndicatorFunding},
			{&r.CreditSpreads.RegimeIndicatorMeta, rpc.RegimeIndicatorCredit},
			{&r.Breadth.RegimeIndicatorMeta, rpc.RegimeIndicatorBreadth},
		}
		pick := rows[:3]
		if witnesses == 2 {
			pick = []struct {
				meta      *rpc.RegimeIndicatorMeta
				indicator string
			}{rows[0], rows[1], rows[3]}
		}
		for _, row := range pick {
			redRow(row.meta)
			row.meta.Thresholds = rpc.RegimeThresholdsFor(row.indicator)
		}
		assembleRegimeRead(r)
		return r.Lifecycle
	}
	if got := read(3); alertShadowRegimePageRank(got) == 0 {
		t.Fatalf("three standing witnesses: stage %s at %s does not page", got.Stage, got.Severity)
	}
	if got := read(2); alertShadowRegimePageRank(got) != 0 {
		t.Fatalf("two witnesses and a co-sign: stage %s at %s pages", got.Stage, got.Severity)
	}
}
