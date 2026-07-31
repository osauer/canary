package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/osauer/canary/v2/internal/breadth/spx"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const marketAuthorityWriteAttempts = 4

// attachCoreMarketAuthority is the single startup switch for every
// market-observation projection. The Server constructors still build legacy
// codecs so the cutover importer can read them, but startup must call this
// under the persistence lock before opening the daemon socket or gateway.
// After success, no listed store reads or writes a legacy path.
func (s *Server) attachCoreMarketAuthority(store *corestore.Store) error {
	if s == nil {
		return errors.New("attach market authority: nil server")
	}
	if store == nil {
		return errors.New("attach market authority: nil corestore")
	}
	if s.zeroGamma == nil || s.gammaOI == nil || s.gammaGrids == nil || s.regimeHistory == nil || s.regimeSeries == nil || s.streaks == nil || s.breadth == nil {
		return errors.New("attach market authority: market stores are not installed")
	}
	if err := s.zeroGamma.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma authority: %w", err)
	}
	if err := s.gammaOI.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma OI authority: %w", err)
	}
	if err := s.gammaGrids.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach gamma expiry-grid authority: %w", err)
	}
	if err := s.regimeHistory.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach regime HMDS authority: %w", err)
	}
	if err := s.regimeSeries.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach official-series authority: %w", err)
	}
	if err := s.streaks.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach regime-streak authority: %w", err)
	}
	if err := s.breadth.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach breadth authority: %w", err)
	}
	if s.contractStore == nil {
		// The legacy constructor can leave this nil when HOME/XDG resolution
		// fails. daemon.db does not need a cache directory, so install a cold
		// codec and attach it directly.
		s.contractStore = ibkrlib.NewContractStore("")
	}
	if err := s.contractStore.UseAuthority(coreContractCacheAuthority{store: store}); err != nil {
		return fmt.Errorf("attach contract-cache authority: %w", err)
	}
	if s.fxRates == nil {
		s.fxRates = newFXRateCache()
	}
	if err := s.fxRates.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach FX-rate authority: %w", err)
	}
	if s.earnings == nil {
		logf := func(string, ...any) {}
		if s.logger != nil {
			logf = s.logger.Warnf
		}
		s.earnings = newEarningsCacheMemory(logf)
	}
	if err := s.earnings.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach earnings authority: %w", err)
	}
	if s.earningsTerminal == nil {
		s.earningsTerminal = newEarningsTerminalStore("")
	}
	if err := s.earningsTerminal.UseCoreStore(context.Background(), store, s.orderNow()); err != nil {
		return fmt.Errorf("attach earnings terminal authority: %w", err)
	}
	if s.marketEvents == nil {
		s.marketEvents = newMarketEventCache(s.now)
	}
	if err := s.marketEvents.UseCoreStore(store); err != nil {
		return fmt.Errorf("attach market-events authority: %w", err)
	}
	if s.membersCachePath != "" {
		if err := spx.UseCoreMembersStore(s.membersCachePath, store); err != nil {
			return fmt.Errorf("attach SPX-members authority: %w", err)
		}
	}
	return nil
}

// loadMarketState reads the current typed market document from daemon.db.
// Callers validate the domain envelope after this transport-level read.
func loadMarketState(store *corestore.Store, scopeKey, kind string) ([]byte, bool, error) {
	if store == nil {
		return nil, false, errors.New("market observation authority is not attached")
	}
	doc, ok, err := store.GetStateDocument(context.Background(), scopeKey, kind)
	if err != nil || !ok {
		return nil, ok, err
	}
	return append([]byte(nil), doc.JSON...), true, nil
}

// saveMarketState atomically publishes the current document and appends the
// exact immutable measurement bytes. A CAS retry handles a concurrent writer
// without ever falling back to a legacy file or losing the observation half
// of the commit.
func saveMarketState(store *corestore.Store, scopeKey, stateKind string, input corestore.ObservationInput) error {
	return saveMarketStateContext(context.Background(), store, scopeKey, stateKind, input)
}

func saveMarketStateContext(ctx context.Context, store *corestore.Store, scopeKey, stateKind string, input corestore.ObservationInput) error {
	if store == nil {
		return errors.New("market observation authority is not attached")
	}
	// Only current-code measurements may publish a current state document.
	// The coupled observation is therefore decision-eligible by construction;
	// legacy importers use append-only APIs and can never cross this boundary.
	input.DecisionEligible = true
	for range marketAuthorityWriteAttempts {
		doc, ok, err := store.GetStateDocument(ctx, scopeKey, stateKind)
		if err != nil {
			return err
		}
		var revision int64
		if ok {
			revision = doc.Revision
		}
		_, _, err = store.CompareAndSwapStateDocumentWithObservations(
			ctx,
			corestore.StateDocumentCAS{
				ScopeKey:         scopeKey,
				Kind:             stateKind,
				ExpectedRevision: revision,
				JSON:             input.Payload,
			},
			[]corestore.ObservationInput{input},
		)
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return fmt.Errorf("save market state %s/%s: %w after %d attempts", scopeKey, stateKind, corestore.ErrRevisionConflict, marketAuthorityWriteAttempts)
}

// saveMarketDocument publishes the current document without appending an
// observation. It is for derived caches — state whose only reader is the
// daemon's own next boot and which the broker will re-serve on request.
// Everything a decision can rest on goes through saveMarketState instead, so
// that its exact bytes stay in the immutable ledger.
func saveMarketDocument(ctx context.Context, store *corestore.Store, scopeKey, stateKind string, payload []byte) error {
	if store == nil {
		return errors.New("market observation authority is not attached")
	}
	for range marketAuthorityWriteAttempts {
		doc, ok, err := store.GetStateDocument(ctx, scopeKey, stateKind)
		if err != nil {
			return err
		}
		var revision int64
		if ok {
			revision = doc.Revision
		}
		_, err = store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
			ScopeKey:         scopeKey,
			Kind:             stateKind,
			ExpectedRevision: revision,
			JSON:             payload,
		})
		if !errors.Is(err, corestore.ErrRevisionConflict) {
			return err
		}
	}
	return fmt.Errorf("save market document %s/%s: %w after %d attempts", scopeKey, stateKind, corestore.ErrRevisionConflict, marketAuthorityWriteAttempts)
}
