// Command store-compat is the store-compatibility gate: a daemon must start on
// the store the previous build wrote, and the previous build must start on
// the store the candidate wrote, or an install cannot be rolled back.
//
// It builds canary-sim (the real daemon against the TWS simulator, in one
// directory of its own, no gateway and no public site) from the previous
// build (the nearest v* tag, or -previous) and from the candidate (the
// working tree, or -candidate), and runs three phases on one state
// directory:
//
//  1. the previous build writes the store and publishes the regime;
//  2. the candidate starts on it, serves the regime and runs until it has
//     published a snapshot of its own, which the gate reads off the stopped
//     store as an advanced revision of regime_snapshot.current.v1;
//  3. the previous build starts on the store the candidate wrote and serves
//     the regime again.
//
// A phase fails with the daemon's own error line. The candidate's run takes
// the regime cache's five-minute freshness; a candidate that only started
// has written nothing a rollback would meet, so the gate fails rather than
// pass on it.
//
// Found 2026-11-24: a build that changed how a regime projection is derived
// from the persisted snapshot refused every start on the live store (start:
// attach regime snapshot authority: reconcile regime snapshot projections:
// regime streak projection content mismatch), behind green unit tests; the
// fixed build wrote a snapshot field the previous build's reader refused, so
// it could not be rolled back.
//
// What it does not prove: the store is the simulator fixture's, not the
// operator's. A document only the live daemon writes is outside it.
//
// Usage: go run ./scripts/store-compat [-previous REV] [-candidate REV] [-keep]
package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/osauer/canary/v3/internal/dial"
	"github.com/osauer/canary/v3/internal/rpc"
	"github.com/osauer/canary/v3/internal/rpc/api"

	_ "modernc.org/sqlite" // reads the stopped daemon's store
)

const (
	// phaseTimeout covers the regime cache's five-minute freshness: a daemon
	// started on a fresh stored snapshot publishes its own once it lapses.
	phaseTimeout = 8 * time.Minute
	stopTimeout  = 30 * time.Second
	// gateTimeout bounds the whole gate, the two builds included.
	gateTimeout = 25 * time.Minute
	// storePath is the daemon's store inside a canary-sim stack.
	storePath = "state/ibkr/daemon.db"
	// simulatorPkg is the simulator's package, laid over a revision's tree.
	simulatorPkg = "pkg/ibkr/twssim"
	// simulatedCalendar is the calendar's simulated sessions, which only
	// canary-sim installs; laid over a revision's tree with the simulator.
	simulatedCalendar = "internal/marketcal/simulated.go"
)

func main() {
	previous := flag.String("previous", "", "the previous build's revision (default: the nearest v* tag of HEAD; before an install, the running build's commit)")
	candidate := flag.String("candidate", "", "the candidate's revision (default: the working tree)")
	keep := flag.Bool("keep", false, "keep the scratch directory")
	flag.Parse()
	// A gate that is cut short leaves no daemon running and no scratch
	// directory behind.
	abandon := func(why string) {
		if p := running.Load(); p != nil {
			_ = p.Kill()
		}
		if root := scratch.Load(); root != nil && !*keep {
			_ = os.RemoveAll(*root)
		}
		fmt.Fprintln(os.Stderr, "store-compat: FAIL:", why)
		os.Exit(1)
	}
	time.AfterFunc(gateTimeout, func() { abandon(fmt.Sprintf("the gate ran past %s", gateTimeout)) })
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() { abandon(fmt.Sprintf("stopped by %s", <-signals)) }()
	if err := run(*previous, *candidate, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "store-compat: FAIL:", err)
		os.Exit(1)
	}
}

// running is the daemon a phase has started and scratch the gate's
// directory; a gate cut short kills the one and removes the other.
var (
	running atomic.Pointer[os.Process]
	scratch atomic.Pointer[string]
)

func run(previous, candidate string, keep bool) error {
	if previous == "" {
		// The previous release is the nearest tag before the candidate: at a
		// commit that carries a tag itself, the one before it, or the gate
		// would run a build against itself.
		from := "HEAD"
		if candidate != "" {
			from = candidate
		}
		if exec.Command("git", "describe", "--tags", "--exact-match", "--match", "v[0-9]*", from).Run() == nil {
			from += "^"
		}
		out, err := exec.Command("git", "describe", "--tags", "--abbrev=0", "--match", "v[0-9]*", from).Output()
		if err != nil {
			return fmt.Errorf("find the previous release tag: %w", err)
		}
		previous = strings.TrimSpace(string(out))
	}
	same, err := sameCommit(previous, candidate)
	if err != nil {
		return err
	}
	if same {
		return fmt.Errorf("previous %s and candidate %s are one commit: the gate would run a build against itself", previous, candidate)
	}
	// /tmp, not TMPDIR: a Unix socket path is limited to about 100 bytes.
	root, err := os.MkdirTemp("/tmp", "sc-")
	if err != nil {
		return err
	}
	scratch.Store(&root)
	if !keep {
		defer os.RemoveAll(root)
	}
	prevBin, err := build(previous, filepath.Join(root, "prev"))
	if err != nil {
		return fmt.Errorf("build %s: %w", previous, err)
	}
	candLabel := "the working tree"
	if candidate != "" {
		candLabel = candidate
	}
	candBin, err := build(candidate, filepath.Join(root, "cand"))
	if err != nil {
		return fmt.Errorf("build %s: %w", candLabel, err)
	}
	stack := filepath.Join(root, "s")
	book := filepath.Join(root, "book.json")
	if err := os.WriteFile(book, []byte(fixtureBook), 0o600); err != nil {
		return err
	}
	fmt.Printf("store-compat: previous %s, candidate %s\n", previous, candLabel)

	_, err = phase(prevBin, stack, book, "previous writes the store", true, true)
	if err != nil {
		return fmt.Errorf("previous writes the store (%s): %w", previous, err)
	}
	written, err := snapshotRevision(stack)
	if err != nil {
		return fmt.Errorf("previous writes the store (%s): %w", previous, err)
	}
	if _, err := phase(candBin, stack, book, "candidate starts on it", true, false); err != nil {
		return fmt.Errorf("candidate (%s) on the store %s wrote: %w", candLabel, previous, err)
	}
	rewritten, err := snapshotRevision(stack)
	if err != nil {
		return fmt.Errorf("candidate (%s): %w", candLabel, err)
	}
	if rewritten <= written {
		return fmt.Errorf("candidate (%s) served a regime of its own but the stored snapshot stayed at revision %d: it wrote nothing for the rollback to meet", candLabel, written)
	}
	fmt.Printf("store-compat: the candidate took the stored regime snapshot from revision %d to %d\n", written, rewritten)
	if _, err := phase(prevBin, stack, book, "previous starts on the candidate's store (rollback)", false, false); err != nil {
		return fmt.Errorf("rollback: %s on the store %s wrote: %w", previous, candLabel, err)
	}
	fmt.Println("store-compat: OK")
	return nil
}

// snapshotRevision reads the stored regime snapshot's revision from the
// stack's store. The daemon holds the store while it runs, so this is read
// between phases.
func snapshotRevision(stack string) (int64, error) {
	db, err := sql.Open("sqlite", "file:"+filepath.Join(stack, filepath.FromSlash(storePath))+"?mode=ro")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var revision int64
	if err := db.QueryRow(`SELECT revision FROM state_documents WHERE kind='regime_snapshot.current.v1'`).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read the stored regime snapshot's revision: %w", err)
	}
	return revision, nil
}

// sameCommit reports whether two revisions name one commit; the working
// tree (an empty candidate) is no commit.
func sameCommit(previous, candidate string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	out, err := exec.Command("git", "rev-parse", previous+"^{commit}", candidate+"^{commit}").Output()
	if err != nil {
		return false, fmt.Errorf("resolve %s and %s: %w", previous, candidate, err)
	}
	revs := strings.Fields(string(out))
	return len(revs) == 2 && revs[0] == revs[1], nil
}

// build compiles canary-sim at rev (the working tree when empty) into dir.
func build(rev, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "canary-sim")
	src := "."
	if rev != "" {
		src = filepath.Join(dir, "src")
		if err := os.MkdirAll(src, 0o700); err != nil {
			return "", err
		}
		archive := exec.Command("sh", "-c", `git archive "$1" | tar -x -C "$2"`, "sh", rev, src)
		if out, err := archive.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git archive: %v: %s", err, out)
		}
		// The simulator is the gate's instrument, not the build under test:
		// every build runs against the working tree's, so a revision whose
		// simulator served stale bars at some hours still writes a store
		// with every streak. Only the daemon is the revision's own.
		sim := filepath.Join(src, filepath.FromSlash(simulatorPkg))
		if err := os.RemoveAll(sim); err != nil {
			return "", err
		}
		overlay := exec.Command("cp", "-R", filepath.FromSlash(simulatorPkg), sim)
		if out, err := overlay.CombinedOutput(); err != nil {
			return "", fmt.Errorf("overlay the simulator: %v: %s", err, out)
		}
		// The simulated calendar is the gate's instrument too. v3.47.0's
		// answered every earlier day with the open session's window, so the
		// last real close never read as settled and a build that judges the
		// HYG/SPY row on settled closes stored no streak, at any hour.
		calendar := exec.Command("cp", filepath.FromSlash(simulatedCalendar), filepath.Join(src, filepath.FromSlash(simulatedCalendar)))
		if out, err := calendar.CombinedOutput(); err != nil {
			return "", fmt.Errorf("overlay the simulated calendar: %v: %s", err, out)
		}
	}
	cmd := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "./cmd/canary-sim")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		if rev != "" {
			return "", fmt.Errorf("go build (this tree's pkg/ibkr/twssim laid over %s; a simulator API that %s's daemon does not compile against fails here): %v: %s", rev, rev, err, out)
		}
		return "", fmt.Errorf("go build: %v: %s", err, out)
	}
	return bin, nil
}

// reading is what a phase's daemon served for the HYG/SPY row.
type reading struct {
	at       time.Time
	row      string
	since    string
	band     string
	sessions int
}

// phase starts bin on the stack, waits until it serves a regime snapshot
// (one it computed itself when publish is set, so the next phase reads what
// this build wrote; one whose HYG/SPY row carries a streak when streak is
// set, so the store holds a projection for the next build to reconcile) and
// stops it.
func phase(bin, stack, book, name string, publish, streak bool) (reading, error) {
	var stderr bytes.Buffer
	cmd := exec.Command(bin, "-dir", stack, "-book", book)
	cmd.Stdout, cmd.Stderr = new(bytes.Buffer), &stderr
	if err := cmd.Start(); err != nil {
		return reading{}, err
	}
	running.Store(cmd.Process)
	defer running.Store(nil)
	// exited is closed when the process is gone; waitErr is then its exit.
	exited := make(chan struct{})
	var waitErr error
	go func() { waitErr = cmd.Wait(); close(exited) }()
	// The US session is open in every phase, whatever the hour: outside it
	// the daemon reads HYG's price as its last close, and a build that
	// measures the row on the close and one that measures it on the price
	// would store the same streak and hide their difference.
	if err := openSession(bin, stack, exited); err != nil {
		_ = cmd.Process.Kill()
		<-exited
		return reading{}, fmt.Errorf("open the simulated US session: %w: %s", err, lastLine(stderr.String()))
	}
	since := time.Time{}
	if publish {
		since = time.Now().UTC()
	}
	got, err := awaitRegime(filepath.Join(stack, "ibkr.sock"), since, streak, exited, func() error { return waitErr }, &stderr)
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-exited
	}
	if err != nil {
		return reading{}, err
	}
	fmt.Printf("store-compat: %s: serving; regime as of %s, HYG/SPY %s since %s (%d sessions)\n",
		name, got.at.Format(time.RFC3339), got.band, got.since, got.sessions)
	return got, nil
}

// openSession opens the simulated US equity session through the stack's
// control channel, as soon as the daemon's process listens on it.
func openSession(bin, stack string, exited <-chan struct{}) error {
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := exec.Command(bin, "ctl", "-dir", stack, "session", "us_equity", "open").CombinedOutput()
		if err == nil {
			return nil
		}
		select {
		case <-exited:
			return nil // awaitRegime reports the exit with the daemon's own line
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func awaitRegime(socket string, started time.Time, streak bool, exited <-chan struct{}, exitErr func() error, stderr *bytes.Buffer) (reading, error) {
	deadline := time.Now().Add(phaseTimeout)
	lastNote := time.Now()
	var last error
	for time.Now().Before(deadline) {
		select {
		case <-exited:
			return reading{}, fmt.Errorf("the daemon exited (%v): %s", exitErr(), lastLine(stderr.String()))
		default:
		}
		r, err := regime(socket)
		switch {
		case err != nil:
			last = err
		case r.at.Before(started):
			last = errors.New("serving only the stored snapshot so far")
		case streak && r.since == "":
			last = fmt.Errorf("the fixture's HYG/SPY row carries no streak yet (%s)", r.row)
		default:
			return r, nil
		}
		if time.Since(lastNote) > 30*time.Second {
			lastNote = time.Now()
			fmt.Printf("store-compat:   waiting: %v\n", last)
		}
		time.Sleep(2 * time.Second)
	}
	return reading{}, fmt.Errorf("no regime snapshot of its own within %s: %v", phaseTimeout, last)
}

func regime(socket string) (reading, error) {
	conn, err := dial.Connect(socket)
	if err != nil {
		return reading{}, err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var snap rpc.RegimeSnapshotResult
	if err := rpc.Call(ctx, conn, api.RegimeSnapshot, rpc.RegimeSnapshotParams{}, &snap); err != nil {
		return reading{}, err
	}
	hyg := snap.HYGSPYDivergence
	r := reading{at: snap.AsOf, row: fmt.Sprintf("status %q band %q error %q missing %v", hyg.Status, hyg.Band, hyg.ErrorMessage, hyg.FieldsMissing)}
	if s := hyg.Streak; s != nil {
		r.since, r.band, r.sessions = s.Since, string(s.Band), s.Sessions
	}
	return r, nil
}

// fixtureBook is the simulated account (the placeholder U1234567, invented
// figures): HYG's daily bars fall into its last close, so it sits more than
// 1% under its 50-day average while SPY sits at its high and the HYG/SPY row
// is red, the state whose streak and latch the store keeps. Its price stands
// off that close, as on any trading day: a build that measures the row on the
// close and one that measures it on the price then store different streaks,
// which a fixture with both equal would hide.
const fixtureBook = `{
  "account": "U1234567",
  "base_currency": "USD",
  "cash": {"USD": 250000},
  "positions": [
    {"con_id": 43652089, "symbol": "HYG", "sec_type": "STK", "currency": "USD", "exchange": "SMART",
     "primary_exchange": "ARCA", "quantity": 0, "avg_cost": 0, "last": 79.7, "prior_close": 80, "daily_drift": -0.0005}
  ]
}`

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
