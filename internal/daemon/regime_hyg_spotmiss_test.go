package daemon

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// hygSpotMissDeps fabricates the observed overnight condition: the HYG spot
// subscribe delivers no tick while daily history is healthy, so the latest
// official close serves as the banding input.
func hygSpotMissDeps(now time.Time, warns *[]string) *regimeDeps {
	bars := make([]ibkrlib.HistoricalBar, 0, 60)
	for i := range 60 {
		d := now.AddDate(0, 0, i-60)
		bars = append(bars, ibkrlib.HistoricalBar{
			Date:  d.Format("2006-01-02"),
			Time:  d,
			Close: 79.0,
			High:  80.0,
		})
	}
	return &regimeDeps{
		snapshot: func(_ context.Context, sym string, _ time.Duration) snapshotQuote {
			// HYG delivers nothing; the test only exercises the HYG row.
			return snapshotQuote{}
		},
		snapshotWith52WHigh: func(_ context.Context, sym string, _ time.Duration) snapshotQuote {
			return snapshotQuote{price: 530, prevClose: 529, week52High: 560, dataType: "live", observedAt: now}
		},
		history: func(_ context.Context, sym string, days int) ([]ibkrlib.HistoricalBar, error) {
			return bars, nil
		},
		logWarnf: func(format string, args ...any) {
			*warns = append(*warns, fmt.Sprintf(format, args...))
		},
		now: func() time.Time { return now },
	}
}

func TestHYGSpotMissWarnsOnlyDuringRTH(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY location: %v", err)
	}

	spotMissWarn := func(warns []string) bool {
		return slices.ContainsFunc(warns, func(w string) bool {
			return strings.Contains(w, "delivered no tick")
		})
	}

	// Overnight on a trading date: expected thin tape. The typed surface must
	// carry the miss; the log must stay quiet.
	offRTH := time.Date(2026, 8, 11, 3, 0, 0, 0, ny)
	var warns []string
	out := fetchRegimeHYGSPY(context.Background(), hygSpotMissDeps(offRTH, &warns))
	if !slices.Contains(out.FieldsMissing, "hyg_spot_tick") {
		t.Fatalf("off-RTH fields_missing = %v, want hyg_spot_tick recorded", out.FieldsMissing)
	}
	if out.HYGPrice == nil || out.HYGDataType != "close" {
		t.Fatalf("off-RTH close fallback not applied: price=%v type=%q", out.HYGPrice, out.HYGDataType)
	}
	if spotMissWarn(warns) {
		t.Fatalf("off-RTH spot miss warned: %q", warns)
	}

	// The same miss during the regular session is a real feed fault and must
	// keep its warning.
	rth := time.Date(2026, 8, 11, 15, 0, 0, 0, ny)
	warns = nil
	out = fetchRegimeHYGSPY(context.Background(), hygSpotMissDeps(rth, &warns))
	if !slices.Contains(out.FieldsMissing, "hyg_spot_tick") {
		t.Fatalf("RTH fields_missing = %v, want hyg_spot_tick recorded", out.FieldsMissing)
	}
	if !spotMissWarn(warns) {
		t.Fatalf("RTH spot miss did not warn: %q", warns)
	}
}

// The regime's dependency warnings go through the gateway outage owner: the
// RTH spot miss during a backend-link loss, or in the settle after its
// restore, logs at debug; once the link has settled it warns again.
// Owner instruction (Oliver, in chat, 2026-10-13 09:04 CEST): dependents of
// an owned outage log through the owner.
func TestRegimeDependencyWarningJoinsTheOwnedOutage(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load NY location: %v", err)
	}
	rth := time.Date(2026, 8, 11, 15, 0, 0, 0, ny)
	var buf bytes.Buffer
	now := rth
	link := ibkrlib.BackendLinkReport{Down: true, ChangedAt: rth.Add(-5 * time.Second)}
	s := &Server{logger: NewLogger(&buf, "debug"), now: func() time.Time { return now }}
	s.backendLink = func() ibkrlib.BackendLinkReport { return link }
	deps := hygSpotMissDeps(rth, new([]string))
	deps.logWarnf = s.regimeDependencyWarnf

	fetchRegimeHYGSPY(context.Background(), deps)
	link = ibkrlib.BackendLinkReport{ChangedAt: rth}
	now = rth.Add(ibkrlib.BackendRestoreSettle / 2)
	fetchRegimeHYGSPY(context.Background(), deps)
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("regime dependency warned inside the owned outage:\n%s", buf.String())
	}
	if got := strings.Count(buf.String(), "Backend link dependency unavailable: regime: HYG spot subscribe delivered no tick"); got != 2 {
		t.Fatalf("debug diagnostics=%d want 2:\n%s", got, buf.String())
	}

	buf.Reset()
	now = rth.Add(2 * ibkrlib.BackendRestoreSettle)
	fetchRegimeHYGSPY(context.Background(), deps)
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "delivered no tick") {
		t.Fatalf("regime dependency warning hidden outside an outage:\n%s", buf.String())
	}
}
