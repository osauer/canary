package logepisode

import (
	"sync"
	"testing"
	"time"
)

func TestIncidentRetainsWarningsAndRecoveryUnderRetryFlood(t *testing.T) {
	var s State
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	warnings := 0
	// The overnight 15-second reconnect loop must not flood logs. It still
	// reminds throughout an eight-hour outage, independent of market guesses.
	for i := range 8 * 60 * 4 {
		warn, count, _ := s.Observe(now.Add(time.Duration(i) * 15 * time.Second))
		if warn {
			warnings++
		}
		if count != uint64(i+1) {
			t.Fatal(count)
		}
	}
	if warnings != 32 {
		t.Fatalf("warnings=%d, want entry plus 31 reminders", warnings)
	}
	count, age := s.Recover(now.Add(8 * time.Hour))
	if count != 1920 || age != 8*time.Hour {
		t.Fatalf("recovery: %d %s", count, age)
	}
	if count, _ := s.Recover(now); count != 0 {
		t.Fatal("duplicate recovery")
	}
	if warn, count, _ := s.Observe(now); !warn || count != 1 {
		t.Fatal("new incident suppressed")
	}
	if warn, _, age := s.Observe(now.Add(-time.Minute)); !warn || age < 0 {
		t.Fatal("clock rollback hidden")
	}
}

func TestConcurrentReadersShareOneWarning(t *testing.T) {
	var s State
	now := time.Now()
	var wg sync.WaitGroup
	warnings := make(chan bool, 100)
	for range 100 {
		wg.Go(func() { warn, _, _ := s.Observe(now); warnings <- warn })
	}
	wg.Wait()
	close(warnings)
	n := 0
	for warn := range warnings {
		if warn {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("warnings=%d", n)
	}
	if count, _ := s.Recover(now); count != 100 {
		t.Fatal(count)
	}
}

func BenchmarkSuppressedIncident(b *testing.B) {
	var s State
	now := time.Now()
	s.Observe(now)
	b.ReportAllocs()
	for b.Loop() {
		s.Observe(now)
	}
}

func TestDependentFailureCannotInventAnIncident(t *testing.T) {
	var s State
	if s.Join() {
		t.Fatal("dependency created an incident")
	}
	now := time.Now()
	s.Observe(now)
	if !s.Join() {
		t.Fatal("dependency did not join")
	}
	if count, _ := s.Recover(now); count != 2 {
		t.Fatal(count)
	}
	if s.Join() {
		t.Fatal("dependency revived recovered incident")
	}
}
