package rpc

import "time"

// PlatformCashSweepSettings exposes the effective ordering preference.
type PlatformCashSweepSettings struct {
	CurrencyPriority SettingsString `json:"currency_priority"`
}

// CashSweepPreferences describes ordering, never sweep or broker readiness.
type CashSweepPreferences struct {
	Revision          int64     `json:"revision"`
	CurrencyPriority  *string   `json:"currency_priority"`
	EffectivePriority string    `json:"effective_priority"`
	Source            string    `json:"source"`
	Writable          bool      `json:"writable"`
	Reason            string    `json:"reason,omitempty"`
	AsOf              time.Time `json:"as_of"`
	RequestID         string    `json:"request_id,omitempty"`
	SavedRevision     int64     `json:"saved_revision,omitempty"`
	Replay            bool      `json:"replay,omitempty"`
}

// SetCashSweepPriorityRequest names one immutable settings edit. A nil priority
// clears the runtime override; the remaining protection policy stays unchanged.
type SetCashSweepPriorityRequest struct {
	CurrencyPriority *string `json:"currency_priority"`
	ExpectedRevision int64   `json:"expected_revision"`
	RequestID        string  `json:"request_id"`
}

// CodeSettingsConflict reports a stale expected settings revision.
const CodeSettingsConflict = "settings_conflict"
