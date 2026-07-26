package daemon

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
)

// TestBreadthMembers_CacheLoadOncePerProcess pins the lazy
// once-per-process members read. Construction must not touch the
// members cache: daemon.New runs before Server.Start acquires the
// instance lock, so every autospawn race loser builds a full Server,
// and an eager load made each loser re-read the cache file and re-log
// "breadth: loaded N members from cache" into the shared daemon log
// (2026-06-09: ~10 interleaved lines per spawn burst, immediately
// before the gamma triples fixed the same way). And N concurrent plus
// repeated entry-point calls must trigger exactly one read — one INFO
// line — per process lifetime. Mirror of
// TestGammaZeroCache_PersistedLoadOncePerScope.
func TestBreadthMembers_CacheLoadOncePerProcess(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path, err := spx.MembersDefaultPath()
	if err != nil {
		t.Fatalf("MembersDefaultPath: %v", err)
	}
	members := make([]string, 503)
	for i := range members {
		members[i] = fmt.Sprintf("SYM%03d", i)
	}
	asOf := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	if err := spx.SaveExternal(path, members, asOf); err != nil {
		t.Fatalf("SaveExternal: %v", err)
	}

	var buf bytes.Buffer
	srv := newTestServer(t)
	srv.logger = NewLogger(&buf, "info")
	srv.installBreadthEngine()
	if got := strings.Count(buf.String(), "members"); got != 0 {
		t.Fatalf("construction read the members cache: %d log lines, want 0 (autospawn losers must stay off the cache file)\nlog:\n%s", got, buf.String())
	}

	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			srv.breadth.Members()
			srv.membersHealth()
		})
	}
	wg.Wait()
	// Repeated sequential calls after the concurrent wave must not
	// re-read either.
	for range 3 {
		srv.breadth.Members()
	}

	got := srv.breadth.Members()
	if len(got) != 503 || got[0] != "SYM000" {
		t.Errorf("engine is not serving the cached list: len=%d first=%q", len(got), got[0])
	}
	log := buf.String()
	if n := strings.Count(log, "loaded 503 members from cache (as_of 2026-06-04)"); n != 1 {
		t.Errorf("cache-load log lines: got %d, want exactly 1\nlog:\n%s", n, log)
	}
	if n := strings.Count(log, "using embedded members list"); n != 0 {
		t.Errorf("embedded-fallback log lines: got %d, want 0 (cache file is present and valid)", n)
	}
}
