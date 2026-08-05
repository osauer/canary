package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

// gammaSkewDiagJournal retains its path codec only for legacy format tests.
// Once daemon authority is attached, beta calibration observations are no
// longer appended because no production reader consumes them.
//
// Lifecycle mirrors gammaZeroStore.Save: appended only on the
// successful, non-cancelled persist path in spawnJob; runtime attachment makes
// append a no-op and can never fail a compute.
type gammaSkewDiagJournal struct {
	// path is retained solely for explicit legacy-import and isolated
	// file-format tests. Production attaches the daemon authority before
	// the cache can run; once attached, append never reads or writes path.
	path      string
	authority *corestore.Store
}

// UseCoreStore switches the journal to daemon.db. There is deliberately no
// file fallback after attachment: a database error must remain visible to the
// daemon's authority-health latch instead of silently splitting history.
func (j *gammaSkewDiagJournal) UseCoreStore(store *corestore.Store) error {
	if j == nil {
		return errors.New("gamma skew diagnostics: nil journal")
	}
	if store == nil {
		return errors.New("gamma skew diagnostics: nil corestore")
	}
	j.authority = store
	return nil
}

// gammaSkewDiagDefaultPath resolves the journal's on-disk location in
// the same private state dir as the order journal and proposal
// outcomes ($XDG_STATE_HOME/ibkr/, default ~/.local/state/ibkr/).
func gammaSkewDiagDefaultPath() (string, error) {
	return defaultTradingStatePath("gamma-skew-diagnostics.jsonl")
}

// gammaSkewDiagLine is the v1 journal record. One line per slice: the
// combined node plus each per-index sub, so SPX and SPY fit
// distributions can be analysed separately. Rankability fields are
// computed on an annotated clone at append time — the served result is
// annotated lazily at serve time and must not be mutated here.
type gammaSkewDiagLine struct {
	V             int                        `json:"v"`
	TS            time.Time                  `json:"ts"`
	SessionKey    string                     `json:"session_key"`
	Session       string                     `json:"session"`
	Scope         string                     `json:"scope"`
	Slice         string                     `json:"slice"`
	AsOf          time.Time                  `json:"as_of"`
	MedianR2      float64                    `json:"median_r2"`
	MinR2         float64                    `json:"min_r2"`
	FitExpiries   int                        `json:"fit_expiries"`
	Expiries      map[string]rpc.SkewFitInfo `json:"expiries,omitempty"`
	PricedLegs    int                        `json:"priced_legs"`
	GEXLegs       int                        `json:"gex_legs"`
	OIObservedPct float64                    `json:"oi_observed_pct"`
	DerivedIVPct  float64                    `json:"derived_iv_pct"`
	Rankability   string                     `json:"rankability"`
	Reason        string                     `json:"reason,omitempty"`
	GammaSign     string                     `json:"gamma_sign,omitempty"`
	ZeroGamma     *float64                   `json:"zero_gamma,omitempty"`
	Warnings      []string                   `json:"warnings,omitempty"`
}

// append journals the slices of one successful compute. The whole
// batch is marshalled into a single buffer and issued as one Write on
// an O_APPEND descriptor so concurrent scope jobs cannot interleave
// partial lines.
func (j *gammaSkewDiagJournal) append(now time.Time, scope, sessionKey string, result *rpc.GammaZeroComputed) error {
	if j == nil || result == nil {
		return nil
	}
	// Quality is annotated lazily on serve-time clones; annotate a
	// clone here too. Annotating the raw combined result would find
	// nil sub-slice Quality and journal every combined line as
	// "blocked: SPX quality missing", silently poisoning the
	// calibration set.
	clone := cloneGammaComputed(result)
	annotateGammaQuality(clone, now)
	lines := gammaSkewDiagLines(now, scope, sessionKey, clone)
	if len(lines) == 0 {
		return nil
	}
	if j.authority != nil {
		return nil
	}
	return j.appendLegacy(lines)
}

const (
	gammaSkewDiagVersion         = 1
	gammaSkewDiagObservationKind = "gamma_skew_diagnostic.v1"
)

func gammaSkewDiagScopeKey(scope, slice string) string {
	return "market/gamma/skew/" + scope + "/" + slice
}

// appendLegacy preserves the old JSONL codec for the one-shot cutover
// importer and format tests. Runtime code must attach corestore first.
func (j *gammaSkewDiagJournal) appendLegacy(lines []gammaSkewDiagLine) error {
	var buf []byte
	for _, line := range lines {
		b, err := json.Marshal(line)
		if err != nil {
			return fmt.Errorf("encode skew diagnostics: %w", err)
		}
		buf = append(buf, b...)
		buf = append(buf, '\n')
	}
	if err := ensurePrivateStateDir(j.path); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", j.path, err)
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		return fmt.Errorf("append %s: %w", j.path, err)
	}
	return f.Close()
}

func gammaSkewDiagLines(now time.Time, scope, sessionKey string, c *rpc.GammaZeroComputed) []gammaSkewDiagLine {
	if c == nil {
		return nil
	}
	lines := []gammaSkewDiagLine{gammaSkewDiagLineFor(now, scope, sessionKey, gammaQualityScope(c), c)}
	for _, key := range []string{"SPX", "SPY"} {
		if sub := c.PerIndex[key]; sub != nil {
			lines = append(lines, gammaSkewDiagLineFor(now, scope, sessionKey, key, sub))
		}
	}
	return lines
}

func gammaSkewDiagLineFor(now time.Time, scope, sessionKey, slice string, c *rpc.GammaZeroComputed) gammaSkewDiagLine {
	line := gammaSkewDiagLine{
		V:          gammaSkewDiagVersion,
		TS:         now,
		SessionKey: sessionKey,
		Scope:      scope,
		Slice:      slice,
		AsOf:       c.AsOf,
		Expiries:   c.SkewFitQuality,
		GammaSign:  c.GammaSign,
		ZeroGamma:  c.ZeroGamma,
		Warnings:   c.Warnings,
	}
	if q := c.Quality; q != nil {
		line.Session = q.Session
		line.MedianR2 = q.Coverage.MedianSkewRSquared
		line.MinR2 = q.Coverage.MinSkewRSquared
		line.FitExpiries = q.Coverage.SkewFitExpiries
		line.PricedLegs = q.Coverage.PricedLegs
		line.GEXLegs = q.Coverage.GEXLegs
		line.OIObservedPct = q.Coverage.OIObservedPct
		line.DerivedIVPct = q.Coverage.DerivedIVPct
		line.Rankability = q.Rankability
		line.Reason = q.RankabilityReason
	}
	return line
}
