package daemon

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/ibkr/v2/internal/daemon/corestore"
)

const coreEventOriginCutover = "legacy_cutover"

type daemonStateCutoverReport struct {
	PlatformSettingsImported bool
	RiskCapitalImported      bool
	CapitalEventsImported    int
	GovernanceEventsImported int
	GovernanceEventsSkipped  int
	NudgesImported           bool
	RulesStageImported       bool
	CapitalDeclaredFlowsBase float64
	LastReconciledAt         time.Time
	LastReconcileReportID    string
	LastReconcileSource      string
	Sources                  []daemonStateCutoverSource
}

type daemonStateCutoverSource struct {
	Kind    string `json:"kind"`
	Path    string `json:"path"`
	SHA256  string `json:"sha256,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`
	Records int    `json:"records,omitempty"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

// initializeFreshDaemonState creates the daemon-owned documents required by
// bindAuthoritativeDaemonState without consulting any legacy path. It is for a
// genuinely fresh custom/offline authority; production cutover instead uses
// prepareDaemonStateCutover so guardrail state is preserved. A fully initialized
// set is an idempotent no-op. A partial set is rejected so an interrupted,
// unpublished authority is discarded rather than guessed back into shape.
func initializeFreshDaemonState(ctx context.Context, core *corestore.Store) error {
	if core == nil {
		return fmt.Errorf("fresh SQLite authority is unavailable")
	}
	defaults := []struct {
		kind  string
		value any
	}{
		{stateKindPlatformSettings, platformSettingsData{Version: platformSettingsDocVersion}},
		{stateKindRiskCapital, riskCapitalSQLiteDocument{
			Version: riskCapitalSQLiteDocVer,
			State:   riskCapitalStateFileV1{Version: riskCapitalStateVer},
		}},
		{stateKindNudges, nudgeStateFileV1{Version: governanceNudgeStateVersion}},
		{stateKindBrief, briefStateFileV1{Version: briefStateVersion, Stamps: map[string]briefStampState{}}},
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
		return fmt.Errorf("fresh daemon-state initialization is partial (%d/%d documents); discard the unpublished authority and retry", present, len(defaults))
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

// prepareDaemonStateCutover is the explicit, one-shot bridge from legacy
// daemon-owned files into an empty daemon.db. It intentionally imports no
// regime/rules/canary/proposal decision history. Original Flex XML and market
// observations are handled by their dedicated importers.
func prepareDaemonStateCutover(ctx context.Context, core *corestore.Store) (daemonStateCutoverReport, error) {
	var report daemonStateCutoverReport
	if core == nil {
		return report, fmt.Errorf("cutover SQLite authority is unavailable")
	}
	head, err := core.AuthorityHead(ctx)
	if err != nil {
		return report, fmt.Errorf("inspect cutover authority head: %w", err)
	}
	if head.HeadGeneration != 0 || head.LastEventSeq != 0 {
		return report, fmt.Errorf("daemon-state cutover requires a new unpublished authority")
	}
	if imported, err := importPlatformSettingsState(ctx, core, &report.Sources); err != nil {
		return report, err
	} else {
		report.PlatformSettingsImported = imported
	}
	capital, governance, skipped, imported, continuity, err := importRiskCapitalState(ctx, core, &report.Sources)
	if err != nil {
		return report, err
	}
	report.RiskCapitalImported = imported
	report.CapitalEventsImported = capital
	report.GovernanceEventsImported = governance
	report.GovernanceEventsSkipped = skipped
	report.CapitalDeclaredFlowsBase = continuity.declaredFlowsBase
	report.LastReconciledAt = continuity.lastReconciledAt
	report.LastReconcileReportID = continuity.lastReconcileReportID
	report.LastReconcileSource = continuity.lastReconcileSource
	if imported, err := importNudgeState(ctx, core, &report.Sources); err != nil {
		return report, err
	} else {
		report.NudgesImported = imported
	}
	if _, err := writeInitialState(ctx, core, stateKindBrief, briefStateFileV1{
		Version: briefStateVersion,
		Stamps:  map[string]briefStampState{},
	}); err != nil {
		return report, fmt.Errorf("initialize clean brief baselines: %w", err)
	}
	if imported, err := importRulesStageState(ctx, core, &report.Sources); err != nil {
		return report, err
	} else {
		report.RulesStageImported = imported
	}
	for i := range report.Sources {
		if report.Sources[i].Status == "validated" {
			report.Sources[i].Status = "imported"
		}
	}
	return report, nil
}

func importPlatformSettingsState(ctx context.Context, core *corestore.Store, sources *[]daemonStateCutoverSource) (bool, error) {
	if _, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindPlatformSettings); err != nil || ok {
		return false, err
	}
	// The legacy file predates the settings-document version stamp, so it is read
	// as version 1 unless it says otherwise, and it spelled the portfolio-stress
	// journal preference "canary". Upgrading carries that stored value into the
	// imported document instead of resetting it to the default at cutover.
	doc := platformSettingsDocument{
		platformSettingsData: platformSettingsData{Version: platformSettingsDocVersionCanary},
	}
	path, err := defaultPlatformSettingsPath()
	if err != nil {
		return false, err
	}
	if _, err := readOptionalJSON("platform_settings", path, &doc, sources); err != nil {
		return false, fmt.Errorf("import platform settings: %w", err)
	}
	state, err := doc.upgrade()
	if err != nil {
		markLastCutoverSourceInvalid(sources, err)
		return false, fmt.Errorf("import platform settings: %w", err)
	}
	// Legacy settings predate the durable control generation. A carried freeze
	// or limit is already material authority at cutover, so seed generation one
	// rather than publishing it as the indistinguishable zero baseline.
	if state.TradingControlGeneration == 0 && !sameTradingControls(platformTradingSettingsData{}, state.Trading) {
		state.TradingControlGeneration = 1
	}
	return writeInitialState(ctx, core, stateKindPlatformSettings, state)
}

func importRiskCapitalState(ctx context.Context, core *corestore.Store, sources *[]daemonStateCutoverSource) (capitalCount, governanceCount, skipped int, imported bool, continuity capitalEventReplay, err error) {
	if _, ok, readErr := core.GetStateDocument(ctx, daemonStateScope, stateKindRiskCapital); readErr != nil || ok {
		return 0, 0, 0, false, continuity, readErr
	}
	if existing, readErr := loadAllCoreEvents(ctx, core, coreEventCapital); readErr != nil {
		return 0, 0, 0, false, continuity, readErr
	} else if len(existing) != 0 {
		return 0, 0, 0, false, continuity, fmt.Errorf("partial cutover: capital events exist without risk capital state")
	}
	state := riskCapitalStateFileV1{Version: riskCapitalStateVer}
	path, err := defaultTradingStatePath(riskCapitalStateFile)
	if err != nil {
		return 0, 0, 0, false, continuity, err
	}
	stateFound, err := readOptionalJSON("risk_capital_state", path, &state, sources)
	if err != nil {
		return 0, 0, 0, false, continuity, fmt.Errorf("import risk capital state: %w", err)
	} else if stateFound && state.Version != riskCapitalStateVer {
		markLastCutoverSourceInvalid(sources, fmt.Errorf("unsupported version %d", state.Version))
		return 0, 0, 0, false, continuity, fmt.Errorf("import risk capital state: unsupported version %d", state.Version)
	}
	if state.BlockLatched && state.LatchEpisodeSeq == 0 {
		state.LatchEpisodeSeq = 1
	}
	doc := riskCapitalSQLiteDocument{Version: riskCapitalSQLiteDocVer, State: state, OverrideSeq: len(state.Overrides)}
	rawDoc, err := json.Marshal(doc)
	if err != nil {
		return 0, 0, 0, false, continuity, err
	}
	capitalEvents, err := readLegacyCapitalEvents(sources)
	if err != nil {
		return 0, 0, 0, false, continuity, err
	}
	events := make([]corestore.EventInput, 0, len(capitalEvents))
	for i, event := range capitalEvents {
		input, err := capitalCutoverEvent(event, i)
		if err != nil {
			return 0, 0, 0, false, continuity, err
		}
		events = append(events, input)
	}
	governance, skipped, err := readLegacyCurrentGovernanceEvents(len(events), sources)
	if err != nil {
		return 0, 0, 0, false, continuity, err
	}
	if !stateFound && (len(capitalEvents) != 0 || len(governance) != 0 || skipped != 0) {
		return 0, 0, 0, false, continuity, fmt.Errorf("import risk capital authority: journals exist without %s", riskCapitalStateFile)
	}
	events = append(events, governance...)
	update := corestore.StateDocumentCAS{ScopeKey: daemonStateScope, Kind: stateKindRiskCapital, JSON: rawDoc}
	if len(events) == 0 {
		_, err = core.CompareAndSwapStateDocument(ctx, update)
	} else {
		_, _, err = core.CompareAndSwapStateDocumentWithEvents(ctx, update, events)
	}
	if err != nil {
		return 0, 0, 0, false, continuity, fmt.Errorf("import risk capital authority: %w", err)
	}
	continuity = replayCapitalEventSlice(capitalEvents)
	if err := verifyCapitalEventContinuity(ctx, core, continuity); err != nil {
		return 0, 0, 0, false, continuity, err
	}
	return len(capitalEvents), len(governance), skipped, true, continuity, nil
}

func readLegacyCapitalEvents(sources *[]daemonStateCutoverSource) ([]capitalEventV1, error) {
	path, err := defaultTradingStatePath(capitalEventsJournalFile)
	if err != nil {
		return nil, err
	}
	lines, err := readLegacyJSONLines("capital_events", path, sources)
	if err != nil {
		return nil, fmt.Errorf("import capital events: %w", err)
	}
	events := make([]capitalEventV1, 0, len(lines))
	for i, raw := range lines {
		var event capitalEventV1
		if err := json.Unmarshal(raw, &event); err != nil || event.Version != 1 || event.At.IsZero() {
			markLastCutoverSourceInvalid(sources, fmt.Errorf("invalid line %d", i+1))
			return nil, fmt.Errorf("import capital events: invalid line %d", i+1)
		}
		switch event.Type {
		case "deposit", "withdrawal":
			if event.AmountBase <= 0 {
				markLastCutoverSourceInvalid(sources, fmt.Errorf("invalid amount on line %d", i+1))
				return nil, fmt.Errorf("import capital events: invalid amount on line %d", i+1)
			}
		case "reconcile":
		default:
			markLastCutoverSourceInvalid(sources, fmt.Errorf("invalid type on line %d", i+1))
			return nil, fmt.Errorf("import capital events: invalid type on line %d", i+1)
		}
		events = append(events, event)
	}
	return events, nil
}

func capitalCutoverEvent(event capitalEventV1, ordinal int) (corestore.EventInput, error) {
	raw, err := json.Marshal(event)
	if err != nil {
		return corestore.EventInput{}, err
	}
	return corestore.EventInput{
		ScopeKey: daemonStateScope, EventKey: coreEventKey(coreEventCapital, event.At, raw, ordinal),
		Type: coreEventCapital, Action: "import", Origin: coreEventOriginCutover,
		OccurredAt: event.At, PayloadJSON: raw,
		Projection: corestore.EventProjection{CapitalEvent: &corestore.CapitalEventProjection{
			Kind: event.Type, AmountBaseText: strconv.FormatFloat(event.AmountBase, 'g', -1, 64),
			EffectiveAt: event.EffectiveAt.UTC().Format(time.RFC3339Nano), ReportID: event.ReportID,
		}},
	}, nil
}

// Only explicit human reconciliation dismissals survive the clean-slate
// cutover. Historical policy states, derived tiers, and advisory decisions may
// have come from buggy code and begin a new semantic epoch.
func readLegacyCurrentGovernanceEvents(ordinalBase int, sources *[]daemonStateCutoverSource) ([]corestore.EventInput, int, error) {
	path, err := defaultTradingStatePath(riskPolicyJournalFile)
	if err != nil {
		return nil, 0, err
	}
	lines, err := readLegacyJSONLines("risk_policy_events", path, sources)
	if err != nil {
		return nil, 0, fmt.Errorf("import risk governance events: %w", err)
	}
	var events []corestore.EventInput
	skipped := 0
	for i, raw := range lines {
		var header struct {
			At                time.Time `json:"at"`
			Kind              string    `json:"kind"`
			PolicyID          string    `json:"policy_id"`
			PolicyVersion     *int64    `json:"policy_version"`
			PolicyFingerprint string    `json:"policy_fingerprint"`
		}
		if err := json.Unmarshal(raw, &header); err != nil || header.Kind == "" || header.At.IsZero() {
			markLastCutoverSourceInvalid(sources, fmt.Errorf("invalid line %d", i+1))
			return nil, 0, fmt.Errorf("import risk governance events: invalid line %d", i+1)
		}
		if header.Kind != "recon_dismiss" {
			skipped++
			continue
		}
		events = append(events, corestore.EventInput{
			ScopeKey: daemonStateScope,
			EventKey: coreEventKey(coreEventRiskPolicy, header.At, raw, ordinalBase+len(events)),
			Type:     coreEventRiskPolicy, Action: "import", Origin: coreEventOriginCutover,
			OccurredAt: header.At, PayloadJSON: raw,
			Projection: corestore.EventProjection{RiskPolicyEvent: &corestore.RiskPolicyEventProjection{
				Kind: header.Kind, PolicyID: header.PolicyID, PolicyVersion: header.PolicyVersion,
				PolicyFingerprint: header.PolicyFingerprint,
			}},
		})
	}
	return events, skipped, nil
}

func importNudgeState(ctx context.Context, core *corestore.Store, sources *[]daemonStateCutoverSource) (bool, error) {
	if _, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindNudges); err != nil || ok {
		return false, err
	}
	state := nudgeStateFileV1{Version: governanceNudgeStateVersion}
	path, err := defaultTradingStatePath(governanceNudgeStateFile)
	if err != nil {
		return false, err
	}
	if found, err := readOptionalJSON("governance_nudges", path, &state, sources); err != nil {
		return false, fmt.Errorf("import governance nudge state: %w", err)
	} else if found && state.Version != governanceNudgeStateVersion {
		markLastCutoverSourceInvalid(sources, fmt.Errorf("unsupported version %d", state.Version))
		return false, fmt.Errorf("import governance nudge state: unsupported version %d", state.Version)
	}
	normalizeNudgeState(&state)
	return writeInitialState(ctx, core, stateKindNudges, state)
}

func importRulesStageState(ctx context.Context, core *corestore.Store, sources *[]daemonStateCutoverSource) (bool, error) {
	if _, ok, err := core.GetStateDocument(ctx, daemonStateScope, stateKindRulesRegimeStage); err != nil || ok {
		return false, err
	}
	state := rulesRegimeStageState{Version: rulesRegimeStageStateVer}
	path, err := defaultTradingStatePath(rulesRegimeStageFile)
	if err != nil {
		return false, err
	}
	if found, err := readOptionalJSON("rules_regime_stage", path, &state, sources); err != nil {
		return false, fmt.Errorf("import rules regime stage: %w", err)
	} else if found && state.Version != rulesRegimeStageStateVer {
		markLastCutoverSourceInvalid(sources, fmt.Errorf("unsupported version %d", state.Version))
		return false, fmt.Errorf("import rules regime stage: unsupported version %d", state.Version)
	}
	if state.Stage != "" {
		state.Bucket = bucketRegimeStage(state.Stage)
		if state.Bucket == "" {
			markLastCutoverSourceInvalid(sources, fmt.Errorf("invalid stage %q", state.Stage))
			return false, fmt.Errorf("import rules regime stage: invalid stage %q", state.Stage)
		}
	}
	return writeInitialState(ctx, core, stateKindRulesRegimeStage, state)
}

func writeInitialState(ctx context.Context, core *corestore.Store, kind string, value any) (bool, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}
	if _, err := core.CompareAndSwapStateDocument(ctx, corestore.StateDocumentCAS{
		ScopeKey: daemonStateScope, Kind: kind, JSON: raw,
	}); err != nil {
		return false, fmt.Errorf("initialize %s: %w", kind, err)
	}
	return true, nil
}

func readOptionalJSON(kind, path string, dst any, sources *[]daemonStateCutoverSource) (bool, error) {
	raw, found, err := readCutoverSource(kind, path, sources)
	if err != nil || !found {
		return found, err
	}
	if err := decodeStrictLegacyJSON(raw, dst); err != nil {
		markLastCutoverSourceInvalid(sources, err)
		return true, err
	}
	(*sources)[len(*sources)-1].Records = 1
	return true, nil
}

func readLegacyJSONLines(kind, path string, sources *[]daemonStateCutoverSource) ([][]byte, error) {
	raw, found, err := readCutoverSource(kind, path, sources)
	if err != nil || !found {
		return nil, err
	}
	var lines [][]byte
	scanner := bufio.NewScanner(strings.NewReader(string(raw)))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		raw := []byte(strings.TrimSpace(scanner.Text()))
		if len(raw) != 0 {
			lines = append(lines, append([]byte(nil), raw...))
		}
	}
	if err := scanner.Err(); err != nil {
		markLastCutoverSourceInvalid(sources, err)
		return nil, err
	}
	(*sources)[len(*sources)-1].Records = len(lines)
	return lines, nil
}

func readCutoverSource(kind, path string, sources *[]daemonStateCutoverSource) ([]byte, bool, error) {
	source := daemonStateCutoverSource{Kind: kind, Path: path, Status: "preflight"}
	*sources = append(*sources, source)
	index := len(*sources) - 1
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		(*sources)[index].Status = "missing"
		return nil, false, nil
	}
	if err != nil {
		markCutoverSourceInvalid(sources, index, err)
		return nil, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		err := fmt.Errorf("source is not a regular non-symlink file")
		markCutoverSourceInvalid(sources, index, err)
		return nil, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		markCutoverSourceInvalid(sources, index, err)
		return nil, false, err
	}
	opened, statErr := f.Stat()
	if statErr != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = f.Close()
		if statErr == nil {
			statErr = fmt.Errorf("source identity changed during preflight")
		}
		markCutoverSourceInvalid(sources, index, statErr)
		return nil, false, statErr
	}
	raw, err := io.ReadAll(f)
	afterRead, afterStatErr := f.Stat()
	if err == nil {
		err = afterStatErr
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		markCutoverSourceInvalid(sources, index, err)
		return nil, false, err
	}
	current, currentErr := os.Lstat(path)
	if currentErr != nil || !os.SameFile(afterRead, current) ||
		int64(len(raw)) != opened.Size() || afterRead.Size() != opened.Size() || !afterRead.ModTime().Equal(opened.ModTime()) {
		err := fmt.Errorf("source changed during preflight")
		markCutoverSourceInvalid(sources, index, err)
		return nil, false, err
	}
	digest := sha256.Sum256(raw)
	(*sources)[index].SHA256 = hex.EncodeToString(digest[:])
	(*sources)[index].Bytes = int64(len(raw))
	(*sources)[index].Status = "validated"
	return raw, true, nil
}

func markLastCutoverSourceInvalid(sources *[]daemonStateCutoverSource, err error) {
	if len(*sources) != 0 {
		markCutoverSourceInvalid(sources, len(*sources)-1, err)
	}
}

func markCutoverSourceInvalid(sources *[]daemonStateCutoverSource, index int, err error) {
	(*sources)[index].Status = "invalid"
	(*sources)[index].Error = err.Error()
}

func verifyCapitalEventContinuity(ctx context.Context, core *corestore.Store, expected capitalEventReplay) error {
	events, err := loadAllCoreEvents(ctx, core, coreEventCapital)
	if err != nil {
		return fmt.Errorf("verify imported capital continuity: %w", err)
	}
	decoded := make([]capitalEventV1, 0, len(events))
	for _, event := range events {
		var value capitalEventV1
		if err := json.Unmarshal(event.PayloadJSON, &value); err != nil {
			return fmt.Errorf("verify imported capital continuity: decode event %d: %w", event.EventSeq, err)
		}
		decoded = append(decoded, value)
	}
	actual := replayCapitalEventSlice(decoded)
	if actual.declaredFlowsBase != expected.declaredFlowsBase ||
		!actual.lastReconciledAt.Equal(expected.lastReconciledAt) ||
		actual.lastReconcileReportID != expected.lastReconcileReportID ||
		actual.lastReconcileSource != expected.lastReconcileSource ||
		actual.lastAutoExtendReportID != expected.lastAutoExtendReportID ||
		!actual.lastAutoExtendedAt.Equal(expected.lastAutoExtendedAt) ||
		len(actual.reconciledReportIDs) != len(expected.reconciledReportIDs) {
		return fmt.Errorf("verify imported capital continuity: semantic parity mismatch")
	}
	for reportID := range expected.reconciledReportIDs {
		if _, ok := actual.reconciledReportIDs[reportID]; !ok {
			return fmt.Errorf("verify imported capital continuity: reconciled report membership mismatch")
		}
	}
	return nil
}
