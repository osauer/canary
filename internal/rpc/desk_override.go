package rpc

import "time"

// The owner's one-click mandate override from Desk's console (owner,
// 2026-10-29 15:31 CET: "one-click: yes, ok"). policy.override stays
// human-origin only; these two methods are the one other way to grant an
// override, and only of mandate.vol_target_pct: Canary prepares exact terms,
// the owner's device signs Desk's review of them, and Canary verifies that
// signature itself against the key pinned in the constitution's
// [desk_device] before it grants. Like the cash policy and authority
// methods they are outside the MCP catalogue and the CLI; only Desk's
// backend calls them, and no origin a caller claims reaches them.
const (
	MethodDeskOverridePrepare = "desk.override.prepare"
	MethodDeskOverrideConfirm = "desk.override.confirm"
	// DeskOverrideTermsKind names the prepared terms; DeskOverrideReviewKind
	// the review the owner's device signs.
	DeskOverrideTermsKind  = "canary.mandate_override"
	DeskOverrideReviewKind = "mandate_override"
	// OverrideGrantedFromConsole marks an override granted on the owner's
	// device from Desk's console.
	OverrideGrantedFromConsole = "console"
)

// DeskOverridePrepareParams asks for terms of one override. Control must be
// mandate.vol_target_pct; reason and hours are mandatory, the hours at most
// override.max_duration_hours.
type DeskOverridePrepareParams struct {
	Control string `json:"control"`
	Reason  string `json:"reason"`
	Hours   int    `json:"hours"`
}

// DeskOverrideTerms are the exact override terms the owner confirms: the
// account they apply to, the control, the reason and hours, and when the
// confirmation must arrive by.
type DeskOverrideTerms struct {
	Version       int       `json:"version"`
	Kind          string    `json:"kind"`
	ID            string    `json:"id"`
	AccountID     string    `json:"account_id"`
	AccountMode   string    `json:"account_mode"`
	Control       string    `json:"control"`
	Reason        string    `json:"reason"`
	Hours         int       `json:"hours"`
	ConfirmBefore time.Time `json:"confirm_before"`
}

// DeskOverridePrepared carries the canonical terms and their digest.
type DeskOverridePrepared struct {
	Terms  string `json:"terms"`
	Digest string `json:"digest"`
}

// DeskOverrideConfirmParams carries the prepared terms and the owner's
// device confirmation of Desk's review of them.
type DeskOverrideConfirmParams struct {
	Terms        string                 `json:"terms"`
	Digest       string                 `json:"digest"`
	Confirmation CashPolicyConfirmation `json:"confirmation"`
}
