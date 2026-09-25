package daemon

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// cboePendingSnapshot is a regime snapshot whose Cboe files both end at a
// synthetic Tuesday close, as when Cboe stops publishing for a few days: the
// VIX3M cross-check is unverified and VVIX ages toward its budget.
func cboePendingSnapshot(asOf time.Time, officialDate string) *rpc.RegimeSnapshotResult {
	vix, vix3m, vvix := 16.0, 18.0, 83.0
	r := &rpc.RegimeSnapshotResult{AsOf: asOf}
	r.VIXTermStructure = rpc.RegimeVIXTerm{
		Status: rpc.RegimeStatusOK, VIX: &vix, VIX3M: &vix3m,
		VIXQuality:        &rpc.Quality{AsOf: asOf.Add(-time.Hour), FreshnessClass: rpc.FreshnessFrozen},
		VIX3MQuality:      &rpc.Quality{AsOf: asOf.Add(-time.Hour), FreshnessClass: rpc.FreshnessFrozen},
		VIX3MCrossCheck:   rpc.VIX3MCrossCheckUnverified,
		VIX3MOfficial:     &vix3m,
		VIX3MOfficialDate: officialDate,
	}
	r.VolOfVol = rpc.RegimeVolOfVol{Status: rpc.RegimeStatusOK, AsOfDate: officialDate, Last: &vvix}
	r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status = "unavailable", "unavailable", "unavailable"
	r.USDJPY.Status, r.Breadth.Status, r.GammaZero.Status = "unavailable", "unavailable", "unavailable"
	return r
}

func regimeHealthRows(t *testing.T, r *rpc.RegimeSnapshotResult, now time.Time, lastSuccess time.Time) map[string]rpc.DataSourceHealth {
	t.Helper()
	annotateRegimeMetadata(r, (&Server{}).populateStreaksWithStore(r, nil))
	r.Fingerprint = rpc.BuildRegimeFingerprint(r)
	raw, _ := json.Marshal(r)
	s := &Server{regimeSnapshots: &regimeSnapshotCache{raw: raw, fingerprint: r.Fingerprint, revision: 1, lastSuccessAt: lastSuccess, freshFor: time.Hour, now: func() time.Time { return now }}}
	if _, err := s.regimeSnapshots.current(); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	rows := map[string]rpc.DataSourceHealth{}
	for _, row := range s.regimeDataHealth(now) {
		rows[row.ID] = row
	}
	return rows
}

// TestRegimeHealthNamesCboePublicationGap witnesses the vix_term and vvix
// rows that read "limited" with no cause while Cboe's files were reachable
// but had stopped at an older close. Each now names the upstream publication
// gap, the newest official close and since when the gap limits it; a failed
// read, a row limited for another reason, or an outdated publication does not.
func TestRegimeHealthNamesCboePublicationGap(t *testing.T) {
	friday := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)   // 06:00 New York, before the window
	saturday := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) // past VVIX's four-day budget
	for _, tc := range []struct {
		name, id, want string
		now            time.Time
		since          time.Time
	}{
		{"vix_term", "regime:vix_term", "Cboe publication pending · last official close 2026-09-22 · unverified since 2026-09-24 20:30Z", friday, time.Date(2026, 9, 24, 20, 30, 0, 0, time.UTC)},
		{"vvix", "regime:vvix", "Cboe publication pending · last official close 2026-09-22 · overdue since 2026-09-26 00:00Z", saturday, time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := regimeHealthRows(t, cboePendingSnapshot(tc.now, "2026-09-22"), tc.now, tc.now)[tc.id]
			if row.State != "limited" || row.CadenceState != "overdue" {
				t.Fatalf("fixture is not a limited overdue row: %+v", row)
			}
			if row.Cause != rpc.DataHealthCauseUpstreamPublicationPending || row.OfficialDate != "2026-09-22" || !row.PendingSince.Equal(tc.since) || row.Receiving != tc.want {
				t.Fatalf("publication gap not named: cause=%q official=%q since=%s receiving=%q", row.Cause, row.OfficialDate, row.PendingSince, row.Receiving)
			}
			if strings.Contains(row.Action, "reachable") {
				t.Fatalf("action blames reachability: %q", row.Action)
			}
		})
	}
	if row := regimeHealthRows(t, cboePendingSnapshot(friday, "2026-09-22"), friday, friday)["regime:vvix"]; row.Cause != "" || row.State == "limited" {
		t.Fatalf("VVIX inside its budget named a gap: %+v", row)
	}

	failedRead := cboePendingSnapshot(friday, "2026-09-22")
	failedRead.VIXTermStructure.VIX3MOfficialDate, failedRead.VIXTermStructure.VIX3MOfficial = "", nil
	otherCause := cboePendingSnapshot(friday, "2026-09-22")
	otherCause.VIXTermStructure.VIXQuality = nil
	for name, tc := range map[string]struct {
		r           *rpc.RegimeSnapshotResult
		lastSuccess time.Time
	}{
		"failed read":          {failedRead, friday},
		"also limited":         {otherCause, friday},
		"outdated publication": {cboePendingSnapshot(friday, "2026-09-22"), friday.Add(-2 * time.Hour)},
	} {
		row := regimeHealthRows(t, tc.r, friday, tc.lastSuccess)["regime:vix_term"]
		if row.State != "limited" || row.Cause != "" || row.OfficialDate != "" || !row.PendingSince.IsZero() {
			t.Fatalf("%s: named a publication gap it cannot establish: %+v", name, row)
		}
	}
}

// TestRegimeWarningsNameCboePublicationGap witnesses the VIX3M warning that
// told the operator to retry once Cboe's file was reachable although it had
// been read and was merely unpublished, and VVIX, which said nothing when its
// close aged past the budget until the row turned stale three days later.
func TestRegimeWarningsNameCboePublicationGap(t *testing.T) {
	codes := func(r *rpc.RegimeSnapshotResult) map[string]rpc.RegimeWarning {
		out := map[string]rpc.RegimeWarning{}
		for _, w := range buildRegimeWarnings(r) {
			out[w.Code] = w
		}
		return out
	}
	friday := time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC)
	got := codes(cboePendingSnapshot(friday, "2026-09-22"))
	pending, ok := got["vix3m_official_close_pending"]
	if !ok || strings.Contains(pending.Action, "reachable") || !strings.Contains(pending.Message, "2026-09-22") || !strings.Contains(pending.Message, "2026-09-24 20:30Z") {
		t.Fatalf("VIX3M publication gap misreported: %+v", got)
	}
	if _, ok := got["vvix_official_close_pending"]; ok {
		t.Fatal("VVIX inside its budget warned")
	}
	failedRead := cboePendingSnapshot(friday, "2026-09-22")
	failedRead.VIXTermStructure.VIX3MOfficialDate = ""
	if w, ok := codes(failedRead)["vix3m_official_close_unavailable"]; !ok || !strings.Contains(w.Action, "reachable") {
		t.Fatalf("failed VIX3M read lost its reachability warning: %+v", w)
	}

	saturday := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	vvix, ok := codes(cboePendingSnapshot(saturday, "2026-09-22"))["vvix_official_close_pending"]
	if !ok || strings.Contains(vvix.Action, "reachable") || !strings.Contains(vvix.Message, "2026-09-26 00:00Z") {
		t.Fatalf("overdue VVIX stayed silent or misreported: %+v", vvix)
	}
	stale := cboePendingSnapshot(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC), "2026-09-22")
	stale.VolOfVol.Status = rpc.RegimeStatusStale
	warnings := buildRegimeWarnings(stale)
	if !slices.ContainsFunc(warnings, func(w rpc.RegimeWarning) bool { return w.Code == "vvix_official_close_pending" }) ||
		slices.ContainsFunc(warnings, func(w rpc.RegimeWarning) bool {
			return w.Scope == "vol_of_vol" && strings.Contains(w.Action, "reachable")
		}) {
		t.Fatalf("week-old VVIX blamed reachability or lost its cause: %+v", warnings)
	}
}
