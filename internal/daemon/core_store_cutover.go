package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
	ibkrlib "github.com/osauer/ibkr/v2/pkg/ibkr"
)

const (
	coreCutoverManifestKind    = "sqlite_authority_cutover_v1"
	coreCutoverManifestVersion = 1
	coreCutoverStatusPending   = "pending_seal"
	coreCutoverStatusBackup    = "sealed_backup_pending"
	coreCutoverStatusSealed    = "sealed"
	coreCutoverStatusFresh     = "fresh"
	coreCutoverActionSeal      = "seal"
	coreCutoverActionRetain    = "retain"
)

type coreCutoverManifest struct {
	Version                   int                     `json:"version"`
	CutoverID                 string                  `json:"cutover_id"`
	CreatedAt                 time.Time               `json:"created_at"`
	CompletedAt               time.Time               `json:"completed_at"`
	Status                    string                  `json:"status"`
	ImportedLegacy            bool                    `json:"imported_legacy"`
	Sources                   []coreCutoverSource     `json:"sources"`
	Counts                    coreCutoverCounts       `json:"counts"`
	StatementSourceSetSHA256  string                  `json:"statement_source_set_sha256,omitempty"`
	StatementProjectionSHA256 string                  `json:"statement_projection_sha256,omitempty"`
	PrepublishBackupPath      string                  `json:"prepublish_backup_path"`
	FinalBackupPath           string                  `json:"final_backup_path,omitempty"`
	MinimumFinalBackupHead    corestore.AuthorityHead `json:"minimum_final_backup_head,omitzero"`
}

type coreCutoverCounts struct {
	RetainedOrderEvents      int `json:"retained_order_events"`
	RetainedOrderChains      int `json:"retained_order_chains"`
	ConsumedPreviewTokens    int `json:"consumed_preview_tokens"`
	PurgeRows                int `json:"purge_rows"`
	PurgeFillCursors         int `json:"purge_fill_cursors"`
	MarketObservationFiles   int `json:"market_observation_files"`
	MarketObservationRecords int `json:"market_observation_records"`
	MarketObservations       int `json:"market_observations"`
	StatementFiles           int `json:"statement_files"`
	StatementRows            int `json:"statement_rows"`
	ImportedCapitalEvents    int `json:"imported_capital_events"`
	ImportedGovernanceEvents int `json:"imported_governance_events"`
	SkippedGovernanceEvents  int `json:"skipped_governance_events"`
}

type coreCutoverSource struct {
	Kind           string `json:"kind"`
	Class          string `json:"class"`
	PreservedClass string `json:"preserved_class,omitempty"`
	SkippedClass   string `json:"skipped_class,omitempty"`
	Action         string `json:"action"`
	Path           string `json:"path"`
	Destination    string `json:"destination,omitempty"`
	Bytes          int64  `json:"bytes,omitempty"`
	SHA256         string `json:"sha256,omitempty"`
	Records        int    `json:"records,omitempty"`
	Status         string `json:"status"`
}

type coreCutoverBuild struct {
	manifest coreCutoverManifest
	doc      corestore.StateDocument
}

func (s *Server) createAndPublishCoreStore(ctx context.Context) (*corestore.Store, coreCutoverBuild, error) {
	var build coreCutoverBuild
	id, err := newCoreCutoverID()
	if err != nil {
		return nil, build, err
	}
	parent := filepath.Dir(s.coreStorePath)
	tempPath := s.coreStorePath + ".cutover-" + id + ".tmp"
	if _, err := os.Lstat(tempPath); err == nil {
		return nil, build, fmt.Errorf("cutover temporary database already exists: %s", tempPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, build, fmt.Errorf("inspect cutover temporary database: %w", err)
	}
	store, err := corestore.Open(ctx, corestore.Options{Path: tempPath})
	if err != nil {
		return nil, build, fmt.Errorf("create unpublished daemon authority: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()

	manifest := coreCutoverManifest{
		Version:        coreCutoverManifestVersion,
		CutoverID:      id,
		CreatedAt:      s.nowUTC(),
		ImportedLegacy: s.importLegacyAuthority,
	}
	backupDir := filepath.Join(parent, "backups")
	manifest.PrepublishBackupPath = filepath.Join(backupDir, "daemon-cutover-"+id+"-prepublish.db")
	if s.importLegacyAuthority {
		manifest.Status = coreCutoverStatusPending
		manifest.FinalBackupPath = filepath.Join(backupDir, "daemon-cutover-"+id+"-sealed.db")
		if err := s.populateLegacyCutover(ctx, store, &manifest); err != nil {
			return nil, build, err
		}
	} else {
		manifest.Status = coreCutoverStatusFresh
		if err := initializeFreshDaemonState(ctx, store); err != nil {
			return nil, build, err
		}
		if err := initializeFreshTradingAuthority(ctx, store); err != nil {
			return nil, build, fmt.Errorf("initialize fresh trading authority: %w", err)
		}
		if err := store.ReplaceStatementProjection(ctx, statementProjectionScope, nil, nil); err != nil {
			return nil, build, fmt.Errorf("initialize fresh statement projection: %w", err)
		}
	}
	if err := initializeEmptyTradingReadiness(ctx, store); err != nil {
		return nil, build, err
	}
	if err := initializeCleanProposalOpportunityAuthority(ctx, store); err != nil {
		return nil, build, err
	}
	doc, err := writeCoreCutoverManifest(ctx, store, manifest, 0)
	if err != nil {
		return nil, build, err
	}
	build = coreCutoverBuild{manifest: manifest, doc: doc}

	if err := verifyCoreStoreForPublication(ctx, store); err != nil {
		return nil, build, err
	}
	if _, err := store.Backup(ctx, manifest.PrepublishBackupPath); err != nil {
		return nil, build, fmt.Errorf("create verified prepublish authority backup: %w", err)
	}
	if err := verifyCoreStoreForPublication(ctx, store); err != nil {
		return nil, build, err
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		return nil, build, fmt.Errorf("read unpublished authority head: %w", err)
	}
	if err := store.Close(); err != nil {
		return nil, build, fmt.Errorf("close unpublished authority: %w", err)
	}
	closed = true
	if err := syncCutoverDatabase(tempPath); err != nil {
		return nil, build, err
	}
	// Establish the external rollback floor before publishing daemon.db.
	// This deliberately prefers a fail-closed orphan watermark if the
	// machine crashes in the tiny interval before the hard link. Allowing an
	// existing database to start without .head would let deletion of that file
	// bless an arbitrarily old same-epoch backup and erase tombstones or floors.
	if err := writeAuthorityWatermark(s.coreStorePath+".head", head); err != nil {
		return nil, build, fmt.Errorf("publish initial authority watermark: %w", err)
	}
	// Publish without replacement semantics. Unix rename would overwrite a
	// target created after the initial Lstat; a same-directory hard link is
	// atomic and fails with EEXIST instead.
	if err := os.Link(tempPath, s.coreStorePath); err != nil {
		return nil, build, fmt.Errorf("publish daemon authority without clobber: %w", err)
	}
	if err := syncPrivateDirectory(parent); err != nil {
		return nil, build, fmt.Errorf("sync published daemon authority directory: %w", err)
	}
	if err := os.Remove(tempPath); err != nil {
		return nil, build, fmt.Errorf("remove published authority temporary link: %w", err)
	}
	if err := syncPrivateDirectory(parent); err != nil {
		return nil, build, fmt.Errorf("sync authority temporary unlink: %w", err)
	}
	if err := syncCutoverDatabase(s.coreStorePath); err != nil {
		return nil, build, err
	}
	store, err = corestore.Open(ctx, s.liveCoreStoreOptions(&head))
	if err != nil {
		return nil, build, fmt.Errorf("reopen published daemon authority: %w", err)
	}
	loaded, loadedDoc, err := loadCoreCutoverManifest(ctx, store)
	if err != nil {
		_ = store.Close()
		return nil, build, err
	}
	if loaded.CutoverID != manifest.CutoverID || loaded.Status != manifest.Status {
		_ = store.Close()
		return nil, build, fmt.Errorf("published daemon authority cutover manifest mismatch")
	}
	build.manifest, build.doc = loaded, loadedDoc
	return store, build, nil
}

func (s *Server) populateLegacyCutover(ctx context.Context, store *corestore.Store, manifest *coreCutoverManifest) error {
	// openCoreStore holds the state-root persistence lock while the
	// unpublished authority is populated. Resolve any legacy decision-file
	// rotation crash under that same lock before either the market importer or
	// the sealing manifest scans archives and live tails. Otherwise an archive
	// published before the live-tail swap could duplicate the unchanged live
	// prefix, and post-swap evidence would not have been proven converged.
	if err := s.recoverLegacyDecisionRotations(ctx); err != nil {
		return fmt.Errorf("recover legacy decision rotation before cutover: %w", err)
	}
	stateReport, err := prepareDaemonStateCutover(ctx, store)
	if err != nil {
		return fmt.Errorf("import daemon safety state: %w", err)
	}
	manifest.Counts.ImportedCapitalEvents = stateReport.CapitalEventsImported
	manifest.Counts.ImportedGovernanceEvents = stateReport.GovernanceEventsImported
	manifest.Counts.SkippedGovernanceEvents = stateReport.GovernanceEventsSkipped
	for _, source := range stateReport.Sources {
		captured, err := captureCutoverSource(source.Kind, "safety_state", coreCutoverActionSeal, source.Path, source.SHA256, source.Bytes, source.Records, source.Status)
		if err != nil {
			return err
		}
		if stateReport.GovernanceEventsSkipped > 0 && source.Kind == "risk_policy_events" {
			captured.SkippedClass = "discarded_governance_rows_clean_epoch"
		}
		mergeCutoverSource(manifest, captured)
	}

	statementReport, err := rebuildStatementProjectionForCutover(ctx, store, s.nowUTC())
	if err != nil {
		return fmt.Errorf("rebuild retained statement projection: %w", err)
	}
	manifest.Counts.StatementFiles = statementReport.FileCount
	manifest.Counts.StatementRows = statementReport.EquityWinnerRows
	manifest.StatementSourceSetSHA256 = statementReport.SourceSetSHA256
	manifest.StatementProjectionSHA256 = statementReport.ProjectionSHA256
	for _, source := range statementReport.Sources {
		if err := addReportedCutoverSource(manifest, "flex_statement", "broker_evidence", coreCutoverActionRetain, source.Path, source.SHA256, source.Bytes, source.EquityRows, source.Status); err != nil {
			return err
		}
	}

	marketReport, err := importLegacyMarketObservations(ctx, store)
	if err != nil {
		return fmt.Errorf("import retained market observations: %w", err)
	}
	if marketReport.StateDocuments != 0 {
		return fmt.Errorf("legacy market import seeded %d current state documents", marketReport.StateDocuments)
	}
	manifest.Counts.MarketObservationFiles = marketReport.ImportedFiles
	manifest.Counts.MarketObservationRecords = marketReport.ImportedRecords
	manifest.Counts.MarketObservations = marketReport.Observations
	for _, source := range marketReport.Artifacts {
		if err := addReportedCutoverSource(manifest, source.Kind, "retained_market_observation", coreCutoverActionSeal, source.Path, source.SHA256, source.Bytes, source.Records, source.Status); err != nil {
			return err
		}
	}
	if err := s.populateLegacyTradingCutover(ctx, store, manifest); err != nil {
		return err
	}

	return s.addSkippedLegacySources(manifest)
}

func (s *Server) populateLegacyTradingCutover(ctx context.Context, store *corestore.Store, manifest *coreCutoverManifest) error {
	orderPath, purgePath, err := s.legacyTradingPaths()
	if err != nil {
		return err
	}
	orderBefore, err := captureCutoverSource("order_journal", "trading_safety", coreCutoverActionSeal, orderPath, "", 0, 0, "")
	if err != nil {
		return err
	}
	purgeBefore, err := captureCutoverSource("purge_ledger", "trading_safety", coreCutoverActionSeal, purgePath, "", 0, 0, "")
	if err != nil {
		return err
	}
	tradingReport, err := importLegacyTradingAuthority(ctx, store, orderPath, purgePath)
	if err != nil {
		return fmt.Errorf("import trading safety authority: %w", err)
	}
	wantOrderDigest := orderBefore.SHA256
	if orderBefore.Status == "missing" {
		empty := sha256.Sum256(nil)
		wantOrderDigest = hex.EncodeToString(empty[:])
	}
	if strings.TrimPrefix(tradingReport.Orders.SourceFingerprint, "sha256:") != wantOrderDigest {
		return fmt.Errorf("legacy order journal changed during cutover")
	}
	if err := verifyCapturedCutoverSourceUnchanged(orderBefore); err != nil {
		return err
	}
	if err := verifyCapturedCutoverSourceUnchanged(purgeBefore); err != nil {
		return err
	}
	mergeCutoverSource(manifest, orderBefore)
	mergeCutoverSource(manifest, purgeBefore)
	manifest.Counts.RetainedOrderEvents = tradingReport.Orders.RetainedEventCount
	manifest.Counts.RetainedOrderChains = tradingReport.Orders.RetainedChainCount
	manifest.Counts.ConsumedPreviewTokens = tradingReport.Orders.ConsumedTokenCount
	manifest.Counts.PurgeRows = tradingReport.Purge.ActiveRows
	manifest.Counts.PurgeFillCursors = tradingReport.Purge.FillCursors
	return nil
}

func initializeEmptyTradingReadiness(ctx context.Context, store *corestore.Store) error {
	if _, ok, err := store.GetStateDocument(ctx, tradingReadinessStateScope, tradingReadinessStateKind); err != nil {
		return err
	} else if ok {
		return nil
	}
	_, err := writeInitialState(ctx, store, tradingReadinessStateKind, tradingReadinessFile{Version: tradingReadinessFileVersion})
	return err
}

func (s *Server) attachCoreStoreAdapters(ctx context.Context, store *corestore.Store) error {
	s.coreStore = store
	if err := s.bindAuthoritativeDaemonState(ctx, store); err != nil {
		return fmt.Errorf("attach daemon state authority: %w", err)
	}
	if err := s.initializeLockedOrderSignerAndReadiness(ctx, store); err != nil {
		return err
	}
	if err := s.attachCoreOrderAuthority(ctx, store); err != nil {
		return fmt.Errorf("attach order authority: %w", err)
	}
	if err := s.attachCoreMarketAuthority(store); err != nil {
		return fmt.Errorf("attach market authority: %w", err)
	}
	if err := s.attachProposalOpportunityAuthority(ctx, store); err != nil {
		return err
	}
	return nil
}

func verifyCoreStoreForPublication(ctx context.Context, store *corestore.Store) error {
	report, err := store.CheckIntegrity(ctx)
	if err != nil {
		return fmt.Errorf("check daemon authority integrity: %w", err)
	}
	if !report.OK() {
		return fmt.Errorf("daemon authority integrity check failed")
	}
	checkpoint, err := store.Checkpoint(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint daemon authority: %w", err)
	}
	if checkpoint.Busy != 0 || checkpoint.LogFrames != 0 {
		return fmt.Errorf("daemon authority WAL did not truncate: busy=%d frames=%d", checkpoint.Busy, checkpoint.LogFrames)
	}
	return nil
}

func writeCoreCutoverManifest(ctx context.Context, store *corestore.Store, manifest coreCutoverManifest, expectedRevision int64) (corestore.StateDocument, error) {
	raw, err := json.Marshal(manifest)
	if err != nil {
		return corestore.StateDocument{}, fmt.Errorf("encode cutover manifest: %w", err)
	}
	doc, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: coreCutoverManifestKind, ExpectedRevision: expectedRevision, JSON: raw,
	})
	if err != nil {
		return corestore.StateDocument{}, fmt.Errorf("write cutover manifest: %w", err)
	}
	return doc, nil
}

func loadCoreCutoverManifest(ctx context.Context, store *corestore.Store) (coreCutoverManifest, corestore.StateDocument, error) {
	var manifest coreCutoverManifest
	doc, ok, err := store.GetStateDocument(ctx, daemonStateScope, coreCutoverManifestKind)
	if err != nil {
		return manifest, doc, err
	}
	if !ok {
		return manifest, doc, fmt.Errorf("daemon authority has no cutover manifest")
	}
	if err := json.Unmarshal(doc.JSON, &manifest); err != nil {
		return manifest, doc, fmt.Errorf("decode cutover manifest: %w", err)
	}
	if manifest.Version != coreCutoverManifestVersion || strings.TrimSpace(manifest.CutoverID) == "" || manifest.CreatedAt.IsZero() {
		return manifest, doc, fmt.Errorf("daemon authority cutover manifest is invalid")
	}
	switch manifest.Status {
	case coreCutoverStatusPending, coreCutoverStatusBackup, coreCutoverStatusSealed, coreCutoverStatusFresh:
	default:
		return manifest, doc, fmt.Errorf("daemon authority cutover status %q is invalid", manifest.Status)
	}
	return manifest, doc, nil
}

func (s *Server) finishCoreCutover(ctx context.Context, store *corestore.Store, build coreCutoverBuild) (coreCutoverBuild, error) {
	if build.manifest.Status == coreCutoverStatusFresh {
		return build, nil
	}
	if !build.manifest.ImportedLegacy {
		return build, fmt.Errorf("daemon authority cutover status %q requires a legacy import", build.manifest.Status)
	}
	if build.manifest.Status == coreCutoverStatusPending {
		manifest, err := s.sealLegacyCutoverSources(build.manifest)
		if err != nil {
			return build, err
		}
		head, err := store.AuthorityHead(ctx)
		if err != nil {
			return build, err
		}
		// Persist a resumable state before creating the final backup. A
		// crash here can recreate or verify the backup without reclassifying
		// an unfinished cutover as complete.
		manifest.Status = coreCutoverStatusBackup
		manifest.CompletedAt = s.nowUTC()
		manifest.MinimumFinalBackupHead = head
		doc, err := writeCoreCutoverManifest(ctx, store, manifest, build.doc.Revision)
		if err != nil {
			return build, err
		}
		build.manifest, build.doc = manifest, doc
	}
	if build.manifest.Status == coreCutoverStatusBackup {
		if err := ensureCutoverBackup(ctx, store, build.manifest.FinalBackupPath, build.manifest.MinimumFinalBackupHead); err != nil {
			return build, err
		}
		manifest := build.manifest
		manifest.Status = coreCutoverStatusSealed
		doc, err := writeCoreCutoverManifest(ctx, store, manifest, build.doc.Revision)
		if err != nil {
			return build, err
		}
		build.manifest, build.doc = manifest, doc
	}
	if build.manifest.Status != coreCutoverStatusSealed {
		return build, fmt.Errorf("daemon authority cutover is not sealed")
	}
	if err := ensureCutoverBackup(ctx, store, build.manifest.FinalBackupPath, build.manifest.MinimumFinalBackupHead); err != nil {
		return build, err
	}
	return build, nil
}

// reconcilePendingStatementCutover makes the retained Flex source snapshot,
// its typed projection, and the still-pending manifest one committed boundary.
// Once sealing has begun, later statement changes are ordinary runtime
// evidence refreshes and must not rewrite the historical cutover snapshot.
func (s *Server) reconcilePendingStatementCutover(ctx context.Context, store *corestore.Store, build coreCutoverBuild) (coreCutoverBuild, error) {
	if build.manifest.Status != coreCutoverStatusPending || !build.manifest.ImportedLegacy {
		return build, nil
	}
	report, err := rebuildStatementProjectionForCutover(ctx, store, s.nowUTC())
	if err != nil {
		return build, fmt.Errorf("reconcile retained statement projection before sealing: %w", err)
	}
	manifest := build.manifest
	manifest.Counts.StatementFiles = report.FileCount
	manifest.Counts.StatementRows = report.EquityWinnerRows
	manifest.StatementSourceSetSHA256 = report.SourceSetSHA256
	manifest.StatementProjectionSHA256 = report.ProjectionSHA256
	sources := make([]coreCutoverSource, 0, len(manifest.Sources)+len(report.Sources))
	for _, source := range manifest.Sources {
		if source.Kind != "flex_statement" {
			sources = append(sources, source)
		}
	}
	manifest.Sources = sources
	for _, source := range report.Sources {
		if err := addReportedCutoverSource(&manifest, "flex_statement", "broker_evidence", coreCutoverActionRetain, source.Path, source.SHA256, source.Bytes, source.EquityRows, source.Status); err != nil {
			return build, err
		}
	}
	doc, err := writeCoreCutoverManifest(ctx, store, manifest, build.doc.Revision)
	if err != nil {
		return build, err
	}
	return coreCutoverBuild{manifest: manifest, doc: doc}, nil
}

func ensureCutoverBackup(ctx context.Context, store *corestore.Store, path string, minimum corestore.AuthorityHead) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("final cutover backup path is empty")
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if _, err := store.Backup(ctx, path); err != nil {
			return fmt.Errorf("create final cutover backup: %w", err)
		}
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect final cutover backup: %w", err)
	}
	// The cutover backup is sealed at the version that was current when the
	// cutover ran, so it is verified against its own version rather than
	// today's. Demanding equality with the current version would turn every
	// later migration into a permanent startup failure.
	if _, err := corestore.VerifySealedBackup(ctx, path, minimum); err != nil {
		return fmt.Errorf("verify final cutover backup: %w", err)
	}
	return nil
}

func (s *Server) addSkippedLegacySources(manifest *coreCutoverManifest) error {
	stateNames := []struct {
		kind, class, name string
	}{
		{"regime_decisions", "discarded_decision_history", "regime-decisions.jsonl"},
		{"rules_decisions", "discarded_decision_history", "rules-decisions.jsonl"},
		// The manifest kind and the file name both stay pre-rename: this entry
		// names retained evidence the cutover seals under the name it was
		// written with. legacyStressDecisionsFile is the single definition.
		{"canary_decisions", "discarded_decision_history", legacyStressDecisionsFile},
		{"proposal_current", "discarded_decision_state", "trade-proposals-current.json"},
		{"proposal_events", "discarded_decision_history", "trade-proposals.jsonl"},
		{"proposal_outcomes", "discarded_decision_history", "trade-proposal-outcomes.jsonl"},
		{"opportunity_current", "discarded_decision_state", "opportunities-current.json"},
		{"opportunity_events", "discarded_decision_history", "opportunities.jsonl"},
		{"brief_baselines", "discarded_presentation_baseline", briefStateFile},
		{"trading_readiness", "reset_safety_proof", "trading-readiness.json"},
		{"legacy_preview_key", "rotated_secret", "order-preview-key"},
		{"legacy_history_index", "discarded_derived_index", "history.db"},
		{"legacy_history_wal", "discarded_derived_index", "history.db-wal"},
		{"legacy_history_shm", "discarded_derived_index", "history.db-shm"},
	}
	for _, item := range stateNames {
		path, err := defaultTradingStatePath(item.name)
		if err != nil {
			return err
		}
		source, err := captureCutoverSource(item.kind, item.class, coreCutoverActionSeal, path, "", 0, 0, "")
		if err != nil {
			return err
		}
		mergeCutoverSource(manifest, source)
	}
	contractDir, err := ibkrlib.DefaultContractStoreDir()
	if err != nil {
		return fmt.Errorf("resolve legacy contract cache: %w", err)
	}
	contractSource, err := captureCutoverSource(
		"contract_cache",
		"discarded_acceleration_cache",
		coreCutoverActionSeal,
		filepath.Join(contractDir, "contracts.json"),
		"",
		0,
		0,
		"",
	)
	if err != nil {
		return err
	}
	mergeCutoverSource(manifest, contractSource)
	rotated, err := defaultTradingStatePath("rotated")
	if err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(rotated, "*"))
	if err != nil {
		return err
	}
	sort.Strings(paths)
	for _, path := range paths {
		source, err := captureCutoverSource("rotated_legacy_evidence", "sealed_legacy_evidence", coreCutoverActionSeal, path, "", 0, 0, "")
		if err != nil {
			return err
		}
		mergeCutoverSource(manifest, source)
	}
	return nil
}

func addReportedCutoverSource(manifest *coreCutoverManifest, kind, class, action, path, digest string, bytes int64, records int, status string) error {
	source, err := captureCutoverSource(kind, class, action, path, digest, bytes, records, status)
	if err != nil {
		return err
	}
	mergeCutoverSource(manifest, source)
	return nil
}

func captureCutoverSource(kind, class, action, path, expectedDigest string, expectedBytes int64, records int, reportedStatus string) (coreCutoverSource, error) {
	source := coreCutoverSource{Kind: kind, Class: class, Action: action, Path: path, Records: records}
	if strings.HasPrefix(class, "discarded_") || strings.HasPrefix(class, "reset_") || class == "sealed_legacy_evidence" {
		source.SkippedClass = class
	} else {
		source.PreservedClass = class
	}
	if strings.TrimSpace(path) == "" {
		return source, fmt.Errorf("cutover source %s has an empty path", kind)
	}
	if strings.ContainsAny(path, "*?[") && reportedStatus == "missing" {
		source.Status = "missing"
		return source, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return source, err
	}
	source.Path = abs
	info, err := os.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		if expectedDigest != "" || expectedBytes != 0 || (reportedStatus != "" && reportedStatus != "missing") {
			return source, fmt.Errorf("cutover source %s disappeared after validation", abs)
		}
		source.Status = "missing"
		return source, nil
	}
	if err != nil {
		return source, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return source, fmt.Errorf("cutover source %s is not a regular non-symlink file", abs)
	}
	digest, size, err := hashRegularFile(abs, info)
	if err != nil {
		return source, err
	}
	expectedDigest = strings.TrimPrefix(expectedDigest, "sha256:")
	if expectedDigest != "" && !strings.EqualFold(expectedDigest, digest) {
		return source, fmt.Errorf("cutover source %s digest changed after validation", abs)
	}
	if expectedBytes != 0 && expectedBytes != size {
		return source, fmt.Errorf("cutover source %s size changed after validation", abs)
	}
	source.Bytes, source.SHA256 = size, digest
	if action == coreCutoverActionRetain {
		source.Status = "retained"
	} else {
		source.Status = "pending_seal"
	}
	return source, nil
}

func verifyCapturedCutoverSourceUnchanged(source coreCutoverSource) error {
	if source.Status == "missing" {
		if _, err := os.Lstat(source.Path); !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("missing cutover source %s appeared during import", source.Path)
		}
		return nil
	}
	_, err := captureCutoverSource(source.Kind, source.Class, source.Action, source.Path, source.SHA256, source.Bytes, source.Records, source.Status)
	return err
}

func mergeCutoverSource(manifest *coreCutoverManifest, source coreCutoverSource) {
	for i := range manifest.Sources {
		if manifest.Sources[i].Path != source.Path {
			continue
		}
		if manifest.Sources[i].Action == coreCutoverActionRetain {
			return
		}
		if source.Action == coreCutoverActionRetain {
			manifest.Sources[i] = source
			return
		}
		if source.Records > manifest.Sources[i].Records {
			manifest.Sources[i].Records = source.Records
		}
		if source.PreservedClass != "" {
			manifest.Sources[i].PreservedClass = source.PreservedClass
		}
		if source.SkippedClass != "" {
			manifest.Sources[i].SkippedClass = source.SkippedClass
		}
		return
	}
	manifest.Sources = append(manifest.Sources, source)
}

func (s *Server) sealLegacyCutoverSources(manifest coreCutoverManifest) (coreCutoverManifest, error) {
	root := filepath.Join(filepath.Dir(s.coreStorePath), "legacy-sealed", manifest.CutoverID)
	if err := ensurePrivateStateDir(filepath.Join(root, "placeholder")); err != nil {
		return manifest, err
	}
	for i := range manifest.Sources {
		source := &manifest.Sources[i]
		if source.Action != coreCutoverActionSeal || source.Status == "missing" || source.Status == "sealed" {
			continue
		}
		nameDigest := sha256.Sum256([]byte(source.Path))
		base := filepath.Base(source.Path)
		if base == "." || base == string(filepath.Separator) || base == "" {
			base = "source"
		}
		destination := filepath.Join(root, hex.EncodeToString(nameDigest[:6])+"-"+base)
		source.Destination = destination
		if err := sealOneCutoverSource(*source); err != nil {
			return manifest, err
		}
		source.Status = "sealed"
		if isLegacyBlockerPath(source.Path) {
			if err := ensureLegacyDirectoryBlocker(source.Path); err != nil {
				return manifest, err
			}
		}
	}
	for _, path := range legacyBlockerPaths() {
		if err := ensureLegacyDirectoryBlocker(path); err != nil {
			return manifest, err
		}
	}
	return manifest, nil
}

func sealOneCutoverSource(source coreCutoverSource) error {
	if source.SHA256 == "" {
		return fmt.Errorf("refuse to seal unhashed source %s", source.Path)
	}
	if info, err := os.Lstat(source.Destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("sealed destination %s is not a regular file", source.Destination)
		}
		digest, size, err := hashRegularFile(source.Destination, info)
		if err != nil || digest != source.SHA256 || size != source.Bytes {
			return fmt.Errorf("sealed destination %s does not match manifest", source.Destination)
		}
		if err := syncSealedFile(source.Destination); err != nil {
			return fmt.Errorf("sync existing sealed destination %s: %w", source.Destination, err)
		}
		if original, err := os.Lstat(source.Path); err == nil && original.Mode().IsRegular() {
			if digest, size, hashErr := hashRegularFile(source.Path, original); hashErr != nil || digest != source.SHA256 || size != source.Bytes {
				return fmt.Errorf("legacy source %s changed after sealed copy publication", source.Path)
			}
			if err := os.Remove(source.Path); err != nil {
				return fmt.Errorf("remove duplicated sealed source %s: %w", source.Path, err)
			}
			if err := syncPrivateDirectory(filepath.Dir(source.Path)); err != nil {
				return err
			}
		} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	info, err := os.Lstat(source.Path)
	if err != nil {
		return fmt.Errorf("inspect source before sealing: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("source %s changed type before sealing", source.Path)
	}
	digest, size, err := hashRegularFile(source.Path, info)
	if err != nil || digest != source.SHA256 || size != source.Bytes {
		return fmt.Errorf("source %s changed before sealing", source.Path)
	}
	if err := os.MkdirAll(filepath.Dir(source.Destination), 0o700); err != nil {
		return err
	}
	if err := os.Link(source.Path, source.Destination); err == nil {
		if err := syncSealedFile(source.Destination); err != nil {
			return err
		}
		if err := os.Remove(source.Path); err != nil {
			return fmt.Errorf("unlink sealed legacy source %s: %w", source.Path, err)
		}
		return syncPrivateDirectory(filepath.Dir(source.Path))
	} else if !errors.Is(err, syscall.EXDEV) {
		return fmt.Errorf("seal source %s without clobber: %w", source.Path, err)
	}
	if err := copySealedFile(source.Path, source.Destination, source.SHA256, source.Bytes); err != nil {
		return err
	}
	if err := os.Remove(source.Path); err != nil {
		return fmt.Errorf("remove copied legacy source %s: %w", source.Path, err)
	}
	return syncPrivateDirectory(filepath.Dir(source.Path))
}

func copySealedFile(source, destination, expectedDigest string, expectedBytes int64) error {
	temp, err := os.CreateTemp(filepath.Dir(destination), ".seal-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() { _ = os.Remove(tempPath) }()
	in, err := os.Open(source)
	if err != nil {
		_ = temp.Close()
		return err
	}
	_, copyErr := io.Copy(temp, in)
	closeInErr := in.Close()
	if copyErr == nil {
		copyErr = closeInErr
	}
	if copyErr == nil {
		copyErr = temp.Chmod(0o600)
	}
	if copyErr == nil {
		copyErr = temp.Sync()
	}
	closeOutErr := temp.Close()
	if copyErr == nil {
		copyErr = closeOutErr
	}
	if copyErr != nil {
		return copyErr
	}
	info, err := os.Lstat(tempPath)
	if err != nil {
		return err
	}
	digest, size, err := hashRegularFile(tempPath, info)
	if err != nil || digest != expectedDigest || size != expectedBytes {
		return fmt.Errorf("copied sealed source verification failed")
	}
	if err := os.Link(tempPath, destination); err != nil {
		return fmt.Errorf("publish copied sealed source without clobber: %w", err)
	}
	if err := syncSealedFile(destination); err != nil {
		return err
	}
	if err := os.Remove(tempPath); err != nil {
		return err
	}
	return syncPrivateDirectory(filepath.Dir(destination))
}

func hashRegularFile(path string, expected fs.FileInfo) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	opened, err := f.Stat()
	if err != nil || !opened.Mode().IsRegular() || (expected != nil && !os.SameFile(expected, opened)) {
		_ = f.Close()
		if err == nil {
			err = fmt.Errorf("file identity changed while opening")
		}
		return "", 0, err
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", 0, err
	}
	if n != opened.Size() {
		return "", 0, fmt.Errorf("file changed while hashing")
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func syncSealedFile(path string) error {
	if err := syncPrivateFile(path); err != nil {
		return err
	}
	return syncPrivateDirectory(filepath.Dir(path))
}

func syncCutoverDatabase(path string) error {
	for _, sidecar := range []string{path + "-wal", path + "-shm"} {
		if info, err := os.Lstat(sidecar); err == nil {
			if !info.Mode().IsRegular() || info.Size() != 0 {
				return fmt.Errorf("refuse main-file publication with SQLite sidecar %s", sidecar)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := syncPrivateFile(path); err != nil {
		return err
	}
	return syncPrivateDirectory(filepath.Dir(path))
}

func syncPrivateFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncPrivateDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		_ = dir.Close()
		return err
	}
	return dir.Close()
}

func ensureLegacyDirectoryBlocker(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create legacy downgrade blocker %s: %w", path, err)
		}
		return syncPrivateDirectory(filepath.Dir(path))
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("legacy downgrade blocker %s is not a directory", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("legacy downgrade blocker %s is not private", path)
	}
	return nil
}

func legacyBlockerPaths() []string {
	var paths []string
	for _, name := range []string{"order-preview-key", "order-journal.jsonl"} {
		if path, err := defaultTradingStatePath(name); err == nil {
			paths = append(paths, path)
		}
	}
	return paths
}

func isLegacyBlockerPath(path string) bool {
	return slices.Contains(legacyBlockerPaths(), path)
}

func (s *Server) legacyTradingPaths() (string, string, error) {
	orderPath, err := defaultOrderJournalPath()
	if err != nil {
		return "", "", err
	}
	purgePath, err := defaultPurgeLedgerPath()
	if err != nil {
		return "", "", err
	}
	return orderPath, purgePath, nil
}

func newCoreCutoverID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate cutover id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Server) nowUTC() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}
