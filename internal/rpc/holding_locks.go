package rpc

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Holding locks are the owner's keep list: a locked name is never selected
// by a bulk reduction, the mandate's automatic reduction or any other sweep.
// A single-line order the owner places on it still works. The owner, not the
// sign of a delta, decides what is a hedge (owner, 2026-10-22 19:50 CEST).
//
// A lock names an underlying and covers every contract of it the book holds
// when a sweep runs: the shares, the calls written on them, the puts, and
// the contract a roll moves into (owner, 2026-10-28 06:41 CET: a locked
// stock had its calls sold because the lock named one contract). A lock
// whose name is not in the book keeps nothing and is reported as not held;
// it is never pruned, since deleting a lock is the owner's.
const (
	MethodHoldingLocksList    = "holding_locks.list"
	MethodHoldingLocksReplace = "holding_locks.replace"
	CodeHoldingLocksConflict  = "holding_locks_conflict"
	HoldingLocksLimit         = 200

	HoldingLockRoleHedge = "hedge"
	HoldingLockRoleCore  = "core"
)

// HoldingLock is one locked name. Symbol is the underlying (a stock's own
// symbol, an option's underlying) and keys the lock. ConID is optional: the
// contract the lock was placed from, kept so a lock saved before 2026-10-28
// still covers that exact contract even where its symbol differs from the
// book's. Contracts and Held are served, never stored: the held contracts
// the lock resolves to in the current book, and whether there is any; Held
// is nil when the book could not be read.
type HoldingLock struct {
	Symbol    string `json:"symbol"`
	Role      string `json:"role"`
	ConID     int    `json:"con_id,omitempty"`
	Contracts []int  `json:"contracts,omitempty"`
	Held      *bool  `json:"held,omitempty"`
}

// HoldingLocks is the accepted keep list and an optional mutation receipt.
type HoldingLocks struct {
	AutomaticLocks            []AutomaticLock `json:"automatic_locks,omitempty"`
	AutomaticLocksInitialized bool            `json:"automatic_locks_initialized"`
	Version                   int             `json:"version"`
	Revision                  int64           `json:"revision"`
	Locks                     []HoldingLock   `json:"locks"`
	AsOf                      time.Time       `json:"as_of"`
	RequestID                 string          `json:"request_id,omitempty"`
	SavedRevision             int64           `json:"saved_revision,omitempty"`
	Replay                    bool            `json:"replay,omitempty"`
}

// HoldingLocksListRequest selects the automatic exclusions without reading
// the broker book to resolve the keep list.
type HoldingLocksListRequest struct {
	AutomaticOnly bool `json:"automatic_only,omitempty"`
}

// Match returns the lock covering a contract: the one naming its underlying,
// else the one placed from that exact contract.
func (l HoldingLocks) Match(symbol string, conID int) (HoldingLock, bool) {
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol != "" {
		if i := slices.IndexFunc(l.Locks, func(h HoldingLock) bool { return h.Symbol == symbol }); i >= 0 {
			return l.Locks[i], true
		}
	}
	if conID > 0 {
		if i := slices.IndexFunc(l.Locks, func(h HoldingLock) bool { return h.ConID == conID }); i >= 0 {
			return l.Locks[i], true
		}
	}
	return HoldingLock{}, false
}

// Resolve maps the keep list onto a book: every held stock and option row
// a lock covers, by exact contract. A sweep selects from the book through
// the resolution, never through the list, so a lock saved on one contract
// holds the whole name and survives a roll.
func (l HoldingLocks) Resolve(pos *PositionsResult) ResolvedHoldingLocks {
	out := ResolvedHoldingLocks{byConID: map[int]HoldingLock{}}
	if pos == nil || len(l.Locks) == 0 {
		return out
	}
	for _, rows := range [][]PositionView{pos.Stocks, pos.Options} {
		for _, row := range rows {
			if row.ConID <= 0 {
				continue
			}
			if lock, ok := l.Match(row.Symbol, row.ConID); ok {
				out.byConID[row.ConID] = lock
			}
		}
	}
	return out
}

// Decorate fills each lock's served fields from the book: the contracts it
// covers now and whether there is any. A nil book leaves Held unknown.
func (l *HoldingLocks) Decorate(pos *PositionsResult) {
	if pos == nil {
		return
	}
	resolved := l.Resolve(pos)
	for i := range l.Locks {
		lock := &l.Locks[i]
		lock.Contracts = nil
		for conID, by := range resolved.byConID {
			if by.Symbol == lock.Symbol {
				lock.Contracts = append(lock.Contracts, conID)
			}
		}
		slices.Sort(lock.Contracts)
		lock.Held = new(len(lock.Contracts) > 0)
	}
}

// ResolvedHoldingLocks is the keep list applied to one book.
type ResolvedHoldingLocks struct {
	byConID map[int]HoldingLock
}

// Locked reports whether the held contract is covered by a lock.
func (r ResolvedHoldingLocks) Locked(conID int) bool {
	_, ok := r.byConID[conID]
	return ok
}

// Role is the covering lock's role, or empty for an unlocked contract.
func (r ResolvedHoldingLocks) Role(conID int) string {
	return r.byConID[conID].Role
}

// HoldingLocksReplaceRequest replaces the keep list under a revision fence.
type HoldingLocksReplaceRequest struct {
	Automatic        *AutomaticLocksUpdate `json:"automatic,omitempty"`
	Locks            []HoldingLock         `json:"locks"`
	ExpectedRevision int64                 `json:"expected_revision"`
	RequestID        string                `json:"request_id"`
}

// NormalizeHoldingLocks orders the list by name, folds the entries of one
// name into one lock (a list saved before 2026-10-28 held one entry per
// contract, so the shares and the calls of a name were two), and rejects
// unknown roles and blank names. Entries of one name under both roles fold
// to hedge: the role only labels what a sweep keeps, and a stored list must
// never become unreadable, since an unreadable list holds every sweep.
// Served fields are dropped.
func NormalizeHoldingLocks(in []HoldingLock) ([]HoldingLock, error) {
	if in == nil || len(in) > HoldingLocksLimit {
		return nil, fmt.Errorf("locks must be an array of at most %d lines", HoldingLocksLimit)
	}
	out := make([]HoldingLock, 0, len(in))
	for _, l := range in {
		l = HoldingLock{Symbol: strings.ToUpper(strings.TrimSpace(l.Symbol)), Role: strings.ToLower(strings.TrimSpace(l.Role)), ConID: max(l.ConID, 0)}
		if l.Symbol == "" || len(l.Symbol) > 64 {
			return nil, fmt.Errorf("each lock needs a symbol")
		}
		if l.Role != HoldingLockRoleHedge && l.Role != HoldingLockRoleCore {
			return nil, fmt.Errorf("lock role must be hedge or core")
		}
		i := slices.IndexFunc(out, func(h HoldingLock) bool { return h.Symbol == l.Symbol })
		if i < 0 {
			out = append(out, l)
			continue
		}
		if out[i].Role != l.Role {
			out[i].Role = HoldingLockRoleHedge
		}
		if out[i].ConID != l.ConID {
			out[i].ConID = 0 // the lock now spans contracts; none is "the" one
		}
	}
	slices.SortFunc(out, func(a, b HoldingLock) int { return strings.Compare(a.Symbol, b.Symbol) })
	return out, nil
}
