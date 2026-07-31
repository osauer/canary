package rpc

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestRegimeFingerprintIgnoresTimestampsRawValuesAndProse(t *testing.T) {
	t.Parallel()
	vix := 18.0
	base := RegimeSnapshotResult{
		AsOf: time.Date(2026, 5, 31, 8, 30, 0, 0, time.UTC),
		Summary: RegimeSummary{
			Label:     "Normal regime",
			Evidence:  "some rendered prose",
			PunchLine: "do not hash this",
		},
		Composite: RegimeComposite{Verdict: "Normal regime", GreenCount: 1, RankedCount: 1, ClusterGreenCount: 1, ClusterRankedCount: 1},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "green"},
			Status:              RegimeStatusOK,
			VIX:                 &vix,
			Notes:               "long methodology prose",
		},
	}
	first := BuildRegimeFingerprint(&base)

	changed := base
	changed.AsOf = changed.AsOf.Add(time.Hour)
	changed.Summary.Evidence = "different rendered prose"
	changed.Summary.PunchLine = "different punch line"
	changed.VIXTermStructure.Notes = "different notes"
	changedVIX := 19.25
	changed.VIXTermStructure.VIX = &changedVIX
	second := BuildRegimeFingerprint(&changed)

	if first != second {
		t.Fatalf("fingerprint changed on timestamp/raw/prose-only mutation: %v != %v", first, second)
	}
}

func TestRegimeFingerprintTracksClassifiedStateAndCanonicalOrdering(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Composite: RegimeComposite{Verdict: "Stress signal present", RedCount: 1, RankedCount: 1, ClusterRedCount: 1, ClusterRankedCount: 1},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red"},
			Status:              RegimeStatusOK,
			FieldsMissing:       []string{"ratio", "vix3m"},
		},
		WarningDetails: []RegimeWarning{
			{Code: "b", Scope: "breadth", Severity: "warning", Message: "ignored"},
			{Code: "a", Scope: "gamma", Severity: "info", Impact: "ignored"},
		},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "partial", StaleClusters: []string{"vol", "credit"}, PartialClusters: []string{"breadth", "gamma"}},
			{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		},
	}
	reordered := base
	reordered.VIXTermStructure.FieldsMissing = []string{"vix3m", "ratio"}
	reordered.WarningDetails = []RegimeWarning{base.WarningDetails[1], base.WarningDetails[0]}
	reordered.DataQuality = []DataQualityHealth{
		{Surface: "gamma", Status: "degraded", DegradedClusters: []string{"gamma"}},
		{Surface: "regime", Status: "partial", StaleClusters: []string{"credit", "vol"}, PartialClusters: []string{"gamma", "breadth"}},
	}
	if first, second := BuildRegimeFingerprint(&base), BuildRegimeFingerprint(&reordered); first != second {
		t.Fatalf("fingerprint should canonicalize slice ordering: %v != %v", first, second)
	}

	changedBand := base
	changedBand.VIXTermStructure.Band = "yellow"
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedBand) {
		t.Fatal("fingerprint did not change when indicator band changed")
	}

	changedWarning := base
	changedWarning.WarningDetails = slices.Clone(base.WarningDetails)
	changedWarning.WarningDetails[0].Severity = "error"
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedWarning) {
		t.Fatal("fingerprint did not change when warning semantic severity changed")
	}

	changedPartial := base
	changedPartial.DataQuality = slices.Clone(base.DataQuality)
	changedPartial.DataQuality[0].PartialClusters = []string{"gamma"}
	if BuildRegimeFingerprint(&base) == BuildRegimeFingerprint(&changedPartial) {
		t.Fatal("fingerprint did not change when partial cluster set changed")
	}
}

func TestRegimeFingerprintMatchesSnapshotAcceptsExactFrozenV1(t *testing.T) {
	t.Parallel()
	snapshot := RegimeSnapshotResult{
		Composite: RegimeComposite{
			Verdict: "Stress signal present", RedCount: 1, RankedCount: 1,
			ClusterRedCount: 1, ClusterRankedCount: 1,
		},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{Band: "red"},
			Status:              RegimeStatusOK,
		},
		SourceHealth: []SourceHealth{{
			Source: "vol", Status: SourceStatusOK, Confidence: "high",
			FingerprintStability: FingerprintStabilitySemanticBuckets,
		}},
	}
	legacy := buildRegimeFingerprint(&snapshot, regimeFingerprintVersionV1, sourceHealthFingerprintsV1(snapshot.SourceHealth))
	const wantLegacyKey = "sha256:2adc454d653c9a2c2a7b6ccfae77e98be6c3267bdfaed87ac41421d12bcd561b"
	if legacy.Key != wantLegacyKey {
		t.Fatalf("frozen v1 key = %q, want %q", legacy.Key, wantLegacyKey)
	}
	snapshot.Fingerprint = legacy
	if !RegimeFingerprintMatchesSnapshot(&snapshot) {
		t.Fatal("exact frozen v1 fingerprint was rejected")
	}

	tampered := snapshot
	tampered.Composite.RedCount++
	if RegimeFingerprintMatchesSnapshot(&tampered) {
		t.Fatal("v1 fingerprint accepted changed classified state")
	}

	withFutureField := snapshot
	withFutureField.SourceHealth = slices.Clone(snapshot.SourceHealth)
	withFutureField.SourceHealth[0].LastFailure = &SourceFailure{Code: SourceFailureTimeout}
	if RegimeFingerprintMatchesSnapshot(&withFutureField) {
		t.Fatal("v1 fingerprint accepted source failure metadata absent from the frozen schema")
	}

	unknown := snapshot
	unknown.Fingerprint.Version = "regime-fp-v99"
	if RegimeFingerprintMatchesSnapshot(&unknown) {
		t.Fatal("unknown regime fingerprint version was accepted")
	}
}

func TestMarketEventsFingerprintTracksSemanticFlagsOnly(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	base := MarketEventsResult{
		Kind:          MarketEventsKind,
		SchemaVersion: MarketEventsSchemaVersion,
		AsOf:          now,
		Symbols:       []string{"XYZ"},
		Flags: []MarketEventFlag{{
			ID:         MarketEventRegSHOThreshold,
			Symbol:     "XYZ",
			Label:      "Reg SHO",
			Status:     MarketEventStatusActive,
			Severity:   MarketEventSeverityWatch,
			Role:       MarketEventRoleContext,
			Source:     "Nasdaq",
			AsOf:       now,
			ObservedAt: now,
			Details:    []string{"raw prose ignored"},
		}},
		SourceHealth: []SourceHealth{{Source: "reg_sho_threshold", Status: SourceStatusOK, Confidence: "high"}},
	}
	first := BuildMarketEventsFingerprint(&base)
	changedTime := base
	changedTime.AsOf = now.Add(time.Minute)
	changedTime.Flags = slices.Clone(base.Flags)
	changedTime.Flags[0].ObservedAt = now.Add(time.Minute)
	changedTime.Flags[0].Details = []string{"different ignored prose"}
	if second := BuildMarketEventsFingerprint(&changedTime); first != second {
		t.Fatalf("fingerprint changed on timestamp/prose-only mutation: %v != %v", first, second)
	}

	changedStatus := base
	changedStatus.Flags = slices.Clone(base.Flags)
	changedStatus.Flags[0].Status = MarketEventStatusRecent
	if BuildMarketEventsFingerprint(&changedStatus) == first {
		t.Fatal("fingerprint did not change when flag status changed")
	}

	failed := base
	failed.SourceHealth = slices.Clone(base.SourceHealth)
	failed.SourceHealth[0].Status = SourceStatusUnknown
	failed.SourceHealth[0].LastFailure = &SourceFailure{Code: SourceFailureTimeout, Stage: SourceFailureStageFTPControlConnect, FailedAt: now, Retryable: true}
	failedFP := BuildMarketEventsFingerprint(&failed)
	if failedFP == first {
		t.Fatal("fingerprint did not change when typed source failure appeared")
	}
	retried := failed
	retried.SourceHealth = slices.Clone(failed.SourceHealth)
	retriedFailure := *failed.SourceHealth[0].LastFailure
	retriedFailure.FailedAt = now.Add(time.Minute)
	retried.SourceHealth[0].LastFailure = &retriedFailure
	if got := BuildMarketEventsFingerprint(&retried); got != failedFP {
		t.Fatalf("fingerprint changed on failure timestamp only: %v != %v", got, failedFP)
	}
}

func TestMarketEventsFingerprintTracksBorrowFeeCoverageSemantics(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 21, 16, 0, 0, 0, time.UTC)
	fee := 82.5
	base := MarketEventsResult{
		Kind: MarketEventsKind, SchemaVersion: MarketEventsSchemaVersion, AsOf: now,
		Symbols: []string{"XYZ"},
		BorrowFeeCoverage: []MarketEventBorrowFeeCoverage{{
			Symbol: "XYZ", ContractConID: 481516, ContractFingerprint: "sha256:contract-a",
			CoverageScope: BorrowFeeCoveragePortfolioOnly, Status: BorrowFeeCoverageScaleUnknown,
			Reason: "scale_not_commissioned", Source: BorrowFeeSourceTWSHistorical,
			DataType: BorrowFeeDataTypeHistorical, AsOf: now.Add(-24 * time.Hour), ObservedAt: now,
			FeeRate: &fee, Entitlement: BorrowFeeEntitlementObserved,
			ScaleStatus: BorrowFeeScaleUnverified, PolicyEligible: false,
			LastFailure: &SourceFailure{Code: SourceFailureNoData, Stage: SourceFailureStageHistoricalFeeDecode, FailedAt: now, Retryable: true},
		}},
	}
	first := BuildMarketEventsFingerprint(&base)

	churn := base
	churn.AsOf = now.Add(time.Minute)
	churn.BorrowFeeCoverage = slices.Clone(base.BorrowFeeCoverage)
	changedFee := 999.0
	churn.BorrowFeeCoverage[0].FeeRate = &changedFee
	churn.BorrowFeeCoverage[0].ObservedAt = now.Add(time.Minute)
	churn.BorrowFeeCoverage[0].AsOf = now.Add(-23 * time.Hour)
	failure := *base.BorrowFeeCoverage[0].LastFailure
	failure.FailedAt = now.Add(time.Minute)
	failure.Retryable = false
	churn.BorrowFeeCoverage[0].LastFailure = &failure
	if got := BuildMarketEventsFingerprint(&churn); got != first {
		t.Fatalf("coverage fingerprint changed on time/value/retry churn: %v != %v", got, first)
	}

	mutations := []struct {
		name   string
		mutate func(*MarketEventBorrowFeeCoverage)
	}{
		{"contract", func(row *MarketEventBorrowFeeCoverage) { row.ContractConID++ }},
		{"contract fingerprint", func(row *MarketEventBorrowFeeCoverage) { row.ContractFingerprint = "sha256:contract-b" }},
		{"scope", func(row *MarketEventBorrowFeeCoverage) { row.CoverageScope = BorrowFeeCoverageGlobal }},
		{"status", func(row *MarketEventBorrowFeeCoverage) { row.Status = BorrowFeeCoverageNotEntitled }},
		{"reason", func(row *MarketEventBorrowFeeCoverage) { row.Reason = "not_entitled" }},
		{"source", func(row *MarketEventBorrowFeeCoverage) { row.Source = BorrowFeeSourceBulkShortStock }},
		{"data type", func(row *MarketEventBorrowFeeCoverage) { row.DataType = BorrowFeeDataTypeBulkFeeRate }},
		{"entitlement", func(row *MarketEventBorrowFeeCoverage) { row.Entitlement = BorrowFeeEntitlementNotEntitled }},
		{"scale", func(row *MarketEventBorrowFeeCoverage) { row.ScaleStatus = BorrowFeeScalePercentAnnualized }},
		{"policy eligibility", func(row *MarketEventBorrowFeeCoverage) { row.PolicyEligible = true }},
		{"failure code", func(row *MarketEventBorrowFeeCoverage) {
			copy := *row.LastFailure
			copy.Code = SourceFailurePacing
			row.LastFailure = &copy
		}},
		{"failure stage", func(row *MarketEventBorrowFeeCoverage) {
			copy := *row.LastFailure
			copy.Stage = SourceFailureStageHistoricalFeeRequest
			row.LastFailure = &copy
		}},
	}
	for _, test := range mutations {
		t.Run(test.name, func(t *testing.T) {
			changed := base
			changed.BorrowFeeCoverage = slices.Clone(base.BorrowFeeCoverage)
			test.mutate(&changed.BorrowFeeCoverage[0])
			if got := BuildMarketEventsFingerprint(&changed); got == first {
				t.Fatalf("fingerprint did not change for %s", test.name)
			}
		})
	}
}

// eligibleRedMeta is a red row whose evidence passed the confirmation gates
// (depth + persistence + freshness) — the only kind of red that may confirm.
func eligibleRedMeta() RegimeIndicatorMeta {
	return RegimeIndicatorMeta{Band: "red", Eligibility: &RegimeEligibility{Eligible: true}}
}

func greenMeta() RegimeIndicatorMeta { return RegimeIndicatorMeta{Band: "green"} }

// buildRegimeLifecycleFixture supplies the complete typed source contract the
// daemon always attaches before lifecycle evaluation. Individual tests can
// still override a status, freshness class, source-health row, or data-quality
// item to exercise fail-closed behavior.
func buildRegimeLifecycleFixture(r *RegimeSnapshotResult) LifecycleState {
	if r == nil {
		return BuildRegimeLifecycle(nil)
	}
	fresh := func(meta *RegimeIndicatorMeta) {
		if meta.Freshness == nil {
			meta.Freshness = &RegimeFreshness{Class: RegimeFreshnessFresh}
		}
	}
	fill := func(status *string, meta *RegimeIndicatorMeta) {
		if strings.TrimSpace(*status) == "" {
			*status = RegimeStatusOK
		}
		fresh(meta)
	}
	fill(&r.VIXTermStructure.Status, &r.VIXTermStructure.RegimeIndicatorMeta)
	fill(&r.VolOfVol.Status, &r.VolOfVol.RegimeIndicatorMeta)
	fill(&r.HYGSPYDivergence.Status, &r.HYGSPYDivergence.RegimeIndicatorMeta)
	fill(&r.CreditSpreads.Status, &r.CreditSpreads.RegimeIndicatorMeta)
	fill(&r.FundingStress.Status, &r.FundingStress.RegimeIndicatorMeta)
	fill(&r.USDJPY.Status, &r.USDJPY.RegimeIndicatorMeta)
	fill(&r.GammaZero.Status, &r.GammaZero.RegimeIndicatorMeta)
	fill(&r.Breadth.Status, &r.Breadth.RegimeIndicatorMeta)
	if r.GammaZero.Status == RegimeStatusOK && r.GammaZero.Envelope.Result == nil {
		r.GammaZero.Envelope = GammaZeroSPXResult{Status: GammaZeroStatusReady, Result: &GammaZeroComputed{
			Quality: &GammaSignalQuality{Rankability: GammaRankabilityRankable},
		}}
	}
	seen := map[string]bool{}
	for _, health := range r.SourceHealth {
		seen[strings.ToLower(strings.TrimSpace(health.Source))] = true
	}
	for _, name := range RegimeClusterNames {
		if !seen[name] {
			r.SourceHealth = append(r.SourceHealth, SourceHealth{Source: name, Status: SourceStatusOK, RefreshState: SourceRefreshCurrent})
		}
	}
	return BuildRegimeLifecycle(r)
}

func fullGreenLifecycleFixture() RegimeSnapshotResult {
	r := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		VolOfVol:         RegimeVolOfVol{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		CreditSpreads:    RegimeCreditSpreads{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		GammaZero:        RegimeGammaZero{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
	}
	buildRegimeLifecycleFixture(&r)
	return r
}

func TestRegimeLifecycleSourceAgeAtLimitFailsClosed(t *testing.T) {
	t.Parallel()
	r := fullGreenLifecycleFixture()
	for i := range r.SourceHealth {
		if r.SourceHealth[i].Source == "credit" {
			r.SourceHealth[i].MaxAgeSeconds = 60
			r.SourceHealth[i].AgeSeconds = 60
		}
	}
	got := BuildRegimeLifecycle(&r)
	if got.Readiness != "degraded" || !lifecycleGovernorNames(got, "readiness_degraded", "input_currency", "credit") {
		t.Fatalf("source at max age must be detected as a defect: %+v", got)
	}
	for i := range r.SourceHealth {
		if r.SourceHealth[i].Source == "fx" {
			r.SourceHealth[i].MaxAgeSeconds = 60
			r.SourceHealth[i].AgeSeconds = 60
		}
	}
	blanked := BuildRegimeLifecycle(&r)
	if blanked.Stage != LifecycleDataQuality || blanked.Readiness != "blocked" || blanked.Confidence != "low" {
		t.Fatalf("two sources at max age must fail closed: %+v", blanked)
	}
}

func TestRegimeLifecycleDistinguishesEarlyWarningFromConfirmedStress(t *testing.T) {
	t.Parallel()
	spyDrop := -2.0
	base := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: eligibleRedMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{
			RegimeIndicatorMeta: greenMeta(),
			SPYChangePct:        &spyDrop,
		},
		FundingStress: RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:        RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
	}
	early := buildRegimeLifecycleFixture(&base)
	if early.Stage != LifecycleEarlyWarning || early.Severity != "watch" {
		t.Fatalf("early lifecycle = %+v, want early_warning/watch", early)
	}
	if len(early.ConfirmedBy) != 0 {
		t.Fatalf("early confirmed_by = %+v, want none until broad stress is confirmed", early.ConfirmedBy)
	}

	confirmed := base
	confirmed.FundingStress.RegimeIndicatorMeta = eligibleRedMeta()
	got := buildRegimeLifecycleFixture(&confirmed)
	if got.Stage != LifecycleConfirmedStress || got.Severity != "act" {
		t.Fatalf("confirmed lifecycle = %+v, want confirmed_stress/act", got)
	}
	if len(got.ConfirmedBy) == 0 {
		t.Fatalf("confirmed_by empty for confirmed stress: %+v", got)
	}
	if early.Fingerprint == got.Fingerprint {
		t.Fatal("lifecycle fingerprint did not change across semantic stage transition")
	}
}

// TestRegimeLifecycleProvisionalRedsDoNotConfirm pins the 2026-06-12
// incident fix at the policy layer: two raw reds whose evidence failed the
// eligibility gates (marginal depth, day-1 streak, overdue gamma cache) must
// warn, not confirm — no confirmed_stress, no act, both clusters disclosed
// in unconfirmed.
func TestRegimeLifecycleProvisionalRedsDoNotConfirm(t *testing.T) {
	t.Parallel()
	provisionalCredit := RegimeIndicatorMeta{Band: "red", Eligibility: &RegimeEligibility{Reasons: []string{"depth_below_min", "streak_1_of_2"}}}
	provisionalGamma := RegimeIndicatorMeta{Band: "red", Eligibility: &RegimeEligibility{Reasons: []string{"data_overdue"}}}
	spyUp := 0.3
	r := RegimeSnapshotResult{
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: provisionalCredit, SPYChangePct: &spyUp},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		GammaZero: RegimeGammaZero{
			RegimeIndicatorMeta: provisionalGamma,
			Status:              RegimeStatusStale,
			Envelope: GammaZeroSPXResult{Result: &GammaZeroComputed{
				Quality: &GammaSignalQuality{Rankability: GammaRankabilityRankable},
			}},
		},
	}
	got := buildRegimeLifecycleFixture(&r)
	// The incident invariant is that neither red confirms. The credit red is
	// visible on current evidence, so the read warns with degraded readiness
	// instead of blanking five healthy clusters over the dead gamma cache.
	if got.Stage != LifecycleEarlyWarning || got.Severity != "watch" || got.Readiness != "degraded" {
		t.Fatalf("lifecycle = stage %q severity %q readiness %q, want early_warning/watch/degraded", got.Stage, got.Severity, got.Readiness)
	}
	if len(got.ConfirmedBy) != 0 {
		t.Fatalf("confirmed_by = %+v, want empty for provisional reds", got.ConfirmedBy)
	}
	if len(got.Unconfirmed) == 0 {
		t.Fatalf("unconfirmed = %+v, want the provisional reds disclosed", got.Unconfirmed)
	}
	if !lifecycleGovernorNames(got, "readiness_degraded", "input_currency", "gamma") {
		t.Fatalf("governors: want the overdue gamma cache disclosed, got %+v", got.Governors)
	}

	// With the overdue gamma red as the only warning evidence, there is no
	// current witness left and the state is undefined, not a market warning.
	lone := r
	lone.HYGSPYDivergence.RegimeIndicatorMeta = greenMeta()
	loneGot := buildRegimeLifecycleFixture(&lone)
	if loneGot.Stage != LifecycleDataQuality || loneGot.Readiness != "blocked" {
		t.Fatalf("an overdue red as the only warning evidence must blank: %+v", loneGot)
	}
}

// TestRegimeLifecycleDegradesReadinessForDataQuality pins the graded response
// to impaired inputs: one impaired cluster degrades the read and names itself,
// a second one blanks it. Before the input-currency model, the first one
// discarded five healthy clusters.
func TestRegimeLifecycleDegradesReadinessForDataQuality(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Summary:          RegimeSummary{Confidence: "high"},
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "stale", StaleClusters: []string{"breadth"}},
		},
	}
	got := buildRegimeLifecycleFixture(&base)
	if got.Stage != LifecycleQuiet {
		t.Fatalf("stage: want quiet beside one impaired cluster, got %+v", got)
	}
	if got.Readiness != "degraded" {
		t.Fatalf("readiness: want degraded, got %+v", got)
	}
	if got.Confidence == "high" {
		t.Fatalf("confidence: want capped below high, got %+v", got)
	}
	if !lifecycleGovernorNames(got, "readiness_degraded", "input_currency", "breadth") {
		t.Fatalf("governors: want breadth disclosed, got %+v", got.Governors)
	}

	second := RegimeSnapshotResult{
		Summary:          RegimeSummary{Confidence: "high"},
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "stale", StaleClusters: []string{"breadth", "funding"}},
		},
	}
	blanked := buildRegimeLifecycleFixture(&second)
	if blanked.Stage != LifecycleDataQuality || blanked.Readiness != "blocked" || blanked.Confidence != "low" {
		t.Fatalf("two impaired clusters must blank the read: %+v", blanked)
	}
}

// lifecycleGovernorNames reports whether the disclosed governors carry the
// given action/reason and name the cluster.
func lifecycleGovernorNames(state LifecycleState, action, reason, cluster string) bool {
	for _, g := range state.Governors {
		if g.Action != action || g.Reason != reason {
			continue
		}
		if slices.Contains(g.Clusters, cluster) {
			return true
		}
	}
	return false
}

func TestRegimeLifecycleDegradesReadinessForWeakRows(t *testing.T) {
	t.Parallel()
	base := RegimeSnapshotResult{
		Summary:          RegimeSummary{Confidence: "high"},
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		Breadth:          RegimeBreadth{RegimeIndicatorMeta: greenMeta()},
		GammaZero:        RegimeGammaZero{Status: RegimeStatusComputing},
	}
	got := buildRegimeLifecycleFixture(&base)
	if got.Stage != LifecycleQuiet {
		t.Fatalf("stage: want quiet beside one defective cluster, got %+v", got)
	}
	if got.Readiness != "degraded" {
		t.Fatalf("readiness: want degraded for computing gamma, got %+v", got)
	}
	if !lifecycleGovernorNames(got, "readiness_degraded", "input_currency", "gamma") {
		t.Fatalf("governors: want gamma disclosed as the defect, got %+v", got.Governors)
	}
	// A computing row is a defect, not a scheduled state: a second one blanks.
	base.CreditSpreads.Status = RegimeStatusComputing
	base.CreditSpreads.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	blanked := BuildRegimeLifecycle(&base)
	if blanked.Stage != LifecycleDataQuality || blanked.Readiness != "blocked" {
		t.Fatalf("two defective clusters must blank the read: %+v", blanked)
	}
}

func TestRegimeLifecycleRequiredInputsFailClosed(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*RegimeSnapshotResult)
	}{
		{
			name: "status_ok_but_freshness_overdue",
			mutate: func(r *RegimeSnapshotResult) {
				r.Breadth.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
			},
		},
		{
			name: "stale_unranked_is_not_an_exemption",
			mutate: func(r *RegimeSnapshotResult) {
				r.GammaZero.Band = ""
				r.GammaZero.Status = RegimeStatusStale
				r.GammaZero.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
				for i := range r.SourceHealth {
					if r.SourceHealth[i].Source == "gamma" {
						r.SourceHealth[i].Status = SourceStatusStale
					}
				}
				r.DataQuality = []DataQualityHealth{{Status: RegimeStatusStale, StaleClusters: []string{"gamma"}}}
			},
		},
		{
			name:   "blank_required_status",
			mutate: func(r *RegimeSnapshotResult) { r.FundingStress.Status = "" },
		},
		{
			name: "missing_required_source_health",
			mutate: func(r *RegimeSnapshotResult) {
				for i := range r.SourceHealth {
					if r.SourceHealth[i].Source == "fx" {
						r.SourceHealth = append(r.SourceHealth[:i], r.SourceHealth[i+1:]...)
						break
					}
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			r := fullGreenLifecycleFixture()
			test.mutate(&r)
			got := BuildRegimeLifecycle(&r)
			// Detection first: none of these shapes may read healthy.
			if got.Readiness != "degraded" || len(got.Governors) == 0 {
				t.Fatalf("state=%+v, want the malformed input detected and disclosed", got)
			}
			// Escalation: a second malformed cluster blanks the read.
			second := r
			second.VolOfVol.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
			blanked := BuildRegimeLifecycle(&second)
			if blanked.Stage != LifecycleDataQuality || blanked.Readiness != "blocked" || blanked.Timing != LifecycleTimingDataQuality {
				t.Fatalf("state=%+v, want explicit blocked data_quality", blanked)
			}
			ApplyRegimeClusterTallies(&second.Composite, BuildRegimeClusterBands(&second))
			if label := RegimeHeadline(second.Composite, blanked.Stage); label != "Market state undefined — data incomplete" {
				t.Fatalf("headline=%q, want explicit undefined state", label)
			}
		})
	}
}

func TestRegimeLifecycleTypedNotDueRemainsExpectedContext(t *testing.T) {
	t.Parallel()
	r := fullGreenLifecycleFixture()
	r.VIXTermStructure.Status = RegimeStatusStale
	r.VIXTermStructure.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
	r.GammaZero.Status = RegimeStatusStale
	r.GammaZero.Freshness = &RegimeFreshness{Class: RegimeFreshnessNotDue}
	r.GammaZero.Envelope.Result.Quality = &GammaSignalQuality{Rankability: GammaRankabilityContextOnly}
	for i := range r.SourceHealth {
		switch r.SourceHealth[i].Source {
		case "vol", "gamma":
			r.SourceHealth[i].Status = SourceStatusStale
			r.SourceHealth[i].RefreshState = SourceRefreshNotDue
		}
	}
	r.DataQuality = []DataQualityHealth{{Status: RegimeStatusStale, StaleClusters: []string{"vol", "gamma"}}}
	got := BuildRegimeLifecycle(&r)
	if got.Stage != LifecycleQuiet || got.Readiness != "ready" {
		t.Fatalf("typed not_due context=%+v, want quiet/ready", got)
	}
}

func TestRegimeLifecycleStaleTapeCannotPreserveStress(t *testing.T) {
	t.Parallel()
	r := fullGreenLifecycleFixture()
	r.TapeSessionState = TapeSessionTradingDate
	r.HYGSPYDivergence.SPYChangePct = new(-7.2)
	r.HYGSPYDivergence.Status = RegimeStatusStale
	r.HYGSPYDivergence.Freshness = &RegimeFreshness{Class: RegimeFreshnessOverdue}
	for i := range r.SourceHealth {
		if r.SourceHealth[i].Source == "credit" {
			r.SourceHealth[i].Status = SourceStatusStale
		}
	}
	r.DataQuality = []DataQualityHealth{{Status: RegimeStatusStale, StaleClusters: []string{"credit"}}}
	got := BuildRegimeLifecycle(&r)
	if got.Stage != LifecycleDataQuality || got.ConfirmedBy != nil {
		t.Fatalf("stale tape state=%+v, want data_quality without confirmation", got)
	}
}

func TestRegimePostureClassifiesPolicyTone(t *testing.T) {
	t.Parallel()

	rankableGamma := func(meta RegimeIndicatorMeta) RegimeGammaZero {
		return RegimeGammaZero{
			RegimeIndicatorMeta: meta,
			Status:              RegimeStatusOK,
			Envelope: GammaZeroSPXResult{Result: &GammaZeroComputed{
				Quality: &GammaSignalQuality{Rankability: GammaRankabilityRankable},
			}},
		}
	}
	yellowMeta := RegimeIndicatorMeta{Band: "yellow"}
	pendingRedMeta := func() RegimeIndicatorMeta {
		return RegimeIndicatorMeta{
			Band:        "red",
			Eligibility: &RegimeEligibility{Eligible: true},
			Thresholds:  &RegimeThresholds{Label: "test_v1", Heuristic: true, PendingBacktest: true},
		}
	}

	tests := []struct {
		name         string
		build        func() RegimeSnapshotResult
		wantLabel    string
		wantTone     string
		wantStage    string
		wantSeverity string
	}{
		{
			name: "one eligible red plus one yellow is early warning watch",
			build: func() RegimeSnapshotResult {
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
					HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
					FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligibleRedMeta()},
					USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
					Breadth:          RegimeBreadth{RegimeIndicatorMeta: yellowMeta},
				}
			},
			wantLabel:    "Stress signal present",
			wantTone:     RegimeToneWatch,
			wantStage:    LifecycleEarlyWarning,
			wantSeverity: "watch",
		},
		{
			name: "two eligible red clusters are confirmed stress",
			build: func() RegimeSnapshotResult {
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
					HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
					FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligibleRedMeta()},
					USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
					Breadth:          RegimeBreadth{RegimeIndicatorMeta: eligibleRedMeta()},
				}
			},
			wantLabel:    "Confirmed stress regime",
			wantTone:     RegimeToneStress,
			wantStage:    LifecycleConfirmedStress,
			wantSeverity: "act",
		},
		{
			name: "confirmed stress capped to watch renders amber watch",
			build: func() RegimeSnapshotResult {
				calmTape := 0.3
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
					HYGSPYDivergence: RegimeHYGSPYDivergence{
						RegimeIndicatorMeta: greenMeta(),
						SPYChangePct:        &calmTape,
					},
					FundingStress: RegimeFundingStress{RegimeIndicatorMeta: pendingRedMeta()},
					USDJPY:        RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
					Breadth:       RegimeBreadth{RegimeIndicatorMeta: pendingRedMeta()},
				}
			},
			wantLabel:    "Confirmed stress regime",
			wantTone:     RegimeToneWatch,
			wantStage:    LifecycleConfirmedStress,
			wantSeverity: "watch",
		},
		{
			name: "all ranked clusters eligible red is risk off",
			build: func() RegimeSnapshotResult {
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: eligibleRedMeta()},
					HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: eligibleRedMeta()},
					CreditSpreads:    RegimeCreditSpreads{RegimeIndicatorMeta: eligibleRedMeta()},
					FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligibleRedMeta()},
					USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: eligibleRedMeta()},
					GammaZero:        rankableGamma(eligibleRedMeta()),
					Breadth:          RegimeBreadth{RegimeIndicatorMeta: eligibleRedMeta()},
				}
			},
			wantLabel:    "Full risk-off conditions",
			wantTone:     RegimeToneRiskOff,
			wantStage:    LifecyclePanic,
			wantSeverity: "urgent",
		},
		{
			name: "too few ranked clusters is data quality",
			build: func() RegimeSnapshotResult {
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
					FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: eligibleRedMeta()},
				}
			},
			wantLabel:    "Market state undefined — data incomplete",
			wantTone:     RegimeToneDataQuality,
			wantStage:    LifecycleDataQuality,
			wantSeverity: "watch",
		},
		{
			name: "one yellow cluster is amber normal watch",
			build: func() RegimeSnapshotResult {
				return RegimeSnapshotResult{
					VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
					HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
					FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
					USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: yellowMeta},
				}
			},
			wantLabel:    "Normal regime",
			wantTone:     RegimeToneWatch,
			wantStage:    LifecycleQuiet,
			wantSeverity: "watch",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			regime := tt.build()
			ApplyRegimeClusterTallies(&regime.Composite, BuildRegimeClusterBands(&regime))
			regime.Lifecycle = buildRegimeLifecycleFixture(&regime)
			regime.Composite.Verdict = RegimeHeadline(regime.Composite, regime.Lifecycle.Stage)
			got := BuildRegimePosture(&regime)
			if got.Label != tt.wantLabel || got.Tone != tt.wantTone || got.Stage != tt.wantStage || got.Severity != tt.wantSeverity {
				t.Fatalf("posture = %+v, want label=%q tone=%q stage=%q severity=%q", got, tt.wantLabel, tt.wantTone, tt.wantStage, tt.wantSeverity)
			}
			if regime.Composite.Verdict != got.Label {
				t.Fatalf("verdict %q != posture label %q — headline drift", regime.Composite.Verdict, got.Label)
			}
		})
	}
}

func TestRegimePostureMarksDegradedNormalAsDataQuality(t *testing.T) {
	t.Parallel()
	regime := RegimeSnapshotResult{
		Summary:          RegimeSummary{Confidence: "high"},
		VIXTermStructure: RegimeVIXTerm{RegimeIndicatorMeta: greenMeta()},
		HYGSPYDivergence: RegimeHYGSPYDivergence{RegimeIndicatorMeta: greenMeta()},
		FundingStress:    RegimeFundingStress{RegimeIndicatorMeta: greenMeta()},
		USDJPY:           RegimeUSDJPY{RegimeIndicatorMeta: greenMeta()},
		DataQuality: []DataQualityHealth{
			{Surface: "regime", Status: "partial", PartialClusters: []string{"credit", "FX"}},
		},
	}
	ApplyRegimeClusterTallies(&regime.Composite, BuildRegimeClusterBands(&regime))
	regime.Lifecycle = buildRegimeLifecycleFixture(&regime)
	got := BuildRegimePosture(&regime)
	if got.Label != "Market state undefined — data incomplete" {
		t.Fatalf("label: want undefined data state, got %+v", got)
	}
	if got.Tone != RegimeToneDataQuality || got.Severity != "watch" || got.Readiness != "blocked" {
		t.Fatalf("posture should block on incomplete data, got %+v", got)
	}
}

func TestRegimeSourceHealthUsesOldestClusterAsOf(t *testing.T) {
	t.Parallel()
	old := time.Date(2026, time.May, 29, 21, 0, 0, 0, time.UTC)
	fresh := time.Date(2026, time.June, 1, 21, 0, 0, 0, time.UTC)
	res := &RegimeSnapshotResult{
		AsOf:      fresh,
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: fresh},
			},
			Status: RegimeStatusOK,
		},
		VolOfVol: RegimeVolOfVol{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: old},
			},
			Status: RegimeStatusOK,
		},
	}
	got := BuildRegimeSourceHealth(res, fresh)
	var vol *SourceHealth
	for i := range got {
		if got[i].Source == "vol" {
			vol = &got[i]
			break
		}
	}
	if vol == nil {
		t.Fatalf("missing vol source health: %+v", got)
	}
	if !vol.AsOf.Equal(old) {
		t.Fatalf("vol as_of = %s, want oldest member timestamp %s", vol.AsOf, old)
	}
	if want := int64(72 * 60 * 60); vol.AgeSeconds != want {
		t.Fatalf("vol age_seconds = %d, want %d", vol.AgeSeconds, want)
	}
}

func TestRegimeSourceHealthUsesPartialDataQuality(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 21, 0, 0, 0, time.UTC)
	res := &RegimeSnapshotResult{
		AsOf:      now,
		Composite: RegimeComposite{ClusterGreenCount: 6, ClusterRankedCount: 6},
		GammaZero: RegimeGammaZero{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "green",
				AsOf: &RegimeAsOfSummary{Time: now},
			},
			Status: RegimeStatusOK,
		},
		DataQuality: []DataQualityHealth{{
			Surface:         "gamma",
			Status:          "partial",
			PartialClusters: []string{"gamma"},
			AsOf:            now,
		}},
	}
	got := BuildRegimeSourceHealth(res, now)
	var gamma *SourceHealth
	for i := range got {
		if got[i].Source == "gamma" {
			gamma = &got[i]
			break
		}
	}
	if gamma == nil {
		t.Fatalf("missing gamma source health: %+v", got)
	}
	if gamma.Status != "partial" || gamma.Confidence != "medium" {
		t.Fatalf("gamma source health = %+v, want partial/medium", gamma)
	}
}

func TestRegimeSourceHealthTreatsMissingRequiredFieldsAsPartial(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 4, 9, 15, 0, 0, time.UTC)
	res := &RegimeSnapshotResult{
		AsOf: now,
		USDJPY: RegimeUSDJPY{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band: "unranked",
				AsOf: &RegimeAsOfSummary{Time: now},
			},
			Status:        RegimeStatusOK,
			FieldsMissing: []string{"close_7d_ago", "weekly_change_pct"},
		},
	}

	got := BuildRegimeSourceHealth(res, now)
	var fx *SourceHealth
	for i := range got {
		if got[i].Source == "fx" {
			fx = &got[i]
			break
		}
	}
	if fx == nil {
		t.Fatalf("missing fx source health: %+v", got)
	}
	if fx.Status != "partial" || fx.Confidence != "medium" {
		t.Fatalf("fx source health = %+v, want partial/medium", fx)
	}
}
