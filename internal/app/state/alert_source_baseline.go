package state

import (
	"fmt"
	"maps"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The optional v4 ledger extension commissions each source independently. Old
// global baselines remain intact for rollback; receipts and dispositions never
// change during migration. A stored current-source watermark is the proof that
// a source was observed under a legacy global baseline.
func migrateAlertSourceBaselines(data *alertDeliveryData) {
	if data.SourceBaselines != nil {
		return
	}
	data.SourceBaselines = make(map[string]map[rpc.AlertSource]alertDeliveryBaseline)
	for scope, baseline := range data.Baselines {
		for source, watermark := range data.SourceWatermarksByScope[scope] {
			if watermark.Before(baseline.SnapshotAsOf) || baseline.EstablishedAt.IsZero() {
				continue
			}
			if data.SourceBaselines[scope] == nil {
				data.SourceBaselines[scope] = make(map[rpc.AlertSource]alertDeliveryBaseline)
			}
			data.SourceBaselines[scope][source] = baseline
		}
	}
}

func cloneAlertSourceBaselines(in map[string]map[rpc.AlertSource]alertDeliveryBaseline) map[string]map[rpc.AlertSource]alertDeliveryBaseline {
	if in == nil {
		return nil // distinguishes the legacy document from an empty new ledger
	}
	out := make(map[string]map[rpc.AlertSource]alertDeliveryBaseline, len(in))
	for scope, rows := range in {
		out[scope] = maps.Clone(rows)
	}
	return out
}

func alertSourceBaselineEstablished(data *alertDeliveryData, scope string, source rpc.AlertSource) bool {
	if data == nil {
		return false
	}
	baseline, ok := data.SourceBaselines[scope][source]
	return ok && !baseline.EstablishedAt.IsZero() && !baseline.SnapshotAsOf.IsZero()
}

func alertSourceCurrent(row rpc.AlertSourceCoverage, now time.Time) bool {
	return row.Covered && row.EvidenceHealth == rpc.AlertEvidenceCurrent && !row.FreshUntil.IsZero() && !now.After(row.FreshUntil)
}

func alertSourceBaselineCount(data *alertDeliveryData) int {
	n := 0
	for _, sources := range data.SourceBaselines {
		n += len(sources)
	}
	return n
}

func (s *Store) establishAlertSourceBaselines(data *alertDeliveryData, snapshot rpc.AlertCandidateSnapshot) error {
	for _, row := range snapshot.Sources {
		if !alertSourceCurrent(row, snapshot.AsOf) || alertSourceBaselineEstablished(data, snapshot.AuthorityScope, row.Source) {
			continue
		}
		if alertSourceBaselineCount(data) >= s.alertDeliveryMaxItems {
			return ErrAlertDeliveryOverflow
		}
		if data.SourceBaselines[snapshot.AuthorityScope] == nil {
			data.SourceBaselines[snapshot.AuthorityScope] = make(map[rpc.AlertSource]alertDeliveryBaseline)
		}
		data.SourceBaselines[snapshot.AuthorityScope][row.Source] = alertDeliveryBaseline{EstablishedAt: snapshot.AsOf.UTC(), SnapshotAsOf: snapshot.AsOf.UTC()}
	}
	return nil
}

func validateAlertSourceBaselines(data *alertDeliveryData, maximum int) error {
	if len(data.SourceBaselines) > maximum || alertSourceBaselineCount(data) > maximum {
		return fmt.Errorf("%w: source baselines exceed capacity", ErrInvalidPersistedState)
	}
	for scope, rows := range data.SourceBaselines {
		if err := rpc.ValidateAlertAuthorityScope(scope); err != nil || len(rows) == 0 {
			return fmt.Errorf("%w: invalid source baseline scope", ErrInvalidPersistedState)
		}
		for source, baseline := range rows {
			if !validAlertDeliverySource(source) || baseline.EstablishedAt.IsZero() || baseline.SnapshotAsOf.IsZero() || baseline.SnapshotAsOf.After(baseline.EstablishedAt) {
				return fmt.Errorf("%w: invalid source baseline", ErrInvalidPersistedState)
			}
		}
	}
	return nil
}
