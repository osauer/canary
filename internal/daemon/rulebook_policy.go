package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Rulebook policy sources: the compiled baseline, or the owner's file.
const (
	rulebookPolicySourceDefault = "compiled-default"
	rulebookPolicySourceFile    = "file"
)

// rulebookPolicyManager owns the Rulebook policy in force. The owner's file
// overrides any subset of the compiled baseline; absent keys keep it. Like the
// protection policy, an edit takes effect only with a higher policy_version,
// and an unreadable or invalid file never replaces the policy in force: the
// status says what happened and the previous thresholds keep evaluating.
type rulebookPolicyManager struct {
	mu             sync.Mutex
	path           string
	reloadInterval time.Duration
	now            func() time.Time
	active         risk.RulebookPolicy
	status         rpc.RulebookPolicyStatus
	loaded         bool
	// onTransition journals status changes; the daemon wires it to the
	// governance event log.
	onTransition func(prev, next rpc.RulebookPolicyStatus)
}

func (s *Server) installRulebookPolicyManager() {
	if s == nil {
		return
	}
	path := config.DefaultRulebookPolicyFile
	if s.cfg != nil {
		path = s.cfg.Rulebook.PolicyFilePath()
	}
	m := newRulebookPolicyManager(path, 30*time.Second, s.now)
	m.onTransition = s.journalRulebookPolicyTransition
	m.reload()
	s.rulebookPolicies = m
}

func newRulebookPolicyManager(path string, interval time.Duration, now func() time.Time) *rulebookPolicyManager {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &rulebookPolicyManager{path: expandUserPath(strings.TrimSpace(path)), reloadInterval: interval, now: now}
}

// rulebookPolicy returns the Rulebook policy in force. A daemon without the
// manager (tests, tools) runs the compiled baseline.
func (s *Server) rulebookPolicy() risk.RulebookPolicy {
	if s == nil || s.rulebookPolicies == nil {
		return risk.DefaultRulebookPolicy()
	}
	p, _ := s.rulebookPolicies.Active()
	return p
}

// rulebookPolicy returns the Rulebook policy in force for the engine's
// server, or the compiled baseline for an engine built without one.
func (e *proposalEngine) rulebookPolicy() risk.RulebookPolicy {
	if e == nil {
		return risk.DefaultRulebookPolicy()
	}
	return e.server.rulebookPolicy()
}

// activeRulebookPolicy returns the policy in force and its status from one
// read, so a result never pairs thresholds with another policy's status.
func (s *Server) activeRulebookPolicy() (risk.RulebookPolicy, rpc.RulebookPolicyStatus) {
	if s == nil || s.rulebookPolicies == nil {
		p := risk.DefaultRulebookPolicy()
		return p, rulebookPolicyStatusFor(p, rpc.RulebookPolicyStatusDefault, rulebookPolicySourceDefault, "", nil, time.Time{})
	}
	return s.rulebookPolicies.Active()
}

func (m *rulebookPolicyManager) Run(ctx context.Context, logf func(string, ...any)) {
	if m == nil {
		return
	}
	t := time.NewTicker(m.reloadInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, before := m.Active()
			m.reload()
			_, after := m.Active()
			if logf != nil && (before.Status != after.Status || before.Fingerprint.Key != after.Fingerprint.Key) {
				logf("rulebook policy %s -> %s (%s v%d)", before.Status, after.Status, after.PolicyID, after.PolicyVersion)
			}
		}
	}
}

// Active returns the policy in force and its status. Both are copies.
func (m *rulebookPolicyManager) Active() (risk.RulebookPolicy, rpc.RulebookPolicyStatus) {
	if m == nil {
		p := risk.DefaultRulebookPolicy()
		return p, rulebookPolicyStatusFor(p, rpc.RulebookPolicyStatusDefault, rulebookPolicySourceDefault, "", nil, time.Time{})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.active
	p.Modes = maps.Clone(m.active.Modes)
	p.HedgeSymbols = slices.Clone(m.active.HedgeSymbols)
	st := m.status
	st.Overrides = slices.Clone(m.status.Overrides)
	return p, st
}

func (m *rulebookPolicyManager) reload() {
	if m == nil {
		return
	}
	now := time.Now().UTC()
	if m.now != nil {
		now = m.now().UTC()
	}
	policy, source, overrides, err := loadRulebookPolicyFile(m.path)

	m.mu.Lock()
	prev := m.status
	switch {
	case err != nil:
		if !m.loaded {
			m.active, m.loaded = risk.DefaultRulebookPolicy(), true
			prev = rpc.RulebookPolicyStatus{}
		}
		m.status = rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusError, m.status.Source, m.path, m.status.Overrides, now)
		m.status.Message = err.Error() + "; the previous policy stays in force"
		if m.status.Source == "" {
			m.status.Source = rulebookPolicySourceDefault
		}
	case !m.loaded:
		m.adopt(policy, source, overrides, now)
		prev = rpc.RulebookPolicyStatus{}
	case source == rulebookPolicySourceDefault && m.status.Source == rulebookPolicySourceFile:
		// A removed file does not silently loosen or tighten the limits in
		// force: the owner restarts the daemon to return to the baseline.
		m.status = rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusDrift, rulebookPolicySourceFile, m.path, m.status.Overrides, now)
		m.status.Message = "the policy file was removed; restart the daemon to return to the compiled baseline"
	case source == rulebookPolicySourceFile && m.status.Source == rulebookPolicySourceDefault,
		policy.Version > m.active.Version:
		// The first file replaces the baseline whatever its version; later
		// edits need a higher policy_version.
		m.adopt(policy, source, overrides, now)
	case policy.FingerprintKey() == m.active.FingerprintKey():
		st := rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusActive, source, m.path, m.status.Overrides, now)
		if source == rulebookPolicySourceDefault {
			st.Status = rpc.RulebookPolicyStatusDefault
		}
		st.LoadedAt = m.status.LoadedAt
		m.status = st
	default:
		m.status = rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusDrift, m.status.Source, m.path, m.status.Overrides, now)
		m.status.Message = fmt.Sprintf("the policy file changed without a higher policy_version (in force: v%d); raise policy_version to apply it", m.active.Version)
	}
	next := m.status
	notify := m.onTransition
	m.mu.Unlock()

	if notify != nil && (prev.Status != next.Status || prev.Fingerprint.Key != next.Fingerprint.Key) {
		notify(prev, next)
	}
}

// adopt puts a validated policy in force. The caller holds m.mu.
func (m *rulebookPolicyManager) adopt(policy risk.RulebookPolicy, source string, overrides []string, now time.Time) {
	m.active, m.loaded = policy, true
	status := rpc.RulebookPolicyStatusActive
	if source == rulebookPolicySourceDefault {
		status = rpc.RulebookPolicyStatusDefault
	}
	m.status = rulebookPolicyStatusFor(policy, status, source, m.path, overrides, now)
	m.status.LoadedAt = now
}

// loadRulebookPolicyFile reads the owner's file over the compiled baseline.
// An absent file is the baseline itself, not an error.
func loadRulebookPolicyFile(path string) (risk.RulebookPolicy, string, []string, error) {
	base := risk.DefaultRulebookPolicy()
	if strings.TrimSpace(path) == "" {
		return base, rulebookPolicySourceDefault, nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, rulebookPolicySourceDefault, nil, nil
	}
	if err != nil {
		return risk.RulebookPolicy{}, rulebookPolicySourceFile, nil, fmt.Errorf("read rulebook policy %s: %w", path, err)
	}
	policy, overrides, err := parseRulebookPolicy(data)
	if err != nil {
		return risk.RulebookPolicy{}, rulebookPolicySourceFile, nil, fmt.Errorf("rulebook policy %s: %w", path, err)
	}
	return policy, rulebookPolicySourceFile, overrides, nil
}

// parseRulebookPolicy decodes a policy file over the compiled baseline and
// lists the threshold and mode keys it sets. Unknown keys are refused so a
// misspelt limit cannot silently keep the baseline.
func parseRulebookPolicy(data []byte) (risk.RulebookPolicy, []string, error) {
	policy := risk.DefaultRulebookPolicy()
	md, err := toml.Decode(string(data), &policy)
	if err != nil {
		return risk.RulebookPolicy{}, nil, fmt.Errorf("parse: %w", err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return risk.RulebookPolicy{}, nil, fmt.Errorf("unknown key(s): %s", strings.Join(keys, ", "))
	}
	policy.Normalize()
	if err := policy.Validate(); err != nil {
		return risk.RulebookPolicy{}, nil, err
	}
	var overrides []string
	for _, key := range md.Keys() {
		name := key.String()
		switch name {
		case "kind", "schema_version", "policy_id", "policy_version":
			continue
		}
		if md.Type(key...) == "Hash" {
			continue
		}
		overrides = append(overrides, name)
	}
	slices.Sort(overrides)
	return policy, overrides, nil
}

func rulebookPolicyStatusFor(p risk.RulebookPolicy, status, source, path string, overrides []string, now time.Time) rpc.RulebookPolicyStatus {
	return rpc.RulebookPolicyStatus{
		Status: status, Source: source, Path: path,
		PolicyID: p.ID, PolicyVersion: p.Version,
		Fingerprint: rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: p.FingerprintKey()},
		Overrides:   slices.Clone(overrides), CheckedAt: now,
	}
}

// journalRulebookPolicyTransition records which thresholds took effect, so a
// verdict can be traced to the policy that produced it after the file moves on.
func (s *Server) journalRulebookPolicyTransition(prev, next rpc.RulebookPolicyStatus) {
	if s == nil || s.riskCapital == nil {
		return
	}
	entry := map[string]any{
		"version": 1, "at": time.Now().UTC(), "kind": "rulebook_policy_status",
		"from": prev.Status, "to": next.Status, "source": next.Source,
		"policy_id": next.PolicyID, "policy_version": next.PolicyVersion,
		"fingerprint_version": next.Fingerprint.Version, "policy_fingerprint": next.Fingerprint.Key,
		"overrides": next.Overrides,
	}
	if next.Message != "" {
		entry["message"] = next.Message
	}
	_ = s.riskCapital.RecordDeskGovernanceEvent(entry)
}
