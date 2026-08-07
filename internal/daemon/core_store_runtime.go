package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
)

const (
	coreStoreRecoveryPollInterval = 5 * time.Second
	coreStoreRecoveryTimeout      = time.Minute
)

// openCoreStore runs only after the socket-specific instance lock has been
// won. The second lock is rooted beside daemon.db, so alternate socket paths
// cannot create two writers for one authority.
func (s *Server) openCoreStore(ctx context.Context) error {
	if s == nil {
		return fmt.Errorf("open daemon authority: nil server")
	}
	if s.coreStorePathErr != nil {
		return fmt.Errorf("resolve daemon authority: %w", s.coreStorePathErr)
	}
	if s.coreStorePath == "" {
		return fmt.Errorf("resolve daemon authority: empty database path")
	}
	lock, err := acquirePersistenceLock(s.coreStorePath)
	if err != nil {
		return err
	}
	minimum, err := loadAuthorityWatermark(s.coreStorePath + ".head")
	if err != nil {
		lock.Release()
		return fmt.Errorf("load daemon authority watermark: %w", err)
	}
	_, statErr := os.Lstat(s.coreStorePath)
	existed := statErr == nil
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		lock.Release()
		return fmt.Errorf("inspect daemon authority: %w", statErr)
	}
	if !existed && minimum != nil {
		lock.Release()
		return fmt.Errorf("daemon authority is missing but its anti-rollback watermark remains")
	}
	if existed && minimum == nil {
		lock.Release()
		return fmt.Errorf("daemon authority anti-rollback watermark is missing; explicit verified recovery is required")
	}

	var (
		store *corestore.Store
		build coreCutoverBuild
	)
	if existed {
		upgradePending, pendingErr := coreSchemaUpgradePending(s.coreStorePath)
		if pendingErr != nil {
			err = pendingErr
		} else if upgradePending {
			minimum, err = s.upgradeCoreStoreSchema(ctx, minimum)
		}
		if err != nil {
			lock.Release()
			return fmt.Errorf("resume daemon authority schema upgrade: %w", err)
		}
		store, err = corestore.Open(ctx, s.liveCoreStoreOptions(minimum))
		if errors.Is(err, corestore.ErrUpgradeRequired) {
			minimum, err = s.upgradeCoreStoreSchema(ctx, minimum)
			if err == nil {
				store, err = corestore.Open(ctx, s.liveCoreStoreOptions(minimum))
			}
		}
		if err == nil {
			build.manifest, build.doc, err = loadCoreCutoverManifest(ctx, store)
		}
	} else {
		store, build, err = s.createAndPublishCoreStore(ctx)
	}
	if err != nil {
		if store != nil {
			_ = store.Close()
		}
		lock.Release()
		return fmt.Errorf("open daemon authority: %w", err)
	}
	cleanup := func(cause error) error {
		_ = store.Close()
		lock.Release()
		s.coreStore = nil
		s.persistenceLock = nil
		return cause
	}
	s.persistenceLock = lock
	s.coreStore = store
	if err := s.repairGammaLastGoodAuthority(ctx, store); err != nil {
		return cleanup(err)
	}
	if err := s.attachCoreStoreAdapters(ctx, store); err != nil {
		return cleanup(err)
	}
	// Original Flex XML remains broker evidence outside SQLite. Reconcile its
	// complete fingerprinted projection before serving so offline additions,
	// restatements, and removals cannot leave daemon.db silently stale. While
	// the cutover is still pending, update the cutover snapshot and projection
	// together before any legacy source is sealed.
	if s.importLegacyAuthority {
		if build.manifest.Status == coreCutoverStatusPending {
			build, err = s.reconcilePendingStatementCutover(ctx, store, build)
			if err != nil {
				return cleanup(err)
			}
		} else if err := s.refreshStatementProjection(ctx); err != nil {
			return cleanup(fmt.Errorf("refresh retained statement projection: %w", err))
		}
	}
	build, err = s.finishCoreCutover(ctx, store, build)
	if err != nil {
		return cleanup(err)
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		return cleanup(fmt.Errorf("read ready daemon authority head: %w", err))
	}
	if err := writeAuthorityWatermark(s.coreStorePath+".head", head); err != nil {
		return cleanup(err)
	}
	return nil
}

func (s *Server) upgradeCoreStoreSchema(ctx context.Context, minimum *corestore.AuthorityHead) (*corestore.AuthorityHead, error) {
	startedAt := time.Now()
	sourceBytes, sourceSizeErr := coreSchemaUpgradeSourceFootprint(s.coreStorePath)
	if s.logger != nil {
		if sourceSizeErr == nil {
			s.logger.Infof(
				"daemon authority: one-time database maintenance starting; pruning reviewed redundant history and compacting %s before broker connection; do not interrupt",
				formatStorageBytes(sourceBytes),
			)
		} else {
			s.logger.Infof("daemon authority: one-time database maintenance starting; pruning reviewed redundant history and compacting the database before broker connection; do not interrupt")
		}
	}

	updatedMinimum, err := ensureCoreStoreSchemaCurrent(ctx, s.coreStorePath, minimum, s.nowUTC())
	if err != nil {
		return nil, err
	}
	if s.logger != nil {
		elapsed := time.Since(startedAt).Round(time.Second)
		targetBytes, targetSizeErr := coreSchemaUpgradeSourceFootprint(s.coreStorePath)
		if sourceSizeErr == nil && targetSizeErr == nil {
			s.logger.Infof(
				"daemon authority: one-time database maintenance completed in %s; database %s -> %s; recovery artifacts verified",
				elapsed,
				formatStorageBytes(sourceBytes),
				formatStorageBytes(targetBytes),
			)
		} else {
			s.logger.Infof(
				"daemon authority: one-time database maintenance completed in %s; recovery artifacts verified",
				elapsed,
			)
		}
	}
	return updatedMinimum, nil
}

func (s *Server) liveCoreStoreOptions(minimum *corestore.AuthorityHead) corestore.Options {
	return corestore.Options{
		Path:        s.coreStorePath,
		MinimumHead: minimum,
		CommitObserver: func(head corestore.AuthorityHead) error {
			return writeAuthorityWatermark(s.coreStorePath+".head", head)
		},
	}
}

func (s *Server) authorityTradingBlocker() (rpc.TradingBlocker, bool) {
	if s == nil || s.coreStore == nil {
		return rpc.TradingBlocker{
			Code:    "daemon_storage_unavailable",
			Message: "daemon authoritative storage is unavailable; broker writes are blocked",
			Action:  "Resolve daemon storage health and restart; use TWS/Gateway directly for an emergency cancellation.",
		}, true
	}
	health := s.coreStore.Health()
	if health.Ready {
		return rpc.TradingBlocker{}, false
	}
	message := "daemon authoritative storage is unhealthy; broker writes are blocked"
	if health.Code != "" {
		message = fmt.Sprintf("daemon authoritative storage reported %s; broker writes are blocked", health.Code)
	}
	return rpc.TradingBlocker{
		Code:    "daemon_storage_unavailable",
		Message: message,
		Action:  "Stop broker writes, resolve the storage fault, and restart; use TWS/Gateway directly for an emergency cancellation.",
	}, true
}

func (s *Server) authoritySubsystemHealth() rpc.SubsystemHealth {
	if s == nil || s.coreStore == nil {
		return rpc.SubsystemHealth{Name: "storage", Status: "unavailable", Message: "daemon.db is not attached"}
	}
	health := s.coreStore.Health()
	if health.Ready {
		return rpc.SubsystemHealth{Name: "storage", Status: "ready"}
	}
	message := "authoritative persistence is latched fail-closed"
	if health.RecoveryEligible {
		message = "authoritative persistence is fail-closed while a transient head-watermark proof is pending"
	}
	return rpc.SubsystemHealth{
		Name:        "storage",
		Status:      "unavailable",
		Message:     message,
		LastError:   health.Code,
		LastErrorAt: health.BlockedAt,
	}
}

func (s *Server) runCoreStoreRecoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(coreStoreRecoveryPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tryCoreStoreRecovery(ctx)
		}
	}
}

func (s *Server) tryCoreStoreRecovery(ctx context.Context) {
	if s == nil || s.coreStore == nil {
		return
	}
	health := s.coreStore.Health()
	if health.Ready || !health.RecoveryEligible || health.Code != "head_watermark" {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, coreStoreRecoveryTimeout)
	recovered, err := s.coreStore.RecoverTransientHeadWatermark(probeCtx)
	cancel()
	if err != nil {
		if !errors.Is(err, corestore.ErrRecoveryNotEligible) {
			// The unavailable storage subsystem is already operator-visible.
			// Retries are frequent by design, so keep repeated proof failures at
			// debug rather than turning a persistent watermark outage into WARN
			// log churn.
			s.debugf("daemon authority: transient head-watermark recovery proof failed: %v", err)
		}
		return
	}
	if recovered && s.logger != nil {
		s.logger.Infof("daemon authority: transient head-watermark latch recovered after integrity, identity, monotonic-head, and external-watermark verification")
	}
}

// closeCoreStore closes SQLite before releasing its state-root lock. It is
// idempotent because both Stop and Start's deferred cleanup may reach it.
func (s *Server) closeCoreStore() error {
	if s == nil {
		return nil
	}
	s.authorityCloseOnce.Do(func() {
		if s.coreStore != nil {
			s.authorityCloseErr = errors.Join(s.authorityCloseErr, s.coreStore.Close())
		}
		if s.persistenceLock != nil {
			s.persistenceLock.Release()
		}
	})
	return s.authorityCloseErr
}
