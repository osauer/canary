package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/rpc"
)

const opportunityPolicyKind = "canary.opportunity_policy"

type opportunityPolicy struct {
	diagnostics []rpc.PolicyDiagnostic
	// Kind checks the file type: canary.opportunity_policy or the legacy ibkr.opportunity_policy alias. It grants no trading permission.
	Kind string `toml:"kind" json:"kind"`
	// SchemaVersion selects the file format. Schema 1 preserves historical omission defaults; schema 2 requires every real setting of an enabled detector.
	SchemaVersion int `toml:"schema_version" json:"schema_version"`
	// PolicyID is the required identity string for this policy (embedded default "opportunity-option-exercise-mvp").
	PolicyID string `toml:"policy_id" json:"policy_id"`
	// PolicyVersion records the owner revision; raise it for material edits. Names, comments and revision-only bumps do not change live action identity.
	PolicyVersion int `toml:"policy_version" json:"policy_version"`
	// Profile is a legacy display label only; it selects no preset and has no operational effect. New files omit it.
	Profile string `toml:"profile" json:"profile"`

	Authority opportunityPolicyAuthority `toml:"authority" json:"authority"`
	Buckets   opportunityPolicyBuckets   `toml:"buckets" json:"buckets"`
}

type opportunityPolicyAuthority struct {
	// ExerciseReduceOnly is retired and ignored. Code always requires exercise to close or reduce the underlying position; this field cannot widen that scope.
	ExerciseReduceOnly bool `toml:"exercise_reduce_only" json:"exercise_reduce_only"`
	// AutoSubmit is unsupported and must be false. Every exercise requires explicit confirmation through the existing gated order path. New files omit this key.
	AutoSubmit bool `toml:"auto_submit" json:"auto_submit"`
}

type opportunityPolicyBuckets struct {
	OptionExercise opportunityOptionExercisePolicy `toml:"option_exercise" json:"option_exercise"`
}

type opportunityOptionExercisePolicy struct {
	// Enabled runs the option-exercise detector (Canary default true). Detection produces candidates, never automatic submission. Set false to disable this detector.
	Enabled bool `toml:"enabled" json:"enabled"`
	// MinTotalGain is the minimum gross gain for the full candidate quantity in the contract currency (Canary default 25). Gain is intrinsic value minus option bid value, before fees, slippage and funding. Both gain thresholds must be met, including equality.
	MinTotalGain float64 `toml:"min_total_gain" json:"min_total_gain"`
	// MinGainPctIntrinsic is the minimum gross gain as a percentage of intrinsic value (Canary default 0.5). It must be met together with min_total_gain.
	MinGainPctIntrinsic float64 `toml:"min_gain_pct_intrinsic" json:"min_gain_pct_intrinsic"`
	// RequireRTH blocks exercise eligibility outside the US regular trading session when true (Canary default true). A blocked candidate may remain visible.
	RequireRTH bool `toml:"require_rth" json:"require_rth"`
	// MaxQuoteAge is the maximum age of option quote evidence and a dated underlying quote, written as a positive duration (Canary default 30s). Missing or stale option evidence blocks eligibility; an undated underlying quote has no age check. This key does not prove executable liquidity.
	MaxQuoteAge string `toml:"max_quote_age" json:"max_quote_age"`
	// AllowNoOptionBid is retired and ignored. An absent or negative option bid prevents a candidate; a zero bid is accepted. New files omit this key.
	AllowNoOptionBid bool `toml:"allow_no_option_bid" json:"allow_no_option_bid"`
	// RequireAmericanStyle requires the current USD stock/ETF heuristic when true (Canary default true). This is not verified contract-style evidence; failing the heuristic blocks eligibility.
	RequireAmericanStyle bool `toml:"require_american_style" json:"require_american_style"`
}

type opportunityPolicyManager struct {
	mu              sync.Mutex
	path            string
	hotReload       bool
	reloadInterval  time.Duration
	now             func() time.Time
	active          opportunityPolicy
	status          rpc.OpportunityPolicyStatus
	lastFingerprint rpc.Fingerprint
	// fileAdopted says the policy in force came from a valid read of the
	// file; until then the next valid file is adopted whatever its version,
	// so a file repaired after a broken start is not held back as drift.
	fileAdopted bool
}

func (s *Server) installOpportunityPolicyManager() {
	if s == nil || s.cfg == nil {
		return
	}
	cfg := s.cfg.Opportunities.WithDefaults()
	pm := newOpportunityPolicyManager(cfg.PolicyFile, cfg.HotReloadEnabled(), cfg.ReloadIntervalDuration(), s.now)
	pm.reload()
	s.opportunityPolicies = pm
}

func newOpportunityPolicyManager(path string, hotReload bool, interval time.Duration, now func() time.Time) *opportunityPolicyManager {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &opportunityPolicyManager{
		path:           expandUserPath(strings.TrimSpace(path)),
		hotReload:      hotReload,
		reloadInterval: interval,
		now:            now,
	}
}

func (m *opportunityPolicyManager) Run(ctx context.Context, logf func(string, ...any)) {
	if m == nil || !m.hotReload {
		return
	}
	t := time.NewTicker(m.reloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			before := m.Status()
			m.reload()
			after := m.Status()
			if logf != nil && before.Status != after.Status {
				logf("opportunity policy status changed: %s -> %s", before.Status, after.Status)
			}
		}
	}
}

func (m *opportunityPolicyManager) Active() (opportunityPolicy, rpc.OpportunityPolicyStatus) {
	if m == nil {
		p := defaultOpportunityPolicy()
		return p, opportunityPolicyStatus(p, rpc.OpportunityPolicyStatusDefault, "", "", time.Now().UTC())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, m.status
}

func (m *opportunityPolicyManager) Status() rpc.OpportunityPolicyStatus {
	_, st := m.Active()
	return st
}

func (m *opportunityPolicyManager) reload() {
	if m == nil {
		return
	}
	now := time.Now().UTC()
	if m.now != nil {
		now = m.now().UTC()
	}
	policy, source, review, err := m.loadPolicy()
	if err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active.PolicyID == "" {
			m.active = defaultOpportunityPolicy()
			m.lastFingerprint = fingerprintOpportunityPolicy(m.active)
		}
		st := opportunityPolicyStatus(m.active, rpc.OpportunityPolicyStatusError, source, err.Error(), now)
		st.Path = m.path
		st.Review = m.status.Review
		m.status = st
		return
	}
	fp := fingerprintOpportunityPolicy(policy)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.PolicyID == "" || !m.fileAdopted {
		m.active = policy
		statusKind := rpc.OpportunityPolicyStatusActive
		if source == "embedded-default" {
			statusKind = rpc.OpportunityPolicyStatusDefault
		}
		st := opportunityPolicyStatus(policy, statusKind, source, "", now)
		st.Path = m.path
		st.Review = review
		m.status = st
		m.lastFingerprint = fp
		m.fileAdopted = source == "file"
		return
	}

	switch {
	case source != "file":
		st := opportunityPolicyStatus(m.active, rpc.OpportunityPolicyStatusDrift, "file", "policy file removed; last loaded settings stay in force", now)
		st.Path, st.Review = m.path, m.status.Review
		m.status = st
	case policy.PolicyVersion > m.active.PolicyVersion:
		m.active = policy
		st := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusActive, source, "", now)
		st.Path = m.path
		st.Review = review
		m.status = st
		m.lastFingerprint = fp
	case policy.PolicyVersion == m.active.PolicyVersion && sameEffectivePolicy(effectiveOpportunityPolicy(policy), effectiveOpportunityPolicy(m.active)):
		m.active, m.lastFingerprint = policy, fp
		st := opportunityPolicyStatus(m.active, m.status.Status, source, "", now)
		if st.Status == "" || st.Status == rpc.OpportunityPolicyStatusDrift || st.Status == rpc.OpportunityPolicyStatusError {
			st.Status = rpc.OpportunityPolicyStatusActive
		}
		st.Path = m.path
		st.Review = review
		m.status = st
	case policy.PolicyVersion <= m.active.PolicyVersion && fp.Key != m.lastFingerprint.Key:
		st := opportunityPolicyStatus(m.active, rpc.OpportunityPolicyStatusDrift, source, "policy file changed without a higher policy_version", now)
		st.Path = m.path
		m.status = st
	}
}

func (m *opportunityPolicyManager) loadPolicy() (opportunityPolicy, string, string, error) {
	if m == nil || strings.TrimSpace(m.path) == "" {
		p := defaultOpportunityPolicy()
		return p, "embedded-default", "", validateOpportunityPolicy(p)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p := defaultOpportunityPolicy()
			return p, "embedded-default", "", validateOpportunityPolicy(p)
		}
		return opportunityPolicy{}, "file", "", fmt.Errorf("read opportunity policy %s: %w", m.path, err)
	}
	p, err := parseOpportunityPolicy(data)
	if err != nil {
		return opportunityPolicy{}, "file", "", fmt.Errorf("opportunity policy %s: %w", m.path, err)
	}
	return p, "file", policyFileReview(data), nil
}

// parseOpportunityPolicy decodes and validates an opportunity policy file,
// refusing unknown keys.
func parseOpportunityPolicy(data []byte) (opportunityPolicy, error) {
	var p opportunityPolicy
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return opportunityPolicy{}, fmt.Errorf("parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return opportunityPolicy{}, fmt.Errorf("unknown opportunity policy key(s): %s", strings.Join(keys, ", "))
	}
	if err := opportunityPolicyPresence(&p, md); err != nil {
		return opportunityPolicy{}, err
	}
	applyOpportunityPolicyDefaults(&p, &md)
	if err := validateOpportunityPolicy(p); err != nil {
		return opportunityPolicy{}, err
	}
	return p, nil
}

func defaultOpportunityPolicy() opportunityPolicy {
	return opportunityPolicy{
		Kind:          opportunityPolicyKind,
		SchemaVersion: 2,
		PolicyID:      "opportunity-option-exercise-mvp",
		PolicyVersion: 1,
		Profile:       "conservative-exercise-mvp",
		Authority: opportunityPolicyAuthority{
			ExerciseReduceOnly: false,
			AutoSubmit:         false,
		},
		Buckets: opportunityPolicyBuckets{
			OptionExercise: opportunityOptionExercisePolicy{
				Enabled:              true,
				MinTotalGain:         25.0,
				MinGainPctIntrinsic:  0.5,
				RequireRTH:           true,
				MaxQuoteAge:          "30s",
				AllowNoOptionBid:     false,
				RequireAmericanStyle: true,
			},
		},
	}
}

func applyOpportunityPolicyDefaults(p *opportunityPolicy, md *toml.MetaData) {
	if p == nil {
		return
	}
	if p.Kind == "" {
		p.Kind = opportunityPolicyKind
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if p.Profile == "" {
		p.Profile = p.PolicyID
	}
	if p.SchemaVersion == 2 {
		return
	}
	defaults := defaultOpportunityPolicy()
	if md != nil && !md.IsDefined("buckets", "option_exercise") {
		p.Buckets.OptionExercise = defaults.Buckets.OptionExercise
	}
	if p.Buckets.OptionExercise.MaxQuoteAge == "" {
		p.Buckets.OptionExercise.MaxQuoteAge = defaults.Buckets.OptionExercise.MaxQuoteAge
	}
}

func validateOpportunityPolicy(p opportunityPolicy) error {
	if p.Kind != opportunityPolicyKind && p.Kind != "ibkr.opportunity_policy" {
		return fmt.Errorf("opportunity policy kind %q is invalid", p.Kind)
	}
	if p.SchemaVersion != 1 && p.SchemaVersion != 2 {
		return fmt.Errorf("opportunity policy schema_version %d is unsupported", p.SchemaVersion)
	}
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("opportunity policy policy_id is required")
	}
	if p.PolicyVersion <= 0 {
		return fmt.Errorf("opportunity policy policy_version must be positive")
	}
	if p.Authority.AutoSubmit {
		return fmt.Errorf("opportunity policy authority.auto_submit is unsupported; every exercise needs explicit confirmation")
	}
	if math.IsNaN(p.Buckets.OptionExercise.MinTotalGain) || math.IsInf(p.Buckets.OptionExercise.MinTotalGain, 0) || math.IsNaN(p.Buckets.OptionExercise.MinGainPctIntrinsic) || math.IsInf(p.Buckets.OptionExercise.MinGainPctIntrinsic, 0) {
		return fmt.Errorf("buckets.option_exercise gain thresholds must be finite")
	}
	if p.Buckets.OptionExercise.Enabled {
		if p.Buckets.OptionExercise.MinTotalGain < 0 {
			return fmt.Errorf("option_exercise.min_total_gain must be non-negative")
		}
		if p.Buckets.OptionExercise.MinGainPctIntrinsic < 0 {
			return fmt.Errorf("option_exercise.min_gain_pct_intrinsic must be non-negative")
		}
		if _, err := p.Buckets.OptionExercise.maxQuoteAgeDuration(); err != nil {
			return err
		}
	}
	return nil
}

func (p opportunityOptionExercisePolicy) maxQuoteAgeDuration() (time.Duration, error) {
	raw := strings.TrimSpace(p.MaxQuoteAge)
	if raw == "" {
		raw = defaultOpportunityPolicy().Buckets.OptionExercise.MaxQuoteAge
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("option_exercise.max_quote_age %q is invalid: %w", p.MaxQuoteAge, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("option_exercise.max_quote_age must be positive")
	}
	return d, nil
}

func opportunityPolicyStatus(p opportunityPolicy, status, source, message string, at time.Time) rpc.OpportunityPolicyStatus {
	fp := fingerprintOpportunityPolicy(p)
	st := rpc.OpportunityPolicyStatus{
		Kind:                 opportunityPolicyKind,
		Status:               status,
		PolicyID:             p.PolicyID,
		PolicyVersion:        p.PolicyVersion,
		Profile:              p.Profile,
		Fingerprint:          fp,
		EffectiveFingerprint: effectiveOpportunityPolicy(p),
		Diagnostics:          p.diagnostics,
		Source:               source,
		LoadedAt:             at,
		LastCheckedAt:        at,
		Message:              message,
	}
	if status == rpc.OpportunityPolicyStatusDrift || status == rpc.OpportunityPolicyStatusError {
		st.Blockers = []rpc.TradingBlocker{{
			Code:    "opportunity_policy_" + status,
			Message: nonEmptyString(message, "opportunity policy is not safe for exercise preview or submit"),
			Action:  "Fix the opportunity policy file and bump policy_version before preview or submit.",
		}}
	}
	if st.Message == "" {
		st.Message = policyDiagnosticsMessage(p.diagnostics)
	}
	return st
}

func fingerprintOpportunityPolicy(p opportunityPolicy) rpc.Fingerprint {
	normalized := struct {
		Kind          string                     `json:"kind"`
		SchemaVersion int                        `json:"schema_version"`
		PolicyID      string                     `json:"policy_id"`
		PolicyVersion int                        `json:"policy_version"`
		Profile       string                     `json:"profile"`
		Authority     opportunityPolicyAuthority `json:"authority"`
		Buckets       opportunityPolicyBuckets   `json:"buckets"`
	}{
		Kind:          strings.TrimSpace(p.Kind),
		SchemaVersion: p.SchemaVersion,
		PolicyID:      strings.TrimSpace(p.PolicyID),
		PolicyVersion: p.PolicyVersion,
		Profile:       strings.TrimSpace(p.Profile),
		Authority:     p.Authority,
		Buckets:       p.Buckets,
	}
	raw, _ := json.Marshal(normalized)
	sum := sha256.Sum256(raw)
	return rpc.Fingerprint{Version: rpc.OpportunityPolicyFingerprintVersion, Key: "sha256:" + hex.EncodeToString(sum[:])}
}
