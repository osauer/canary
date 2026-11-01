package rpc

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// AutomaticLock excludes new automatic orders only. It is separate from the
// keep list, which also excludes the owner's bulk reductions.
type AutomaticLock struct {
	Scope       string `json:"scope"`
	Symbol      string `json:"symbol"`
	ConID       int    `json:"con_id,omitempty"`
	AccountID   string `json:"account_id"`
	AccountMode string `json:"account_mode"`
}

// AutomaticLocksUpdate changes one exclusion, or imports an older Desk list.
type AutomaticLocksUpdate struct {
	Action string         `json:"action"`
	Lock   *AutomaticLock `json:"lock,omitempty"`
	// Initialize imports an older Desk preference once; it never overwrites
	// an accepted daemon list, including an explicitly emptied list.
	Locks []AutomaticLock `json:"locks,omitempty"`
}

// NormalizeAutomaticLocks validates and orders account-scoped exclusions.
func NormalizeAutomaticLocks(locks []AutomaticLock) ([]AutomaticLock, error) {
	if len(locks) > HoldingLocksLimit {
		return nil, fmt.Errorf("too many automatic locks")
	}
	out := slices.Clone(locks)
	for _, l := range out {
		if l.Symbol == "" || len(l.Symbol) > 64 || strings.TrimSpace(l.Symbol) != l.Symbol || strings.ToUpper(l.Symbol) != l.Symbol || l.AccountID == "" || len(l.AccountID) > 128 || l.AccountMode != "paper" && l.AccountMode != "live" || l.ConID < 0 || l.Scope != "position" && l.Scope != "underlying" || l.Scope == "position" && l.ConID == 0 || l.Scope == "underlying" && l.ConID != 0 {
			return nil, fmt.Errorf("automatic lock requires an account and an exact position or underlying")
		}
	}
	slices.SortFunc(out, func(a, b AutomaticLock) int {
		return cmp.Or(strings.Compare(a.AccountID, b.AccountID), strings.Compare(a.AccountMode, b.AccountMode), strings.Compare(a.Scope, b.Scope), strings.Compare(a.Symbol, b.Symbol), cmp.Compare(a.ConID, b.ConID))
	})
	return slices.Compact(out), nil
}

// AutomaticMatch applies exact-position or underlying scope within one account.
func (l HoldingLocks) AutomaticMatch(account, mode, symbol string, conID int) bool {
	return slices.ContainsFunc(l.AutomaticLocks, func(v AutomaticLock) bool {
		return v.AccountID == account && v.AccountMode == mode && (v.Scope == "position" && v.ConID == conID || v.Scope == "underlying" && v.Symbol == symbol)
	})
}
