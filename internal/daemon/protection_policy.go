package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/rpc"
)

const protectionPolicyKind = "ibkr.protection_policy"

type protectionPolicy struct {
	// Kind must be "ibkr.protection_policy"; any other value fails the load.
	Kind string `toml:"kind" json:"kind"`
	// SchemaVersion is the policy schema revision; only 1 is supported.
	SchemaVersion int `toml:"schema_version" json:"schema_version"`
	// PolicyID is the required identity string for this policy (embedded default "protection-mvp").
	PolicyID string `toml:"policy_id" json:"policy_id"`
	// PolicyVersion is the monotonic policy revision; bump it to make the daemon adopt file edits — an edited file at an unchanged version reports drift instead.
	PolicyVersion int `toml:"policy_version" json:"policy_version"`
	// Profile is a human-readable label for the parameter set (embedded default "theta-priority-mvp"); falls back to policy_id when empty.
	Profile string `toml:"profile" json:"profile"`

	Authority protectionPolicyAuthority `toml:"authority" json:"authority"`
	Buckets   protectionPolicyBuckets   `toml:"buckets" json:"buckets"`
}

type protectionPolicyAuthority struct {
	// CloseReduceOnly restricts proposals to reducing or closing existing positions; must be true in the MVP schema.
	CloseReduceOnly bool `toml:"close_reduce_only" json:"close_reduce_only"`
	// AutoSubmit would let proposals submit themselves; must be false — proposals are advisory and every broker write stays behind the gated order path.
	AutoSubmit bool `toml:"auto_submit" json:"auto_submit"`
}

type protectionPolicyBuckets struct {
	ThetaHygiene  protectionThetaPolicy `toml:"theta_hygiene" json:"theta_hygiene"`
	RiskReduction protectionRiskPolicy  `toml:"risk_reduction" json:"risk_reduction"`
	TrailingStop  protectionTrailPolicy `toml:"trailing_stop" json:"trailing_stop"`
}

type protectionThetaPolicy struct {
	// Enabled turns the near-dated time-decay hygiene bucket on (default true).
	Enabled bool `toml:"enabled" json:"enabled"`
	// MaxDTE only considers options expiring within this many days (default 21).
	MaxDTE int `toml:"max_dte" json:"max_dte"`
	// MinAbsThetaPerDay is a dust floor that cheaply skips trivially small positions before the extrinsic decomposition (default 5.0 per day). It is NOT the
	// materiality gate — absolute dollar theta scales with position size and
	// price, so on its own it flags every large near-dated option regardless
	// of whether time decay is a meaningful fraction of the position.
	MinAbsThetaPerDay float64 `toml:"min_abs_theta_per_day" json:"min_abs_theta_per_day"`
	// MinExtrinsicPctOfMark is the materiality gate: below this percent of mark the option is intrinsic-dominated and a theta-saving close is suppressed (default 40). Theta only erodes
	MinExtrinsicPctOfMark float64 `toml:"min_extrinsic_pct_of_mark" json:"min_extrinsic_pct_of_mark"`
	// MaxSpreadPctOfMid skips quotes whose bid/ask spread exceeds this percent of mid — too wide to act on (default 25).
	MaxSpreadPctOfMid float64 `toml:"max_spread_pct_of_mid" json:"max_spread_pct_of_mid"`
}

type protectionRiskPolicy struct {
	// Enabled turns the single-name concentration-reduction bucket on (default true).
	Enabled bool `toml:"enabled" json:"enabled"`
	// SingleNameTargetPctNLV is the target ceiling for one name's exposure as a percent of net liquidation value (default 25).
	SingleNameTargetPctNLV float64 `toml:"single_name_target_pct_nlv" json:"single_name_target_pct_nlv"`
	// MaxOrderNotional caps the notional of a single generated reduction order (default 10000).
	MaxOrderNotional float64 `toml:"max_order_notional" json:"max_order_notional"`
}

type protectionTrailPolicy struct {
	// Enabled turns the trailing-stop bucket on (default true).
	Enabled bool `toml:"enabled" json:"enabled"`
	// TIF applies to every trailing-stop proposal in this bucket: DAY or
	// GTC, empty means DAY.
	TIF      string                      `toml:"tif" json:"tif,omitempty"`
	StockETF protectionTrailAssetPolicy  `toml:"stock_etf" json:"stock_etf"`
	Options  protectionTrailOptionPolicy `toml:"options" json:"options"`
}

type protectionTrailAssetPolicy struct {
	// Enabled turns trailing-stop proposals for stock/ETF positions on (default true).
	Enabled bool `toml:"enabled" json:"enabled"`
	// OrderType is TRAIL or TRAIL LIMIT (default TRAIL).
	OrderType string `toml:"order_type" json:"order_type"`
	// DefaultPct is the standard trailing distance in percent (default 8).
	DefaultPct float64 `toml:"default_pct" json:"default_pct"`
	// FallbackPct is the trailing distance used when the default cannot be applied; must lie within [min_pct, max_pct] (default 10).
	FallbackPct float64 `toml:"fallback_pct" json:"fallback_pct,omitempty"`
	// MinPct is the lower bound on the trailing distance (default 2).
	MinPct float64 `toml:"min_pct" json:"min_pct"`
	// MaxPct is the upper bound on the trailing distance (default 15).
	MaxPct float64 `toml:"max_pct" json:"max_pct"`
	// MaxSpreadPctOfMid skips stock/ETF quotes wider than this percent of mid (default 2).
	MaxSpreadPctOfMid float64 `toml:"max_spread_pct_of_mid" json:"max_spread_pct_of_mid"`
	// LimitOffsetAbs is the absolute limit offset for the stop; required positive when order_type is TRAIL LIMIT.
	LimitOffsetAbs float64 `toml:"limit_offset_abs" json:"limit_offset_abs,omitempty"`
}

type protectionTrailOptionPolicy struct {
	// Enabled turns trailing-stop proposals for option positions on (default false).
	Enabled bool `toml:"enabled" json:"enabled"`
	// OrderType is TRAIL or TRAIL LIMIT (default TRAIL LIMIT).
	OrderType string `toml:"order_type" json:"order_type"`
	// DefaultPct is the standard trailing distance in percent (default 30).
	DefaultPct float64 `toml:"default_pct" json:"default_pct"`
	// MinPct is the lower bound on the trailing distance (default 20).
	MinPct float64 `toml:"min_pct" json:"min_pct"`
	// MaxPct is the upper bound on the trailing distance (default 50).
	MaxPct float64 `toml:"max_pct" json:"max_pct"`
	// MaxSpreadPctOfMid skips option quotes wider than this percent of mid (default 25).
	MaxSpreadPctOfMid float64 `toml:"max_spread_pct_of_mid" json:"max_spread_pct_of_mid"`
	// MinTrailAbs is the minimum absolute trailing amount in dollars (default 0.10).
	MinTrailAbs float64 `toml:"min_trail_abs" json:"min_trail_abs"`
	// SpreadMultiple sizes the trailing amount as a multiple of the observed spread (default 2).
	SpreadMultiple float64 `toml:"spread_multiple" json:"spread_multiple"`
	// LimitOffsetAbs is the absolute limit offset for the stop; required positive when order_type is TRAIL LIMIT (default 0.05).
	LimitOffsetAbs float64 `toml:"limit_offset_abs" json:"limit_offset_abs,omitempty"`
	// AllowShortProfitTrail permits trailing profit stops on short option positions (default false).
	AllowShortProfitTrail bool `toml:"allow_short_profit_trail" json:"allow_short_profit_trail"`
}

type protectionPolicyManager struct {
	mu              sync.Mutex
	path            string
	hotReload       bool
	reloadInterval  time.Duration
	now             func() time.Time
	active          protectionPolicy
	status          rpc.ProtectionPolicyStatus
	lastFingerprint rpc.Fingerprint
}

func (s *Server) installProtectionPolicyManager() {
	if s == nil || s.cfg == nil {
		return
	}
	cfg := s.cfg.AutoTrade.WithDefaults()
	pm := newProtectionPolicyManager(cfg.PolicyFile, cfg.HotReloadEnabled(), cfg.ReloadIntervalDuration(), s.now)
	pm.reload()
	s.protectionPolicies = pm
}

func newProtectionPolicyManager(path string, hotReload bool, interval time.Duration, now func() time.Time) *protectionPolicyManager {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	return &protectionPolicyManager{
		path:           expandUserPath(strings.TrimSpace(path)),
		hotReload:      hotReload,
		reloadInterval: interval,
		now:            now,
	}
}

func (m *protectionPolicyManager) Run(ctx context.Context, logf func(string, ...any)) {
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
				logf("protection policy status changed: %s -> %s", before.Status, after.Status)
			}
		}
	}
}

func (m *protectionPolicyManager) Active() (protectionPolicy, rpc.ProtectionPolicyStatus) {
	if m == nil {
		p := defaultProtectionPolicy()
		return p, protectionPolicyStatus(p, rpc.ProtectionPolicyStatusDefault, "", "", time.Now().UTC())
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.active, m.status
}

func (m *protectionPolicyManager) Status() rpc.ProtectionPolicyStatus {
	_, st := m.Active()
	return st
}

func (m *protectionPolicyManager) reload() {
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
			m.active = defaultProtectionPolicy()
		}
		st := protectionPolicyStatus(m.active, rpc.ProtectionPolicyStatusError, source, err.Error(), now)
		st.Path = m.path
		m.status = st
		return
	}
	fp := fingerprintProtectionPolicy(policy)

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active.PolicyID == "" {
		m.active = policy
		statusKind := rpc.ProtectionPolicyStatusActive
		if source == "embedded-default" {
			statusKind = rpc.ProtectionPolicyStatusDefault
		}
		st := protectionPolicyStatus(policy, statusKind, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
		return
	}

	switch {
	case policy.PolicyVersion > m.active.PolicyVersion:
		m.active = policy
		st := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusActive, source, "", now)
		st.Path = m.path
		m.status = st
		m.lastFingerprint = fp
	case policy.PolicyVersion == m.active.PolicyVersion && fp.Key == m.lastFingerprint.Key:
		st := protectionPolicyStatus(m.active, m.status.Status, source, "", now)
		if st.Status == "" || st.Status == rpc.ProtectionPolicyStatusDrift || st.Status == rpc.ProtectionPolicyStatusError {
			st.Status = rpc.ProtectionPolicyStatusActive
		}
		st.Path = m.path
		m.status = st
	case policy.PolicyVersion <= m.active.PolicyVersion && fp.Key != m.lastFingerprint.Key:
		st := protectionPolicyStatus(m.active, rpc.ProtectionPolicyStatusDrift, source, "policy file changed without a higher policy_version", now)
		st.Path = m.path
		m.status = st
	}
}

func (m *protectionPolicyManager) loadPolicy() (protectionPolicy, string, error) {
	if m == nil || strings.TrimSpace(m.path) == "" {
		p := defaultProtectionPolicy()
		return p, "embedded-default", validateProtectionPolicy(p)
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			p := defaultProtectionPolicy()
			return p, "embedded-default", validateProtectionPolicy(p)
		}
		return protectionPolicy{}, "file", fmt.Errorf("read protection policy %s: %w", m.path, err)
	}
	var p protectionPolicy
	md, err := toml.Decode(string(data), &p)
	if err != nil {
		return protectionPolicy{}, "file", fmt.Errorf("parse protection policy %s: %w", m.path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return protectionPolicy{}, "file", fmt.Errorf("unknown protection policy key(s): %s", strings.Join(keys, ", "))
	}
	applyProtectionPolicyDefaults(&p, &md)
	if err := validateProtectionPolicy(p); err != nil {
		return protectionPolicy{}, "file", err
	}
	return p, "file", nil
}

func defaultProtectionPolicy() protectionPolicy {
	return protectionPolicy{
		Kind:          protectionPolicyKind,
		SchemaVersion: 1,
		PolicyID:      "protection-mvp",
		PolicyVersion: 1,
		Profile:       "theta-priority-mvp",
		Authority: protectionPolicyAuthority{
			CloseReduceOnly: true,
			AutoSubmit:      false,
		},
		Buckets: protectionPolicyBuckets{
			ThetaHygiene: protectionThetaPolicy{
				Enabled:               true,
				MaxDTE:                21,
				MinAbsThetaPerDay:     5.0,
				MinExtrinsicPctOfMark: 40.0,
				MaxSpreadPctOfMid:     25.0,
			},
			RiskReduction: protectionRiskPolicy{
				Enabled:                true,
				SingleNameTargetPctNLV: 25.0,
				MaxOrderNotional:       10000.0,
			},
			TrailingStop: protectionTrailPolicy{
				Enabled: true,
				StockETF: protectionTrailAssetPolicy{
					Enabled:           true,
					OrderType:         rpc.OrderTypeTRAIL,
					DefaultPct:        8.0,
					FallbackPct:       10.0,
					MinPct:            2.0,
					MaxPct:            15.0,
					MaxSpreadPctOfMid: 2.0,
				},
				Options: protectionTrailOptionPolicy{
					Enabled:               false,
					OrderType:             rpc.OrderTypeTRAILLIMIT,
					DefaultPct:            30.0,
					MinPct:                20.0,
					MaxPct:                50.0,
					MaxSpreadPctOfMid:     25.0,
					MinTrailAbs:           0.10,
					SpreadMultiple:        2.0,
					LimitOffsetAbs:        0.05,
					AllowShortProfitTrail: false,
				},
			},
		},
	}
}

func applyProtectionPolicyDefaults(p *protectionPolicy, md *toml.MetaData) {
	if p == nil {
		return
	}
	if p.Kind == "" {
		p.Kind = protectionPolicyKind
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = 1
	}
	if p.Profile == "" {
		p.Profile = p.PolicyID
	}
	defaults := defaultProtectionPolicy()
	// Backfill the extrinsic materiality gate so a pre-existing policy file
	// written before this knob existed (it carries max_dte / min_abs_theta but
	// not min_extrinsic_pct_of_mark) keeps validating instead of decoding the
	// field to 0.0 and failing the positive-value check. Done before the
	// trailing_stop branch below because that branch can return early for files
	// with no [buckets.trailing_stop] table.
	if p.Buckets.ThetaHygiene.Enabled && p.Buckets.ThetaHygiene.MinExtrinsicPctOfMark == 0 {
		p.Buckets.ThetaHygiene.MinExtrinsicPctOfMark = defaults.Buckets.ThetaHygiene.MinExtrinsicPctOfMark
	}
	if md != nil && !md.IsDefined("buckets", "trailing_stop") {
		p.Buckets.TrailingStop = defaults.Buckets.TrailingStop
		return
	}
	if md != nil && md.IsDefined("buckets", "trailing_stop") && !md.IsDefined("buckets", "trailing_stop", "stock_etf") {
		p.Buckets.TrailingStop.StockETF = defaults.Buckets.TrailingStop.StockETF
	}
	if p.Buckets.TrailingStop.StockETF.FallbackPct == 0 {
		p.Buckets.TrailingStop.StockETF.FallbackPct = defaults.Buckets.TrailingStop.StockETF.FallbackPct
	}
	if md != nil && md.IsDefined("buckets", "trailing_stop") && !md.IsDefined("buckets", "trailing_stop", "options") {
		p.Buckets.TrailingStop.Options = defaults.Buckets.TrailingStop.Options
	}
}

func validateProtectionPolicy(p protectionPolicy) error {
	if p.Kind != protectionPolicyKind {
		return fmt.Errorf("protection policy kind %q is invalid", p.Kind)
	}
	if p.SchemaVersion != 1 {
		return fmt.Errorf("protection policy schema_version %d is unsupported", p.SchemaVersion)
	}
	if strings.TrimSpace(p.PolicyID) == "" {
		return fmt.Errorf("protection policy policy_id is required")
	}
	if p.PolicyVersion <= 0 {
		return fmt.Errorf("protection policy policy_version must be positive")
	}
	if !p.Authority.CloseReduceOnly {
		return fmt.Errorf("protection policy authority.close_reduce_only must be true in MVP")
	}
	if p.Authority.AutoSubmit {
		return fmt.Errorf("protection policy authority.auto_submit must be false in MVP")
	}
	if p.Buckets.ThetaHygiene.Enabled {
		if p.Buckets.ThetaHygiene.MaxDTE <= 0 {
			return fmt.Errorf("theta_hygiene.max_dte must be positive")
		}
		if p.Buckets.ThetaHygiene.MinAbsThetaPerDay <= 0 {
			return fmt.Errorf("theta_hygiene.min_abs_theta_per_day must be positive")
		}
		if p.Buckets.ThetaHygiene.MinExtrinsicPctOfMark <= 0 {
			return fmt.Errorf("theta_hygiene.min_extrinsic_pct_of_mark must be positive")
		}
		if p.Buckets.ThetaHygiene.MaxSpreadPctOfMid <= 0 {
			return fmt.Errorf("theta_hygiene.max_spread_pct_of_mid must be positive")
		}
	}
	if p.Buckets.RiskReduction.Enabled {
		if p.Buckets.RiskReduction.SingleNameTargetPctNLV <= 0 {
			return fmt.Errorf("risk_reduction.single_name_target_pct_nlv must be positive")
		}
		if p.Buckets.RiskReduction.MaxOrderNotional <= 0 {
			return fmt.Errorf("risk_reduction.max_order_notional must be positive")
		}
	}
	// Checked even when the bucket is disabled: tif is a closed two-value
	if tif := strings.TrimSpace(p.Buckets.TrailingStop.TIF); tif != "" &&
		!strings.EqualFold(tif, rpc.OrderTIFDay) && !strings.EqualFold(tif, rpc.OrderTIFGTC) {
		return fmt.Errorf("trailing_stop.tif %q is invalid; use DAY or GTC", p.Buckets.TrailingStop.TIF)
	}
	if p.Buckets.TrailingStop.Enabled {
		if err := validateTrailAssetPolicy("trailing_stop.stock_etf", p.Buckets.TrailingStop.StockETF); err != nil {
			return err
		}
		if err := validateTrailOptionPolicy("trailing_stop.options", p.Buckets.TrailingStop.Options); err != nil {
			return err
		}
	}
	return nil
}

func validateTrailAssetPolicy(prefix string, p protectionTrailAssetPolicy) error {
	if !p.Enabled {
		return nil
	}
	if !supportedTrailOrderType(p.OrderType) {
		return fmt.Errorf("%s.order_type must be TRAIL or TRAIL LIMIT", prefix)
	}
	if p.DefaultPct <= 0 || p.MinPct <= 0 || p.MaxPct <= 0 || p.MinPct > p.DefaultPct || p.DefaultPct > p.MaxPct {
		return fmt.Errorf("%s percent bounds must satisfy 0 < min_pct <= default_pct <= max_pct", prefix)
	}
	if p.FallbackPct <= 0 || p.FallbackPct < p.MinPct || p.FallbackPct > p.MaxPct {
		return fmt.Errorf("%s fallback_pct must satisfy min_pct <= fallback_pct <= max_pct", prefix)
	}
	if p.MaxSpreadPctOfMid <= 0 {
		return fmt.Errorf("%s.max_spread_pct_of_mid must be positive", prefix)
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && p.LimitOffsetAbs <= 0 {
		return fmt.Errorf("%s.limit_offset_abs must be positive for TRAIL LIMIT", prefix)
	}
	return nil
}

func validateTrailOptionPolicy(prefix string, p protectionTrailOptionPolicy) error {
	if !p.Enabled {
		return nil
	}
	if !supportedTrailOrderType(p.OrderType) {
		return fmt.Errorf("%s.order_type must be TRAIL or TRAIL LIMIT", prefix)
	}
	if p.DefaultPct <= 0 || p.MinPct <= 0 || p.MaxPct <= 0 || p.MinPct > p.DefaultPct || p.DefaultPct > p.MaxPct {
		return fmt.Errorf("%s percent bounds must satisfy 0 < min_pct <= default_pct <= max_pct", prefix)
	}
	if p.MaxSpreadPctOfMid <= 0 {
		return fmt.Errorf("%s.max_spread_pct_of_mid must be positive", prefix)
	}
	if p.MinTrailAbs <= 0 {
		return fmt.Errorf("%s.min_trail_abs must be positive", prefix)
	}
	if p.SpreadMultiple <= 0 {
		return fmt.Errorf("%s.spread_multiple must be positive", prefix)
	}
	if strings.EqualFold(p.OrderType, rpc.OrderTypeTRAILLIMIT) && p.LimitOffsetAbs <= 0 {
		return fmt.Errorf("%s.limit_offset_abs must be positive for TRAIL LIMIT", prefix)
	}
	return nil
}

// effectiveTIF resolves the bucket TIF for proposal generation: GTC when
// the policy says so, DAY otherwise (including unset). Other values never
// reach here — validateProtectionPolicy rejects the file.
func (p protectionTrailPolicy) effectiveTIF() string {
	if strings.EqualFold(strings.TrimSpace(p.TIF), rpc.OrderTIFGTC) {
		return rpc.OrderTIFGTC
	}
	return rpc.OrderTIFDay
}

func supportedTrailOrderType(orderType string) bool {
	switch strings.ToUpper(strings.TrimSpace(orderType)) {
	case rpc.OrderTypeTRAIL, rpc.OrderTypeTRAILLIMIT:
		return true
	default:
		return false
	}
}

func protectionPolicyStatus(p protectionPolicy, status, source, message string, at time.Time) rpc.ProtectionPolicyStatus {
	fp := fingerprintProtectionPolicy(p)
	st := rpc.ProtectionPolicyStatus{
		Kind:          protectionPolicyKind,
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
	if status == rpc.ProtectionPolicyStatusDrift || status == rpc.ProtectionPolicyStatusError {
		st.Blockers = []rpc.TradingBlocker{{
			Code:    "policy_" + status,
			Message: nonEmptyString(message, "protection policy is not safe for writes"),
			Action:  "Fix the protection policy file and bump policy_version before preview or submit.",
		}}
	}
	return st
}

func fingerprintProtectionPolicy(p protectionPolicy) rpc.Fingerprint {
	normalized := struct {
		Kind          string                    `json:"kind"`
		SchemaVersion int                       `json:"schema_version"`
		PolicyID      string                    `json:"policy_id"`
		PolicyVersion int                       `json:"policy_version"`
		Profile       string                    `json:"profile"`
		Authority     protectionPolicyAuthority `json:"authority"`
		Buckets       protectionPolicyBuckets   `json:"buckets"`
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
	return rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:" + hex.EncodeToString(sum[:])}
}

func expandUserPath(path string) string {
	if path == "" || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func nonEmptyString(v, fallback string) string {
	if strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}
