package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/logepisode"
)

// authorityIncident is the single warning owner for a fail-closed authority
// store. The store reports its Ready-to-blocked transition and its proof
// recovery through corestore.Options.HealthObserver; the incident announces
// the latch once with its code and cause, closes it with a bookend carrying
// the duration and the dependent failure count, and lets dependents that only
// see corestore.ErrBlocked join at debug through the Logger hook. A dependent
// can never open the incident. Status and health RPC surfaces keep reading the
// store's Health directly and are unaffected by this cadence.
type authorityIncident struct {
	server *Server
	state  logepisode.State
}

// armAuthorityIncident creates the incident for the live store about to be
// opened and points the daemon logger's blocked-store hook at it. Each live
// open starts with no incident; the most recent arm wins, which matches the
// single live Store the Server holds.
func (s *Server) armAuthorityIncident() *authorityIncident {
	incident := &authorityIncident{server: s}
	if s != nil {
		s.logger.SetBlockedIncidentHook(incident.join)
	}
	return incident
}

func (a *authorityIncident) join() bool { return a.state.Join() }

// observe is the corestore health observer. The store calls it outside its
// locks, once per transition, with the error the failing operation returned.
func (a *authorityIncident) observe(health corestore.Health, cause error) {
	now := a.server.gatewayLogClock()
	if health.Ready {
		a.recovered(now)
		return
	}
	warn, _, _ := a.state.Observe(now)
	if !warn || a.server.logger == nil {
		return
	}
	recovery := "restart required"
	if health.RecoveryEligible {
		recovery = "recovery-eligible: transient proof retries every " + coreStoreRecoveryPollInterval.String()
	}
	a.server.logger.Warnf(
		"daemon authority: persistence latched fail-closed (%s): %v; broker writes are blocked; %s; dependent persistence failures continue in debug logs",
		health.Code, cause, recovery,
	)
	if (health.Code == "lost_write" || health.Code == "outside_opener") && a.server.productionStateDatabase {
		go a.server.keepLatchEvidence(health.Code, now)
	}
}

// keepLatchEvidence saves the store's files while the latch holds writes
// still. The next open folds the WAL into the database, which took the page
// images of the 2026-11-06 latch with it, and removes an outside opener's
// daemon.db-shm.
func (s *Server) keepLatchEvidence(code string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), lostWriteEvidenceBudget)
	defer cancel()
	kept, err := preserveAuthorityEvidence(ctx, s.coreStorePath, code, now)
	if err != nil {
		s.logger.Warnf("daemon authority: could not keep the %s files: %v", code, err)
		return
	}
	s.logger.Warnf("daemon authority: the %s files are kept in %s; read them only with sqlite3 \"file:daemon.db?immutable=1\"", code, kept)
}

// lostWriteEvidenceBudget bounds the copy of the store's files.
const lostWriteEvidenceBudget = 5 * time.Minute

// preserveAuthorityEvidence copies the database, its WAL and watermark as they
// stand, and a daemon.db-shm if an outside opener left one. The log's index
// lives in the daemon's memory, so it is not in the copy; the next open
// rebuilds it from the WAL. A child process does the copy: opening and closing
// any of these files in this process would drop the daemon's exclusive lock on
// daemon.db and let an outside opener in.
func preserveAuthorityEvidence(ctx context.Context, path, code string, now time.Time) (string, error) {
	dir := filepath.Join(filepath.Dir(path), "damaged", strings.ReplaceAll(code, "_", "-")+"-"+now.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	args := []string{"-p"}
	for _, suffix := range []string{"", "-wal", "-shm", ".head"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			args = append(args, path+suffix)
		}
	}
	out, err := exec.CommandContext(ctx, "/bin/cp", append(args, dir)...).CombinedOutput()
	if err != nil {
		return dir, fmt.Errorf("copy into %s: %w: %s", dir, err, strings.TrimSpace(string(out)))
	}
	return dir, nil
}

// recovered closes the incident. A zero count means none was open, so a
// Ready report without a preceding latch never claims a recovery.
func (a *authorityIncident) recovered(now time.Time) {
	count, age := a.state.Recover(now)
	if count == 0 || a.server.logger == nil {
		return
	}
	a.server.logger.Warnf(
		"daemon authority: persistence recovered after %s (%d dependent failures while blocked)",
		age.Round(time.Second), count-1,
	)
}
