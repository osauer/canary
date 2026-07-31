package daemon

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The calendar these tests key on: Friday 2026-07-17 is the last completed
// VIX3M publication window before Monday pre-open, Thursday 2026-07-16 the one
// before it.
var (
	vix3mPreOpen      = time.Date(2026, 7, 20, 8, 0, 0, 0, newYorkLocation())
	vix3mInWindow     = time.Date(2026, 7, 20, 10, 0, 0, 0, newYorkLocation())
	vix3mLastSession  = "2026-07-17"
	vix3mPriorSession = "2026-07-16"
)

func officialClose(date string, value float64) officialVIX3MClose {
	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		panic(err)
	}
	return officialVIX3MClose{value: value, date: d, ok: true}
}

func TestFetchCBOEVIX3MSeriesParsesOfficialCloses(t *testing.T) {
	// Cboe's real shape, plus the rows a parser must not trust: a short
	// record, a non-numeric close, an unparseable date, and a blank.
	body := strings.Join([]string{
		"DATE,OPEN,HIGH,LOW,CLOSE",
		"07/15/2026,20.31,21.65,19.49,21.50",
		"07/16/2026,20.33,20.59,19.45",
		"07/16/2026,20.33,20.59,19.45,n/a",
		"not-a-date,20.33,20.59,19.45,19.90",
		"07/16/2026,20.33,20.59,19.45,19.86",
		"07/17/2026,20.29,20.63,19.73,",
		"07/17/2026,20.29,20.63,19.73,19.50",
	}, "\n") + "\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	t.Cleanup(func(prev string) func() {
		return func() { cboeVIX3MHistoryURL = prev }
	}(cboeVIX3MHistoryURL))
	cboeVIX3MHistoryURL = srv.URL

	points, err := fetchCBOEVIX3MSeries(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(points) != 3 {
		t.Fatalf("points=%d (%+v), want the 3 well-formed rows", len(points), points)
	}
	latest, ok := latestSeriesPoint(points)
	if !ok || latest.Value != 19.50 || latest.Date.Format("2006-01-02") != vix3mLastSession {
		t.Fatalf("latest=%+v, want 19.50 on %s", latest, vix3mLastSession)
	}
	if points[0].Date.After(points[len(points)-1].Date) {
		t.Fatalf("series is not oldest-first: %+v", points)
	}

	// A file whose value column is absent must fail rather than silently
	// resolving to a neighbouring column.
	srvNoClose := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("DATE,OPEN,HIGH,LOW\n07/17/2026,20.29,20.63,19.73\n"))
	}))
	defer srvNoClose.Close()
	cboeVIX3MHistoryURL = srvNoClose.URL
	if _, err := fetchCBOEVIX3MSeries(context.Background()); err == nil {
		t.Fatal("accepted a CSV with no CLOSE column")
	}
}

// Off-window the broker keeps answering whatever the value's real age, so the
// served leg and its age bound come from Cboe's dated close wherever one
// exists, and the verdict records what the two sources established.
func TestResolveVIX3MOffWindowPicksTheDatedClose(t *testing.T) {
	cases := []struct {
		name      string
		gateway   *float64
		official  officialVIX3MClose
		wantValue float64
		wantSrc   string
		wantCheck string
	}{
		{
			name:      "agreeing legs serve the dated close",
			gateway:   new(19.50),
			official:  officialClose(vix3mLastSession, 19.50),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceOfficial,
			wantCheck: rpc.VIX3MCrossCheckAgree,
		},
		{
			name:      "rounding inside the tolerance still agrees",
			gateway:   new(19.44),
			official:  officialClose(vix3mLastSession, 19.50),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceOfficial,
			wantCheck: rpc.VIX3MCrossCheckAgree,
		},
		{
			name:      "a leg serving another session disagrees",
			gateway:   new(21.50),
			official:  officialClose(vix3mLastSession, 19.50),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceOfficial,
			wantCheck: rpc.VIX3MCrossCheckDisagree,
		},
		{
			name:      "no broker leg at all leaves the official close alone",
			gateway:   nil,
			official:  officialClose(vix3mLastSession, 19.50),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceOfficial,
			wantCheck: rpc.VIX3MCrossCheckOfficialOnly,
		},
		{
			name:      "one session behind is publication lag, not a defect",
			gateway:   new(19.50),
			official:  officialClose(vix3mPriorSession, 19.86),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceGateway,
			wantCheck: rpc.VIX3MCrossCheckPendingPublication,
		},
		{
			name:      "two sessions behind is a lapsed source",
			gateway:   new(19.50),
			official:  officialClose("2026-07-15", 21.50),
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceGateway,
			wantCheck: rpc.VIX3MCrossCheckUnverified,
		},
		{
			name:      "no official close leaves the broker leg uncorroborated",
			gateway:   new(19.50),
			official:  officialVIX3MClose{},
			wantValue: 19.50,
			wantSrc:   rpc.VIX3MSourceGateway,
			wantCheck: rpc.VIX3MCrossCheckUnverified,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := rpc.RegimeVIXTerm{VIX3M: tc.gateway}
			if tc.gateway != nil {
				row.VIX3MSource = rpc.VIX3MSourceGateway
			}
			resolveVIX3MOffWindow(&row, tc.official, vix3mPreOpen)

			if row.VIX3M == nil || *row.VIX3M != tc.wantValue {
				t.Fatalf("served VIX3M=%v, want %v", row.VIX3M, tc.wantValue)
			}
			if row.VIX3MSource != tc.wantSrc {
				t.Fatalf("source=%q, want %q", row.VIX3MSource, tc.wantSrc)
			}
			if row.VIX3MCrossCheck != tc.wantCheck {
				t.Fatalf("cross-check=%q, want %q", row.VIX3MCrossCheck, tc.wantCheck)
			}
			if tc.official.ok && (row.VIX3MOfficial == nil || *row.VIX3MOfficial != tc.official.value) {
				t.Fatalf("official value not retained: %v", row.VIX3MOfficial)
			}
			if tc.wantSrc == rpc.VIX3MSourceOfficial {
				// The leg's honest vintage is the window that produced the
				// close, not read time and not a calendar inference.
				wantAt := time.Date(2026, 7, 17, 16, 30, 0, 0, newYorkLocation())
				if row.VIX3MQuality == nil || !row.VIX3MQuality.AsOf.Equal(wantAt) {
					t.Fatalf("official leg quality=%+v, want as-of %s", row.VIX3MQuality, wantAt)
				}
			}
			if tc.wantCheck == rpc.VIX3MCrossCheckDisagree &&
				(row.VIX3MGatewayLast == nil || *row.VIX3MGatewayLast != *tc.gateway) {
				t.Fatalf("disagreement dropped the broker's own reading: %v", row.VIX3MGatewayLast)
			}
		})
	}
}

// The failure this closes: a gateway that keeps ANSWERING with a stale value.
// Off-window that is indistinguishable from a quiet market until a dated close
// contradicts it, and today the row would read healthy indefinitely.
func TestVIX3MDisagreementFailsClosedAndNamesTheBrokerLeg(t *testing.T) {
	deps := (&fakeDeps{
		now: vix3mPreOpen,
		snapshots: map[string]fakeQuote{
			"VIX":   {price: 17.2, dataType: rpc.MarketDataFrozen},
			"VIX3M": {price: 21.5, dataType: rpc.MarketDataFrozen},
		},
		vix3m: []regimeSeriesPoint{{Date: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), Value: 19.5}},
	}).build()

	row := fetchRegimeVIXTerm(context.Background(), deps)
	if row.VIX3MCrossCheck != rpc.VIX3MCrossCheckDisagree {
		t.Fatalf("cross-check=%q, want disagree", row.VIX3MCrossCheck)
	}
	if row.VIX3M == nil || *row.VIX3M != 19.5 {
		t.Fatalf("served VIX3M=%v, want Cboe's published close", row.VIX3M)
	}
	if row.VIX3MGatewayLast == nil || *row.VIX3MGatewayLast != 21.5 {
		t.Fatalf("gateway reading=%v, want it retained as evidence", row.VIX3MGatewayLast)
	}
	if row.Ratio == nil || *row.Ratio != 17.2/19.5 {
		t.Fatalf("ratio=%v, want it recomputed against the official close", row.Ratio)
	}
	if row.Status != rpc.RegimeStatusStale {
		t.Fatalf("status=%q, want stale — an official close is never confirmable", row.Status)
	}

	res := &rpc.RegimeSnapshotResult{AsOf: vix3mPreOpen, VIXTermStructure: row}
	if got := vixTermCadenceClass(res, vix3mPreOpen); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("cadence=%q, want overdue — a contradicted leg may not claim not_due", got)
	}
	if rpc.RegimeClusterExpectedNotDue(*res, "vol") {
		t.Fatal("a contradicted VIX3M leg still won the vol cluster's not_due exemption")
	}
	w, ok := warningForVIX3MCrossCheck(row)
	if !ok || w.Code != "vix3m_source_disagreement" || w.Severity != "error" {
		t.Fatalf("warning=%+v ok=%v, want a named error-severity disagreement", w, ok)
	}
	if !strings.Contains(w.Message, "21.50") || !strings.Contains(w.Message, "19.50") {
		t.Fatalf("warning message %q omits one of the two readings", w.Message)
	}
}

// not_due exempts a row from every age bound, so it is available only to a leg
// whose vintage a dated close established.
func TestVIX3MOffWindowLegRequiresOfficialCorroboration(t *testing.T) {
	ratio := 0.88
	quality := &rpc.Quality{AsOf: vix3mPreOpen, FreshnessClass: rpc.FreshnessFrozen, Confidence: rpc.ConfidenceFirm}
	res := &rpc.RegimeSnapshotResult{
		AsOf: vix3mPreOpen,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale, Ratio: &ratio,
			VIXQuality: quality, VIX3MQuality: quality,
		},
	}
	vouched := map[string]bool{
		rpc.VIX3MCrossCheckAgree:              true,
		rpc.VIX3MCrossCheckOfficialOnly:       true,
		rpc.VIX3MCrossCheckPendingPublication: true,
		rpc.VIX3MCrossCheckDisagree:           false,
		rpc.VIX3MCrossCheckUnverified:         false,
		"":                                    false,
	}
	for verdict, wantNotDue := range vouched {
		res.VIXTermStructure.VIX3MCrossCheck = verdict
		want := rpc.RegimeFreshnessOverdue
		if wantNotDue {
			want = rpc.RegimeFreshnessNotDue
		}
		if got := vixTermCadenceClass(res, vix3mPreOpen); got != want {
			t.Fatalf("cross-check %q cadence=%q, want %q", verdict, got, want)
		}
	}
}

// Cboe publishes only closes, so inside the window the gateway stays the source
// and the previous session's close never substitutes for a live tick.
func TestFetchRegimeVIXTermKeepsTheGatewayInsideTheWindow(t *testing.T) {
	deps := (&fakeDeps{
		now: vix3mInWindow,
		snapshots: map[string]fakeQuote{
			"VIX":   {price: 17.2, dataType: rpc.MarketDataLive},
			"VIX3M": {price: 21.5, dataType: rpc.MarketDataLive},
		},
		vix3m: []regimeSeriesPoint{{Date: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), Value: 19.5}},
	}).build()

	row := fetchRegimeVIXTerm(context.Background(), deps)
	if row.VIX3M == nil || *row.VIX3M != 21.5 || row.VIX3MSource != rpc.VIX3MSourceGateway {
		t.Fatalf("in-window VIX3M=%v source=%q, want the live gateway tick", row.VIX3M, row.VIX3MSource)
	}
	if row.VIX3MCrossCheck != "" || row.VIX3MOfficial != nil {
		t.Fatalf("in-window cross-check=%q official=%v, want the check not to run", row.VIX3MCrossCheck, row.VIX3MOfficial)
	}
	if row.Status != rpc.RegimeStatusOK {
		t.Fatalf("status=%q, want ok for two live legs", row.Status)
	}
	res := &rpc.RegimeSnapshotResult{AsOf: vix3mInWindow, VIXTermStructure: row}
	if got := vixTermCadenceClass(res, vix3mInWindow); got != rpc.RegimeFreshnessFresh {
		t.Fatalf("in-window cadence=%q, want fresh", got)
	}
}

// An official close alone repairs a row the gateway could not supply — and a
// row neither source can supply still reaches overdue rather than reading
// healthy on last-good display evidence.
func TestFetchRegimeVIXTermFallsBackThenFailsClosed(t *testing.T) {
	base := func() *fakeDeps {
		return &fakeDeps{
			now: vix3mPreOpen,
			snapshots: map[string]fakeQuote{
				"VIX": {price: 17.2, dataType: rpc.MarketDataFrozen},
			},
		}
	}

	withOfficial := base()
	withOfficial.vix3m = []regimeSeriesPoint{{Date: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), Value: 19.5}}
	row := fetchRegimeVIXTerm(context.Background(), withOfficial.build())
	if row.Status != rpc.RegimeStatusStale || row.VIX3M == nil || *row.VIX3M != 19.5 {
		t.Fatalf("row=%+v, want the official close standing in for the absent tick", row)
	}
	if row.VIX3MCrossCheck != rpc.VIX3MCrossCheckOfficialOnly {
		t.Fatalf("cross-check=%q, want official_only", row.VIX3MCrossCheck)
	}

	warnings := []string{}
	neither := base()
	neither.vix3mErr = errors.New("cdn unreachable")
	neither.warnLog = &warnings
	row = fetchRegimeVIXTerm(context.Background(), neither.build())
	if row.Status != rpc.RegimeStatusError || row.VIX3M != nil || row.Ratio != nil {
		t.Fatalf("row=%+v, want an unranked row when neither source is usable", row)
	}
	if !strings.Contains(row.ErrorMessage, "Cboe official close") {
		t.Fatalf("error_message=%q, want it to name both missing sources", row.ErrorMessage)
	}
	if row.VIX3MCrossCheck != rpc.VIX3MCrossCheckUnverified {
		t.Fatalf("cross-check=%q, want unverified", row.VIX3MCrossCheck)
	}
	res := &rpc.RegimeSnapshotResult{AsOf: vix3mPreOpen, VIXTermStructure: row}
	if got := vixTermCadenceClass(res, vix3mPreOpen); got != rpc.RegimeFreshnessOverdue {
		t.Fatalf("cadence=%q, want overdue", got)
	}
	if len(warnings) == 0 || !strings.Contains(strings.Join(warnings, " "), "cdn unreachable") {
		t.Fatalf("warn log=%v, want the fetch failure surfaced to the operator", warnings)
	}
}

// A dated close is better evidence than a frozen quote, but it is still
// end-of-day: it may not spend a persistence session or confirm stress the way
// a live same-session tick can.
func TestOfficialVIX3MCloseCannotConfirmStress(t *testing.T) {
	deps := (&fakeDeps{
		now: vix3mPreOpen,
		snapshots: map[string]fakeQuote{
			"VIX":   {price: 21.0, dataType: rpc.MarketDataFrozen},
			"VIX3M": {price: 19.5, dataType: rpc.MarketDataFrozen},
		},
		vix3m: []regimeSeriesPoint{{Date: time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC), Value: 19.5}},
	}).build()

	res := &rpc.RegimeSnapshotResult{AsOf: vix3mPreOpen}
	res.VIXTermStructure = fetchRegimeVIXTerm(context.Background(), deps)
	if res.VIXTermStructure.VIX3MCrossCheck != rpc.VIX3MCrossCheckAgree {
		t.Fatalf("cross-check=%q, want agree", res.VIXTermStructure.VIX3MCrossCheck)
	}
	if res.VIXTermStructure.Ratio == nil || *res.VIXTermStructure.Ratio < 1.05 {
		t.Fatalf("ratio=%v, want a deep inversion so only the cadence gate can block it", res.VIXTermStructure.Ratio)
	}

	store := NewStreakStore(t.TempDir())
	policy := (&Server{}).populateStreaksWithStore(res, store)[rpc.RegimeIndicatorVIXTerm]
	if policy.band != "red" {
		t.Fatalf("band=%q, want the inversion still displayed", policy.band)
	}
	if policy.freshness == nil || policy.freshness.Class != rpc.RegimeFreshnessNotDue {
		t.Fatalf("freshness=%+v, want not_due", policy.freshness)
	}
	if policy.eligibility == nil || policy.eligibility.Eligible ||
		len(policy.eligibility.Reasons) != 1 || policy.eligibility.Reasons[0] != "data_not_due" {
		t.Fatalf("eligibility=%+v, want ineligible for data_not_due", policy.eligibility)
	}
	if info := store.Get(StreakKeyVIXTerm); info != nil && info.Sessions != 0 {
		t.Fatalf("an official close banked a persistence session: %+v", info)
	}
}
