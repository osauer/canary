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
	st.Missing = slices.Clone(m.status.Missing)
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
	read, err := loadRulebookPolicyFile(m.path)

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
		m.adopt(read, now)
		prev = rpc.RulebookPolicyStatus{}
	case read.source == rulebookPolicySourceDefault && m.status.Source == rulebookPolicySourceFile:
		// A removed file does not silently loosen or tighten the limits in
		// force: the owner restarts the daemon to return to the baseline.
		m.status = rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusDrift, rulebookPolicySourceFile, m.path, m.status.Overrides, now)
		m.status.Message = "the policy file was removed; restart the daemon to return to the compiled baseline"
	case read.source == rulebookPolicySourceFile && m.status.Source == rulebookPolicySourceDefault,
		read.policy.Version > m.active.Version:
		// The first file replaces the baseline whatever its version; later
		// edits need a higher policy_version.
		m.adopt(read, now)
	case read.policy.FingerprintKey() == m.active.FingerprintKey():
		st := rulebookPolicyStatusFor(m.active, rpc.RulebookPolicyStatusActive, read.source, m.path, m.status.Overrides, now)
		if read.source == rulebookPolicySourceDefault {
			st.Status = rpc.RulebookPolicyStatusDefault
		}
		st.LoadedAt = m.status.LoadedAt
		st.Message = retiredRulebookKeysNote(read.retired)
		st.Missing, st.Review = slices.Clone(read.missing), read.review
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
func (m *rulebookPolicyManager) adopt(read rulebookPolicyRead, now time.Time) {
	m.active, m.loaded = read.policy, true
	status := rpc.RulebookPolicyStatusActive
	if read.source == rulebookPolicySourceDefault {
		status = rpc.RulebookPolicyStatusDefault
	}
	m.status = rulebookPolicyStatusFor(read.policy, status, read.source, m.path, read.overrides, now)
	m.status.LoadedAt = now
	m.status.Message = retiredRulebookKeysNote(read.retired)
	m.status.Missing, m.status.Review = slices.Clone(read.missing), read.review
}

// rulebookPolicyRead is one read of the owner's file over the compiled
// baseline: the policy, where it came from, the keys the file sets, and the
// retired keys it still carries, which no rule reads.
type rulebookPolicyRead struct {
	policy    risk.RulebookPolicy
	source    string
	overrides []string
	retired   []string
	// missing lists the keys Canary's template writes that the file lacks;
	// they follow Canary's defaults until the file carries them.
	missing []string
	// review is PolicyReviewUnreviewed while the file is Canary's template.
	review string
}

// loadRulebookPolicyFile reads the owner's file over the compiled baseline.
// An absent file is the baseline itself, not an error.
func loadRulebookPolicyFile(path string) (rulebookPolicyRead, error) {
	base := rulebookPolicyRead{policy: risk.DefaultRulebookPolicy(), source: rulebookPolicySourceDefault}
	if strings.TrimSpace(path) == "" {
		return base, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return rulebookPolicyRead{}, fmt.Errorf("read rulebook policy %s: %w", path, err)
	}
	read, err := parseRulebookPolicy(data)
	if err != nil {
		return rulebookPolicyRead{}, fmt.Errorf("rulebook policy %s: %w", path, err)
	}
	read.source = rulebookPolicySourceFile
	read.review = policyFileReview(data)
	return read, nil
}

// parseRulebookPolicy decodes a policy file over the compiled baseline and
// lists the threshold and mode keys it sets. Unknown keys are refused so a
// misspelt limit cannot silently keep the baseline. A retired key is listed
// instead: refusing it would void every limit the file sets over a key that
// changes nothing.
func parseRulebookPolicy(data []byte) (rulebookPolicyRead, error) {
	policy := risk.DefaultRulebookPolicy()
	md, err := toml.Decode(string(data), &policy)
	if err != nil {
		return rulebookPolicyRead{}, fmt.Errorf("parse: %w", err)
	}
	var unknown, retired []string
	for _, k := range md.Undecoded() {
		if name := k.String(); retiredRulebookKey(name) {
			retired = append(retired, name)
		} else {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		return rulebookPolicyRead{}, fmt.Errorf("unknown key(s): %s", strings.Join(unknown, ", "))
	}
	policy.Normalize()
	if err := policy.Validate(); err != nil {
		return rulebookPolicyRead{}, err
	}
	var overrides []string
	defined := map[string]bool{}
	for _, key := range md.Keys() {
		name := key.String()
		defined[name] = true
		switch name {
		case "kind", "schema_version", "policy_id", "policy_version":
			continue
		}
		if md.Type(key...) == "Hash" || retiredRulebookKey(name) {
			continue
		}
		overrides = append(overrides, name)
	}
	var missing []string
	for _, key := range rulebookTemplateKeySet() {
		if !defined[key] {
			missing = append(missing, key)
		}
	}
	slices.Sort(overrides)
	slices.Sort(retired)
	return rulebookPolicyRead{policy: policy, overrides: overrides, retired: retired, missing: missing}, nil
}

// retiredCashSellOnlyReason says why cash_sell_only_pct is retired.
const retiredCashSellOnlyReason = "no rule reads cash_sell_only_pct (the cash reserve is cash_reserve_min_pct)"

// retiredRulebookKey reports whether key is one an owner file may still carry
// although no rule reads it: cash_sell_only_pct in a regime set, which
// canary policy default rulebook wrote until v3.11.1. Loading ignores it and
// says so, set refuses it, and any edit removes it.
func retiredRulebookKey(key string) bool {
	set, leaf, ok := strings.Cut(key, ".")
	return ok && leaf == "cash_sell_only_pct" &&
		(set == "regime_calm" || set == "regime_early_warning" || set == "regime_confirmed")
}

// retiredRulebookKeysNote is the status note for the retired keys a file
// carries, or "" when it carries none.
func retiredRulebookKeysNote(keys []string) string {
	if len(keys) == 0 {
		return ""
	}
	return fmt.Sprintf("ignored %s: %s; canary rules policy reset KEY removes it", strings.Join(keys, ", "), retiredCashSellOnlyReason)
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
