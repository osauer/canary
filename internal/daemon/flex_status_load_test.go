package daemon

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestFlexStatusUsesCommittedProjectionUnderPollingLoad(t *testing.T) {
	now := berlinTestTime(t, 2026, 9, 19, 7, 7)
	s, _ := newFlexScheduleTestServer(t, &now)
	name := "flex-" + flexQueryFingerprint("daily-report") + "-friday.xml"
	writeFlexFixture(t, name, "20260919;070700", "20250919", "20260918", "")
	persistFlexScheduleTestState(t, s, flexFetchStateV2{
		Stage: rpc.ReconReportStateCurrent, LastAttempt: now, LastSuccess: now,
		TargetDate: utcDate(2026, 9, 18), CoverageTo: utcDate(2026, 9, 18),
	})
	// Status describes committed evidence. It must neither parse mutable input
	// nor acquire the parser's resource slot on each poll.
	dir, _ := flexStatementsDirPath()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("incomplete incoming XML"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 160 {
		wg.Go(func() {
			status := s.flexFetchStatusAt(now)
			if status.State != rpc.ReconReportStateCurrent || !status.CoverageTo.Equal(utcDate(2026, 9, 18)) {
				t.Errorf("status lost committed evidence: %+v", status)
			}
		})
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("health polling did not complete promptly")
	}
	// A failed refresh must not publish partial input; successful replacement
	// retracts the old summary atomically.
	if err := s.refreshStatementProjection(t.Context()); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if err := os.Remove(filepath.Join(dir, name)); err != nil {
		t.Fatal(err)
	}
	if err := s.refreshStatementProjection(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.flexFetchStatusAt(now); !got.CoverageTo.IsZero() || got.State == rpc.ReconReportStateCurrent {
		t.Fatalf("removed projection remains current: %+v", got)
	}
}
