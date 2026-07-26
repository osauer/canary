package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/rpc"
)

const opportunityPolicyKind = "ibkr.opportunity_policy"

type opportunityPolicy struct {
	// Kind must be "ibkr.opportunity_policy"; any other value fails the load.
	Kind string `toml:"kind" json:"kind"`
	// SchemaVersion is the policy schema revision; only 1 is supported.
	SchemaVersion int `toml:"schema_version" json:"schema_version"`
	// PolicyID is the required identity string for this policy (embedded default "opportunity-option-exercise-mvp").
	PolicyID string `toml:"policy_id" json:"policy_id"`
	// PolicyVersion is the monotonic policy revision; bump it to make the daemon adopt file edits — an edited file at an unchanged version reports drift instead.
	PolicyVersion int `toml:"policy_version" json:"policy_version"`
	// Profile is a human-readable label for the parameter set (embedded default "conservative-exercise-mvp"); falls back to policy_id when empty.
	Profile string `toml:"profile" json:"profile"`

	Authority opportunityPolicyAuthority `toml:"authority" json:"authority"`
	Buckets   opportunityPolicyBuckets   `toml:"buckets" json:"buckets"`
}

type opportunityPolicyAuthority struct {
	// ExerciseReduceOnly is retained for schema compatibility. Option exercise
	// exposure effects are informational; broker writes are centrally gated.
	ExerciseReduceOnly bool `toml:"exercise_reduce_only" json:"exercise_reduce_only"`
	// AutoSubmit would let exercise opportunities submit themselves; must be false — opportunities are advisory and every broker write stays behind the gated order path.
	AutoSubmit bool `toml:"auto_submit" json:"auto_submit"`
}

type opportunityPolicyBuckets struct {
	OptionExercise opportunityOptionExercisePolicy `toml:"option_exercise" json:"option_exercise"`
}

type opportunityOptionExercisePolicy struct {
	// Enabled turns the early-exercise opportunity detector on (default true).
	Enabled bool `toml:"enabled" json:"enabled"`
	// MinTotalGain is the minimum total dollar gain required to flag an exercise opportunity (default 25).
	MinTotalGain float64 `toml:"min_total_gain" json:"min_total_gain"`
	// MinGainPctIntrinsic is the minimum gain as a percent of intrinsic value (default 0.5).
	MinGainPctIntrinsic float64 `toml:"min_gain_pct_intrinsic" json:"min_gain_pct_intrinsic"`
	// RequireRTH only flags opportunities during regular trading hours (default true).
	RequireRTH bool `toml:"require_rth" json:"require_rth"`
	// MaxQuoteAge is the oldest quote still considered fresh, as a Go duration string (default "30s").
	MaxQuoteAge string `toml:"max_quote_age" json:"max_quote_age"`
	// AllowNoOptionBid is retained for schema compatibility. Exercise
	// opportunities require an executable option bid in the MVP detector.
	AllowNoOptionBid bool `toml:"allow_no_option_bid" json:"allow_no_option_bid"`
	// RequireAmericanStyle limits detection to American-style options, the only style that can be exercised early (default true).
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
	policy, source, err := m.loadPolicy()
	if err != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.active.PolicyID == "" {
			m.active = defaultOpportunityPolicy()
		}
		st := opportunityPolicyStatus(m.active, rpc.OpportunityPolicyStatusError, source, err.Error(), now)
		st.Path = m.path
		m.status = st
		return
	}
	fp := fingerprintOpportunityPolicy(policy)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.PolicyID == "" {
		m.active = policy
		statusKind := rpc.OpportunityPolicyStatusActive
		if source == "embedded-default" {
			statusKind = rpc.OpportunityPolicyStatusDefault
		}
		st := opportunityPolicyStatus(policy, statusKind, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
		return
	}

	switch {
	case policy.PolicyVersion > m.active.PolicyVersion:
		m.active = policy
		st := opportunityPolicyStatus(policy, rpc.OpportunityPolicyStatusActive, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
	case policy.PolicyVersion == m.active.PolicyVersion && fp.Key == m.lastFingerprint.Key:
		st := opportunityPolicyStatus(m.active, m.status.Status, source, "", now)
		if st.Status == "" || st.Status == rpc.OpportunityPolicyStatusDrift || st.Status == rpc.OpportunityPolicyStatusError {
			st.Status = rpc.OpportunityPolicyStatusActive
		}
		st.Path = m.path
		m.status = st
	case policy.PolicyVersion <= m.active.PolicyVersion && fp.Key != m.lastFingerprint.Key:
		st := opportunityPolicyStatus(m.active, rpc.OpportunityPolicyStatusDrift, source, "policy file changed without a higher policy_version", now)
		st.Path = m.path
		m.status = st
	}
}

func (m *opportunityPolicyManager) loadPolicy() (opportunityPolicy, string, error) {
	if m == nil || strings.TrimSpace(m.path) == "" {
		p := defaultOpportunityPolicy()
		return p, "embedded-default", validateOpportunityPolicy(p)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p := defaultOpportunityPolicy()
			return p, "embedded-default", validateOpportunityPolicy(p)
		}
		return opportunityPolicy{}, "file", fmt.Errorf("read opportunity policy %s: %w", m.path, err)
	}
	var p opportunityPolicy
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return opportunityPolicy{}, "file", fmt.Errorf("parse opportunity policy %s: %w", m.path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return opportunityPolicy{}, "file", fmt.Errorf("unknown opportunity policy key(s): %s", strings.Join(keys, ", "))
	}
	applyOpportunityPolicyDefaults(&p, &md)
	if err := validateOpportunityPolicy(p); err != nil {
		return opportunityPolicy{}, "file", err
	}
	return p, "file", nil
}

func defaultOpportunityPolicy() opportunityPolicy {
	return opportunityPolicy{
		Kind:          opportunityPolicyKind,
		SchemaVersion: 1,
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
	defaults := defaultOpportunityPolicy()
	if md != nil && !md.IsDefined("buckets", "option_exercise") {
		p.Buckets.OptionExercise = defaults.Buckets.OptionExercise
	}
	if p.Buckets.OptionExercise.MaxQuoteAge == "" {
		p.Buckets.OptionExercise.MaxQuoteAge = defaults.Buckets.OptionExercise.MaxQuoteAge
	}
}

func validateOpportunityPolicy(p opportunityPolicy) error {
	if p.Kind != opportunityPolicyKind {
		return fmt.Errorf("opportunity policy kind %q is invalid", p.Kind)
	}
	if p.SchemaVersion != 1 {
		return fmt.Errorf("opportunity policy schema_version %d is unsupported", p.SchemaVersion)
	}
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("opportunity policy policy_id is required")
	}
	if p.PolicyVersion <= 0 {
		return fmt.Errorf("opportunity policy policy_version must be positive")
	}
	if p.Authority.AutoSubmit {
		return fmt.Errorf("opportunity policy authority.auto_submit must be false in MVP")
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
		Kind:          opportunityPolicyKind,
		Status:        status,
		PolicyID:      p.PolicyID,
		PolicyVersion: p.PolicyVersion,
		Profile:       p.Profile,
		Fingerprint:   fp,
		Source:        source,
		LoadedAt:      at,
		LastCheckedAt: at,
		Message:       message,
	}
	if status == rpc.OpportunityPolicyStatusDrift || status == rpc.OpportunityPolicyStatusError {
		st.Blockers = []rpc.TradingBlocker{{
			Code:    "opportunity_policy_" + status,
			Message: nonEmptyString(message, "opportunity policy is not safe for exercise preview or submit"),
			Action:  "Fix the opportunity policy file and bump policy_version before preview or submit.",
		}}
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
