package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A typed provider answer (no_date_published, unsupported_security) is the
// provider responding, not breaking. Only attempts carrying a failure record
// may reach the log, and the line must name the symbol it is about.
func TestEarningsProviderOutcomeLogGate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/SYNTH/") {
			fmt.Fprint(w, `{"data":{"announcement":"Earnings announcement* for SYNTH: "},"message":null,"status":{"rCode":200}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	record := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	// One sink for both levels: a benign answer may not log at either.
	cache := newEarningsCacheCold(t.TempDir(), record, record)
	cache.fetchURL = srv.URL + "/api/analyst/%s/earnings-date"

	cache.refreshTarget(context.Background(), earningsRefreshTarget{Symbol: "SYNTH"})
	mu.Lock()
	benign := append([]string(nil), lines...)
	mu.Unlock()
	if len(benign) != 0 {
		t.Fatalf("benign no-date answer must not log, got %q", benign)
	}
	state := cache.symbols["SYNTH"]
	if got := state.Providers[earningsNasdaqProvider].LastAttempt.Status; got != "no_date_published" {
		t.Fatalf("no-date answer status = %q, want no_date_published", got)
	}

	cache.refreshTarget(context.Background(), earningsRefreshTarget{Symbol: "FAIL"})
	mu.Lock()
	failed := append([]string(nil), lines...)
	mu.Unlock()
	want := "earnings provider nasdaq outcome symbol=FAIL status=transport_failure code=protocol_rejected stage=nasdaq_request retryable=true"
	if len(failed) != 1 || failed[0] != want {
		t.Fatalf("failed attempt log = %q, want exactly [%q]", failed, want)
	}
}

// The account holds no Wall Street Horizon feed, so ibkr_wsh answers
// not_entitled for every followed name on every daily retry while Nasdaq
// alone decides the dates, which the brief tells the reader. The first
// verdict for a name warns; a retry that only confirms it is not news and
// drops to DEBUG (on 2026-10-06..08 it warned at 06:00 every day). Any other
// outcome in between makes the next not_entitled news again.
func TestEarningsUnentitledProviderWarnsOncePerVerdict(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"announcement":"Earnings announcement* for SYNTH: "},"message":null,"status":{"rCode":200}}`)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var warns, debugs []string
	sink := func(dst *[]string) func(string, ...any) {
		return func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			*dst = append(*dst, fmt.Sprintf(format, args...))
		}
	}
	cache := newEarningsCacheCold(t.TempDir(), sink(&warns), sink(&debugs))
	cache.fetchURL = srv.URL + "/api/analyst/%s/earnings-date"
	now := time.Date(2026, 10, 6, 4, 0, 0, 0, time.UTC)
	cache.clock = func() time.Time { return now }
	unentitled := transportFailureResult(rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false, now)
	timeout := transportFailureResult(rpc.SourceFailureTimeout, rpc.SourceFailureStageWSHEvent, true, now)
	answer := unentitled
	if err := cache.setSecondaryProvider(earningsWSHProvider, func(context.Context, string) (earningsProviderFetchResult, error) {
		return answer, errors.New("synthetic WSH outcome")
	}); err != nil {
		t.Fatal(err)
	}
	const unentitledLine = "earnings provider ibkr_wsh outcome symbol=SYNTH status=transport_failure code=not_entitled stage=wsh_event retryable=false"

	for _, step := range []struct {
		name   string
		answer earningsProviderFetchResult
		warn   bool
	}{
		{"first not_entitled verdict", unentitled, true},
		{"next day confirms it", unentitled, false},
		{"day after confirms it again", unentitled, false},
		{"a different failure", timeout, true},
		{"not_entitled after a different failure", unentitled, true},
	} {
		answer = step.answer
		mu.Lock()
		warns, debugs = nil, nil
		mu.Unlock()
		cache.refreshTarget(context.Background(), earningsRefreshTarget{Symbol: "SYNTH"})
		mu.Lock()
		gotWarns, gotDebugs := append([]string(nil), warns...), append([]string(nil), debugs...)
		mu.Unlock()
		if step.warn {
			if len(gotWarns) != 1 || len(gotDebugs) != 0 {
				t.Fatalf("%s: want one WARN, got warns=%q debugs=%q", step.name, gotWarns, gotDebugs)
			}
		} else if len(gotWarns) != 0 || len(gotDebugs) != 1 || gotDebugs[0] != unentitledLine {
			t.Fatalf("%s: want no WARN and one DEBUG %q, got warns=%q debugs=%q", step.name, unentitledLine, gotWarns, gotDebugs)
		}
		// The stored attempt keeps its full shape: the brief and the legacy
		// aggregate reader both key on it.
		got := cache.symbols["SYNTH"].Providers[earningsWSHProvider].LastAttempt
		if wantUnentitled := step.answer.Failure.Code == rpc.SourceFailureNotEntitled; earningsProviderUnentitled(got.Status, got.LastFailure) != wantUnentitled {
			t.Fatalf("%s: stored attempt %+v lost its verdict", step.name, got)
		}
		now = now.Add(earningsNonRetryableFailureRetry + time.Hour)
	}
}

// A learned date is an event the desk keeps. Nasdaq carried 2026-10-29 for
// a held name until the day, then answered its no-date text on 2026-10-30, and every
// later look read "no date published" as if the print had never been known
// (analysis 2026-10-31). After the day the name reads reported on that date,
// a later no-date answer never erases it, and a restart reads the event back
// from daemon.db. A date the vendor pulls before the day is withdrawn, not
// reported, and the row says so.
func TestEarningsLearnedDateIsKeptAsAReportedEvent(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	answers := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		for sym, date := range answers {
			if strings.Contains(r.URL.Path, "/"+sym+"/") {
				fmt.Fprintf(w, `{"data":{"announcement":"Earnings announcement* for %s: %s"},"message":null,"status":{"rCode":200}}`, sym, date)
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	answer := func(sym, date string) {
		mu.Lock()
		defer mu.Unlock()
		answers[sym] = date
	}

	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	berlin, _ := time.LoadLocation("Europe/Berlin")
	now := time.Date(2026, 10, 28, 16, 36, 0, 0, berlin)
	open := func() *earningsCache {
		cache := newEarningsCacheCold(t.TempDir(), nil, nil)
		cache.fetchURL = srv.URL + "/api/analyst/%s/earnings-date"
		cache.clock = func() time.Time { return now }
		if err := cache.UseCoreStore(store); err != nil {
			t.Fatal(err)
		}
		return cache
	}
	cache := open()

	answer("SYNTH", "Oct 29, 2026")
	answer("PULLD", "Nov 20, 2026")
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH"})
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "PULLD"})
	if view, ok := cache.resolution("SYNTH"); !ok || view.Status != rpc.EarningsStatusDate || view.Entry.Date != "2026-10-29" {
		t.Fatalf("the day before the print: %+v", view)
	}

	// The day after: the vendor has rolled over to "no date" for the name
	// that printed and pulled the future date of the other one.
	now = time.Date(2026, 10, 30, 16, 36, 0, 0, berlin)
	answer("SYNTH", "")
	answer("PULLD", "")
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH"})
	cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "PULLD"})
	view, ok := cache.resolution("SYNTH")
	if !ok || view.Status != rpc.EarningsStatusReported || view.Reason != earningsReasonReported || view.ReportedOn != "2026-10-29" {
		t.Fatalf("the day after the print must read reported on 2026-10-29, got %+v", view)
	}
	if view.Entry.Date != "" {
		t.Fatalf("a reported event is not a next date: %+v", view.Entry)
	}
	if view, ok := cache.resolution("PULLD"); !ok || view.Status != rpc.EarningsStatusNoDatePublished || view.Reason != earningsReasonDateWithdrawn {
		t.Fatalf("a date pulled before the day reads withdrawn, got %+v", view)
	}

	// A week of no-date answers changes nothing: the event is kept.
	for day := 1; day <= 7; day++ {
		now = now.Add(24 * time.Hour)
		cache.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH"})
	}
	if view, ok := cache.resolution("SYNTH"); !ok || view.Status != rpc.EarningsStatusReported || view.ReportedOn != "2026-10-29" {
		t.Fatalf("a later no-date answer erased the event: %+v", view)
	}

	// A restart reads the events back from the store.
	restarted := open()
	if view, ok := restarted.resolution("SYNTH"); !ok || view.Status != rpc.EarningsStatusReported || view.ReportedOn != "2026-10-29" {
		t.Fatalf("after a restart: %+v", view)
	}
	events, err := store.ListEarningsEvents(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var synth, pulled *corestore.EarningsEvent
	for i := range events {
		switch events[i].Symbol {
		case "SYNTH":
			synth = &events[i]
		case "PULLD":
			pulled = &events[i]
		}
	}
	if synth == nil || synth.Date != "2026-10-29" || synth.Source != earningsNasdaqProvider || synth.WithdrawnAt != nil ||
		!synth.FirstSeen.Equal(time.Date(2026, 10, 28, 16, 36, 0, 0, berlin)) || !synth.LastSeen.Equal(synth.FirstSeen) {
		t.Fatalf("stored event = %+v", synth)
	}
	if pulled == nil || pulled.Date != "2026-11-20" || pulled.WithdrawnAt == nil || !pulled.WithdrawnAt.Equal(time.Date(2026, 10, 30, 16, 36, 0, 0, berlin)) {
		t.Fatalf("withdrawn event = %+v", pulled)
	}

	// The vendor publishes the next quarter: the name reads a date again.
	now = now.Add(24 * time.Hour)
	answer("SYNTH", "Jan 28, 2027")
	restarted.refreshTarget(t.Context(), earningsRefreshTarget{Symbol: "SYNTH"})
	if view, ok := restarted.resolution("SYNTH"); !ok || view.Status != rpc.EarningsStatusDate || view.Entry.Date != "2027-01-28" {
		t.Fatalf("a newly published next date: %+v", view)
	}
}
