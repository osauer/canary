package daemon

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// An observation that waits for the producer lock is named with its wait and
// its hold, so a long broker evidence hold can be split into the two.
func TestSlowAlertObservationIsNamedWithWaitAndHold(t *testing.T) {
	var mu sync.Mutex
	var lines []string
	c := &alertShadowComposer{warnf: func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}}
	release, err := c.lockObservation(t.Context(), "Rulebook")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(lines) != 0 {
		t.Fatalf("a quick observation was named: %v", lines)
	}
	c.mu.Lock()
	go func() { time.Sleep(alertObservationWarnAfter + 50*time.Millisecond); c.mu.Unlock() }()
	release, err = c.lockObservation(t.Context(), "Rulebook")
	if err != nil {
		t.Fatal(err)
	}
	release()
	mu.Lock()
	defer mu.Unlock()
	if len(lines) != 1 || !strings.Contains(lines[0], "Rulebook observation waited 1.") || !strings.Contains(lines[0], "held it") {
		t.Fatalf("slow observation not named with wait and hold: %v", lines)
	}
}
