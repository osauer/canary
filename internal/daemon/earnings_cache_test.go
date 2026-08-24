package daemon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// A typed provider answer (no_date_published, unsupported_security) is the
// provider responding, not breaking. Only attempts carrying a failure record
// may reach the log, and the line must name the symbol it is about.
func TestEarningsProviderOutcomeLogGate(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/NOW/") {
			fmt.Fprint(w, `{"data":{"announcement":"Earnings announcement* for NOW: "},"message":null,"status":{"rCode":200}}`)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	var mu sync.Mutex
	var lines []string
	cache := newEarningsCacheCold(t.TempDir(), func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	})
	cache.fetchURL = srv.URL + "/api/analyst/%s/earnings-date"

	cache.refreshTarget(context.Background(), earningsRefreshTarget{Symbol: "NOW"})
	mu.Lock()
	benign := append([]string(nil), lines...)
	mu.Unlock()
	if len(benign) != 0 {
		t.Fatalf("benign no-date answer must not log, got %q", benign)
	}
	state := cache.symbols["NOW"]
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
