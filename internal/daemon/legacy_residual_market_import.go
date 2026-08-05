package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

// preflightLegacyResidualMarketObservations adds the market/reference caches
// missed by the original importer. Every file is fully decoded and every row
// validated before importLegacyMarketObservations performs its first SQLite
// mutation. Plans append exact bytes only; none publishes current state.
func preflightLegacyResidualMarketObservations(manifest *legacyMarketImportManifest) ([]legacyMarketImportPlan, error) {
	cacheDir, err := fxRateStoreDefaultDir()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy residual cache directory: %w", err)
	}
	membersPath, err := spx.MembersDefaultPath()
	if err != nil {
		return nil, fmt.Errorf("resolve legacy SPX-members path: %w", err)
	}
	var plans []legacyMarketImportPlan
	if plan, ok, err := preflightLegacyFXRates(manifest, filepath.Join(cacheDir, fxRateStoreFilename)); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	if plan, ok, err := preflightLegacyEarnings(manifest, filepath.Join(cacheDir, earningsStoreFilename)); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	if plan, ok, err := preflightLegacySPXMembers(manifest, membersPath); err != nil {
		return nil, err
	} else if ok {
		plans = append(plans, plan)
	}
	return plans, nil
}

func preflightLegacyFXRates(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "fx_rates", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var env fxRatePersistEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != fxRatePersistVersion || env.Rates == nil {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid FX-rate envelope: decode=%v version=%d", err, env.Version))
	}
	var observedAt time.Time
	for pair, row := range env.Rates {
		parts := strings.Split(pair, "/")
		if len(parts) != 2 || fxPairKey(parts[0], parts[1]) != pair || row.Rate <= 0 || row.At.IsZero() || row.At.After(manifest.StartedAt.Add(time.Minute)) {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid FX-rate row %q", pair))
		}
		if row.At.After(observedAt) {
			observedAt = row.At
		}
	}
	if observedAt.IsZero() {
		observedAt = manifest.StartedAt
	}
	manifest.Artifacts[index].Records = len(env.Rates)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": env.Version, "pair_count": len(env.Rates),
		"method": "legacy IBKR ledger or FX snapshot quote cache",
	})
	input := corestore.ObservationInput{
		ScopeKey: fxAuthorityScope, Source: fxObservationSource, Kind: fxObservationKind,
		ObservedAt: observedAt, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return observationOnlyImportPlan(index, input), true, nil
}

func preflightLegacyEarnings(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "earnings_dates", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	var env earningsPersistEnvelopeV1
	if err := json.Unmarshal(raw, &env); err != nil || env.Version != earningsLegacyVersion || env.Entries == nil {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, fmt.Errorf("invalid earnings envelope: decode=%v version=%d", err, env.Version))
	}
	var observedAt time.Time
	for symbol, row := range env.Entries {
		if err := validateEarningsRow(symbol, row, manifest.StartedAt.Add(time.Minute)); err != nil {
			return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, err)
		}
		if row.ObservedAt.After(observedAt) {
			observedAt = row.ObservedAt
		}
	}
	if observedAt.IsZero() {
		observedAt = manifest.StartedAt
	}
	manifest.Artifacts[index].Records = len(env.Entries)
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": env.Version, "entry_count": len(env.Entries),
		"method": "legacy Nasdaq earnings calendar cache",
	})
	input := corestore.ObservationInput{
		ScopeKey: earningsAuthorityScope, Source: earningsObservationSource, Kind: earningsObservationKind,
		ObservedAt: observedAt, ContentType: "application/json", Payload: raw, MetadataJSON: metadata,
	}
	return observationOnlyImportPlan(index, input), true, nil
}

func preflightLegacySPXMembers(manifest *legacyMarketImportManifest, path string) (legacyMarketImportPlan, bool, error) {
	raw, index, ok, err := readLegacyArtifact(manifest, "spx_members", path)
	if err != nil || !ok {
		return legacyMarketImportPlan{}, false, err
	}
	observedAt, records, err := spx.ValidateLegacyMembersObservation(raw)
	if err != nil {
		return legacyMarketImportPlan{}, false, invalidLegacyArtifact(manifest, index, err)
	}
	manifest.Artifacts[index].Records = records
	metadata := legacyMarketMetadata(manifest.Artifacts[index], map[string]any{
		"version": 1, "member_count": records,
		"method": "legacy Wikipedia S&P 500 constituent snapshot",
	})
	return legacyMarketImportPlan{
		artifactIndex: index, observations: 1,
		apply: func(ctx context.Context, store *corestore.Store) error {
			return spx.ImportLegacyMembersObservation(ctx, store, raw, metadata, observedAt)
		},
	}, true, nil
}

func observationOnlyImportPlan(index int, input corestore.ObservationInput) legacyMarketImportPlan {
	_ = input
	return legacyMarketImportPlan{
		artifactIndex: index,
		apply:         func(context.Context, *corestore.Store) error { return nil },
	}
}
