package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// riskPolicyDefaultPath is where the operator's risk constitution lives.
// Deliberately not a config key in v1: the constitution is the authority
// over risk numbers, and making its own location configurable before the
// surface has burned in just adds a second thing to audit.
const riskPolicyDefaultPath = "~/.config/ibkr/policies/risk-policy.toml"

// riskPolicyManager mirrors protectionPolicyManager's load/drift semantics
// with one deliberate difference: there is NO embedded default. A missing
// file is status "absent" and every control is unapproved — material
// limits exist only when the operator writes them (interview decision,
// 2026-07-12). Load or validation errors keep the last good policy active
// and disclose loudly; they add no trading blockers in v1 because every
// constitution control is advisory/shadow (stale-posture decision 7).
type riskPolicyManager struct {
	mu              sync.Mutex
	path            string
	reloadInterval  time.Duration
	now             func() time.Time
	active          *risk.Constitution
	status          string
	source          string
	message         string
	loadedAt        time.Time
	lastCheckedAt   time.Time
	lastFingerprint string
	onTransition    func(prev, next string, c *risk.Constitution)
}

func (s *Server) installRiskPolicyManager() {
	if s == nil {
		return
	}
	m := newRiskPolicyManager(riskPolicyDefaultPath, 30*time.Second, s.now)
	m.onTransition = s.journalRiskPolicyTransition
	// First reload is deferred until Start has bound SQLite authority. This
	// prevents construction-time policy transitions from leaking into the
	// sealed legacy JSONL journal before the daemon owns its state lock.
	s.riskPolicies = m
}

func newRiskPolicyManager(path string, interval time.Duration, now func() time.Time) *riskPolicyManager {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &riskPolicyManager{
		path:           expandUserPath(strings.TrimSpace(path)),
		reloadInterval: interval,
		now:            now,
		status:         rpc.RiskPolicyStatusAbsent,
	}
}

func (m *riskPolicyManager) Run(ctx context.Context, logf func(string, ...any)) {
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
			before := m.snapshot()
			m.reload()
			after := m.snapshot()
			if logf != nil && before.status != after.status {
				logf("risk policy status changed: %s -> %s", before.status, after.status)
			}
		}
	}
}

type riskPolicySnapshot struct {
	policy        *risk.Constitution
	status        string
	source        string
	path          string
	message       string
	loadedAt      time.Time
	lastCheckedAt time.Time
}

// snapshot returns the active constitution (nil when absent) plus manager
// provenance. Callers must treat the pointer as read-only.
func (m *riskPolicyManager) snapshot() riskPolicySnapshot {
	if m == nil {
		return riskPolicySnapshot{status: rpc.RiskPolicyStatusAbsent, message: "risk policy manager not installed"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return riskPolicySnapshot{
		policy: m.active, status: m.status, source: m.source, path: m.path,
		message: m.message, loadedAt: m.loadedAt, lastCheckedAt: m.lastCheckedAt,
	}
}

func (m *riskPolicyManager) reload() {
	if m == nil {
		return
	}
	now := m.now().UTC()
	policy, err := m.loadPolicy()

	m.mu.Lock()
	prevStatus := m.status
	m.lastCheckedAt = now
	switch {
	case err != nil && errors.Is(err, os.ErrNotExist):
		// No file: absent is a first-class state, and a previously active
		// policy deliberately stays active — deleting the file is not a
		// sanctioned way to retire policy (that is a revision); the view
		// discloses the mismatch.
		m.status = rpc.RiskPolicyStatusAbsent
		m.source = "none"
		if m.active != nil {
			m.message = "risk policy file removed; last loaded policy stays active — restore the file or publish a revision"
		} else {
			m.message = "no risk policy file; every capital control is unapproved until one exists"
		}
	case err != nil:
		m.status = rpc.RiskPolicyStatusError
		m.message = err.Error()
		if m.active != nil {
			m.message += " (last good policy stays active)"
		}
	default:
		fp := policy.FingerprintKey()
		switch {
		case m.active == nil, policy.PolicyVersion > m.active.PolicyVersion:
			m.active = policy
			m.status = rpc.RiskPolicyStatusActive
			m.source = "file"
			m.message = ""
			m.loadedAt = now
			m.lastFingerprint = fp
		case policy.PolicyVersion == m.active.PolicyVersion && fp == m.lastFingerprint:
			if m.status == rpc.RiskPolicyStatusDrift || m.status == rpc.RiskPolicyStatusError || m.status == rpc.RiskPolicyStatusAbsent {
				m.status = rpc.RiskPolicyStatusActive
				m.message = ""
			}
			m.source = "file"
		default:
			m.status = rpc.RiskPolicyStatusDrift
			m.message = "risk policy file changed without a higher policy_version; bump policy_version to adopt the change"
		}
	}
	next, active := m.status, m.active
	m.mu.Unlock()

	if m.onTransition != nil && prevStatus != next {
		m.onTransition(prevStatus, next, active)
	}
}

func (m *riskPolicyManager) loadPolicy() (*risk.Constitution, error) {
	if strings.TrimSpace(m.path) == "" {
		return nil, os.ErrNotExist
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var c risk.Constitution
	md, err := toml.Decode(string(data), &c)
	if err != nil {
		return nil, fmt.Errorf("parse risk policy %s: %w", m.path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown risk policy key(s): %s", strings.Join(keys, ", "))
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
