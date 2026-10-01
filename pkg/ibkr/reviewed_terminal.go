package ibkr

import (
	"maps"
	"slices"
	"strconv"
	"strings"
)

// ReviewedTerminalStock is one stock the caller holds reviewed terminal
// evidence for (for example an equity the issuer cancelled). ConID, when
// positive, narrows routed keys that carry an explicit contract ID: a key
// naming a different ConID for the same ticker is not covered.
type ReviewedTerminalStock struct {
	ConID  int
	Reason string
}

// defaultReviewedTerminalReason labels an entry supplied without a reason.
const defaultReviewedTerminalReason = "reviewed terminal evidence"

// SetReviewedTerminal atomically replaces the set of stocks, keyed by symbol
// (case-insensitive), that the Connector treats as inactive without asking
// the broker. Unlike the heuristic inactive mark, which needs repeated broker
// confirmations and expires, membership has no TTL and survives connection
// loss; it ends only when a later call omits the symbol, which restores
// ordinary behaviour.
//
// Membership covers the bare symbol, its default route and any STK route key
// for it whose explicit ConID (if any) matches. Any market-data subscription
// a newly added stock still holds is detached: its retry timer is stopped and
// its broker request cancelled (or its slot released). The method sends no
// other broker request. A nil or empty map clears the set.
func (c *Connector) SetReviewedTerminal(stocks map[string]ReviewedTerminalStock) {
	if c == nil {
		return
	}
	next := make(map[string]ReviewedTerminalStock, len(stocks))
	for symbol, entry := range stocks {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || strings.Contains(symbol, "|") {
			continue
		}
		entry.Reason = strings.TrimSpace(entry.Reason)
		if entry.Reason == "" {
			entry.Reason = defaultReviewedTerminalReason
		}
		next[symbol] = entry
	}

	added := make(map[string]ReviewedTerminalStock)
	var removed []string
	c.inactiveMu.Lock()
	previous := c.reviewedTerminal
	for symbol, entry := range next {
		if prior, ok := previous[symbol]; !ok || prior.ConID != entry.ConID {
			added[symbol] = entry
			delete(c.inactiveCandidates, symbol)
		}
	}
	for symbol := range previous {
		if _, kept := next[symbol]; !kept {
			removed = append(removed, symbol)
		}
	}
	c.reviewedTerminal = next
	c.inactiveMu.Unlock()

	if len(added) == 0 && len(removed) == 0 {
		return
	}

	// The set is published before detaching, so a concurrent subscribe is
	// refused by inactiveReason rather than re-creating what is torn down.
	var keys []string
	c.subMu.RLock()
	for key := range c.subscriptions {
		if _, covered := reviewedTerminalMatch(added, key); covered {
			keys = append(keys, key)
		}
	}
	c.subMu.RUnlock()
	slices.Sort(keys)
	var post func()
	for _, key := range keys {
		post = joinPostActions(post, c.detachSubscription(key))
	}

	for _, symbol := range slices.Sorted(maps.Keys(added)) {
		c.logInfo("Suppressing broker requests for %s (%s)", symbol, added[symbol].Reason)
	}
	slices.Sort(removed)
	for _, symbol := range removed {
		c.logInfo("Lifting reviewed terminal suppression for %s", symbol)
	}

	// Broker-side cleanup runs outside every Connector lock.
	if post != nil {
		post()
	}
}

// reviewedTerminalReason reports whether key is covered by the reviewed
// terminal set alone, ignoring heuristic inactive marks.
func (c *Connector) reviewedTerminalReason(key string) (string, bool) {
	c.inactiveMu.RLock()
	defer c.inactiveMu.RUnlock()
	return reviewedTerminalMatch(c.reviewedTerminal, key)
}

// reviewedTerminalMatch reports whether a bare symbol or market-data route key
// is covered by set. Compound keys (see MarketDataKeyForContract) match only
// for STK, and only when any explicit ConID agrees with the entry's.
func reviewedTerminalMatch(set map[string]ReviewedTerminalStock, key string) (string, bool) {
	if len(set) == 0 {
		return "", false
	}
	key = strings.ToUpper(strings.TrimSpace(key))
	symbol, rest, compound := strings.Cut(key, "|")
	entry, ok := set[symbol]
	if !ok {
		return "", false
	}
	if !compound {
		return entry.Reason, true
	}
	secType, _, _ := strings.Cut(rest, "|")
	if secType != "STK" {
		return "", false
	}
	if entry.ConID > 0 {
		if i := strings.LastIndex(rest, "|CONID:"); i >= 0 && rest[i+len("|CONID:"):] != strconv.Itoa(entry.ConID) {
			return "", false
		}
	}
	return entry.Reason, true
}
