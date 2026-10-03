package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

// openLiveAuthorityForLogging opens a store through the Server's live options,
// so the real CommitObserver, HealthObserver and logger hook are wired, with a
// switch that makes watermark persistence fail on demand.
func openLiveAuthorityForLogging(t *testing.T, s *Server) (*corestore.Store, *atomic.Bool) {
	t.Helper()
	opts := s.liveCoreStoreOptions(nil)
	var fail atomic.Bool
	persist := opts.CommitObserver
	opts.CommitObserver = func(head corestore.AuthorityHead) error {
		if fail.Load() {
			return errors.New("watermark unavailable")
		}
		return persist(head)
	}
	store, err := corestore.Open(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s.coreStore = store
	return store, &fail
}

func TestAuthorityIncidentWarnsOnceAndBookendsRecovery(t *testing.T) {
	for _, level := range []string{"warn", "debug"} {
		t.Run(level, func(t *testing.T) {
			var buf bytes.Buffer
			s := &Server{logger: NewLogger(&buf, level), coreStorePath: filepath.Join(privateTestDir(t), "daemon.db")}
			store, fail := openLiveAuthorityForLogging(t, s)
			mutate := func(kind string) error {
				_, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{ScopeKey: "test", Kind: kind, JSON: []byte(`{}`)})
				return err
			}
			if err := mutate("warm"); err != nil {
				t.Fatal(err)
			}
			if buf.Len() != 0 {
				t.Fatalf("healthy mutation logged: %q", buf.String())
			}

			fail.Store(true)
			if err := mutate("latch"); err == nil {
				t.Fatal("observer failure must fail the mutation")
			}
			out := buf.String()
			if strings.Count(out, "level=WARN") != 1 ||
				!strings.Contains(out, "daemon authority: persistence latched fail-closed (head_watermark): persist committed authority head: watermark unavailable; broker writes are blocked; recovery-eligible: transient proof retries every 5s; dependent persistence failures continue in debug logs") {
				t.Fatalf("latch announcement: %q", out)
			}
			if blocker, blocked := s.authorityTradingBlocker(); !blocked || blocker.Code != "daemon_storage_unavailable" || !strings.Contains(blocker.Message, "head_watermark") {
				t.Fatalf("trading blocker=%+v blocked=%v", blocker, blocked)
			}
			if health := s.authoritySubsystemHealth(); health.Status != "unavailable" || health.LastError != "head_watermark" {
				t.Fatalf("subsystem health=%+v", health)
			}

			const dependents = 25
			for i := range dependents {
				s.warnf("contract cache: save %d: %v", i, fmt.Errorf("persist proposal snapshot: %w", corestore.ErrBlocked))
			}
			if err := mutate("blocked"); !errors.Is(err, corestore.ErrBlocked) {
				t.Fatalf("mutation while latched=%v", err)
			}
			out = buf.String()
			if strings.Count(out, "level=WARN") != 1 {
				t.Fatalf("dependents duplicated the latch warning: %q", out)
			}
			if level == "debug" && strings.Count(out, "level=DEBUG msg=\"contract cache: save") != dependents {
				t.Fatalf("dependent diagnostics lost at debug: %q", out)
			}
			if level == "warn" && strings.Contains(out, "contract cache") {
				t.Fatalf("dependent diagnostics leaked below debug: %q", out)
			}
			// An independent defect keeps its warning during the incident.
			s.warnf("stress: decisions journal append failed: %v", errors.New("disk on fire"))
			if strings.Count(buf.String(), "level=WARN") != 2 {
				t.Fatalf("independent warning hidden: %q", buf.String())
			}

			// A proof that still cannot persist the watermark stays quiet at WARN.
			s.tryCoreStoreRecovery(t.Context())
			if strings.Count(buf.String(), "level=WARN") != 2 || strings.Contains(buf.String(), "persistence recovered") {
				t.Fatalf("failed proof must not claim recovery: %q", buf.String())
			}

			fail.Store(false)
			s.tryCoreStoreRecovery(t.Context())
			out = buf.String()
			if strings.Count(out, "level=WARN") != 3 || strings.Count(out, "daemon authority: persistence recovered after") != 1 ||
				!strings.Contains(out, fmt.Sprintf("(%d dependent failures while blocked)", dependents)) {
				t.Fatalf("recovery bookend: %q", out)
			}
			if strings.Contains(out, "transient head-watermark latch recovered") {
				t.Fatalf("legacy INFO bookend doubled the recovery: %q", out)
			}
			if _, blocked := s.authorityTradingBlocker(); blocked {
				t.Fatal("trading blocker remained after recovery")
			}
			if err := mutate("after"); err != nil {
				t.Fatalf("mutation after recovery: %v", err)
			}
			// With the incident closed, a blocked-store symptom is a standalone warning again.
			s.warnf("fx rate cache: persist: %v", corestore.ErrBlocked)
			if strings.Count(buf.String(), "level=WARN") != 4 {
				t.Fatalf("dependent warning hidden after recovery: %q", buf.String())
			}
		})
	}
}

func TestAuthorityIncidentPermanentLatchSaysRestartRequired(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 10, 2, 11, 16, 54, 0, time.UTC)
	s := &Server{logger: NewLogger(&buf, "warn"), now: func() time.Time { return now }}
	incident := s.armAuthorityIncident()
	incident.observe(corestore.Health{Code: "busy", BlockedAt: now}, errors.New("database is locked (5) (SQLITE_BUSY)"))
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "latched fail-closed (busy): database is locked") || !strings.Contains(out, "restart required") {
		t.Fatalf("permanent latch announcement: %q", out)
	}
	for range 3 {
		if !incident.join() {
			t.Fatal("dependent could not join the open incident")
		}
	}
	now = now.Add(5*time.Minute + 30*time.Second)
	incident.observe(corestore.Health{Ready: true}, nil)
	if !strings.Contains(buf.String(), "persistence recovered after 5m30s (3 dependent failures while blocked)") {
		t.Fatalf("bookend: %q", buf.String())
	}
}

func TestAuthorityIncidentNeverOpensWithoutTheOwner(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{logger: NewLogger(&buf, "warn")}
	incident := s.armAuthorityIncident()
	if incident.join() {
		t.Fatal("dependent opened an incident")
	}
	s.warnf("alert producer: %v", fmt.Errorf("persist alert episode registry: %w", corestore.ErrBlocked))
	if strings.Count(buf.String(), "level=WARN") != 1 {
		t.Fatalf("blocked-store warning hidden with no incident open: %q", buf.String())
	}
	incident.observe(corestore.Health{Ready: true}, nil)
	if strings.Contains(buf.String(), "recovered") {
		t.Fatalf("Ready without a latch claimed recovery: %q", buf.String())
	}
	// A Server without a logger must still arm and observe without panicking.
	quiet := (&Server{}).armAuthorityIncident()
	quiet.observe(corestore.Health{Code: "ioerr"}, errors.New("disk I/O error"))
	quiet.observe(corestore.Health{Ready: true}, nil)
}
