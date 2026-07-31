package rpc

import (
	"strings"
	"time"
)

// Input currency is the single model for how a regime input reports whether it
// is current: one typed class per evidence unit, and one consumption policy
// saying what each class may do (internal-docs/design/regime-input-currency.md).
//
// An evidence unit is a measurement a consumer actually reads — a row, or a
// single leg where consumers differ (the VIX day-change leg is read by the tape
// arms without the VIX3M leg the ratio needs). Cluster currency is a derived
// roll-up, never the primitive: judging currency per cluster and consuming it as
// fatal for the whole read is what let one flaky leg discard five healthy
// clusters.
//
// Classes are ordered by severity; a roll-up takes the worst member.

// Currency grades separate what a defect does to the read: a fatal-grade unit
// is a data-quality defect, a degrade-grade unit degrades readiness and caps
// confidence, and the scheduled classes cost nothing.
const (
	RegimeCurrencyGradeNone    = "none"
	RegimeCurrencyGradeDegrade = "degrade"
	RegimeCurrencyGradeFatal   = "fatal"
)

// RegimeCurrencyMayConfirm reports whether evidence at this currency may
// CONFIRM stress or take the day-one fast path. Deliberately an allowlist on
// fresh rather than a denylist of known-bad classes: a class added later must
// fail closed until its authority is decided, not inherit confirmation.
func RegimeCurrencyMayConfirm(class string) bool {
	return regimeCurrencyClass(class) == RegimeFreshnessFresh
}

// RegimeCurrencyMayContext reports whether evidence at this currency may stay
// visible, band, and warn. It never implies confirmation.
func RegimeCurrencyMayContext(class string) bool {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessFresh, RegimeFreshnessNotDue, RegimeFreshnessPending, RegimeFreshnessStale:
		return true
	default:
		return false
	}
}

// RegimeCurrencyScheduled reports the two classes that explain an absent newer
// observation by the source's own schedule rather than by a defect: the window
// is closed, or its refresh is in flight inside a bounded window. They carry
// identical authority and callers must treat them alike.
func RegimeCurrencyScheduled(class string) bool {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessNotDue, RegimeFreshnessPending:
		return true
	default:
		return false
	}
}

// RegimeCurrencyGrade maps a currency class to what it costs the read.
func RegimeCurrencyGrade(class string) string {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessFresh, RegimeFreshnessNotDue, RegimeFreshnessPending:
		return RegimeCurrencyGradeNone
	case RegimeFreshnessStale:
		return RegimeCurrencyGradeDegrade
	default:
		return RegimeCurrencyGradeFatal
	}
}

func regimeCurrencyClass(class string) string {
	return strings.ToLower(strings.TrimSpace(class))
}

// regimeCurrencyRank orders the classes so a roll-up can take the worst.
// Unknown and empty classes rank with overdue — untyped evidence fails closed.
func regimeCurrencyRank(class string) int {
	switch regimeCurrencyClass(class) {
	case RegimeFreshnessFresh:
		return 0
	case RegimeFreshnessNotDue:
		return 1
	case RegimeFreshnessPending:
		return 2
	case RegimeFreshnessStale:
		return 3
	default:
		return 4
	}
}

func worseRegimeCurrency(a, b string) string {
	if regimeCurrencyRank(b) > regimeCurrencyRank(a) {
		return b
	}
	return a
}

// RegimeRowCurrency is one row's currency: its served cadence class, reconciled
// with the row status. A class that claims more than the status supports is
// demoted rather than trusted, and a row with no typed freshness is overdue.
func RegimeRowCurrency(status string, freshness *RegimeFreshness) string {
	if freshness == nil {
		return RegimeFreshnessOverdue
	}
	usable := false
	switch strings.ToLower(strings.TrimSpace(status)) {
	case RegimeStatusOK:
		usable = true
	case RegimeStatusStale:
		// A stale row can carry a scheduled or bounded class, never a fresh one.
		usable = regimeCurrencyClass(freshness.Class) != RegimeFreshnessFresh
	}
	if !usable {
		return RegimeFreshnessOverdue
	}
	switch regimeCurrencyClass(freshness.Class) {
	case RegimeFreshnessFresh:
		return RegimeFreshnessFresh
	case RegimeFreshnessNotDue:
		return RegimeFreshnessNotDue
	case RegimeFreshnessPending:
		return RegimeFreshnessPending
	case RegimeFreshnessStale:
		return RegimeFreshnessStale
	default:
		return RegimeFreshnessOverdue
	}
}

// RegimeClusterCurrency rolls a cluster's rows up to their worst currency. A
// cluster with no known rows is overdue.
//
// The scheduled classes carry a bound: not_due and pending both claim that no
// newer observation exists yet, which stops being credible once the served
// max age for the cluster has passed. Without this a dead subscription still
// serving its last value off-hours reads healthy indefinitely.
func RegimeClusterCurrency(r RegimeSnapshotResult, name string) string {
	rows := regimeLifecycleClusterRows(r, name)
	if len(rows) == 0 {
		return RegimeFreshnessOverdue
	}
	class := RegimeFreshnessFresh
	for _, row := range rows {
		class = worseRegimeCurrency(class, RegimeRowCurrency(row.status, row.freshness))
	}
	switch class {
	case RegimeFreshnessNotDue, RegimeFreshnessPending:
		if !regimeClusterWithinMaxAge(r, name) {
			return RegimeFreshnessOverdue
		}
	}
	return class
}

// regimeClusterWithinMaxAge bounds a scheduled class by the served staleness
// policy for the cluster, measured from the snapshot's own clock against the
// weakest contributing row.
func regimeClusterWithinMaxAge(r RegimeSnapshotResult, name string) bool {
	maxAge := RegimeSourceMaxAgeSeconds(strings.ToLower(strings.TrimSpace(name)))
	if maxAge <= 0 {
		return true
	}
	metas := regimeClusterRowMetas(&r, strings.ToLower(strings.TrimSpace(name)))
	if len(metas) == 0 {
		return false
	}
	values := make([]RegimeAsOfSummary, 0, len(metas))
	for _, meta := range metas {
		values = append(values, metaAsOf(meta))
	}
	asOf := weakestRegimeAsOf(values)
	now := r.AsOf
	if now.IsZero() || asOf.IsZero() {
		// No measurable age. The bound cannot fire, and inventing staleness
		// from a missing provenance field would blank a read that the source
		// contract says is fine; the aggregate source-health age check is the
		// single place that decides, and a cluster with no typed evidence at
		// all is already overdue on its row status.
		return true
	}
	return now.Sub(asOf) < time.Duration(maxAge)*time.Second
}

// gammaBlockedOnSessionCadenceOnly reports whether the only thing keeping a
// gamma result from ranking is that it was computed for a different session.
// Decided from the typed gate list, not from reason prose: a result blocked on
// coverage, OI, model source, entitlement, or pacing is a real defect however
// its cadence reads.
func gammaBlockedOnSessionCadenceOnly(c *GammaZeroComputed) bool {
	if c == nil {
		return false
	}
	return gammaQualityBlockedOnSessionCadence(c.Quality)
}

func gammaQualityBlockedOnSessionCadence(q *GammaSignalQuality) bool {
	if q == nil {
		return false
	}
	blocked := false
	for _, gate := range q.Gates {
		if gate.Status != GammaQualityGateBlock {
			continue
		}
		switch gate.Name {
		case GammaQualityGateFreshness:
			if !strings.EqualFold(strings.TrimSpace(q.Freshness), GammaFreshnessSessionMismatch) {
				return false
			}
		case GammaQualityGateSPXCoverage:
			// The combined node carries the SPX slice's verdict; descend once.
			spx, ok := q.ByUnderlying["SPX"]
			if !ok || !gammaQualityBlockedOnSessionCadence(&spx) {
				return false
			}
		default:
			return false
		}
		blocked = true
	}
	return blocked
}

// RegimeVIXTapeCurrency is the currency of the VIX day-change leg on its own.
// The tape arms read that leg; the VIX3M leg they do not read is the thin index
// that times out, and losing it must not demote a live VIX print. A live tick
// is the only current state for a leg that publishes continuously on weekdays.
func RegimeVIXTapeCurrency(r RegimeSnapshotResult) string {
	q := r.VIXTermStructure.VIXQuality
	if q == nil || q.AsOf.IsZero() || r.VIXTermStructure.VIX == nil {
		return RegimeFreshnessOverdue
	}
	if !strings.EqualFold(strings.TrimSpace(q.FreshnessClass), FreshnessLive) {
		return RegimeFreshnessOverdue
	}
	return RegimeFreshnessFresh
}
