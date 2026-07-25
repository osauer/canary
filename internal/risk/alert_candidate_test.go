package risk

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestBuildAlertEpisodeKeyIsOpaqueStableAndDomainSeparated(t *testing.T) {
	const sensitive = "ACCOUNT-SECRET/ORDER-SECRET/SYMBOL-SECRET"
	first, err := BuildAlertEpisodeKey(AlertSourceRulebook, AlertKindPortfolioRisk, "  "+sensitive+"  ", "concentration")
	if err != nil {
		t.Fatal(err)
	}
	again, err := BuildAlertEpisodeKey(AlertSourceRulebook, AlertKindPortfolioRisk, sensitive, "concentration")
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("episode key is not stable across boundary whitespace: %q != %q", first, again)
	}
	if strings.Contains(first, sensitive) || !strings.HasPrefix(first, alertEpisodeKeyPrefix) || len(first) != len(alertEpisodeKeyPrefix)+64 {
		t.Fatalf("episode key is not opaque: %q", first)
	}

	variants := []struct {
		source AlertSource
		kind   AlertKind
		parts  []string
	}{
		{AlertSourceRegime, AlertKindPortfolioRisk, []string{sensitive, "concentration"}},
		{AlertSourceRulebook, AlertKindMarketState, []string{sensitive, "concentration"}},
		{AlertSourceRulebook, AlertKindPortfolioRisk, []string{"concentration", sensitive}},
		{AlertSourceRulebook, AlertKindPortfolioRisk, []string{sensitive, "different"}},
	}
	for _, variant := range variants {
		got, err := BuildAlertEpisodeKey(variant.source, variant.kind, variant.parts...)
		if err != nil {
			t.Fatal(err)
		}
		if got == first {
			t.Fatalf("domain-separated identity collided for %#v", variant)
		}
	}
}

func TestBuildAlertEpisodeKeyRejectsUnboundedOrUntypedIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		source AlertSource
		kind   AlertKind
		parts  []string
	}{
		{"unknown source", "broker-free-text", AlertKindPortfolioRisk, []string{"x"}},
		{"unknown kind", AlertSourceStress, "caller-selected", []string{"x"}},
		{"missing parts", AlertSourceStress, AlertKindPortfolioRisk, nil},
		{"blank part", AlertSourceStress, AlertKindPortfolioRisk, []string{" \n\t"}},
		{"oversized part", AlertSourceStress, AlertKindPortfolioRisk, []string{strings.Repeat("x", 1025)}},
		{"too many parts", AlertSourceStress, AlertKindPortfolioRisk, make([]string, 17)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := BuildAlertEpisodeKey(test.source, test.kind, test.parts...); err == nil {
				t.Fatal("invalid episode identity was accepted")
			}
		})
	}
}

func TestAlertCandidateSeparatesEpisodeAndChangingEvidence(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	first := validAlertCandidate(t, now)
	second := first
	second.EvidenceFingerprint = testAlertFingerprint("b")
	second.ObservedAt = now.Add(time.Minute)
	second.EvidenceAsOf = now.Add(time.Minute)
	if first.EpisodeKey != second.EpisodeKey || first.EvidenceFingerprint == second.EvidenceFingerprint {
		t.Fatalf("identity separation failed: first=%#v second=%#v", first, second)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := second.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBuildAlertOccurrenceKeySeparatesDaemonOccurrenceFromEpisodeAndEvidence(t *testing.T) {
	episode, err := BuildAlertEpisodeKey(AlertSourceStress, AlertKindPortfolioRisk, "root-problem")
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildAlertOccurrenceKey(episode, "daemon-occurrence-1")
	if err != nil {
		t.Fatal(err)
	}
	firstAgain, err := BuildAlertOccurrenceKey(episode, "  daemon-occurrence-1  ")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildAlertOccurrenceKey(episode, "daemon-occurrence-2")
	if err != nil {
		t.Fatal(err)
	}
	if first != firstAgain || first == second || first == episode {
		t.Fatalf("occurrence identity separation failed: first=%q again=%q second=%q episode=%q", first, firstAgain, second, episode)
	}
	if !strings.HasPrefix(first, alertOccurrenceKeyPrefix) || strings.Contains(first, "daemon-occurrence") {
		t.Fatalf("occurrence key is not opaque: %q", first)
	}
	for _, test := range []struct {
		episode string
		parts   []string
	}{
		{"bad", []string{"x"}},
		{episode, nil},
		{episode, []string{""}},
		{episode, []string{strings.Repeat("x", 1025)}},
	} {
		if _, err := BuildAlertOccurrenceKey(test.episode, test.parts...); err == nil {
			t.Fatalf("invalid occurrence identity was accepted: %#v", test)
		}
	}
}

func TestAlertOpaqueIdentityDomainsAreStableAcrossWireSchemaChanges(t *testing.T) {
	authority, err := BuildAlertAuthorityScope(" du123 ", " PAPER ")
	if err != nil {
		t.Fatal(err)
	}
	episode, err := BuildAlertEpisodeKey(AlertSourceStress, AlertKindPortfolioRisk, "DU123", "paper", "portfolio_canary")
	if err != nil {
		t.Fatal(err)
	}
	occurrence, err := BuildAlertOccurrenceKey(episode, "sequence:1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"alert-authority-scope-v1:ec9e283b2fbc97b50e1c2ec31b84e0eec8e09f087945275c9cd25700179af4e7",
		"alert-episode-v1:2a1770b3398ce40f3109db336ea570897505a39efe7fb5c12029ab4f696b337a",
		"alert-occurrence-v1:a699174f7a3d1d4aa8ceb2e3be502c71f8d95cddf3cd807f670c27dbdfc23c5f",
	}
	if got := []string{authority, episode, occurrence}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stable alert identities changed: got=%q want=%q", got, want)
	}
	if AlertCandidateSnapshotVersion == alertEpisodeIdentityVersion || AlertCandidateSnapshotVersion == alertOccurrenceIdentityVersion || AlertCandidateSnapshotVersion == alertAuthorityIdentityVersion {
		t.Fatal("wire schema version is coupled to a stable identity domain")
	}
}

func TestAlertOccurrenceKeyRotatesOnlyForDaemonOccurrenceDecisions(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	opened := validAlertCandidate(t, now)

	evidenceRevision := opened
	evidenceRevision.EvidenceFingerprint = testAlertFingerprint("b")
	evidenceRevision.ObservedAt = now.Add(time.Minute)
	evidenceRevision.EvidenceAsOf = now.Add(time.Minute)
	if evidenceRevision.OccurrenceKey != opened.OccurrenceKey {
		t.Fatal("evidence-only revision rotated occurrence key")
	}

	recovered := evidenceRevision
	recovered.State = AlertEpisodeRecovered
	recovered.StateChangedAt = now.Add(2 * time.Minute)
	recovered.ObservedAt = now.Add(2 * time.Minute)
	recovered.EvidenceAsOf = now.Add(2 * time.Minute)
	if recovered.OccurrenceKey != opened.OccurrenceKey {
		t.Fatal("recovery rotated occurrence key")
	}

	qualifyingEscalation := evidenceRevision
	qualifyingEscalation.State = AlertEpisodeEscalated
	qualifyingEscalation.Severity = AlertSeverityAct
	qualifyingEscalation.StateChangedAt = now.Add(2 * time.Minute)
	qualifyingEscalation.ObservedAt = now.Add(2 * time.Minute)
	qualifyingEscalation.EvidenceAsOf = now.Add(2 * time.Minute)
	qualifyingEscalation.OccurrenceKey = mustTestAlertOccurrenceKey(t, opened.EpisodeKey, "daemon-page-worthy-escalation-1")
	if qualifyingEscalation.OccurrenceKey == opened.OccurrenceKey {
		t.Fatal("daemon-qualified escalation reused opening occurrence key")
	}

	reopened := opened
	reopened.ObservedAt = now.Add(3 * time.Minute)
	reopened.EvidenceAsOf = now.Add(3 * time.Minute)
	reopened.StateChangedAt = now.Add(3 * time.Minute)
	reopened.OccurrenceKey = mustTestAlertOccurrenceKey(t, opened.EpisodeKey, "daemon-reopen-2")
	if reopened.OccurrenceKey == opened.OccurrenceKey {
		t.Fatal("daemon reopen reused prior occurrence key")
	}

	for name, candidate := range map[string]AlertCandidate{
		"opened": opened, "evidence revision": evidenceRevision, "recovered": recovered,
		"qualifying escalation": qualifyingEscalation, "reopened": reopened,
	} {
		if err := candidate.Validate(); err != nil {
			t.Fatalf("%s candidate invalid: %v", name, err)
		}
	}
}

func TestAlertCandidateRejectsMalformedAndIncoherentValues(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	valid := validAlertCandidate(t, now)
	tests := []struct {
		name   string
		mutate func(*AlertCandidate)
	}{
		{"episode key", func(c *AlertCandidate) { c.EpisodeKey = "sha256:" + strings.Repeat("a", 64) }},
		{"uppercase episode key", func(c *AlertCandidate) { c.EpisodeKey = alertEpisodeKeyPrefix + strings.Repeat("A", 64) }},
		{"occurrence key", func(c *AlertCandidate) { c.OccurrenceKey = "occurrence-private" }},
		{"evidence fingerprint", func(c *AlertCandidate) { c.EvidenceFingerprint = "raw-evidence" }},
		{"source", func(c *AlertCandidate) { c.Source = "broker-selected" }},
		{"kind", func(c *AlertCandidate) { c.Kind = "free-text" }},
		{"state", func(c *AlertCandidate) { c.State = "cleared-by-caller" }},
		{"severity", func(c *AlertCandidate) { c.Severity = "panic-now" }},
		{"presentation", func(c *AlertCandidate) { c.PresentationCode = "free_text" }},
		{"evidence health", func(c *AlertCandidate) { c.EvidenceHealth = "probably-fine" }},
		{"destination", func(c *AlertCandidate) { c.Destination = "https://attacker.invalid" }},
		{"missing evidence time", func(c *AlertCandidate) { c.EvidenceAsOf = time.Time{} }},
		{"missing transition time", func(c *AlertCandidate) { c.StateChangedAt = time.Time{} }},
		{"missing observation time", func(c *AlertCandidate) { c.ObservedAt = time.Time{} }},
		{"future evidence", func(c *AlertCandidate) { c.EvidenceAsOf = c.ObservedAt.Add(time.Nanosecond) }},
		{"future transition", func(c *AlertCandidate) { c.StateChangedAt = c.ObservedAt.Add(time.Nanosecond) }},
		{"unproven recovery", func(c *AlertCandidate) { c.State = AlertEpisodeRecovered; c.EvidenceHealth = AlertEvidenceStale }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("invalid candidate passed validation: %#v", candidate)
			}
			if raw, err := json.Marshal(candidate); err == nil {
				t.Fatalf("invalid candidate reached JSON wire: %s", raw)
			}
		})
	}
}

func TestAlertCandidateHasNoDisplayOccurrenceOrDeliveryTargetFields(t *testing.T) {
	typeOf := reflect.TypeFor[AlertCandidate]()
	wantFields := []string{
		"Destination", "EpisodeKey", "EvidenceAsOf", "EvidenceFingerprint", "EvidenceHealth", "Kind", "ObservedAt",
		"OccurrenceKey", "PresentationCode", "Severity", "Source", "State", "StateChangedAt",
	}
	gotFields := make([]string, 0, typeOf.NumField())
	for field := range typeOf.Fields() {
		gotFields = append(gotFields, field.Name)
	}
	sort.Strings(gotFields)
	if !reflect.DeepEqual(gotFields, wantFields) {
		t.Fatalf("AlertCandidate fields = %v, want restricted %v", gotFields, wantFields)
	}

	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	const hostile = "ACCOUNT SYMBOL ORDER DEVICE TOKEN PRIVATE MESSAGE"
	key, err := BuildAlertEpisodeKey(AlertSourceOrderIntegrity, AlertKindOrderIntegrity, hostile)
	if err != nil {
		t.Fatal(err)
	}
	candidate := validAlertCandidate(t, now)
	candidate.EpisodeKey = key
	candidate.OccurrenceKey = mustTestAlertOccurrenceKey(t, key, "occurrence-1")
	raw, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), hostile) {
		t.Fatalf("private identity leaked to candidate wire: %s", raw)
	}
	for _, forbidden := range []string{"title", "body", "message", "details", "account_id", "symbol", "order_id", "display_id", "target_id", "device_id", "delivery_id"} {
		if strings.Contains(string(raw), `"`+forbidden+`"`) {
			t.Fatalf("forbidden field %q reached candidate wire: %s", forbidden, raw)
		}
	}
}

func TestAlertCoverageSetSemantics(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		coverage AlertCoverage
		valid    bool
	}{
		{"complete", completeAlertCoverage(now), true},
		{"complete stale", func() AlertCoverage { c := completeAlertCoverage(now); c.Freshness = AlertCoverageStale; return c }(), true},
		{"partial", AlertCoverage{State: AlertCoveragePartial, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceRegime}, CoveredSources: []AlertSource{AlertSourceStress}}, true},
		{"unavailable", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageUnknown, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}, CoveredSources: []AlertSource{}}, true},
		{"nil expected", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageUnknown, AsOf: now, CoveredSources: []AlertSource{}}, false},
		{"nil covered", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageUnknown, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}}, false},
		{"unknown expected", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageUnknown, AsOf: now, ExpectedSources: []AlertSource{"raw"}, CoveredSources: []AlertSource{}}, false},
		{"duplicate expected", AlertCoverage{State: AlertCoverageComplete, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceStress}, CoveredSources: []AlertSource{AlertSourceStress}}, false},
		{"covered outside expected", AlertCoverage{State: AlertCoveragePartial, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceRegime}, CoveredSources: []AlertSource{AlertSourceRulebook}}, false},
		{"false complete", AlertCoverage{State: AlertCoverageComplete, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceRegime}, CoveredSources: []AlertSource{AlertSourceStress}}, false},
		{"false partial empty", AlertCoverage{State: AlertCoveragePartial, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}, CoveredSources: []AlertSource{}}, false},
		{"false partial full", AlertCoverage{State: AlertCoveragePartial, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}, CoveredSources: []AlertSource{AlertSourceStress}}, false},
		{"false unavailable", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageUnknown, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}, CoveredSources: []AlertSource{AlertSourceStress}}, false},
		{"unavailable current", AlertCoverage{State: AlertCoverageUnavailable, Freshness: AlertCoverageCurrent, AsOf: now, ExpectedSources: []AlertSource{AlertSourceStress}, CoveredSources: []AlertSource{}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.coverage.Validate()
			if (err == nil) != test.valid {
				t.Fatalf("Validate() error = %v, want valid=%v", err, test.valid)
			}
		})
	}
}

func TestAlertSnapshotClearRequiresCompleteCurrentCoverage(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)

	clear := validAlertSnapshot(now)
	if err := clear.Validate(); err != nil || !clear.IsClear() {
		t.Fatalf("complete current empty snapshot was not clear: err=%v snapshot=%#v", err, clear)
	}

	partial := clear
	partial.Sources = append([]AlertSourceCoverage(nil), clear.Sources...)
	partial.CurrentState = AlertSnapshotUnknown
	partial.Coverage.State = AlertCoveragePartial
	partial.Coverage.CoveredSources = []AlertSource{AlertSourceStress}
	partial.Sources[1].Covered = false
	partial.Sources[1].EvidenceHealth = AlertEvidenceUnavailable
	if err := partial.Validate(); err != nil || partial.IsClear() {
		t.Fatalf("partial empty snapshot did not remain unknown: err=%v snapshot=%#v", err, partial)
	}
	falseClear := partial
	falseClear.CurrentState = AlertSnapshotClear
	if err := falseClear.Validate(); err == nil || falseClear.IsClear() {
		t.Fatal("partial empty snapshot claimed clear")
	}

	stale := clear
	stale.CurrentState = AlertSnapshotUnknown
	stale.Coverage.Freshness = AlertCoverageStale
	if err := stale.Validate(); err != nil || stale.IsClear() {
		t.Fatalf("stale empty snapshot did not remain unknown: err=%v snapshot=%#v", err, stale)
	}

	misdatedCurrent := clear
	misdatedCurrent.Coverage.AsOf = now.Add(-time.Hour)
	if err := misdatedCurrent.Validate(); err == nil || misdatedCurrent.IsClear() {
		t.Fatal("current coverage with an older authority timestamp claimed clear")
	}

	active := partial
	active.CurrentState = AlertSnapshotActive
	active.Candidates = []AlertCandidate{validAlertCandidate(t, now)}
	if err := active.Validate(); err != nil || active.IsClear() {
		t.Fatalf("active partial snapshot failed: err=%v snapshot=%#v", err, active)
	}

	recovered := validAlertCandidate(t, now)
	recovered.State = AlertEpisodeRecovered
	recovered.EvidenceHealth = AlertEvidenceCurrent
	clear.Candidates = []AlertCandidate{recovered}
	if err := clear.Validate(); err != nil || !clear.IsClear() {
		t.Fatalf("current recovered occurrence should permit clear: err=%v snapshot=%#v", err, clear)
	}
}

func TestAlertSnapshotRejectsDuplicateEpisodesFutureOrUncoveredState(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	valid := validAlertSnapshot(now)
	candidate := validAlertCandidate(t, now)

	duplicate := valid
	duplicate.CurrentState = AlertSnapshotActive
	duplicate.Candidates = []AlertCandidate{candidate, candidate}
	duplicate.Candidates[1].EvidenceFingerprint = testAlertFingerprint("b")
	if err := duplicate.Validate(); err == nil {
		t.Fatal("duplicate episode key was accepted")
	}

	duplicateOccurrence := valid
	duplicateOccurrence.CurrentState = AlertSnapshotActive
	duplicateOccurrence.Candidates = []AlertCandidate{candidate, candidate}
	duplicateOccurrence.Candidates[1].Source = AlertSourceRegime
	duplicateOccurrence.Candidates[1].Kind = AlertKindMarketState
	duplicateOccurrence.Candidates[1].PresentationCode = AlertPresentationRegimeMarketStress
	duplicateOccurrence.Candidates[1].EpisodeKey, _ = BuildAlertEpisodeKey(AlertSourceRegime, AlertKindMarketState, "separate-root-problem")
	duplicateOccurrence.Candidates[1].EvidenceFingerprint = testAlertFingerprint("b")
	if err := duplicateOccurrence.Validate(); err == nil {
		t.Fatal("duplicate occurrence key across distinct root episodes was accepted")
	}

	future := valid
	future.CurrentState = AlertSnapshotActive
	future.Candidates = []AlertCandidate{candidate}
	future.Candidates[0].ObservedAt = now.Add(time.Nanosecond)
	future.Candidates[0].EvidenceAsOf = future.Candidates[0].ObservedAt
	if err := future.Validate(); err == nil {
		t.Fatal("future candidate was accepted")
	}

	outside := valid
	outside.CurrentState = AlertSnapshotActive
	outside.Candidates = []AlertCandidate{candidate}
	outside.Candidates[0].Source = AlertSourceRulebook
	outside.Candidates[0].EpisodeKey, _ = BuildAlertEpisodeKey(AlertSourceRulebook, AlertKindPortfolioRisk, "book")
	outside.Candidates[0].OccurrenceKey = mustTestAlertOccurrenceKey(t, outside.Candidates[0].EpisodeKey, "occurrence-1")
	if err := outside.Validate(); err == nil {
		t.Fatal("candidate outside declared coverage universe was accepted")
	}
}

func TestAlertSnapshotCurrentEvidenceRequiresCoveredSource(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	candidate := validAlertCandidate(t, now)
	candidate.Source = AlertSourceRegime
	candidate.Kind = AlertKindMarketState
	candidate.PresentationCode = AlertPresentationRegimeMarketStress
	candidate.EpisodeKey, _ = BuildAlertEpisodeKey(AlertSourceRegime, AlertKindMarketState, "regime-root")
	candidate.OccurrenceKey = mustTestAlertOccurrenceKey(t, candidate.EpisodeKey, "occurrence-1")

	snapshot := validAlertSnapshot(now)
	snapshot.CurrentState = AlertSnapshotActive
	snapshot.Coverage.State = AlertCoveragePartial
	snapshot.Coverage.CoveredSources = []AlertSource{AlertSourceStress}
	snapshot.Sources[1].Covered = false
	snapshot.Sources[1].EvidenceHealth = AlertEvidenceUnavailable
	snapshot.Candidates = []AlertCandidate{candidate}
	if err := snapshot.Validate(); err == nil {
		t.Fatal("current candidate from an uncovered source was accepted")
	}

	snapshot.Candidates[0].EvidenceHealth = AlertEvidenceStale
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("stale retained candidate from expected uncovered source was rejected: %v", err)
	}

	snapshot.Candidates[0].State = AlertEpisodeRecovered
	snapshot.Candidates[0].EvidenceHealth = AlertEvidenceStale
	if err := snapshot.Validate(); err == nil {
		t.Fatal("stale uncovered candidate claimed recovery")
	}
}

func TestAlertJSONRoundTripAndExactObjectBoundary(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	input := validAlertSnapshot(now)
	input.CurrentState = AlertSnapshotActive
	input.Candidates = []AlertCandidate{validAlertCandidate(t, now)}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AlertCandidateSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, input) {
		t.Fatalf("round trip mismatch:\n got %#v\nwant %#v", decoded, input)
	}

	for _, test := range []struct {
		name string
		raw  string
	}{
		{"unknown top-level key", strings.TrimSuffix(string(raw), "}") + `,"account_id":"secret"}`},
		{"duplicate top-level key", strings.TrimSuffix(string(raw), "}") + `,"as_of":"2026-07-20T20:00:00Z"}`},
		{"missing top-level key", `{"schema_version":"alert-candidate-snapshot-v1"}`},
		{"null top-level key", strings.Replace(string(raw), `"candidates":[`, `"candidates":null,"ignored":[`, 1)},
		{"trailing value", string(raw) + `{}`},
		{"unknown candidate key", strings.Replace(string(raw), `"observed_at":"2026-07-20T20:00:00Z"`, `"observed_at":"2026-07-20T20:00:00Z","symbol":"secret"`, 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got AlertCandidateSnapshot
			if err := json.Unmarshal([]byte(test.raw), &got); err == nil {
				t.Fatalf("adversarial JSON was accepted: %s", test.raw)
			}
		})
	}
}

func TestAlertPresentationCodeIsClosedAndSourceBound(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	candidate := validAlertCandidate(t, now)
	if candidate.PresentationCode != AlertPresentationPortfolioStress {
		t.Fatalf("presentation code=%q", candidate.PresentationCode)
	}
	candidate.PresentationCode = AlertPresentationRulebookSingleNameExposure
	if err := candidate.Validate(); err == nil {
		t.Fatal("candidate accepted another source's presentation code")
	}
}

func validAlertCandidate(t *testing.T, now time.Time) AlertCandidate {
	t.Helper()
	key, err := BuildAlertEpisodeKey(AlertSourceStress, AlertKindPortfolioRisk, "synthetic-book-condition")
	if err != nil {
		t.Fatal(err)
	}
	return AlertCandidate{
		EpisodeKey:          key,
		OccurrenceKey:       mustTestAlertOccurrenceKey(t, key, "occurrence-1"),
		EvidenceFingerprint: testAlertFingerprint("a"),
		Source:              AlertSourceStress,
		Kind:                AlertKindPortfolioRisk,
		PresentationCode:    AlertPresentationPortfolioStress,
		State:               AlertEpisodeOpen,
		Severity:            AlertSeverityWatch,
		EvidenceHealth:      AlertEvidenceCurrent,
		Destination:         AlertDestinationMonitor,
		EvidenceAsOf:        now.Add(-time.Minute),
		StateChangedAt:      now.Add(-2 * time.Minute),
		ObservedAt:          now,
	}
}

func mustTestAlertOccurrenceKey(t *testing.T, episodeKey string, identity string) string {
	t.Helper()
	key, err := BuildAlertOccurrenceKey(episodeKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func completeAlertCoverage(now time.Time) AlertCoverage {
	return AlertCoverage{
		State:           AlertCoverageComplete,
		Freshness:       AlertCoverageCurrent,
		AsOf:            now,
		ExpectedSources: []AlertSource{AlertSourceStress, AlertSourceRegime},
		CoveredSources:  []AlertSource{AlertSourceStress, AlertSourceRegime},
	}
}

func validAlertSnapshot(now time.Time) AlertCandidateSnapshot {
	authority, err := BuildAlertAuthorityScope("DU-TEST", "paper")
	if err != nil {
		panic(err)
	}
	return AlertCandidateSnapshot{
		SchemaVersion: AlertCandidateSnapshotVersion, AuthorityScope: authority,
		AsOf:         now,
		CurrentState: AlertSnapshotClear,
		Coverage:     completeAlertCoverage(now),
		Sources: []AlertSourceCoverage{
			{Source: AlertSourceStress, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent, InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true},
			{Source: AlertSourceRegime, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent, InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true},
		},
		Candidates: []AlertCandidate{},
	}
}

func testAlertFingerprint(digit string) string {
	return alertEvidenceFingerprintPrefix + strings.Repeat(digit, 64)
}
