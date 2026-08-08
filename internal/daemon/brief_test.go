package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func dailyBriefPolicyTOML() string {
	return strings.Replace(validRiskPolicyTOML, "[cadence.morning]\nclass = \"advisory\"",
		"[cadence.morning]\nclass = \"advisory\"\n\n[cadence.eod]\nclass = \"advisory\"\n\n[cadence.weekly]\nclass = \"advisory\"", 1)
}

func TestComposeBriefCapturesBoundaryAfterInputs(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	brief, rules := s.composeBrief(context.Background())
	if brief == nil || rules == nil {
		t.Fatalf("composeBrief returned brief=%v rules=%v", brief, rules)
	}
	if brief.AsOf.Before(rules.AsOf) {
		t.Fatalf("brief boundary %s precedes rulebook snapshot %s", brief.AsOf, rules.AsOf)
	}
	for _, source := range rules.InputHealth {
		if !source.AsOf.IsZero() && brief.AsOf.Before(source.AsOf) {
			t.Fatalf("brief boundary %s precedes %s input %s", brief.AsOf, source.Source, source.AsOf)
		}
	}
}

func TestBriefAckOriginIdempotenceAndAuditFields(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	now := time.Date(2026, 7, 18, 8, 30, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now

	statePath, _ := defaultTradingStatePath(briefStateFile)
	journalPath, _ := defaultTradingStatePath(riskPolicyJournalFile)
	for _, origin := range []string{"", rpc.OrderOriginAgent, "unknown"} {
		_, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
			Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:rendered", Origin: origin,
		}))
		if err == nil || !strings.Contains(err.Error(), "human-only") {
			t.Fatalf("origin %q: err=%v, want human-only refusal", origin, err)
		}
	}
	for _, path := range []string{statePath, journalPath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("refused ack wrote %s: %v", path, err)
		}
	}

	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:rendered", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.AlreadyStamped || ack.Kind != rpc.BriefKindMorning || ack.Day != "2026-07-18" {
		t.Fatalf("ack=%+v", ack)
	}
	records := s.riskCapital.Artefacts()
	if len(records) != 1 || records[0].Origin != rpc.OrderOriginHumanTTY || records[0].BriefFingerprint != "sha256:rendered" {
		t.Fatalf("artefact records=%+v", records)
	}
	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	var line map[string]any
	if err := json.Unmarshal(data, &line); err != nil {
		t.Fatal(err)
	}
	if line["kind"] != "artefact_completed" || line["origin"] != rpc.OrderOriginHumanTTY || line["brief_fingerprint"] != "sha256:rendered" {
		t.Fatalf("journal=%v", line)
	}

	before := string(data)
	repeat, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMorning, BriefFingerprint: "sha256:different", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !repeat.AlreadyStamped || repeat.At != ack.At {
		t.Fatalf("repeat=%+v want idempotent receipt at %s", repeat, ack.At)
	}
	after, _ := os.ReadFile(journalPath)
	if string(after) != before {
		t.Fatal("repeat ack appended another journal entry")
	}
}

func TestBriefArtefactExtensionPreservesLegacyPolicyPathAndJSON(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, err := s.handleRiskPolicyArtefact(context.Background(), rawParams(t, rpc.ArtefactParams{
		Artefact: rpc.BriefKindMorning,
		Note:     "ordinary policy artefact",
		Origin:   rpc.OrderOriginHumanTTY,
	}))
	if err != nil || !res.OK {
		t.Fatalf("existing policy artefact path: result=%+v err=%v", res, err)
	}
	records := s.riskCapital.Artefacts()
	if len(records) != 1 || records[0].BriefFingerprint != "" || records[0].Origin != rpc.OrderOriginHumanTTY {
		t.Fatalf("existing policy artefact record=%+v", records)
	}

	// Older persisted records and journal-shaped JSON omit both extension
	// fields. Go's typed decoder must continue accepting those lines.
	var legacy rpc.ArtefactRecord
	if err := json.Unmarshal([]byte(`{"artefact":"morning","class":"advisory","completed_at":"2026-07-18T08:00:00Z"}`), &legacy); err != nil {
		t.Fatalf("legacy artefact JSON: %v", err)
	}
	if legacy.Artefact != rpc.BriefKindMorning || legacy.Origin != "" || legacy.BriefFingerprint != "" {
		t.Fatalf("legacy artefact decoded=%+v", legacy)
	}
}

func TestBriefFirstIncompleteAndExplicitKind(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.Local)
	c := &risk.Constitution{Cadence: risk.ConstitutionCadence{
		Morning: risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
		EOD:     risk.ConstitutionArtefact{Class: risk.EnforcementAdvisory},
	}}
	policy := &rpc.RiskPolicyResult{}
	if kind, reason := briefStampTarget(policy, c, now); kind != rpc.BriefKindMorning || reason != "" {
		t.Fatalf("initial target=%q reason=%q", kind, reason)
	}
	policy.Cadence = []rpc.ArtefactRecord{{Artefact: rpc.BriefKindMorning, Class: risk.EnforcementAdvisory, CompletedAt: now.Add(-time.Hour)}}
	if kind, reason := briefStampTarget(policy, c, now); kind != rpc.BriefKindEOD || reason != "" {
		t.Fatalf("after morning target=%q reason=%q", kind, reason)
	}
	policy.Cadence = append(policy.Cadence, rpc.ArtefactRecord{Artefact: rpc.BriefKindEOD, Class: risk.EnforcementAdvisory, CompletedAt: now})
	if kind, reason := briefStampTarget(policy, c, now); kind != "" || reason != "both daily artefacts complete" {
		t.Fatalf("complete target=%q reason=%q", kind, reason)
	}

	// The explicit kind is honored even while the default target is morning.
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindEOD, BriefFingerprint: "sha256:eod-override", Origin: rpc.OrderOriginHumanTTY,
	}))
	if err != nil || ack.Kind != rpc.BriefKindEOD || ack.AlreadyStamped {
		t.Fatalf("explicit eod ack=%+v err=%v", ack, err)
	}
}

func TestMonthlyBriefAckOriginPinsFingerprintIdempotencyAndRollover(t *testing.T) {
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC) // Monday, August's first working day; 12:00 Europe/Berlin, after 09:00 due.
	s := newV4NudgeTestServer(t, now)
	policy := s.riskPolicies.snapshot().policy
	month := "2026-08"
	rendered, _ := s.composeBrief(context.Background())
	params := rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: month, Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
	}
	statePath, _ := defaultTradingStatePath(governanceNudgeStateFile)
	for _, origin := range []string{"", rpc.OrderOriginAgent, rpc.OrderOriginHumanTTY} {
		bad := params
		bad.Origin = origin
		if _, err := s.handleBriefAck(context.Background(), rawParams(t, bad)); err == nil || !strings.Contains(err.Error(), "paired-device") {
			t.Fatalf("origin %q err=%v, want paired-device refusal", origin, err)
		}
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("refused origins wrote monthly state: %v", err)
	}
	badMonth := params
	badMonth.Month = "2026-07"
	if _, err := s.handleBriefAck(context.Background(), rawParams(t, badMonth)); err == nil || !strings.Contains(err.Error(), "month") {
		t.Fatalf("bad month err=%v", err)
	}
	badEvidence := params
	badEvidence.Evidence = "gesture"
	if _, err := s.handleBriefAck(context.Background(), rawParams(t, badEvidence)); err == nil || !strings.Contains(err.Error(), "render evidence") {
		t.Fatalf("bad evidence err=%v", err)
	}

	// An unreadable sibling pin blocks completion without creating state.
	protection := s.protectionPolicies
	s.protectionPolicies = nil
	if _, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil || !strings.Contains(err.Error(), "matching policy pins") {
		t.Fatalf("unavailable pin err=%v", err)
	}
	s.protectionPolicies = protection

	if err := s.nudges.observeConfirmedFlows(nudgeConfirmedFlowSnapshot{
		PolicyVersion: 4, ReportIdentity: opaqueIdentity("report", "cutover"),
	}); err != nil {
		t.Fatal(err)
	}
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.AlreadyStamped || ack.Kind != rpc.BriefKindMonthly || ack.Month != month || ack.Evidence != rpc.BriefAckEvidenceRender {
		t.Fatalf("ack=%+v", ack)
	}
	coverage, _, _ := s.nudges.confirmedSnapshot(nil)
	if coverage == nil {
		t.Fatal("monthly completion removed confirmed-flow coverage")
	}
	repeat, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || !repeat.AlreadyStamped || !repeat.At.Equal(ack.At) || repeat.BriefFingerprint != ack.BriefFingerprint {
		t.Fatalf("repeat=%+v err=%v", repeat, err)
	}
	conflict := params
	conflict.BriefFingerprint = opaqueIdentity("brief", "different-render")
	if got, err := s.handleBriefAck(context.Background(), rawParams(t, conflict)); err == nil || got != nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("conflicting render result=%+v err=%v", got, err)
	}
	statePath = s.nudges.path
	s.nudges = &nudgeStateStore{path: statePath, now: s.now}
	restarted, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || !restarted.AlreadyStamped || restarted.BriefFingerprint != ack.BriefFingerprint || !restarted.At.Equal(ack.At) {
		t.Fatalf("restart retry=%+v err=%v", restarted, err)
	}
	policyIdentity := nudgePolicyIdentity(policy)
	evaluation, completion := s.briefMonthlyPulse(policy, &rpc.RiskPolicyResult{Inventory: s.riskPolicyInventory(policy)}, s.buildReconReport(), now)
	if evaluation.Status != risk.MonthlyPulseStatusCompleted || completion == nil || !completion.CompletedAt.Equal(ack.At) {
		t.Fatalf("monthly evaluation=%+v completion=%+v", evaluation, completion)
	}
	// A current policy revision reopens the month and invalidates the old
	// rendered identity instead of recording it under the new authority.
	s.riskPolicies.mu.Lock()
	revisedWarning := 3
	s.riskPolicies.active.Cadence.Nudges.ReconcileWarningDays = &revisedWarning
	s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
	s.riskPolicies.mu.Unlock()
	if got, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil || got != nil {
		t.Fatalf("stale-policy render result=%+v err=%v", got, err)
	}
	// A within-month policy fingerprint change has no matching completion and
	// therefore reopens the pulse.
	revisedIdentity := opaqueIdentity("risk-policy", "revised")
	reopened := risk.EvaluateMonthlyPulse(risk.MonthlyPulseInput{
		Now: now, Cadence: policy.Cadence, PolicyFingerprint: revisedIdentity,
		PolicyEvidenceReady: true, Completion: s.nudges.monthlyCompletion(month, revisedIdentity),
	})
	if reopened.Status != risk.MonthlyPulseStatusDue {
		t.Fatalf("revised policy status=%s, want due (old identity %s)", reopened.Status, policyIdentity)
	}

	// The next month is a distinct key and can complete once it is due.
	now = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	s.riskPolicies.mu.Lock()
	s.riskPolicies.now = s.now
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	next := params
	next.Month = "2026-09"
	nextRendered, _ := s.composeBrief(context.Background())
	next.BriefFingerprint = nextRendered.BriefFingerprint
	nextAck, err := s.handleBriefAck(context.Background(), rawParams(t, next))
	if err != nil || nextAck.AlreadyStamped || nextAck.Month != "2026-09" {
		t.Fatalf("next month ack=%+v err=%v", nextAck, err)
	}
}

func TestMonthlyBriefAckUsesIssuedRenderNotVolatileRecomposition(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	rendered, _ := s.composeBrief(context.Background())
	if rendered.BriefFingerprint == "" {
		t.Fatal("monthly render has no fingerprint")
	}

	// Account/capital values are visible brief content but are not monthly
	// governance authority. A phone receipt remains valid when they move after
	// the render and before acknowledgement.
	s.riskCapital.mu.Lock()
	s.riskCapital.loadLocked()
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 260000
	s.riskCapital.state.LastEquityBase = 245000
	s.riskCapital.state.LastEquityAsOf = now
	s.riskCapital.mu.Unlock()

	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
	}))
	if err != nil || ack == nil || !ack.OK {
		t.Fatalf("issued render rejected after volatile mutation: ack=%+v err=%v", ack, err)
	}
}

func TestMonthlyRenderReceiptsIssueExpireRestartAndBound(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	t.Run("unissued and expired fail closed", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		unissued := opaqueIdentity("brief", "never-rendered")
		if _, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
			Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: unissued, Origin: rpc.OrderOriginPairedDevice,
		})); err == nil || !strings.Contains(err.Error(), "render receipt") {
			t.Fatalf("unissued fingerprint error=%v", err)
		}
		rendered, _ := s.composeBrief(context.Background())
		now = now.Add(monthlyRenderReceiptTTL + time.Second)
		s.now = func() time.Time { return now }
		s.riskCapital.now = s.now
		s.riskPolicies.mu.Lock()
		s.riskPolicies.now = s.now
		s.riskPolicies.lastCheckedAt = now
		s.riskPolicies.mu.Unlock()
		if _, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
			Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
		})); err == nil || !strings.Contains(err.Error(), "render receipt") {
			t.Fatalf("expired fingerprint error=%v", err)
		}
	})

	t.Run("restart requires rerender", func(t *testing.T) {
		now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
		s := newV4NudgeTestServer(t, now)
		rendered, _ := s.composeBrief(context.Background())
		// Receipts are intentionally memory-only; a daemon restart loses them
		// and requires the phone to render the current brief again.
		s.monthlyRenderReceipts = nil
		params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
		if _, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil {
			t.Fatal("pre-restart receipt survived memory reset")
		}
		rerendered, _ := s.composeBrief(context.Background())
		params.BriefFingerprint = rerendered.BriefFingerprint
		if ack, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err != nil || ack == nil || !ack.OK {
			t.Fatalf("rerendered receipt ack=%+v err=%v", ack, err)
		}
	})

	t.Run("bounded pruning", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		for i := range monthlyRenderReceiptLimit + 5 {
			s.issueMonthlyRenderReceipt(opaqueIdentity("brief", fmt.Sprint(i)), "2026-08", opaqueIdentity("authority", "same"), now.Add(time.Duration(i)*time.Second))
		}
		if got := len(s.monthlyRenderReceipts); got != monthlyRenderReceiptLimit {
			t.Fatalf("receipt count=%d want=%d", got, monthlyRenderReceiptLimit)
		}
		later := now.Add(monthlyRenderReceiptTTL + time.Hour)
		s.issueMonthlyRenderReceipt(opaqueIdentity("brief", "fresh"), "2026-08", opaqueIdentity("authority", "same"), later)
		if got := len(s.monthlyRenderReceipts); got != 1 {
			t.Fatalf("expired receipt pruning left %d rows", got)
		}
	})
}

func TestMonthlyReceiptSurvivesMutationAtFormerSecondRead(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	rendered, _ := s.composeBrief(context.Background())
	s.nudgeBeforeCommit = func(kind string) {
		if kind != "monthly" {
			return
		}
		s.riskCapital.mu.Lock()
		s.riskCapital.state.LastEquityBase = 230000
		s.riskCapital.state.LastEquityAsOf = now
		s.riskCapital.mu.Unlock()
	}
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
	}))
	if err != nil || ack == nil || !ack.OK {
		t.Fatalf("stable receipt rejected at former second read: ack=%+v err=%v", ack, err)
	}
}

func TestMonthlyRenderReceiptRequiresSameRenderedAuthority(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		mutate func(*Server)
	}{
		{name: "policy reload", mutate: func(s *Server) {
			s.riskPolicies.mu.Lock()
			defer s.riskPolicies.mu.Unlock()
			revised := 3
			s.riskPolicies.active.Cadence.Nudges.ReconcileWarningDays = &revised
			s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
		}},
		{name: "protection pin reload", mutate: func(s *Server) {
			s.protectionPolicies.mu.Lock()
			next := s.protectionPolicies.status.PolicyVersion + 1
			s.protectionPolicies.status.PolicyVersion = next
			s.protectionPolicies.mu.Unlock()
			s.riskPolicies.mu.Lock()
			s.riskPolicies.active.Inventory.Protection.Version = strconv.Itoa(next)
			s.riskPolicies.lastFingerprint = s.riskPolicies.active.FingerprintKey()
			s.riskPolicies.mu.Unlock()
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newV4NudgeTestServer(t, now)
			s.monthlyRenderBeforeIssue = func() {
				s.monthlyRenderBeforeIssue = nil
				tt.mutate(s)
			}
			rendered, _ := s.composeBrief(context.Background())
			s.monthlyRenderMu.Lock()
			receipts := len(s.monthlyRenderReceipts)
			s.monthlyRenderMu.Unlock()
			if receipts != 0 {
				t.Fatalf("authority-raced render issued %d receipt(s)", receipts)
			}
			if _, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
				Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
				BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
			})); err == nil {
				t.Fatal("authority-raced render was accepted")
			}
		})
	}

	t.Run("stable render remains usable", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		rendered, _ := s.composeBrief(context.Background())
		ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
			Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
		}))
		if err != nil || ack == nil || !ack.OK {
			t.Fatalf("stable render ack=%+v err=%v", ack, err)
		}
	})
}

func TestMonthlyCompletionRecoversWithFreshReceiptAfterExpiry(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	firstRender, _ := s.composeBrief(context.Background())
	params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: firstRender.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
	attempts := 0
	s.nudges.writeState = func(path string, raw []byte) error {
		attempts++
		if attempts == 1 {
			return errors.New("injected transient atomic rename failure")
		}
		return writePrivateStateAtomic(path, raw)
	}
	if ack, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil || ack != nil {
		t.Fatalf("first completion unexpectedly succeeded: ack=%+v err=%v", ack, err)
	}
	if s.nudges.healthOK() {
		t.Fatal("failed completion did not fault snapshot reads")
	}

	now = now.Add(monthlyRenderReceiptTTL + time.Second)
	s.now = func() time.Time { return now }
	s.riskCapital.now = s.now
	s.nudges.now = s.now
	s.riskPolicies.mu.Lock()
	s.riskPolicies.now = s.now
	s.riskPolicies.loadedAt = now
	s.riskPolicies.lastCheckedAt = now
	s.riskPolicies.mu.Unlock()
	freshRender, _ := s.composeBrief(context.Background())
	if freshRender.Ready.MonthlyPulse == nil || freshRender.Ready.MonthlyPulse.Status != rpc.BriefMonthlyPulseDue {
		t.Fatalf("fault recovery monthly ready=%+v, want conservatively due", freshRender.Ready.MonthlyPulse)
	}
	s.monthlyRenderMu.Lock()
	freshReceipt, issued := s.monthlyRenderReceipts[freshRender.BriefFingerprint]
	s.monthlyRenderMu.Unlock()
	if !issued || !freshReceipt.ExpiresAt.After(now) {
		t.Fatalf("fault recovery issued=%v receipt=%+v fingerprint=%q", issued, freshReceipt, freshRender.BriefFingerprint)
	}
	params.BriefFingerprint = freshRender.BriefFingerprint
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, params))
	if err != nil || ack == nil || !ack.OK || ack.AlreadyStamped {
		t.Fatalf("fresh receipt recovery ack=%+v err=%v", ack, err)
	}
	if attempts != 2 || !s.nudges.healthOK() {
		t.Fatalf("recovery attempts=%d healthy=%v", attempts, s.nudges.healthOK())
	}
	path := s.nudges.path
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	authority := s.currentNudgeAuthority(now)
	evaluation, completion := s.governanceMonthlyPulseForAuthority(authority, authority.policy, s.buildReconReport(), now)
	if evaluation.Status != risk.MonthlyPulseStatusCompleted || completion == nil || !completion.CompletedAt.Equal(ack.At) {
		t.Fatalf("reloaded recovery evaluation=%+v completion=%+v ack=%+v", evaluation, completion, ack)
	}
}

func TestCorruptNudgeStoreCannotIssueMonthlyRecoveryReceipt(t *testing.T) {
	// Monday, August's first working day: the pulse is due, so the absent
	// receipt below is proven withheld by the corrupt store, not by not-due.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	path := filepath.Join(t.TempDir(), governanceNudgeStateFile)
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.nudges = &nudgeStateStore{path: path, now: s.now}
	s.riskCapital.nudges = s.nudges
	rendered, _ := s.composeBrief(context.Background())
	s.monthlyRenderMu.Lock()
	_, issued := s.monthlyRenderReceipts[rendered.BriefFingerprint]
	s.monthlyRenderMu.Unlock()
	if issued {
		t.Fatal("corrupt loaded store issued a monthly recovery receipt")
	}
}

func TestCanceledBriefCompositionNeverMintsMonthlyReceipt(t *testing.T) {
	// Monday, August's first working day: the pulse is due, so an uncanceled
	// render would mint — cancellation is what must withhold the receipt.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	activeReceipts := func(s *Server, at time.Time) int {
		s.monthlyRenderMu.Lock()
		defer s.monthlyRenderMu.Unlock()
		count := 0
		for _, receipt := range s.monthlyRenderReceipts {
			if receipt.ExpiresAt.After(at) {
				count++
			}
		}
		return count
	}

	t.Run("already canceled normal due render", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.composeBrief(ctx)
		if got := activeReceipts(s, now); got != 0 {
			t.Fatalf("canceled normal render minted %d active receipt(s)", got)
		}
	})

	t.Run("already canceled fault recovery after expiry", func(t *testing.T) {
		current := now
		s := newV4NudgeTestServer(t, current)
		first, _ := s.composeBrief(context.Background())
		params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: first.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
		s.nudges.writeState = func(string, []byte) error { return errors.New("injected persistent atomic rename failure") }
		if ack, err := s.handleBriefAck(context.Background(), rawParams(t, params)); err == nil || ack != nil {
			t.Fatalf("fault setup ack=%+v err=%v", ack, err)
		}
		current = current.Add(monthlyRenderReceiptTTL + time.Second)
		s.now = func() time.Time { return current }
		s.riskCapital.now = s.now
		s.nudges.now = s.now
		s.riskPolicies.mu.Lock()
		s.riskPolicies.now = s.now
		s.riskPolicies.loadedAt = current
		s.riskPolicies.lastCheckedAt = current
		s.riskPolicies.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.composeBrief(ctx)
		if got := activeReceipts(s, current); got != 0 {
			t.Fatalf("canceled recovery render minted %d active receipt(s)", got)
		}
	})

	t.Run("cancellation at before issue seam", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		ctx, cancel := context.WithCancel(context.Background())
		s.monthlyRenderBeforeIssue = cancel
		s.composeBrief(ctx)
		if got := activeReceipts(s, now); got != 0 {
			t.Fatalf("before-issue cancellation minted %d active receipt(s)", got)
		}
	})

	t.Run("cancellation immediately before receipt persistence", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		ctx, cancel := context.WithCancel(context.Background())
		s.monthlyRenderBeforePersist = cancel
		s.composeBrief(ctx)
		if got := activeReceipts(s, now); got != 0 {
			t.Fatalf("pre-persist cancellation minted %d active receipt(s)", got)
		}
	})

	t.Run("preexisting valid receipt survives later canceled render", func(t *testing.T) {
		current := now
		s := newV4NudgeTestServer(t, current)
		first, _ := s.composeBrief(context.Background())
		s.monthlyRenderMu.Lock()
		original, ok := s.monthlyRenderReceipts[first.BriefFingerprint]
		s.monthlyRenderMu.Unlock()
		if !ok {
			t.Fatal("stable initial render issued no receipt")
		}
		current = current.Add(time.Minute)
		s.now = func() time.Time { return current }
		s.riskCapital.now = s.now
		s.nudges.now = s.now
		s.riskPolicies.mu.Lock()
		s.riskPolicies.now = s.now
		s.riskPolicies.lastCheckedAt = current
		s.riskPolicies.mu.Unlock()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.composeBrief(ctx)
		s.monthlyRenderMu.Lock()
		preserved, stillPresent := s.monthlyRenderReceipts[first.BriefFingerprint]
		s.monthlyRenderMu.Unlock()
		if !stillPresent || preserved != original || activeReceipts(s, current) != 1 {
			t.Fatalf("canceled render changed prior receipt: present=%v got=%+v want=%+v", stillPresent, preserved, original)
		}
	})

	t.Run("uncanceled render issues receipt", func(t *testing.T) {
		s := newV4NudgeTestServer(t, now)
		rendered, _ := s.composeBrief(context.Background())
		s.monthlyRenderMu.Lock()
		receipt, ok := s.monthlyRenderReceipts[rendered.BriefFingerprint]
		s.monthlyRenderMu.Unlock()
		if !ok || !receipt.ExpiresAt.After(now) {
			t.Fatalf("uncanceled render receipt=%+v issued=%v", receipt, ok)
		}
	})
}

func TestConcurrentIdenticalMonthlyAcknowledgementsAreIdempotent(t *testing.T) {
	for iteration := range 10 {
		now := time.Date(2026, 8, 3, 10, 0, iteration, 0, time.UTC)
		s := newV4NudgeTestServer(t, now)
		rendered, _ := s.composeBrief(context.Background())
		params := rpc.BriefAckParams{Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
			BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice}
		requests := []*rpc.Request{rawParams(t, params), rawParams(t, params)}
		ready := make(chan struct{}, 2)
		release := make(chan struct{})
		s.monthlyAckBeforeWriteLock = func() {
			ready <- struct{}{}
			<-release
		}
		type outcome struct {
			ack *rpc.BriefAckResult
			err error
		}
		outcomes := make(chan outcome, 2)
		var started sync.WaitGroup
		started.Add(2)
		for _, request := range requests {
			go func(req *rpc.Request) {
				started.Done()
				ack, err := s.handleBriefAck(context.Background(), req)
				outcomes <- outcome{ack: ack, err: err}
			}(request)
		}
		started.Wait()
		<-ready
		<-ready
		close(release)
		newCount, existingCount := 0, 0
		for range 2 {
			result := <-outcomes
			if result.err != nil || result.ack == nil || !result.ack.OK {
				t.Fatalf("iteration %d concurrent ack=%+v err=%v", iteration, result.ack, result.err)
			}
			if result.ack.AlreadyStamped {
				existingCount++
			} else {
				newCount++
			}
		}
		if newCount != 1 || existingCount != 1 {
			t.Fatalf("iteration %d new=%d existing=%d", iteration, newCount, existingCount)
		}
		s.nudges.mu.Lock()
		rows := len(s.nudges.state.MonthlyCompletions)
		s.nudges.mu.Unlock()
		if rows != 1 {
			t.Fatalf("iteration %d persisted %d monthly rows", iteration, rows)
		}
	}
}

func TestV3BriefMonthlyExtensionIsBehaviorCompatible(t *testing.T) {
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	policy := s.briefPolicyResult(nil, context.Canceled, now)
	process := s.composeBriefProcess(policy, s.riskPolicies.snapshot().policy, nil, nil, now)
	if process.MonthlyPulse != nil {
		t.Fatalf("v1-v3 brief unexpectedly gained monthly row: %+v", process.MonthlyPulse)
	}
	legacyKind, legacyReason := briefStampTarget(policy, s.riskPolicies.snapshot().policy, now)
	kind, reason := s.briefStampTarget(policy, s.riskPolicies.snapshot().policy, now)
	if kind != legacyKind || reason != legacyReason {
		t.Fatalf("v3 target changed: method=%q/%q legacy=%q/%q", kind, reason, legacyKind, legacyReason)
	}
}

func TestBriefAndNudgeMonthlyParityDueBlockedAndComplete(t *testing.T) {
	// Monday, August's first working day, past the 09:00 Berlin due instant.
	now := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	s := newV4NudgeTestServer(t, now)
	constitution := s.riskPolicies.snapshot().policy
	policy := &rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive, Inventory: s.riskPolicyInventory(constitution)}

	dueSnapshot := s.composeNudgesSnapshot()
	dueProcess := s.composeBriefProcess(policy, constitution, nil, nil, now)
	if !candidateKindPresent(dueSnapshot.Candidates, rpc.NudgeKindMonthlyPulse) || dueProcess.MonthlyPulse == nil || dueProcess.MonthlyPulse.Status != rpc.BriefMonthlyPulseDue {
		t.Fatalf("due parity snapshot=%+v process=%+v", dueSnapshot.Candidates, dueProcess.MonthlyPulse)
	}

	s.riskPolicies.mu.Lock()
	s.riskPolicies.status = rpc.RiskPolicyStatusDrift
	s.riskPolicies.mu.Unlock()
	blockedSnapshot := s.composeNudgesSnapshot()
	blockedProcess := s.composeBriefProcess(policy, constitution, nil, nil, now)
	if candidateKindPresent(blockedSnapshot.Candidates, rpc.NudgeKindMonthlyPulse) || blockedProcess.MonthlyPulse == nil || blockedProcess.MonthlyPulse.Status != rpc.BriefMonthlyPulseBlocked {
		t.Fatalf("blocked parity snapshot=%+v process=%+v", blockedSnapshot.Candidates, blockedProcess.MonthlyPulse)
	}

	s.riskPolicies.mu.Lock()
	s.riskPolicies.status = rpc.RiskPolicyStatusActive
	s.riskPolicies.mu.Unlock()
	rendered, _ := s.composeBrief(context.Background())
	ack, err := s.handleBriefAck(context.Background(), rawParams(t, rpc.BriefAckParams{
		Kind: rpc.BriefKindMonthly, Month: "2026-08", Evidence: rpc.BriefAckEvidenceRender,
		BriefFingerprint: rendered.BriefFingerprint, Origin: rpc.OrderOriginPairedDevice,
	}))
	if err != nil || !ack.OK {
		t.Fatalf("monthly completion ack=%+v err=%v", ack, err)
	}
	completedSnapshot := s.composeNudgesSnapshot()
	completedProcess := s.composeBriefProcess(policy, constitution, nil, nil, now)
	if candidateKindPresent(completedSnapshot.Candidates, rpc.NudgeKindMonthlyPulse) || completedProcess.MonthlyPulse == nil || completedProcess.MonthlyPulse.Status != rpc.BriefMonthlyPulseCompleted {
		t.Fatalf("complete parity snapshot=%+v process=%+v", completedSnapshot.Candidates, completedProcess.MonthlyPulse)
	}
}

func TestBriefProcessAggregateIncludesMonthlyStatus(t *testing.T) {
	ok := briefOK("ok")
	base := rpc.BriefProcessSection{
		Reconcile:  rpc.BriefReconcileRow{BriefRowState: ok},
		AutoExtend: rpc.BriefAutoExtendRow{BriefRowState: ok},
		OneTap:     rpc.BriefOneTapRow{BriefRowState: ok},
		RulesDelta: rpc.BriefRulesDeltaRow{BriefRowState: ok},
		Artefacts:  rpc.BriefArtefactsRow{BriefRowState: ok},
	}
	for _, tt := range []struct {
		status string
		want   string
	}{
		{rpc.BriefMonthlyPulseNotDue, rpc.BriefStatusOK},
		{rpc.BriefMonthlyPulseCompleted, rpc.BriefStatusOK},
		{rpc.BriefMonthlyPulseDue, rpc.BriefStatusDegraded},
		{rpc.BriefMonthlyPulseBlocked, rpc.BriefStatusDegraded},
	} {
		t.Run(tt.status, func(t *testing.T) {
			process := base
			process.MonthlyPulse = &rpc.BriefMonthlyPulseRow{Status: tt.status}
			if got := briefProcessSectionState(process); got.Status != tt.want {
				t.Fatalf("monthly %s aggregate=%+v, want %s", tt.status, got, tt.want)
			}
		})
	}
}

func TestBriefSnapshotPurityAndDegradedRows(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	root := os.Getenv("XDG_STATE_HOME")
	before := stateTree(t, root)
	for range 3 {
		res, _ := s.composeBrief(context.Background())
		if res.Ready.Regime.Status != rpc.BriefStatusUnavailable || res.Review.SessionPnL.Status != rpc.BriefStatusUnavailable {
			t.Fatalf("gateway rows not unavailable: regime=%+v session_pnl=%+v", res.Ready.Regime, res.Review.SessionPnL)
		}
		if res.Ready.Capital.Status == "" || res.Review.Reconcile.Status == "" || res.BriefFingerprint == "" {
			t.Fatalf("policy/process rows did not render: %+v", res)
		}
	}
	after := stateTree(t, root)
	if !slices.Equal(before, after) {
		t.Fatalf("brief.snapshot mutated state tree: before=%v after=%v", before, after)
	}
}

func stateTree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err == nil && path != root {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	slices.Sort(out)
	return out
}

func TestBriefRulesDeltaAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, briefStateFile)
	baseline := &rpc.RulesResult{
		PolicyFingerprint: &rpc.Fingerprint{Key: "sha256:old"},
		Rules:             []risk.RuleRow{{ID: "kept", Status: risk.RuleStatusPass}, {ID: "removed", Status: risk.RuleStatusWatch}},
	}
	store := &briefStateStore{path: path}
	at := time.Date(2026, 7, 17, 17, 0, 0, 0, time.UTC)
	if err := store.stamp(rpc.BriefKindEOD, "sha256:brief", at, baseline); err != nil {
		t.Fatal(err)
	}
	s := &Server{briefState: &briefStateStore{path: path}}
	current := &rpc.RulesResult{
		PolicyFingerprint: &rpc.Fingerprint{Key: "sha256:new"},
		Rules:             []risk.RuleRow{{ID: "kept", Status: risk.RuleStatusAct}, {ID: "added", Status: risk.RuleStatusPass}},
	}
	delta := s.briefRulesDelta(current)
	if !delta.RulebookFingerprintChanged || len(delta.Transitions) != 1 || delta.Transitions[0].RuleID != "kept" ||
		!slices.Equal(delta.Added, []string{"added"}) || !slices.Equal(delta.Removed, []string{"removed"}) || !delta.BaselineAt.Equal(at) {
		t.Fatalf("delta=%+v", delta)
	}
	// The kept rule worsened to act: a risk deterioration lifts the row to
	// attention instead of hiding under data-quality vocabulary.
	if delta.Status != rpc.BriefStatusAttention || !strings.Contains(delta.Detail, "worsened to act") {
		t.Fatalf("act transition must render attention: %+v", delta.BriefRowState)
	}
	observed, threshold := 15.5, 15.0
	stable := &rpc.RulesResult{
		PolicyFingerprint: &rpc.Fingerprint{Key: "sha256:old"},
		Rules: []risk.RuleRow{
			{ID: "kept", Status: risk.RuleStatusPass, Observed: &observed, Threshold: &threshold},
			{ID: "removed", Status: risk.RuleStatusWatch},
		},
	}
	if got := s.briefRulesDelta(stable); got.Status != rpc.BriefStatusOK || len(got.Transitions) != 0 ||
		len(got.Added) != 0 || len(got.Removed) != 0 || got.RulebookFingerprintChanged {
		t.Fatalf("typed evidence-only change must not be reported as a state delta: %+v", got)
	}
	if got := (&Server{briefState: &briefStateStore{path: filepath.Join(dir, "missing.json")}}).briefRulesDelta(current); got.Detail != "no delta baseline yet" {
		t.Fatalf("no-baseline detail=%q", got.Detail)
	}
}

func TestBriefNilMoneyAndGreeksDegradeWithoutZeroFill(t *testing.T) {
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "AAPL", SecType: "OPT", Right: "C", Quantity: 1},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 1, Multiplier: 100},
	}}
	premium := briefPremiumAtRisk(pos, "EUR")
	if premium.Status != rpc.BriefStatusDegraded || premium.AmountBase != nil || premium.ExcludedLegs != 2 {
		t.Fatalf("premium=%+v", premium)
	}
	hedge := briefHedgeCost(pos, "EUR")
	if hedge.Status != rpc.BriefStatusDegraded || hedge.AmountBase != nil || hedge.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", hedge)
	}
}

func TestBriefProposalsSessionSummaryFromJournal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trade-proposal-outcomes.jsonl")
	lines := []proposalOutcomeMark{
		// Older session: one proposal offered (marked), not acted.
		{Version: 1, MarkDate: "2026-07-17", State: proposalOutcomeStateMarked, ProposalKey: "P-OLD-1"},
		// Latest session (2026-07-18): three distinct proposals offered, one
		// submitted+filled (acted once, deduped), one only marked.
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateMarked, ProposalKey: "P-A"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateSubmitted, ProposalKey: "P-B"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateFilled, ProposalKey: "P-B"},
		{Version: 1, MarkDate: "2026-07-18", State: proposalOutcomeStateMarked, ProposalKey: "P-C"},
	}
	var buf strings.Builder
	for _, m := range lines {
		raw, _ := json.Marshal(m)
		buf.Write(raw)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	store := newProposalOutcomeStore(path)
	offered, acted, day, ok, err := store.SessionSummary()
	if err != nil || !ok {
		t.Fatalf("summary ok=%v err=%v", ok, err)
	}
	if day != "2026-07-18" || offered != 3 || acted != 1 {
		t.Fatalf("latest session summary day=%q offered=%d acted=%d", day, offered, acted)
	}

	// The wire row leaks no proposal identity — counts and the day only.
	s := &Server{proposalOutcomes: store}
	row := s.briefProposals(time.Now())
	if row.Status != rpc.BriefStatusOK || row.Offered != 3 || row.Acted != 1 || row.Day != "2026-07-18" {
		t.Fatalf("proposals row=%+v", row)
	}
	raw, _ := json.Marshal(row)
	for _, forbidden := range []string{"P-A", "P-B", "P-C", "proposal_key"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("proposals row leaked %q: %s", forbidden, raw)
		}
	}

	// Missing journal reads as a clean "no proposals" row, never an error.
	empty := &Server{proposalOutcomes: newProposalOutcomeStore(filepath.Join(dir, "missing.jsonl"))}
	if got := empty.briefProposals(time.Now()); got.Status != rpc.BriefStatusOK || got.Offered != 0 {
		t.Fatalf("missing-journal proposals row=%+v", got)
	}
}

func TestBriefCapitalEventsRegroupsLatchAndPeak(t *testing.T) {
	age := 4
	consumed := 30.4
	peak := 260000.0
	peakAsOf := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	latch := rpc.BriefLatchRow{BriefRowState: briefAttention("engaged"), Latched: true, AgeDays: &age, ConsumedPctAtLatch: &consumed}
	capital := rpc.BriefCapitalRow{BriefRowState: briefOK("ok"), AdjustedPeakBase: &peak, PeakAsOf: peakAsOf, BaseCurrency: "EUR"}
	got := briefCapitalEvents(capital, latch)
	if got.Status != rpc.BriefStatusAttention || !got.Latched || got.LatchAgeDays == nil || *got.LatchAgeDays != 4 {
		t.Fatalf("latched capital events=%+v", got)
	}
	if got.AdjustedPeakBase == nil || *got.AdjustedPeakBase != peak || !got.PeakAsOf.Equal(peakAsOf) {
		t.Fatalf("peak provenance did not flow: %+v", got)
	}

	// An absent constitution renders capital events unavailable, not a clean line.
	if unavailable := briefCapitalEvents(rpc.BriefCapitalRow{BriefRowState: briefUnavailable("absent")}, rpc.BriefLatchRow{}); unavailable.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("absent constitution capital events=%+v", unavailable)
	}
	// A quiet book reads ok.
	if quiet := briefCapitalEvents(rpc.BriefCapitalRow{BriefRowState: briefOK("ok")}, rpc.BriefLatchRow{BriefRowState: briefOK("open")}); quiet.Status != rpc.BriefStatusOK || quiet.Latched {
		t.Fatalf("quiet capital events=%+v", quiet)
	}
}

func TestBriefResultContainsNoPrivateIdentityOrTokenFields(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	res, _ := s.composeBrief(context.Background())
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"account_id", "order_id", "order_ref", "preview_token", "submit_eligible"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("brief result contains forbidden field %q: %s", forbidden, text)
		}
	}
}

func TestUnreconciledClockSharedProjection(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	last := now.Add(-5 * 24 * time.Hour)
	maxDays := 7
	clock := risk.EvaluateUnreconciledClock(&maxDays, last, time.Time{}, now)
	if !clock.Approved || clock.Stale || !clock.Deadline.Equal(last.Add(7*24*time.Hour)) || clock.DaysRemaining == nil || *clock.DaysRemaining != 2 {
		t.Fatalf("clock=%+v", clock)
	}
	override := now.Add(4 * 24 * time.Hour)
	clock = risk.EvaluateUnreconciledClock(&maxDays, last, override, now)
	if !clock.Deadline.Equal(override) || clock.DaysRemaining == nil || *clock.DaysRemaining != 4 {
		t.Fatalf("override clock=%+v", clock)
	}
	never := risk.EvaluateUnreconciledClock(&maxDays, time.Time{}, time.Time{}, now)
	if !never.Stale || !never.Deadline.IsZero() || never.DaysRemaining != nil {
		t.Fatalf("never clock=%+v", never)
	}
}

func TestBriefRiskStatusDerivesFromValues(t *testing.T) {
	now := time.Date(2026, 7, 19, 20, 0, 0, 0, time.UTC)
	base := func() *rpc.RiskPolicyResult {
		consumed := 10.0
		return &rpc.RiskPolicyResult{Status: rpc.RiskPolicyStatusActive,
			Capital: rpc.CapitalStateReport{Tier: risk.CapitalTierOK, Enforcement: "shadow", ConsumedPct: &consumed}}
	}

	blocked := base()
	consumed := 1589.7
	blocked.Capital.Tier = risk.CapitalTierBlock
	blocked.Capital.ConsumedPct = &consumed
	blocked.Capital.BlockLatched = true
	blocked.Capital.LatchedAt = now.Add(-4 * 24 * time.Hour)
	blocked.Capital.PeakAsOf = now.Add(-3 * time.Hour)
	latchPct := 30.41
	blocked.Capital.LatchConsumedPct = &latchPct
	blocked.Capital.Enforcement = "shadow"
	out := composeBriefRisk(blocked, now)
	if out.Capital.Status != rpc.BriefStatusAttention || out.Latch.Status != rpc.BriefStatusAttention {
		t.Fatalf("blocked tier must render attention: capital=%+v latch=%+v", out.Capital.BriefRowState, out.Latch.BriefRowState)
	}
	if out.Capital.PeakAsOf.IsZero() || out.Latch.ConsumedPctAtLatch == nil || *out.Latch.ConsumedPctAtLatch != latchPct {
		t.Fatalf("provenance must flow into the brief: capital=%+v latch=%+v", out.Capital, out.Latch)
	}
	if !strings.Contains(out.Capital.Detail, "shadow enforcement journals what would block") {
		t.Fatalf("shadow enforcement must not imply an active block: %q", out.Capital.Detail)
	}
	if out.Latch.AgeDays == nil || *out.Latch.AgeDays != 4 || !out.Latch.Latched {
		t.Fatalf("latch row=%+v", out.Latch)
	}
	if out.Status != rpc.BriefStatusAttention || !strings.Contains(out.Detail, "need attention") {
		t.Fatalf("section must roll up worst child: %+v", out.BriefRowState)
	}

	warn := base()
	warn.Capital.Tier = risk.CapitalTierWarn
	if got := composeBriefRisk(warn, now); got.Capital.Status != rpc.BriefStatusAttention {
		t.Fatalf("warn tier=%+v", got.Capital.BriefRowState)
	}

	overConsumed := base()
	full := 120.0
	overConsumed.Capital.ConsumedPct = &full
	if got := composeBriefRisk(overConsumed, now); got.Capital.Status != rpc.BriefStatusAttention {
		t.Fatalf("consumed>=100%% with ok tier must still render attention: %+v", got.Capital.BriefRowState)
	}

	unapproved := base()
	unapproved.Capital.Tier = risk.CapitalTierUnapproved
	unapproved.Capital.ConsumedPct = nil
	if got := composeBriefRisk(unapproved, now); got.Capital.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unapproved=%+v", got.Capital.BriefRowState)
	}

	override := base()
	override.Overrides = []rpc.OverrideRecord{{Control: "drawdown.block", Active: true, ExpiresAt: now.Add(time.Hour)}}
	got := composeBriefRisk(override, now)
	if got.Overrides.Status != rpc.BriefStatusAttention || len(got.Overrides.Rows) != 1 {
		t.Fatalf("active override=%+v", got.Overrides)
	}

	if got := composeBriefRisk(base(), now); got.Status != rpc.BriefStatusOK || got.Detail != "risk and limits section complete" {
		t.Fatalf("healthy section=%+v", got.BriefRowState)
	}
}

func TestBriefSectionStateWorstChildAndCompleteness(t *testing.T) {
	att := briefSectionState("risk", briefOK(""), briefAttention(""), briefDegraded(""))
	if att.Status != rpc.BriefStatusAttention || !strings.Contains(att.Detail, "1 of 3 rows needs attention") || !strings.Contains(att.Detail, "1 degraded or unavailable") {
		t.Fatalf("attention rollup=%+v", att)
	}
	deg := briefSectionState("market", briefOK(""), briefDegraded(""), briefUnavailable(""))
	if deg.Status != rpc.BriefStatusDegraded || !strings.Contains(deg.Detail, "2 of 3 rows degraded or unavailable") {
		t.Fatalf("degraded rollup=%+v", deg)
	}
	if got := briefSectionState("x", briefUnavailable(""), briefUnavailable("")); got.Status != rpc.BriefStatusUnavailable {
		t.Fatalf("all-unavailable rollup=%+v", got)
	}
	if got := briefSectionState("x", briefOK("")); got.Status != rpc.BriefStatusOK || got.Detail != "x section complete" {
		t.Fatalf("ok rollup=%+v", got)
	}
}

func TestBriefClosedSessionDowngradesExpectedColdness(t *testing.T) {
	asOf := time.Date(2026, 7, 17, 21, 58, 0, 0, time.UTC)
	events := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{
		{Source: "trading_halts", Status: rpc.SourceStatusStale, AsOf: asOf},
		{Source: "reg_sho_threshold", Status: rpc.SourceStatusOK, AsOf: asOf},
		{Source: "borrow_fee", Status: rpc.SourceStatusDegraded, AsOf: asOf},
	}}
	rules := &rpc.RulesResult{}

	byKind := func(rows []rpc.BriefMarketEventRow) map[string]rpc.BriefMarketEventRow {
		out := map[string]rpc.BriefMarketEventRow{}
		for _, row := range rows {
			out[row.Kind] = row
		}
		return out
	}

	closed := byKind(briefMarketEventRows(events, rules, nil, false))
	if row := closed["halt"]; row.Status != rpc.BriefStatusOK || !strings.Contains(row.Detail, "no fresh update expected while the market is closed") || !strings.Contains(row.Detail, "last checked") {
		t.Fatalf("closed stale halt row=%+v", row.BriefRowState)
	}
	if row := closed["ssr"]; row.Status != rpc.BriefStatusOK || strings.Contains(row.Detail, "market is closed") {
		t.Fatalf("healthy ssr source must render plain ok: %+v", row.BriefRowState)
	}
	// Degraded is abnormal-for-session and keeps its weight even while closed.
	if row := closed["borrow"]; row.Status != rpc.BriefStatusDegraded || !strings.Contains(row.Detail, "source health is degraded") {
		t.Fatalf("closed degraded borrow row=%+v", row.BriefRowState)
	}
	borrowMissed := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusStale, RefreshState: rpc.SourceRefreshNotDue, AsOf: asOf,
	}}}
	if row := byKind(briefMarketEventRows(borrowMissed, rules, nil, false))["borrow"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("stale last-good from before the latest completed session was quieted: %+v", row.BriefRowState)
	}
	borrowCold := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusUnknown, RefreshState: rpc.SourceRefreshNotDue, AsOf: asOf,
	}}}
	if row := byKind(briefMarketEventRows(borrowCold, rules, nil, false))["borrow"]; row.Status != rpc.BriefStatusOK || !strings.Contains(row.Detail, "no fresh update expected") {
		t.Fatalf("not-yet-due cold borrow source=%+v", row.BriefRowState)
	}
	// A status outside the known vocabulary is never quiet-eligible: only
	// stale/unknown may read as expected idleness while the market is closed.
	weird := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{Source: "trading_halts", Status: "auth_failed", AsOf: asOf}}}
	if row := byKind(briefMarketEventRows(weird, rules, nil, false))["halt"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unrecognized status must degrade even closed: %+v", row.BriefRowState)
	}

	open := byKind(briefMarketEventRows(events, rules, nil, true))
	if row := open["halt"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open-session stale source must stay degraded: %+v", row.BriefRowState)
	}
	if row := open["borrow"]; row.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open-session degraded source must stay degraded: %+v", row.BriefRowState)
	}

	for _, row := range briefMarketEventRows(nil, rules, errors.New("positions unavailable"), false) {
		if row.Status != rpc.BriefStatusDegraded {
			t.Fatalf("hard source error must degrade even closed: %s=%+v", row.Kind, row.BriefRowState)
		}
	}

	cold := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusCold}
	if got := composeBriefGamma(cold, false, asOf); got.Status != rpc.BriefStatusDegraded || !strings.Contains(got.Detail, rpc.DataCadenceNoLastGood) {
		t.Fatalf("closed cold gamma=%+v", got.BriefRowState)
	}
	if got := composeBriefGamma(cold, true, asOf); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("open cold gamma=%+v", got.BriefRowState)
	}
	if got := composeBriefGamma(&rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusError}, false, asOf); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("gamma error must degrade even closed: %+v", got.BriefRowState)
	}
	mondayPreopen := time.Date(2026, 7, 20, 5, 5, 0, 0, time.UTC)
	lastSession := &rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: &rpc.GammaZeroComputed{
		AsOf: asOf, SpotUnderlying: 6300, GammaSign: "negative",
		Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityContextOnly, RankabilityReason: "freshness: market is closed; cached gamma is context only"},
	}}
	if got := composeBriefGamma(lastSession, false, mondayPreopen); got.Status != rpc.BriefStatusOK || !strings.Contains(got.Detail, "no newer regular-session compute is due") {
		t.Fatalf("last-completed-session gamma=%+v", got.BriefRowState)
	}
	blocked := *lastSession
	blockedResult := *lastSession.Result
	blocked.Result = &blockedResult
	blocked.Result.Quality = &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked, RankabilityReason: "oi_observed_coverage: SPX OI is incomplete"}
	if got := composeBriefGamma(&blocked, false, mondayPreopen); got.Status != rpc.BriefStatusDegraded || !strings.Contains(got.Detail, "OI is incomplete") {
		t.Fatalf("blocked last-session gamma=%+v", got.BriefRowState)
	}
}

func TestBriefBorrowHealthRequiresShortStockExposure(t *testing.T) {
	events := &rpc.MarketEventsResult{SourceHealth: []rpc.SourceHealth{{
		Source: "borrow_fee", Status: rpc.SourceStatusUnknown,
		LastFailure: &rpc.SourceFailure{Code: "timeout", Stage: "ftp_control_connect", Retryable: true},
	}}}
	rules := &rpc.RulesResult{}
	row := func(rows []rpc.BriefMarketEventRow) rpc.BriefMarketEventRow {
		for _, candidate := range rows {
			if candidate.Kind == "borrow" {
				return candidate
			}
		}
		return rpc.BriefMarketEventRow{}
	}
	allLong := false
	if got := row(briefMarketEventRows(events, rules, nil, true, &allLong)); got.Status != rpc.BriefStatusOK || !strings.Contains(got.Detail, "no short-stock exposure") {
		t.Fatalf("all-long borrow row=%+v, want explicit non-required OK", got.BriefRowState)
	}
	short := true
	if got := row(briefMarketEventRows(events, rules, nil, true, &short)); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("short-book borrow row=%+v, want degraded", got.BriefRowState)
	}
	if got := row(briefMarketEventRows(events, rules, nil, true, nil)); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("unknown exposure borrow row=%+v, want fail-closed degraded", got.BriefRowState)
	}

	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{Stocks: []rpc.PositionView{{Quantity: 10}}}, nil); relevant == nil || *relevant {
		t.Fatalf("all-long relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Stock: &rpc.PositionView{Quantity: -1}}}}, nil); relevant == nil || !*relevant {
		t.Fatalf("grouped short relevance=%v, want true", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{Stocks: []rpc.PositionView{{SecType: "FUT", Quantity: -1}}}, nil); relevant == nil || *relevant {
		t.Fatalf("short future relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(&rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{{Stock: &rpc.PositionView{SecType: "IND", Quantity: -1}}}}, nil); relevant == nil || *relevant {
		t.Fatalf("short index relevance=%v, want false", relevant)
	}
	if relevant := briefBorrowFeeRelevant(nil, errors.New("positions unavailable")); relevant != nil {
		t.Fatalf("unavailable positions relevance=%v, want nil", relevant)
	}
}

func TestBriefEarningsRowEscalatesWhenGoverningRuleUnknown(t *testing.T) {
	rules := &rpc.RulesResult{
		Rules:    []risk.RuleRow{{ID: "catalyst_coverage", Status: risk.RuleStatusUnknown}},
		Earnings: []rpc.EarningsInfo{{Symbol: "MSFT", Date: "2026-07-29", Source: "fetched"}},
	}
	rows := briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false)
	var earnings, halt rpc.BriefMarketEventRow
	for _, row := range rows {
		switch row.Kind {
		case "earnings":
			earnings = row
		case "halt":
			halt = row
		}
	}
	if earnings.Status != rpc.BriefStatusAttention || !strings.Contains(earnings.Detail, "catalyst coverage") || earnings.Count != 1 {
		t.Fatalf("earnings row must escalate on unknown governing rule: %+v", earnings)
	}
	if halt.Status != rpc.BriefStatusOK {
		t.Fatalf("halt row must not inherit the earnings escalation: %+v", halt.BriefRowState)
	}

	rules.Earnings = []rpc.EarningsInfo{{
		Symbol: "NOW", Source: "unknown", Status: rpc.EarningsStatusNoDatePublished,
		Reason: rpc.EarningsStatusNoDatePublished,
	}}
	for _, row := range briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false) {
		if row.Kind != "earnings" {
			continue
		}
		if row.Status != rpc.BriefStatusAttention || row.Count != 1 || len(row.Symbols) != 1 || row.Symbols[0] != "NOW" ||
			!strings.Contains(row.Detail, "NOW (no date published)") {
			t.Fatalf("sole unresolved catalyst must remain visible and non-green: %+v", row)
		}
	}

	rules.Rules[0].Status = risk.RuleStatusPass
	rules.Earnings = []rpc.EarningsInfo{{Symbol: "MSFT", Date: "2026-07-29", Source: "fetched", Status: rpc.EarningsStatusDate}}
	for _, row := range briefMarketEventRows(&rpc.MarketEventsResult{}, rules, nil, false) {
		if row.Kind == "earnings" && row.Status != rpc.BriefStatusOK {
			t.Fatalf("passing governing rule must not escalate: %+v", row.BriefRowState)
		}
	}
}

func TestBriefWSHEntitlementNoticeRequiresExactTypedTuple(t *testing.T) {
	makeRules := func(provider, code, stage string, retryable bool) *rpc.RulesResult {
		return &rpc.RulesResult{Earnings: []rpc.EarningsInfo{{Symbol: "SYNTH1", Providers: []rpc.EarningsProviderInfo{{
			Provider: provider, LastFailure: &rpc.SourceFailure{Code: code, Stage: stage, Retryable: retryable},
		}}}}}
	}
	for _, tc := range []struct {
		name  string
		rules *rpc.RulesResult
		want  bool
	}{
		{"metadata", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), true},
		{"event", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHEvent, false), true},
		{"provider", makeRules("nasdaq", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, false), false},
		{"code", makeRules("ibkr_wsh", rpc.SourceFailureInvalidPayload, rpc.SourceFailureStageWSHMetadata, false), false},
		{"stage", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHContractResolve, false), false},
		{"retryable", makeRules("ibkr_wsh", rpc.SourceFailureNotEntitled, rpc.SourceFailureStageWSHMetadata, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := briefWSHEntitlementNotice(tc.rules)
			if (got != "") != tc.want {
				t.Fatalf("notice=%q want=%t", got, tc.want)
			}
			if strings.Contains(got, "ibkr_wsh") || strings.Contains(got, "not_entitled") {
				t.Fatalf("notice exposed provider internals: %q", got)
			}
		})
	}
}

func TestBriefMoversAggregateByUnderlyingWithResidual(t *testing.T) {
	pos := &rpc.PositionsResult{ByUnderlying: []rpc.PositionGroup{
		{Underlying: "spy", GroupDailyPnLBase: new(10500.50)},
		{Underlying: "MSFT", GroupDailyPnLBase: new(-1600.25)},
		{Underlying: "GOOG", GroupDailyPnLBase: new(800.10)},
		{Underlying: "AMZN", GroupDailyPnLBase: new(-750.40)},
		{Underlying: "TSLA", GroupDailyPnLBase: new(-1400.30)},
		{Underlying: "ZVZZT"},
	}}
	row := briefMovers(pos, false)
	if len(row.Rows) != 3 || row.Rows[0].Symbol != "SPY" || row.Rows[1].Symbol != "MSFT" || row.Rows[2].Symbol != "TSLA" {
		t.Fatalf("rows=%+v", row.Rows)
	}
	if row.OtherPnLBase == nil || row.OtherCount != 2 {
		t.Fatalf("residual=%+v count=%d", row.OtherPnLBase, row.OtherCount)
	}
	if diff := *row.OtherPnLBase - (800.10 - 750.40); diff < -0.001 || diff > 0.001 {
		t.Fatalf("residual sum=%v", *row.OtherPnLBase)
	}
	if !strings.Contains(row.Detail, "by underlying") || !strings.Contains(row.Detail, "off-session marks") {
		t.Fatalf("detail=%q", row.Detail)
	}
	if open := briefMovers(pos, true); strings.Contains(open.Detail, "off-session") {
		t.Fatalf("open-session detail=%q", open.Detail)
	}
	if got := briefMovers(&rpc.PositionsResult{}, true); got.Status != rpc.BriefStatusDegraded {
		t.Fatalf("empty movers=%+v", got.BriefRowState)
	}
}

func briefTestCurrentAccountDataAuthority(source rpc.AccountDataSource) *rpc.AccountDataAuthority {
	return &rpc.AccountDataAuthority{
		Scope:        rpc.AccountDataScope{AccountID: "DU123", AccountMode: rpc.AccountModePaper},
		Source:       source,
		Availability: rpc.AccountDataAvailable,
		Freshness:    rpc.AccountDataFreshnessCurrent,
		AsOf:         time.Date(2026, 8, 6, 9, 30, 0, 0, time.UTC),
	}
}

func TestBriefPortfolioRequiresCurrentAccountDataAuthority(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	dailyPnL := 125.0
	account := &rpc.AccountResult{
		NetLiquidation: 230175, DailyPnL: &dailyPnL, BaseCurrency: "EUR",
		Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourceAccountSummaryRequest),
	}
	currentPositions := briefTestCurrentAccountDataAuthority(rpc.AccountDataSourcePortfolioStream)

	tests := []struct {
		name      string
		authority *rpc.AccountDataAuthority
	}{
		{name: "missing"},
		{name: "unavailable", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}},
		{name: "stale", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessStale}},
		{name: "unknown age", authority: &rpc.AccountDataAuthority{Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessUnknown}},
	}
	for _, tc := range tests {
		t.Run("positions "+tc.name, func(t *testing.T) {
			out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: tc.authority}, nil, nil, true)
			for name, row := range map[string]rpc.BriefRowState{
				"movers": out.Movers.BriefRowState, "premium": out.PremiumAtRisk.BriefRowState, "hedge": out.HedgeCost.BriefRowState,
			} {
				if row.Status != rpc.BriefStatusUnavailable {
					t.Fatalf("%s row=%+v, want unavailable", name, row)
				}
			}
			if out.PremiumAtRisk.AmountBase != nil || out.HedgeCost.AmountBase != nil {
				t.Fatalf("unavailable positions produced clean zero amounts: premium=%v hedge=%v", out.PremiumAtRisk.AmountBase, out.HedgeCost.AmountBase)
			}
		})
	}

	t.Run("account unavailable", func(t *testing.T) {
		unavailable := *account
		unavailable.Authority = &rpc.AccountDataAuthority{Availability: rpc.AccountDataUnavailable, Freshness: rpc.AccountDataFreshnessUnknown}
		out := s.composeBriefPortfolio(&unavailable, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.Account.Status != rpc.BriefStatusUnavailable || out.Account.EquityBase != nil || out.Account.DailyPnLBase != nil {
			t.Fatalf("unavailable account row=%+v, want unavailable without money", out.Account)
		}
	})

	t.Run("current empty book", func(t *testing.T) {
		out := s.composeBriefPortfolio(account, &rpc.PositionsResult{Authority: currentPositions}, nil, nil, true)
		if out.PremiumAtRisk.Status != rpc.BriefStatusOK || out.HedgeCost.Status != rpc.BriefStatusOK ||
			out.PremiumAtRisk.AmountBase == nil || *out.PremiumAtRisk.AmountBase != 0 ||
			out.HedgeCost.AmountBase == nil || *out.HedgeCost.AmountBase != 0 {
			t.Fatalf("current empty book lost its genuine zero: premium=%+v hedge=%+v", out.PremiumAtRisk, out.HedgeCost)
		}
	})
}

func TestBriefPremiumDisclosesUnknownHedgeClassification(t *testing.T) {
	s := newRiskPolicyTestServer(t, dailyBriefPolicyTOML())
	pos := &rpc.PositionsResult{Options: []rpc.PositionView{
		{Symbol: "NOW", SecType: "OPT", Right: "C", Quantity: 10, MarketValueBase: new(4265.0)},
		{Symbol: "SPY", SecType: "OPT", Right: "P", Quantity: 50, Multiplier: 100, MarketValueBase: new(54544.0)},
	}, Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourcePortfolioStream)}
	acct := &rpc.AccountResult{NetLiquidation: 230175, DailyPnL: new(7389.46), BaseCurrency: "EUR", Authority: briefTestCurrentAccountDataAuthority(rpc.AccountDataSourceAccountSummaryRequest)}
	out := s.composeBriefPortfolio(acct, pos, nil, nil, false)
	if out.PremiumAtRisk.Status != rpc.BriefStatusDegraded || !strings.Contains(out.PremiumAtRisk.Detail, "protective share") {
		t.Fatalf("premium must disclose unknown hedge classification: %+v", out.PremiumAtRisk)
	}
	if out.PremiumAtRisk.IncludedLegs != 2 || out.PremiumAtRisk.AmountBase == nil {
		t.Fatalf("premium amount must stay complete: %+v", out.PremiumAtRisk)
	}
	if out.HedgeCost.ExcludedLegs != 1 {
		t.Fatalf("hedge=%+v", out.HedgeCost)
	}
	if !strings.Contains(out.Account.Detail, "market closed") {
		t.Fatalf("closed-session account detail=%q", out.Account.Detail)
	}
}
