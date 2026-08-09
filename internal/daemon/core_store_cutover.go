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
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

const (
	v3BootstrapKind           = "v3_bootstrap"
	legacyCutoverManifestKind = "sqlite_authority_cutover_v1"
)

type v3Bootstrap struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
}

// createAndPublishCoreStore initializes a fresh v3 authority out of place,
// verifies it, establishes the rollback floor, and publishes it atomically.
// Existing v1/v2 file authority is never interpreted here: the stable 2.x
// daemon is the only supported bridge into SQLite.
func (s *Server) createAndPublishCoreStore(ctx context.Context) (*corestore.Store, error) {
	if s.productionStateDatabase {
		if err := rejectUnbridgedFileAuthority(); err != nil {
			return nil, err
		}
	}
	id, err := newCoreCutoverID()
	if err != nil {
		return nil, err
	}
	parent := filepath.Dir(s.coreStorePath)
	tempPath := s.coreStorePath + ".v3-" + id + ".tmp"
	if _, err := os.Lstat(tempPath); err == nil {
		return nil, fmt.Errorf("v3 bootstrap temporary database already exists: %s", tempPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspect v3 bootstrap database: %w", err)
	}
	store, err := corestore.Open(ctx, corestore.Options{Path: tempPath})
	if err != nil {
		return nil, fmt.Errorf("create unpublished v3 authority: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = store.Close()
		}
	}()
	if err := initializeFreshDaemonState(ctx, store); err != nil {
		return nil, err
	}
	if err := initializeFreshTradingAuthority(ctx, store); err != nil {
		return nil, fmt.Errorf("initialize fresh trading authority: %w", err)
	}
	if err := store.ReplaceStatementProjection(ctx, statementProjectionScope, nil, nil); err != nil {
		return nil, fmt.Errorf("initialize fresh statement projection: %w", err)
	}
	if err := initializeCleanProposalOpportunityAuthority(ctx, store); err != nil {
		return nil, err
	}
	if _, err := writeInitialState(ctx, store, v3BootstrapKind, v3Bootstrap{Version: 1, CreatedAt: s.nowUTC()}); err != nil {
		return nil, err
	}
	if err := verifyCoreStoreForPublication(ctx, store); err != nil {
		return nil, err
	}
	head, err := store.AuthorityHead(ctx)
	if err != nil {
		return nil, fmt.Errorf("read unpublished v3 authority head: %w", err)
	}
	if err := store.Close(); err != nil {
		return nil, fmt.Errorf("close unpublished v3 authority: %w", err)
	}
	closed = true
	if err := syncCutoverDatabase(tempPath); err != nil {
		return nil, err
	}
	if err := writeAuthorityWatermark(s.coreStorePath+".head", head); err != nil {
		return nil, fmt.Errorf("publish initial authority watermark: %w", err)
	}
	if err := os.Link(tempPath, s.coreStorePath); err != nil {
		return nil, fmt.Errorf("publish v3 authority without clobber: %w", err)
	}
	if err := syncPrivateDirectory(parent); err != nil {
		return nil, err
	}
	if err := os.Remove(tempPath); err != nil {
		return nil, fmt.Errorf("remove published v3 temporary link: %w", err)
	}
	if err := syncPrivateDirectory(parent); err != nil {
		return nil, err
	}
	if err := syncCutoverDatabase(s.coreStorePath); err != nil {
		return nil, err
	}
	store, err = corestore.Open(ctx, s.liveCoreStoreOptions(&head))
	if err != nil {
		return nil, fmt.Errorf("reopen published v3 authority: %w", err)
	}
	return store, nil
}

func rejectUnbridgedFileAuthority() error {
	settingsPath, err := defaultPlatformSettingsPath()
	if err != nil {
		return err
	}
	paths := []string{settingsPath}
	for _, name := range []string{
		"order-journal.jsonl", "order-preview-key", riskCapitalStateFile,
		capitalEventsJournalFile, riskPolicyJournalFile, governanceNudgeStateFile,
		rulesRegimeStageFile,
	} {
		path, err := defaultTradingStatePath(name)
		if err != nil {
			return err
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("v3 found pre-SQLite authority at %s: start the latest stable 2.x daemon once to bridge it before starting v3", path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("inspect pre-v3 authority %s: %w", path, err)
		}
	}
	return nil
}

// validateMajorBridge accepts databases created by v3 or completed by stable
// 2.x. An interrupted 2.x file cutover must be resumed by 2.x, which still
// owns those legacy codecs and their sealing protocol.
func (s *Server) validateMajorBridge(ctx context.Context, store *corestore.Store) error {
	if !s.productionStateDatabase {
		return nil
	}
	if doc, ok, err := store.GetStateDocument(ctx, daemonStateScope, v3BootstrapKind); err != nil {
		return err
	} else if ok {
		var marker v3Bootstrap
		if json.Unmarshal(doc.JSON, &marker) != nil || marker.Version != 1 || marker.CreatedAt.IsZero() {
			return fmt.Errorf("v3 authority bootstrap marker is invalid")
		}
		return nil
	}
	doc, ok, err := store.GetStateDocument(ctx, daemonStateScope, legacyCutoverManifestKind)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("existing daemon authority has no supported v2/v3 bridge marker")
	}
	var legacy struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(doc.JSON, &legacy) != nil || (legacy.Status != "sealed" && legacy.Status != "fresh") {
		return fmt.Errorf("v2 authority cutover is incomplete: start the latest stable 2.x daemon once before starting v3")
	}
	return nil
}

func initializeFreshDaemonState(ctx context.Context, core *corestore.Store) error {
	if core == nil {
		return fmt.Errorf("fresh SQLite authority is unavailable")
	}
	defaults := []struct {
		kind  string
		value any
	}{
		{stateKindPlatformSettings, platformSettingsData{Version: platformSettingsDocVersion}},
		{stateKindRiskCapital, riskCapitalSQLiteDocument{Version: riskCapitalSQLiteDocVer, State: riskCapitalStateFileV1{Version: riskCapitalStateVer}}},
		{stateKindNudges, nudgeStateFileV1{Version: governanceNudgeStateVersion}},
		{stateKindRulesRegimeStage, rulesRegimeStageState{Version: rulesRegimeStageStateVer}},
	}
	present := 0
	for _, item := range defaults {
		if _, ok, err := core.GetStateDocument(ctx, daemonStateScope, item.kind); err != nil {
			return fmt.Errorf("inspect fresh %s: %w", item.kind, err)
		} else if ok {
			present++
		}
	}
	if present == len(defaults) {
		return nil
	}
	if present != 0 {
		return fmt.Errorf("fresh daemon-state initialization is partial (%d/%d documents)", present, len(defaults))
	}
	head, err := core.AuthorityHead(ctx)
	if err != nil {
		return fmt.Errorf("inspect fresh authority head: %w", err)
	}
	if head.HeadGeneration != 0 || head.LastEventSeq != 0 {
		return fmt.Errorf("fresh daemon-state initialization requires an unused authority")
	}
	for _, item := range defaults {
		if _, err := writeInitialState(ctx, core, item.kind, item.value); err != nil {
			return err
		}
	}
	return nil
}

func writeInitialState(ctx context.Context, store *corestore.Store, kind string, value any) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if _, err := store.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: kind, JSON: raw}); err != nil {
		return false, fmt.Errorf("initialize %s: %w", kind, err)
	}
	return true, nil
}

func (s *Server) attachCoreStoreAdapters(ctx context.Context, store *corestore.Store) error {
	s.coreStore = store
	if err := s.bindAuthoritativeDaemonState(ctx, store); err != nil {
		return fmt.Errorf("attach daemon state authority: %w", err)
	}
	if err := s.initializeLockedOrderSigner(); err != nil {
		return err
	}
	if err := s.attachCoreOrderAuthority(ctx, store); err != nil {
		return fmt.Errorf("attach order authority: %w", err)
	}
	if err := s.attachCoreMarketAuthority(store); err != nil {
		return fmt.Errorf("attach market authority: %w", err)
	}
	return s.attachProposalOpportunityAuthority(ctx, store)
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
	if closeErr := f.Close(); err == nil {
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

func newCoreCutoverID() (string, error) {
	var raw [12]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate v3 bootstrap id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func (s *Server) nowUTC() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}
