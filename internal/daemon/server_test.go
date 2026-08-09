package daemon

import (
	"bytes"
	"context"

	"encoding/json"
	"errors"
	"fmt"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"math"

	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func shortTempDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "ibkrd-test-")
	if err != nil {
		t.Fatalf("mkdtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func mustTestValue[T any](t *testing.T, load func() (T, error)) T {
	t.Helper()
	value, err := load()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustTestNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type wshContractClient struct {
	payload string
	err     error
	detail  *ibkrlib.ContractDetailsLite
}

func (c wshContractClient) FetchWSHEarnings(context.Context, string) (string, error) {
	return c.payload, c.err
}

func (c wshContractClient) ResolveWSHStockIdentity(context.Context, string, int) (*ibkrlib.ContractDetailsLite, error) {
	return c.detail, c.err
}

func TestWSHProviderAndIdentityContracts(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	for _, payload := range []string{
		`[{"event_type":"wshe_ed","data":{"earnings_date":"20260810","time_of_day":"AFTER MARKET"}}]`,
		`{"events":[{"event_code":"wsh_ed","date":"2026-08-10","time":"AMC"}]}`,
	} {
		got, err := parseWSHEarningsPayload([]byte(payload), now)
		if err != nil || got.Status != rpc.EarningsStatusDate || got.Entry.Date != "2026-08-10" || got.Entry.TimeOfDay != "amc" {
			t.Fatalf("valid WSH payload result = %+v, err=%v", got, err)
		}
	}
	for _, payload := range []string{
		`[] {}`,
		`{"data":[]}`,
		`[{"earnings_date":"20260810","date":"2026-08-11"}]`,
	} {
		got, err := parseWSHEarningsPayload([]byte(payload), now)
		if !errors.Is(err, errWSHEarningsPayloadInvalid) || got.Status != rpc.EarningsStatusFormatChange || got.Failure == nil || got.Failure.Retryable {
			t.Fatalf("invalid WSH payload result = %+v, err=%v", got, err)
		}
	}

	for _, tc := range []struct {
		err       error
		status    string
		code      string
		stage     string
		retryable bool
	}{
		{&ibkrlib.WSHError{Kind: ibkrlib.WSHErrorEntitlementRequired, Operation: "event_data"}, rpc.EarningsStatusTransportFailure, rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false},
		{&ibkrlib.WSHError{Kind: ibkrlib.WSHErrorMalformedResponse, Operation: "event_data"}, rpc.EarningsStatusFormatChange, rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHDecode, false},
		{context.DeadlineExceeded, rpc.EarningsStatusTransportFailure, rpc.SourceFailureTimeout, rpc.SourceFailureStageWSHEvent, true},
	} {
		got := classifyWSHEarningsError(tc.err, now)
		if got.Status != tc.status || got.Failure == nil || got.Failure.Code != tc.code || got.Failure.Stage != tc.stage || got.Failure.Retryable != tc.retryable {
			t.Fatalf("WSH error %v classified as %+v", tc.err, got)
		}
	}

	for _, tc := range []struct {
		detail  *ibkrlib.ContractDetailsLite
		outcome string
		fails   bool
	}{
		{&ibkrlib.ContractDetailsLite{ConID: 42, SecType: "STK", StockType: "COMMON"}, earningsIdentityIssuer, false},
		{&ibkrlib.ContractDetailsLite{ConID: 42, SecType: "STK", StockType: "ETF"}, earningsIdentityNotApplicable, false},
		{&ibkrlib.ContractDetailsLite{ConID: 43, SecType: "STK", StockType: "COMMON"}, earningsIdentityUnknown, true},
	} {
		got, err := fetchEarningsIdentityFrom(t.Context(), "SYNTH", 42, now, wshContractClient{detail: tc.detail})
		if got.Outcome != tc.outcome || (err != nil) != tc.fails || (got.Failure != nil) != tc.fails {
			t.Fatalf("WSH identity %+v result = %+v, err=%v", tc.detail, got, err)
		}
	}

	got, err := fetchWSHEarningsProviderFrom(t.Context(), "SYNTH", now, wshContractClient{payload: `[{"earnings_date":"20260810"}]`})
	if err != nil || got.Status != rpc.EarningsStatusDate {
		t.Fatalf("WSH provider result = %+v, err=%v", got, err)
	}
}

func TestDispatchMethodsMatchRPCTimingCatalog(t *testing.T) {
	t.Parallel()

	constants := rpcMethodConstants(t)
	dispatched := dispatchMethodValues(t, constants)
	catalogued := make(map[string]bool)
	for _, timing := range rpc.MethodTimings() {
		catalogued[timing.Method] = true
	}
	for method := range dispatched {
		if !catalogued[method] {
			t.Errorf("dispatched method %q has no rpc timing entry", method)
		}
	}
	for method := range catalogued {
		if !dispatched[method] {
			t.Errorf("rpc timing entry %q has no daemon dispatch case", method)
		}
	}
}

func rpcMethodConstants(t *testing.T) map[string]string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join("..", "rpc", "*.go"))
	if err != nil {
		t.Fatalf("glob rpc files: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string]string{}
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				values, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range values.Names {
					if !strings.HasPrefix(name.Name, "Method") || i >= len(values.Values) {
						continue
					}
					lit, ok := values.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err == nil {
						out[name.Name] = value
					}
				}
			}
		}
	}
	return out
}

func dispatchMethodValues(t *testing.T, constants map[string]string) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parse server.go: %v", err)
	}
	out := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				sel, ok := expr.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "rpc" || !strings.HasPrefix(sel.Sel.Name, "Method") {
					continue
				}
				value, ok := constants[sel.Sel.Name]
				if !ok {
					t.Errorf("dispatch uses unresolved rpc.%s", sel.Sel.Name)
					continue
				}
				out[value] = true
			}
			return true
		})
	}
	return out
}

func TestStartOpensSocketBeforeGatewayHandshake(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)
	sockPath := filepath.Join(dir, "ibkrd.sock")

	tlsFalse := false
	cfg := &config.Resolved{
		Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99), TLS: &tlsFalse},
	}

	cfg.Daemon.SetIdleTimeout(0)

	srv := New(Options{
		Config:            cfg,
		SocketPath:        sockPath,
		Version:           "test",
		Logger:            NewLogger(&bytes.Buffer{}, "error"),
		StateDatabasePath: filepath.Join(dir, "daemon.db"),
	})

	srv.orderJournal = newOrderJournalStore(filepath.Join(dir, "order-journal.jsonl"))
	acceptCheck := make(chan error, 1)
	srv.initialAcceptLoopStartedForTest = func() {
		srv.mu.Lock()
		inFlight := srv.connectInFlight
		srv.mu.Unlock()
		if !inFlight {
			acceptCheck <- errors.New("RPC accept loop exposed before initial connection claimed the in-flight gate")
			return
		}
		acceptCheck <- nil
	}
	startCheck := make(chan error, 1)
	srv.attempterFactory = func(_ discover.Endpoint) connectAttempter {
		return &fakeAttempter{
			blockUntilCtxDone: true,
			startCheck: func() error {
				fi, err := os.Stat(sockPath)
				if err != nil {
					return err
				}
				if fi.Mode()&os.ModeSocket == 0 {
					return errors.New("daemon socket was not published before gateway handshake")
				}
				return nil
			},
			startCheckResult: startCheck,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startReturned := make(chan error, 1)
	go func() {
		startReturned <- srv.Start(ctx)
	}()

	select {
	case err := <-acceptCheck:
		if err != nil {
			t.Fatal(err)
		}
	case err := <-startReturned:
		t.Fatalf("Start returned before exposing the accept loop: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("accept loop was not exposed")
	}

	select {
	case err := <-startCheck:
		if err != nil {
			t.Fatalf("gateway handshake started before daemon socket was published: %v", err)
		}
	case err := <-startReturned:
		t.Fatalf("Start returned before gateway handshake: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("gateway handshake did not start")
	}

	cancel()
	select {
	case <-startReturned:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return within 3s of cancellation")
	}
	srv.Stop()
}

type fakeAttempter struct {
	connectOk         bool
	startErr          error
	lastError         string
	blockUntilCtxDone bool
	startCheck        func() error
	startCheckResult  chan<- error
	connected         atomic.Bool
	stopCalls         atomic.Int32

	setMarketDataType atomic.Int32
	requestedAccount  atomic.Value
}

func (f *fakeAttempter) Start(ctx context.Context) error {
	if f.startCheck != nil && f.startCheckResult != nil {
		f.startCheckResult <- f.startCheck()
	}
	if f.blockUntilCtxDone {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.startErr != nil {
		return f.startErr
	}
	if f.connectOk {
		f.connected.Store(true)
	}
	return nil
}
func (f *fakeAttempter) Stop() error {
	f.stopCalls.Add(1)
	f.connected.Store(false)
	return nil
}
func (f *fakeAttempter) IsConnected() bool { return f.connected.Load() }
func (f *fakeAttempter) UsingTLS() bool    { return false }
func (f *fakeAttempter) LastError() string { return f.lastError }
func (f *fakeAttempter) SetMarketDataType(t int) error {
	f.setMarketDataType.Store(int32(t))
	return nil
}
func (f *fakeAttempter) RequestAccountUpdates(account string) error {
	f.requestedAccount.Store(account)
	return nil
}
func (f *fakeAttempter) SubscribeAccountPnL(account string) error {

	return nil
}

func TestMajorBridgeAcceptsCompletedV2AndRejectsInterruptedV2(t *testing.T) {
	store, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(privateTestDir(t), "daemon.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	s := &Server{productionStateDatabase: true}
	write := func(status string, revision int64) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"status": status, "created_at": time.Now().UTC()})
		if _, err := store.CompareAndSwapStateDocument(t.Context(), corestore.StateDocumentCAS{
			ScopeKey: daemonStateScope, Kind: legacyCutoverManifestKind, ExpectedRevision: revision, JSON: raw,
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("pending_seal", 0)
	if err := s.validateMajorBridge(t.Context(), store); err == nil {
		t.Fatal("interrupted v2 cutover was accepted")
	}
	write("sealed", 1)
	if err := s.validateMajorBridge(t.Context(), store); err != nil {
		t.Fatalf("completed v2 bridge: %v", err)
	}
}

func privateTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCoreSchemaMaintenanceUpgradeResumesEveryDurableBoundaryWithoutRestampingHead(t *testing.T) {
	phases := []string{
		coreSchemaPhaseIntent,
		coreSchemaPhaseCandidate,
		coreSchemaPhaseWatermark,
		coreSchemaPhaseQuiesced,
		coreSchemaPhaseRenamed,
		coreSchemaPhaseSynced,
		coreSchemaPhaseVerified,
		coreSchemaPhaseTarget,
		coreSchemaPhaseReceipt,
		coreSchemaPhaseRetired,
		coreSchemaPhaseRetireSync,
	}
	for _, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			databasePath, source := newFakeMaintenanceSchemaAuthority(t, 2)
			watermarkPath := databasePath + ".head"
			fixedModTime := time.Unix(1_700_000_000, 123_000_000)
			mustTestNoError(t, os.Chtimes(watermarkPath, fixedModTime, fixedModTime))
			watermarkBefore := mustTestValue(t, func() ([]byte, error) { return os.ReadFile(watermarkPath) })
			watermarkInfoBefore := mustTestValue(t, func() (os.FileInfo, error) { return os.Stat(watermarkPath) })

			minimum := source.Head
			ops := fakeCoreSchemaUpgradeOps()
			ops.after = func(reached string) error {
				if reached == phase {
					return errors.New("injected crash")
				}
				return nil
			}
			if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
				t.Fatalf("maintenance upgrade did not stop after %s", phase)
			}
			manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
			if err != nil || !exists {
				t.Fatalf("durable maintenance manifest after %s: exists=%v err=%v", phase, exists, err)
			}
			artifacts := mustTestValue(t, func() (coreSchemaUpgradeArtifacts, error) {
				return coreSchemaUpgradeArtifactPaths(databasePath, manifest)
			})

			resumedMinimum, err := loadAuthorityWatermark(watermarkPath)
			if err != nil || resumedMinimum == nil {
				t.Fatalf("load preserved watermark: head=%+v err=%v", resumedMinimum, err)
			}
			gotHead, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, resumedMinimum, time.Now(), fakeCoreSchemaUpgradeOps())
			if err != nil {
				t.Fatalf("resume maintenance after %s: %v", phase, err)
			}
			if gotHead == nil || *gotHead != source.Head {
				t.Fatalf("maintenance head=%+v want preserved %+v", gotHead, source.Head)
			}
			published := readFakeSchemaFile(t, databasePath)
			if published.Version != contractCachePruneMigrationVersion || published.Head != source.Head || published.Evidence != source.Evidence {
				t.Fatalf("published maintenance authority=%+v", published)
			}
			watermarkAfter := mustTestValue(t, func() ([]byte, error) { return os.ReadFile(watermarkPath) })
			if !bytes.Equal(watermarkBefore, watermarkAfter) {
				t.Fatal("head-preserving maintenance rewrote watermark bytes")
			}
			info := mustTestValue(t, func() (os.FileInfo, error) { return os.Stat(watermarkPath) })
			if !info.ModTime().Equal(fixedModTime) {
				t.Fatalf("head-preserving maintenance restamped watermark mtime: got %s want %s", info.ModTime(), fixedModTime)
			}
			if !os.SameFile(watermarkInfoBefore, info) {
				t.Fatal("head-preserving maintenance replaced watermark inode")
			}
			if pending, err := coreSchemaUpgradePending(databasePath); err != nil || pending {
				t.Fatalf("maintenance manifest pending=%v err=%v", pending, err)
			}
			if _, err := os.Lstat(artifacts.backup); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("oversized source backup was not retired: %v", err)
			}
			if _, err := os.Stat(artifacts.targetBackup); err != nil {
				t.Fatalf("compact target backup missing: %v", err)
			}
			receipt, ok, err := loadCoreSchemaMaintenanceReceipt(artifacts.receipt)
			if err != nil || !ok {
				t.Fatalf("maintenance receipt: ok=%v err=%v", ok, err)
			}
			if receipt.UpgradeID != manifest.UpgradeID || receipt.Version != 1 || receipt.EventDiscard != nil ||
				receipt.Discard.MigrationVersion != contractCachePruneMigrationVersion || receipt.Discard.Selector != contractCachePruneSelector ||
				receipt.Discard.RemovedRows != 2 || receipt.Discard.PayloadBytes != 200 ||
				receipt.Source.SchemaVersion != contractCachePruneMigrationVersion-1 || receipt.Source.Head != source.Head ||
				receipt.Target.SchemaVersion != contractCachePruneMigrationVersion || receipt.Target.Head != source.Head {
				t.Fatalf("maintenance receipt does not bind exact repair: %+v", receipt)
			}
			targetDigest, targetBytes, err := hashPrivateUpgradeArtifact(artifacts.targetBackup)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Target.SHA256 != targetDigest || receipt.Target.Bytes != targetBytes {
				t.Fatalf("maintenance receipt target fingerprint=%+v want %s/%d", receipt.Target, targetDigest, targetBytes)
			}
			mustTestNoError(t, requireIndependentUpgradeArtifacts(databasePath, artifacts.targetBackup))
		})
	}
}

func TestCoreSchemaMaintenanceFailsClosedWithoutReceiptAfterPublication(t *testing.T) {
	databasePath, source := newFakeMaintenanceSchemaAuthority(t, 1)
	minimum := source.Head
	ops := fakeCoreSchemaUpgradeOps()
	ops.after = func(phase string) error {
		if phase == coreSchemaPhaseVerified {
			return errors.New("injected crash")
		}
		return nil
	}
	if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), ops); err == nil {
		t.Fatal("maintenance did not stop after publication verification")
	}
	manifest, exists, err := loadCoreSchemaUpgradeManifest(databasePath)
	if err != nil || !exists {
		t.Fatal(err)
	}
	artifacts := mustTestValue(t, func() (coreSchemaUpgradeArtifacts, error) {
		return coreSchemaUpgradeArtifactPaths(databasePath, manifest)
	})
	mustTestNoError(t, os.Remove(artifacts.backup))
	if _, err := ensureCoreStoreSchemaCurrentWithOps(t.Context(), databasePath, &minimum, time.Now(), fakeCoreSchemaUpgradeOps()); err == nil {
		t.Fatal("missing source backup without receipt was accepted")
	}
	if pending, err := coreSchemaUpgradePending(databasePath); err != nil || !pending {
		t.Fatalf("failed-closed maintenance manifest pending=%v err=%v", pending, err)
	}
}

func newFakeMaintenanceSchemaAuthority(t *testing.T, rows int64) (string, fakeSchemaFile) {
	t.Helper()
	root := t.TempDir()
	mustTestNoError(t, os.Chmod(root, 0o700))
	path := filepath.Join(root, "daemon.db")
	source := fakeSchemaFile{
		Version:       contractCachePruneMigrationVersion - 1,
		TargetVersion: contractCachePruneMigrationVersion,
		Head: corestore.AuthorityHead{
			AuthorityEpoch: "ffeeddccbbaa99887766554433221100", HeadGeneration: 12,
			LastEventSeq: 91, SignerGeneration: 4,
		},
		Evidence:        "preserved-state-and-near-match-evidence",
		MaintenanceRows: rows,
	}
	writeFakeSchemaFile(t, path, source)
	mustTestNoError(t, writeAuthorityWatermark(path+".head", source.Head))
	return path, source
}

type fakeSchemaFile struct {
	Version         int                     `json:"version"`
	TargetVersion   int                     `json:"target_version,omitempty"`
	Head            corestore.AuthorityHead `json:"head"`
	Evidence        string                  `json:"evidence"`
	MaintenanceRows int64                   `json:"maintenance_rows,omitempty"`
}

func fakeCoreSchemaUpgradeOps() coreSchemaUpgradeOps {
	return coreSchemaUpgradeOps{
		inspect: fakeInspectSchema, prepare: fakePrepareSchemaUpgrade,
		recompute: fakeRecomputeSchemaUpgradeMaintenance, targetBackup: fakePrepareSchemaTargetBackup,
		quiesce: func(ctx context.Context, opts corestore.QuiesceOptions) (corestore.Inspection, error) {
			inspection, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.Path, MinimumHead: &opts.ExpectedHead})
			if err != nil {
				return corestore.Inspection{}, err
			}
			if inspection.SchemaVersion != opts.ExpectedSchemaVersion || inspection.Head != opts.ExpectedHead {
				return corestore.Inspection{}, fmt.Errorf("fake quiesce identity mismatch")
			}
			return inspection, nil
		},
	}
}

func fakeRecomputeSchemaUpgradeMaintenance(
	ctx context.Context,
	opts corestore.RecomputeUpgradeMaintenanceOptions,
) (corestore.UpgradeMaintenanceResult, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.SourcePath, MinimumHead: &opts.ExpectedHead, TargetVersion: opts.TargetVersion,
	})
	if err != nil {
		return corestore.UpgradeMaintenanceResult{}, err
	}
	if source.SchemaVersion != opts.ExpectedSchemaVersion || source.Head != opts.ExpectedHead {
		return corestore.UpgradeMaintenanceResult{}, fmt.Errorf("fake maintenance-proof source mismatch")
	}
	if opts.TargetVersion != contractCachePruneMigrationVersion {
		return corestore.UpgradeMaintenanceResult{}, nil
	}
	file, err := readFakeSchemaFileE(opts.SourcePath)
	if err != nil {
		return corestore.UpgradeMaintenanceResult{}, err
	}
	return corestore.UpgradeMaintenanceResult{
		Discards: []corestore.ObservationDiscardSummary{{
			MigrationVersion: contractCachePruneMigrationVersion, MigrationName: contractCachePruneMigrationName,
			Selector: contractCachePruneSelector, RemovedRows: file.MaintenanceRows, PayloadBytes: file.MaintenanceRows * 100,
			OrderedDigestSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		}},
		Compacted:                      true,
		SourceBackupRetirementRequired: file.MaintenanceRows > 0,
	}, nil
}

func fakeInspectSchema(_ context.Context, opts corestore.InspectOptions) (corestore.Inspection, error) {
	info, err := os.Lstat(opts.Path)
	if err != nil {
		return corestore.Inspection{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return corestore.Inspection{}, fmt.Errorf("fake authority is not regular")
	}
	raw, err := os.ReadFile(opts.Path)
	if err != nil {
		return corestore.Inspection{}, err
	}
	var file fakeSchemaFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return corestore.Inspection{}, err
	}
	targetVersion := file.TargetVersion
	if targetVersion == 0 {
		targetVersion = contractCachePruneMigrationVersion
	}
	if opts.TargetVersion != 0 {
		if opts.TargetVersion > targetVersion {
			return corestore.Inspection{}, fmt.Errorf("unsupported fake target version %d", opts.TargetVersion)
		}
		targetVersion = opts.TargetVersion
	}
	if file.Version < 1 || file.Version > targetVersion {
		return corestore.Inspection{}, fmt.Errorf("unsupported fake schema version %d", file.Version)
	}
	if opts.MinimumHead != nil {
		minimum := *opts.MinimumHead
		if file.Head.AuthorityEpoch != minimum.AuthorityEpoch || file.Head.HeadGeneration < minimum.HeadGeneration || file.Head.LastEventSeq < minimum.LastEventSeq || file.Head.SignerGeneration < minimum.SignerGeneration {
			return corestore.Inspection{}, corestore.ErrRollback
		}
	}
	status := corestore.InspectionUpgradeRequired
	if file.Version == targetVersion {
		status = corestore.InspectionCurrent
	}
	var transition corestore.UpgradeHeadTransition
	if status == corestore.InspectionUpgradeRequired {
		transition = corestore.UpgradeHeadTransitionAdvanceOnce
		if file.Version == 3 && targetVersion == 4 {
			transition = corestore.UpgradeHeadTransitionPreserve
		}
	}
	return corestore.Inspection{
		Path: opts.Path, SchemaVersion: file.Version, TargetVersion: targetVersion,
		Status: status, Head: file.Head,
		Integrity:      corestore.IntegrityReport{QuickCheckResults: []string{"ok"}},
		HeadTransition: transition,
	}, nil
}

func fakePrepareSchemaUpgrade(ctx context.Context, opts corestore.UpgradeOptions) (corestore.UpgradeResult, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.SourcePath, MinimumHead: opts.MinimumHead, TargetVersion: opts.TargetVersion,
	})
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	if source.Status != corestore.InspectionUpgradeRequired {
		return corestore.UpgradeResult{}, fmt.Errorf("fake source is already current")
	}
	if opts.ResetUnboundArtifacts {
		for _, path := range []string{opts.CandidatePath, opts.BackupPath} {
			if info, err := os.Lstat(path); err == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
					return corestore.UpgradeResult{}, fmt.Errorf("fake unbound artifact is invalid")
				}
				if err := os.Remove(path); err != nil {
					return corestore.UpgradeResult{}, err
				}
			} else if !errors.Is(err, fs.ErrNotExist) {
				return corestore.UpgradeResult{}, err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(opts.BackupPath), 0o700); err != nil {
		return corestore.UpgradeResult{}, err
	}
	if _, err := os.Lstat(opts.BackupPath); errors.Is(err, fs.ErrNotExist) {
		file, readErr := readFakeSchemaFileE(opts.SourcePath)
		if readErr != nil {
			return corestore.UpgradeResult{}, readErr
		}
		if err := writeFakeSchemaFileE(opts.BackupPath, file); err != nil {
			return corestore.UpgradeResult{}, err
		}
	} else if err != nil {
		return corestore.UpgradeResult{}, err
	}
	backupInspection, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.BackupPath, MinimumHead: &source.Head, TargetVersion: opts.TargetVersion,
	})
	if err != nil || backupInspection.SchemaVersion != source.SchemaVersion || backupInspection.Head != source.Head {
		return corestore.UpgradeResult{}, fmt.Errorf("fake backup mismatch: %w", err)
	}
	if info, err := os.Lstat(opts.CandidatePath); err == nil {
		if !opts.ReplaceCandidate || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return corestore.UpgradeResult{}, fmt.Errorf("fake candidate cannot be replaced")
		}
		if err := os.Remove(opts.CandidatePath); err != nil {
			return corestore.UpgradeResult{}, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return corestore.UpgradeResult{}, err
	}
	sourceFile, err := readFakeSchemaFileE(opts.SourcePath)
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	targetVersion := sourceFile.TargetVersion
	if targetVersion == 0 {
		targetVersion = contractCachePruneMigrationVersion
	}
	if opts.TargetVersion != 0 {
		targetVersion = opts.TargetVersion
	}
	transition := corestore.UpgradeHeadTransitionAdvanceOnce
	if sourceFile.Version == 3 && targetVersion == 4 {
		transition = corestore.UpgradeHeadTransitionPreserve
	} else {
		sourceFile.Head.HeadGeneration++
	}
	sourceFile.Version = targetVersion
	if err := writeFakeSchemaFileE(opts.CandidatePath, sourceFile); err != nil {
		return corestore.UpgradeResult{}, err
	}
	candidate, err := fakeInspectSchema(ctx, corestore.InspectOptions{
		Path: opts.CandidatePath, MinimumHead: &sourceFile.Head, TargetVersion: targetVersion,
	})
	if err != nil {
		return corestore.UpgradeResult{}, err
	}
	result := corestore.UpgradeResult{
		Source:         source,
		Backup:         corestore.BackupInfo{Path: opts.BackupPath, SchemaVersion: source.SchemaVersion, Head: source.Head, Integrity: source.Integrity},
		Candidate:      candidate,
		HeadTransition: transition,
	}
	if targetVersion == contractCachePruneMigrationVersion {
		result.Maintenance = corestore.UpgradeMaintenanceResult{
			Discards: []corestore.ObservationDiscardSummary{{
				MigrationVersion: contractCachePruneMigrationVersion, MigrationName: contractCachePruneMigrationName,
				Selector: contractCachePruneSelector, RemovedRows: sourceFile.MaintenanceRows, PayloadBytes: sourceFile.MaintenanceRows * 100,
				OrderedDigestSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
			}},
			Compacted:                      true,
			SourceBackupRetirementRequired: sourceFile.MaintenanceRows > 0,
		}
	}
	return result, nil
}

func fakePrepareSchemaTargetBackup(ctx context.Context, opts corestore.UpgradeTargetBackupOptions) (corestore.BackupInfo, error) {
	source, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.SourcePath, MinimumHead: &opts.ExpectedHead})
	if err != nil {
		return corestore.BackupInfo{}, err
	}
	if source.Status != corestore.InspectionCurrent ||
		source.SchemaVersion != opts.ExpectedSchemaVersion ||
		source.Head != opts.ExpectedHead {
		return corestore.BackupInfo{}, fmt.Errorf("fake target-backup source mismatch")
	}
	if _, err := os.Lstat(opts.BackupPath); errors.Is(err, fs.ErrNotExist) {
		file, readErr := readFakeSchemaFileE(opts.SourcePath)
		if readErr != nil {
			return corestore.BackupInfo{}, readErr
		}
		if err := writeFakeSchemaFileE(opts.BackupPath, file); err != nil {
			return corestore.BackupInfo{}, err
		}
	} else if err != nil {
		return corestore.BackupInfo{}, err
	}
	backup, err := fakeInspectSchema(ctx, corestore.InspectOptions{Path: opts.BackupPath, MinimumHead: &opts.ExpectedHead})
	if err != nil {
		return corestore.BackupInfo{}, err
	}
	if backup.Status != corestore.InspectionCurrent ||
		backup.SchemaVersion != opts.ExpectedSchemaVersion ||
		backup.Head != opts.ExpectedHead {
		return corestore.BackupInfo{}, fmt.Errorf("fake target backup mismatch")
	}
	return corestore.BackupInfo{Path: opts.BackupPath, SchemaVersion: backup.SchemaVersion, Head: backup.Head, Integrity: backup.Integrity}, nil
}

func writeFakeSchemaFile(t *testing.T, path string, file fakeSchemaFile) {
	t.Helper()
	if err := writeFakeSchemaFileE(path, file); err != nil {
		t.Fatal(err)
	}
}

func writeFakeSchemaFileE(path string, file fakeSchemaFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

func readFakeSchemaFile(t *testing.T, path string) fakeSchemaFile {
	t.Helper()
	file, err := readFakeSchemaFileE(path)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func readFakeSchemaFileE(path string) (fakeSchemaFile, error) {
	var file fakeSchemaFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return file, err
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return file, err
	}
	return file, nil
}

func TestDecorateQuoteMarksOldLivePriceStale(t *testing.T) {
	t.Parallel()
	srv := &Server{}
	loc := mustLocation(t, "America/New_York")
	last, prev := 652.10, 650.25
	asOf := time.Date(2026, 5, 26, 10, 30, 0, 0, loc)
	q := &rpc.Quote{
		Symbol:    "SPY",
		Last:      &last,
		PrevClose: &prev,
		DataType:  rpc.MarketDataLive,
		PriceAt:   asOf.Add(-20 * time.Minute),
		AsOf:      asOf,
	}

	srv.decorateQuote(q, marketcal.MarketUSEquity)

	if q.Price == nil || *q.Price != last {
		t.Fatalf("Price = %v, want last %.2f", q.Price, last)
	}
	if q.PriceSource != "last" {
		t.Fatalf("PriceSource = %q, want last", q.PriceSource)
	}
	if q.Change == nil || math.Abs(*q.Change-1.85) > 0.0001 {
		t.Fatalf("Change = %v, want 1.85", q.Change)
	}
	if !q.Stale {
		t.Fatal("expected stale quote during open market")
	}
	if !strings.Contains(q.StaleReason, "20m old") {
		t.Fatalf("StaleReason = %q, want age detail", q.StaleReason)
	}
	if got, want := q.PriceAsOf, "Frozen: May 26 at 10:10:00 AM EDT"; got != want {
		t.Fatalf("PriceAsOf = %q, want %q", got, want)
	}
	if q.DataType != rpc.MarketDataFrozen {
		t.Fatalf("DataType = %q, want frozen for stale selected price", q.DataType)
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load location %q: %v", name, err)
	}
	return loc
}

type fakeConnector struct {
	mu             sync.Mutex
	cache          map[string]*ibkrlib.MarketData
	dataType       int
	subscribed     map[string]int
	contracts      map[string]ibkrlib.Contract
	unsubscribed   map[string]int
	subscribeError error

	subscribeDelay time.Duration
}

func newFakeConnector() *fakeConnector {
	return &fakeConnector{
		cache:        map[string]*ibkrlib.MarketData{},
		subscribed:   map[string]int{},
		contracts:    map[string]ibkrlib.Contract{},
		unsubscribed: map[string]int{},
		dataType:     1,
	}
}

func (f *fakeConnector) SubscribeMarketData(ctx context.Context, symbol string, _ []string) error {
	if err := f.awaitSubscription(ctx); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed[symbol]++
	return nil
}

func (f *fakeConnector) SubscribeMarketDataWithContract(ctx context.Context, contract ibkrlib.Contract, _ []string) (string, error) {
	if err := f.awaitSubscription(ctx); err != nil {
		return "", err
	}
	key := ibkrlib.MarketDataKeyForContract(contract)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subscribed[key]++
	f.contracts[key] = contract
	return key, nil
}

func (f *fakeConnector) awaitSubscription(ctx context.Context) error {
	if f.subscribeError != nil {
		return f.subscribeError
	}
	if f.subscribeDelay <= 0 {
		return nil
	}
	select {
	case <-time.After(f.subscribeDelay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakeConnector) UnsubscribeMarketData(symbol string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unsubscribed[symbol]++
	return nil
}

func (f *fakeConnector) MarketDataSnapshot() map[string]*ibkrlib.MarketData {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]*ibkrlib.MarketData, len(f.cache))
	for k, v := range f.cache {
		copy := *v
		out[k] = &copy
	}
	return out
}

func (f *fakeConnector) MarketDataTypeForSymbol(_ string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dataType
}

func (f *fakeConnector) putTick(symbol string, bid, ask, last float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cache[symbol] = &ibkrlib.MarketData{Bid: bid, Ask: ask, Last: last}
}

func (f *fakeConnector) subCount(symbol string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subscribed[symbol]
}

type testManager struct {
	*subManager
	gatewayUp *atomic.Bool
}

func newTestManager(fake *fakeConnector) *testManager {
	gatewayUp := &atomic.Bool{}
	gatewayUp.Store(true)
	m := &subManager{
		subs:     map[string]*subEntry{},
		coalesce: 5 * time.Millisecond,
		connector: func() ibkrMarketConnector {
			if !gatewayUp.Load() {
				return nil
			}
			return fake
		},
	}
	return &testManager{subManager: m, gatewayUp: gatewayUp}
}

func TestFanoutSharesIBKRLine(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	a := subscribeTest(t, m, "AAPL")
	b := subscribeTest(t, m, "AAPL")

	if got := fake.subCount("AAPL"); got != 1 {
		t.Fatalf("SubscribeMarketData called %d times, want 1 (refcount must collapse)", got)
	}
	if got := m.activeCount(); got != 1 {
		t.Fatalf("activeCount: got %d, want 1", got)
	}

	fake.putTick("AAPL", 100.0, 100.05, 100.02)
	fA := receiveFrame(t, a, 200*time.Millisecond)
	fB := receiveFrame(t, b, 200*time.Millisecond)
	if *fA.Bid != *fB.Bid || *fA.Ask != *fB.Ask || *fA.Last != *fB.Last {
		t.Errorf("subscribers got different frames: A=%+v B=%+v", fA, fB)
	}
}

func TestGatewayLostEmitsTerminalFrame(t *testing.T) {
	t.Parallel()
	fake := newFakeConnector()
	m := newTestManager(fake)
	defer m.Close()

	frames := subscribeTest(t, m, "AAPL")

	fake.putTick("AAPL", 100.0, 100.1, 100.05)
	receiveFrame(t, frames, 200*time.Millisecond)

	m.gatewayUp.Store(false)

	terminal := receiveFrame(t, frames, 200*time.Millisecond)
	if terminal.Error == nil {
		t.Fatalf("expected terminal error frame, got %+v", terminal)
	}
	if terminal.Error.Code != rpc.FrameErrGatewayLost {
		t.Errorf("error code: got %q, want %q", terminal.Error.Code, rpc.FrameErrGatewayLost)
	}

	select {
	case _, ok := <-frames:
		if ok {
			t.Errorf("frame channel should be closed after gateway_lost")
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("frame channel did not close after gateway_lost")
	}
}

func subscribeTest(t *testing.T, manager *testManager, symbol string) <-chan rpc.Frame {
	t.Helper()
	frames, release, err := manager.Subscribe(context.Background(), symbol)
	if err != nil {
		t.Fatalf("Subscribe %s: %v", symbol, err)
	}
	t.Cleanup(release)
	return frames
}

func receiveFrame(t *testing.T, ch <-chan rpc.Frame, timeout time.Duration) rpc.Frame {
	t.Helper()
	select {
	case f, ok := <-ch:
		if !ok {
			t.Fatalf("frame channel closed prematurely")
		}
		return f
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for frame after %s", timeout)
		return rpc.Frame{}
	}
}
