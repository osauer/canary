package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const gammaAuthorityProvenanceRecoveredObservation = "recovered_legacy_observation"

// repairGammaLastGoodAuthority repairs the one cutover gap where the legacy
// importer retained a validated gamma envelope as immutable evidence but did
// not publish last-known-good state. The exact observation remains
// decision-ineligible; the promoted state adds explicit recovery provenance,
// which keeps it context-only until a current compute replaces it. This is an
// explicit, gamma-specific promotion, not a generic observation fallback.
//
// The repair is idempotent and runs before the gamma cache is hydrated. A
// current state document always wins; retained history can never overwrite it.
func (s *Server) repairGammaLastGoodAuthority(ctx context.Context, store *corestore.Store) error {
	if store == nil {
		return errors.New("repair gamma last-good authority: nil corestore")
	}
	for _, scope := range knownGammaScopes {
		scopeKey := gammaZeroAuthorityScope(scope)
		if _, ok, err := store.GetStateDocument(ctx, scopeKey, gammaZeroStateKind); err != nil {
			return fmt.Errorf("inspect gamma last-good scope=%s: %w", scope, err)
		} else if ok {
			continue
		}

		observation, ok, err := store.LatestQuarantinedObservationForRecovery(ctx, scopeKey, gammaZeroSource, gammaZeroObservationKind)
		if err != nil {
			return fmt.Errorf("read retained gamma observation scope=%s: %w", scope, err)
		}
		if !ok {
			continue
		}
		payload, err := validatedGammaLastGoodStatePayload(scope, observation)
		if err != nil {
			s.warnf("gamma authority repair skipped scope=%s: %v", scope, err)
			continue
		}

		_, err = store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey: scopeKey,
			Kind:     gammaZeroStateKind,
			JSON:     payload,
		})
		if errors.Is(err, corestore.ErrRevisionConflict) {
			// A concurrent current-code writer won. Re-read to distinguish the
			// harmless race from a genuinely missing state document.
			if _, ok, readErr := store.GetStateDocument(ctx, scopeKey, gammaZeroStateKind); readErr == nil && ok {
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("publish gamma last-good scope=%s: %w", scope, err)
		}
	}
	return nil
}

func validatedGammaLastGoodStatePayload(scope string, observation corestore.Observation) ([]byte, error) {
	if observation.ContentType != "application/json" {
		return nil, fmt.Errorf("unexpected content type %q", observation.ContentType)
	}
	var metadata struct {
		ImportedFromLegacy bool `json:"imported_from_legacy"`
	}
	if err := json.Unmarshal(observation.MetadataJSON, &metadata); err != nil || !metadata.ImportedFromLegacy || observation.DecisionEligible {
		return nil, errors.New("observation is not quarantined legacy gamma evidence")
	}
	var envelope gammaZeroPersistEnvelope
	if err := decodeStrictLegacyJSON(observation.Payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode retained envelope: %w", err)
	}
	if envelope.Version != currentGammaPersistVersion || envelope.Scope != scope {
		return nil, fmt.Errorf("envelope identity mismatch: version=%d scope=%q", envelope.Version, envelope.Scope)
	}
	if envelope.Method != gammaMethodToken || envelope.Result == nil || envelope.Result.Method != envelope.Method {
		return nil, errors.New("envelope methodology is not current")
	}
	if envelope.Result.AsOf.IsZero() || !observation.ObservedAt.Equal(envelope.Result.AsOf) {
		return nil, errors.New("envelope observation time is invalid")
	}
	if envelope.SessionKey != nySessionKey(envelope.Result.AsOf) {
		return nil, errors.New("envelope session key does not match result time")
	}
	if err := validateRecoveredGammaShape(scope, envelope.SessionKey, envelope.Result); err != nil {
		return nil, err
	}
	if err := validateGammaComputed(envelope.Result); err != nil {
		return nil, err
	}
	markRecoveredGammaAuthority(envelope.Result)
	payload, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("encode recovered gamma envelope: %w", err)
	}
	return payload, nil
}

func validateRecoveredGammaShape(scope, sessionKey string, result *rpc.GammaZeroComputed) error {
	if result == nil {
		return errors.New("recovered gamma result is missing")
	}
	if result.AsOf.After(time.Now().Add(5 * time.Minute)) {
		return errors.New("recovered gamma result timestamp is in the future")
	}
	if result.Method != gammaMethodToken || nySessionKey(result.AsOf) != sessionKey {
		return errors.New("recovered gamma result method or session is invalid")
	}
	resultScope := strings.ToLower(strings.TrimSpace(result.Scope))
	switch scope {
	case rpc.GammaZeroScopeSPY, rpc.GammaZeroScopeSPX:
		if resultScope != scope || len(result.PerIndex) != 0 {
			return fmt.Errorf("single-scope result shape mismatch: envelope=%q result=%q children=%d", scope, result.Scope, len(result.PerIndex))
		}
	case rpc.GammaZeroScopeCombined:
		switch resultScope {
		case rpc.GammaZeroScopeCombined:
			if len(result.PerIndex) != 2 {
				return fmt.Errorf("combined result requires SPY and SPX children, got %d", len(result.PerIndex))
			}
			spy, spyOK := result.PerIndex["SPY"]
			spx, spxOK := result.PerIndex["SPX"]
			if !spyOK || !spxOK || spy == nil || spx == nil {
				return errors.New("combined result is missing SPY or SPX child")
			}
			if err := validateRecoveredGammaChild("SPY", rpc.GammaZeroScopeSPY, sessionKey, spy); err != nil {
				return err
			}
			if err := validateRecoveredGammaChild("SPX", rpc.GammaZeroScopeSPX, sessionKey, spx); err != nil {
				return err
			}
			if !result.AsOf.Equal(combinedGammaAsOf(spy.AsOf, spx.AsOf)) {
				return errors.New("combined result timestamp does not match its children")
			}
		case rpc.GammaZeroScopeSPY:
			if len(result.PerIndex) != 0 || !gammaIndexUnavailable(result, "SPX") {
				return errors.New("combined-scope SPY fallback shape is invalid")
			}
		case rpc.GammaZeroScopeSPX:
			if len(result.PerIndex) != 0 || !gammaIndexUnavailable(result, "SPY") {
				return errors.New("combined-scope SPX fallback shape is invalid")
			}
		default:
			return fmt.Errorf("combined-scope result has invalid scope %q", result.Scope)
		}
	default:
		return fmt.Errorf("unknown gamma scope %q", scope)
	}
	return nil
}

func validateRecoveredGammaChild(label, scope, sessionKey string, child *rpc.GammaZeroComputed) error {
	if child == nil || strings.ToLower(strings.TrimSpace(child.Scope)) != scope ||
		child.Method != gammaMethodToken || child.AsOf.IsZero() ||
		nySessionKey(child.AsOf) != sessionKey || len(child.PerIndex) != 0 ||
		child.AsOf.After(time.Now().Add(5*time.Minute)) {
		return fmt.Errorf("combined result child %s has invalid scope, method, session, or nesting", label)
	}
	return nil
}

func markRecoveredGammaAuthority(result *rpc.GammaZeroComputed) {
	if result == nil {
		return
	}
	result.AuthorityProvenance = gammaAuthorityProvenanceRecoveredObservation
	for _, sub := range result.PerIndex {
		markRecoveredGammaAuthority(sub)
	}
}
