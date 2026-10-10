package rpc

import "time"

// Desk authority is a broker-enforced delegation, not a CLI strategy engine.
// Desk/Torok owns deterministic signals and operating preferences. Canary owns
// signature verification, revocation, broker gates and at-most-once dispatch.
const (
	MethodDeskAuthorityPrepare = "desk.authority.prepare"
	MethodDeskAuthorityConfirm = "desk.authority.confirm"
	MethodDeskAuthorityControl = "desk.authority.control"
	MethodDeskAuthorityStatus  = "desk.authority.status"
)

// DeskAuthorityTerms binds the exact account, scope and execution semantics signed by the owner.
type DeskAuthorityTerms struct {
	AuthorityEpoch string    `json:"authority_epoch"`
	Version        int       `json:"version"`
	Kind           string    `json:"kind"`
	ID             string    `json:"id"`
	AccountID      string    `json:"account_id"`
	AccountMode    string    `json:"account_mode"`
	MaximumScope   string    `json:"maximum_scope"`
	ControllerHash string    `json:"controller_hash"`
	Generation     int64     `json:"generation"`
	ConfirmBefore  time.Time `json:"confirm_before"`
	Pricing        string    `json:"pricing"`
	Algorithm      string    `json:"algorithm"`
	TIF            string    `json:"tif"`
	Protection     string    `json:"protection"`
	Capacity       string    `json:"capacity"`
	Selection      string    `json:"selection"`
}

// DeskAuthorityPrepareParams requests terms for a controller capability and maximum scope.
type DeskAuthorityPrepareParams struct {
	Scope          string `json:"scope"`
	ControllerHash string `json:"controller_hash"`
}

// DeskAuthorityConfirmParams carries the prepared terms and device confirmation.
type DeskAuthorityConfirmParams struct {
	Terms        string                 `json:"terms"`
	Digest       string                 `json:"digest"`
	Confirmation CashPolicyConfirmation `json:"confirmation"`
}

// DeskAuthorityControlParams updates the controller state with a generation fence.
// Generation is daemon-held, never a caller assertion that
// its settings are current. A restriction is effective only after this update.
type DeskAuthorityControlParams struct {
	ID                 string `json:"id"`
	Capability         string `json:"capability"`
	ExpectedGeneration int64  `json:"expected_generation"`
	RequestID          string `json:"request_id"`
	Scope              string `json:"scope"`
	Running            bool   `json:"running"`
	PreferencesHash    string `json:"preferences_hash"`
	AdaptivePriority   string `json:"adaptive_priority"`
}

// DeskAuthorityStatus reports persisted scope and effective controller authority.
type DeskAuthorityStatus struct {
	Terms            *DeskAuthorityTerms `json:"terms,omitempty"`
	Generation       int64               `json:"generation"`
	Scope            string              `json:"scope"`
	Running          bool                `json:"running"`
	PreferencesHash  string              `json:"preferences_hash,omitempty"`
	AdaptivePriority string              `json:"adaptive_priority,omitempty"`
	// HeldBy names what holds a full mandate at protection: "drawdown_brake"
	// from each brake engagement until the owner confirms full again on a
	// device after the brake clears. Empty when nothing holds it.
	HeldBy      string    `json:"held_by,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitzero"`
	Verified    string    `json:"verified,omitempty"`
}

// DeskAuthorityPrepared contains canonical terms and their confirmation digest.
type DeskAuthorityPrepared struct {
	Terms  string `json:"terms"`
	Digest string `json:"digest"`
}
