// Package logepisode bounds repeated diagnostics without changing producer state.
package logepisode

import (
	"sync"
	"time"
)

// State retains one incident, without keys or broker data. The zero value is ready.
type State struct {
	mu          sync.Mutex
	since, last time.Time
	count       uint64
}

// Observe counts a failure and requests a warning on entry, every fifteen minutes,
// or after a clock rollback. Callers must still publish every failure to health.
func (s *State) Observe(now time.Time) (warn bool, count uint64, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == 0 {
		s.since = now
	}
	s.count++
	warn = s.count == 1 || now.Before(s.last) || now.Sub(s.last) >= 15*time.Minute
	if warn {
		s.last = now
	}
	return warn, s.count, max(0, now.Sub(s.since))
}

// Recover ends the incident and returns its failure count and duration. A zero
// count means no incident was open; reconnect alone is not proof of data recovery.
func (s *State) Recover(now time.Time) (count uint64, age time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count, age = s.count, max(0, now.Sub(s.since))
	s.since, s.last, s.count = time.Time{}, time.Time{}, 0
	return count, age
}

// Join counts a dependent failure only if an incident already exists. A
// dependency cannot open an incident whose recovery belongs to another owner.
func (s *State) Join() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count == 0 {
		return false
	}
	s.count++
	return true
}
