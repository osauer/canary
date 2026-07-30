package rpc

import "github.com/osauer/canary/v2/internal/risk"

// Alert candidate wire types are aliases of the pure risk contract. Keeping a
// single concrete definition makes conversion loss impossible and ensures RPC
// JSON validation cannot drift into a second policy evaluator.
type (
	// AlertSource identifies an allowlisted alert producer.
	AlertSource = risk.AlertSource
	// AlertKind classifies the condition represented by a candidate.
	AlertKind = risk.AlertKind
	// AlertEpisodeState describes whether an episode opened, escalated, or recovered.
	AlertEpisodeState = risk.AlertEpisodeState
	// AlertSeverity is the stable urgency classification of a candidate.
	AlertSeverity = risk.AlertSeverity
	// AlertPresentationCode is the closed redacted copy key for a candidate.
	AlertPresentationCode = risk.AlertPresentationCode
	// AlertEvidenceHealth reports the quality of evidence behind a candidate.
	AlertEvidenceHealth = risk.AlertEvidenceHealth
	// AlertDestination identifies an allowed presentation surface.
	AlertDestination = risk.AlertDestination
	// AlertCoverageState reports whether the producer evaluated its full universe.
	AlertCoverageState = risk.AlertCoverageState
	// AlertCoverageFreshness reports whether coverage evidence is current.
	AlertCoverageFreshness = risk.AlertCoverageFreshness
	// AlertSourceCoverage reports one expected producer's current evidence state.
	AlertSourceCoverage = risk.AlertSourceCoverage
	// AlertSnapshotState distinguishes conclusively clear, active, and unknown snapshots.
	AlertSnapshotState = risk.AlertSnapshotState
	// AlertCandidate is the shared pure-risk candidate contract.
	AlertCandidate = risk.AlertCandidate
	// AlertCoverage is the shared pure-risk coverage contract.
	AlertCoverage = risk.AlertCoverage
	// AlertCandidateSnapshot is the shared validated source-neutral snapshot.
	AlertCandidateSnapshot = risk.AlertCandidateSnapshot
)

// Alert-candidate constants re-export the pure risk vocabulary unchanged so
// RPC adapters and risk evaluation share one set of wire values.
const (
	// AlertCandidateSnapshotVersion identifies a stable wire schema.
	AlertCandidateSnapshotVersion = risk.AlertCandidateSnapshotVersion

	AlertSourceStress         = risk.AlertSourceStress
	AlertSourceRegime         = risk.AlertSourceRegime
	AlertSourceRulebook       = risk.AlertSourceRulebook
	AlertSourceRiskPolicy     = risk.AlertSourceRiskPolicy
	AlertSourceProtection     = risk.AlertSourceProtection
	AlertSourceOrderIntegrity = risk.AlertSourceOrderIntegrity
	AlertSourceReconciliation = risk.AlertSourceReconciliation
	AlertSourceGovernance     = risk.AlertSourceGovernance
	AlertSourceDataHealth     = risk.AlertSourceDataHealth
	AlertSourceDelivery       = risk.AlertSourceDelivery

	AlertKindMarketState             = risk.AlertKindMarketState
	AlertKindPortfolioRisk           = risk.AlertKindPortfolioRisk
	AlertKindMarginSafety            = risk.AlertKindMarginSafety
	AlertKindDrawdown                = risk.AlertKindDrawdown
	AlertKindProtectionGap           = risk.AlertKindProtectionGap
	AlertKindOrderIntegrity          = risk.AlertKindOrderIntegrity
	AlertKindReconciliationException = risk.AlertKindReconciliationException
	AlertKindGovernance              = risk.AlertKindGovernance
	AlertKindPolicyDrift             = risk.AlertKindPolicyDrift
	AlertKindDataHealth              = risk.AlertKindDataHealth
	AlertKindDeliveryHealth          = risk.AlertKindDeliveryHealth

	AlertEpisodeOpen      = risk.AlertEpisodeOpen
	AlertEpisodeEscalated = risk.AlertEpisodeEscalated
	AlertEpisodeRecovered = risk.AlertEpisodeRecovered

	AlertSeverityObserve = risk.AlertSeverityObserve
	AlertSeverityWatch   = risk.AlertSeverityWatch
	AlertSeverityAct     = risk.AlertSeverityAct
	AlertSeverityUrgent  = risk.AlertSeverityUrgent

	AlertPresentationPortfolioStress                  = risk.AlertPresentationPortfolioStress
	AlertPresentationMarginCushion                    = risk.AlertPresentationMarginCushion
	AlertPresentationRegimeMarketStress               = risk.AlertPresentationRegimeMarketStress
	AlertPresentationRulebookSingleNameExposure       = risk.AlertPresentationRulebookSingleNameExposure
	AlertPresentationRulebookOptionLinePremium        = risk.AlertPresentationRulebookOptionLinePremium
	AlertPresentationRulebookCashSellOnly             = risk.AlertPresentationRulebookCashSellOnly
	AlertPresentationRulebookExtrinsicBudget          = risk.AlertPresentationRulebookExtrinsicBudget
	AlertPresentationRulebookExpiryRunway             = risk.AlertPresentationRulebookExpiryRunway
	AlertPresentationRulebookCatalystCoverage         = risk.AlertPresentationRulebookCatalystCoverage
	AlertPresentationRulebookOverwriteEarnings        = risk.AlertPresentationRulebookOverwriteEarnings
	AlertPresentationRulebookEarningsSizeFreeze       = risk.AlertPresentationRulebookEarningsSizeFreeze
	AlertPresentationRulebookRedOnGreen               = risk.AlertPresentationRulebookRedOnGreen
	AlertPresentationRulebookWinnerTrim               = risk.AlertPresentationRulebookWinnerTrim
	AlertPresentationRulebookGreenDayAction           = risk.AlertPresentationRulebookGreenDayAction
	AlertPresentationRulebookHedgeIntegrity           = risk.AlertPresentationRulebookHedgeIntegrity
	AlertPresentationRulebookExitDiscipline           = risk.AlertPresentationRulebookExitDiscipline
	AlertPresentationRulebookFXExposure               = risk.AlertPresentationRulebookFXExposure
	AlertPresentationProtectionOrphanedOrder          = risk.AlertPresentationProtectionOrphanedOrder
	AlertPresentationProtectionReconciliationRequired = risk.AlertPresentationProtectionReconciliationRequired
	AlertPresentationOrderIntegrityMismatch           = risk.AlertPresentationOrderIntegrityMismatch
	AlertPresentationDataHealthGateway                = risk.AlertPresentationDataHealthGateway
	AlertPresentationDataHealthStorage                = risk.AlertPresentationDataHealthStorage
	AlertPresentationDataHealthProposals              = risk.AlertPresentationDataHealthProposals
	AlertPresentationDataHealthOpportunities          = risk.AlertPresentationDataHealthOpportunities
	AlertPresentationDataHealthDataFarms              = risk.AlertPresentationDataHealthDataFarms
	AlertPresentationDataHealthRegime                 = risk.AlertPresentationDataHealthRegime
	AlertPresentationDataHealthGamma                  = risk.AlertPresentationDataHealthGamma
	AlertPresentationDataHealthQuality                = risk.AlertPresentationDataHealthQuality
	AlertPresentationRiskPolicyLimitWouldBlock        = risk.AlertPresentationRiskPolicyLimitWouldBlock
	AlertPresentationRiskPolicyDrawdownLatched        = risk.AlertPresentationRiskPolicyDrawdownLatched
	AlertPresentationRiskPolicyDrift                  = risk.AlertPresentationRiskPolicyDrift
	AlertPresentationReconciliationDue                = risk.AlertPresentationReconciliationDue
	AlertPresentationReconciliationException          = risk.AlertPresentationReconciliationException
	AlertPresentationReconciliationConfirmedFlow      = risk.AlertPresentationReconciliationConfirmedFlow
	AlertPresentationGovernanceMonthlyPulse           = risk.AlertPresentationGovernanceMonthlyPulse
	AlertPresentationDeliveryHealth                   = risk.AlertPresentationDeliveryHealth
	AlertPresentationRulebookLegacyCondition          = risk.AlertPresentationRulebookLegacyCondition
	AlertPresentationRiskPolicyLegacyCondition        = risk.AlertPresentationRiskPolicyLegacyCondition
	AlertPresentationReconciliationLegacyCondition    = risk.AlertPresentationReconciliationLegacyCondition
	AlertPresentationGovernanceLegacyCondition        = risk.AlertPresentationGovernanceLegacyCondition

	AlertEvidenceCurrent     = risk.AlertEvidenceCurrent
	AlertEvidencePartial     = risk.AlertEvidencePartial
	AlertEvidenceStale       = risk.AlertEvidenceStale
	AlertEvidenceUnavailable = risk.AlertEvidenceUnavailable
	AlertEvidenceError       = risk.AlertEvidenceError

	AlertDestinationMonitor = risk.AlertDestinationMonitor
	AlertDestinationAlerts  = risk.AlertDestinationAlerts
	AlertDestinationBrief   = risk.AlertDestinationBrief

	AlertCoverageComplete    = risk.AlertCoverageComplete
	AlertCoveragePartial     = risk.AlertCoveragePartial
	AlertCoverageUnavailable = risk.AlertCoverageUnavailable

	AlertCoverageCurrent = risk.AlertCoverageCurrent
	AlertCoverageStale   = risk.AlertCoverageStale
	AlertCoverageUnknown = risk.AlertCoverageUnknown

	AlertSnapshotClear   = risk.AlertSnapshotClear
	AlertSnapshotActive  = risk.AlertSnapshotActive
	AlertSnapshotUnknown = risk.AlertSnapshotUnknown
)

// BuildAlertEpisodeKey delegates opaque identity construction to the pure
// contract; RPC adapters never reinterpret its semantic inputs.
func BuildAlertEpisodeKey(source AlertSource, kind AlertKind, identityParts ...string) (string, error) {
	return risk.BuildAlertEpisodeKey(source, kind, identityParts...)
}

// BuildAlertOccurrenceKey delegates daemon-authored opening, reopen, and
// qualifying-escalation identity to the pure contract. Apps consume the opaque
// result; they do not mint it or decide when it rotates.
func BuildAlertOccurrenceKey(episodeKey string, identityParts ...string) (string, error) {
	return risk.BuildAlertOccurrenceKey(episodeKey, identityParts...)
}

// BuildAlertAuthorityScope returns the opaque account/mode authority carried
// by private candidate snapshots. Raw account and mode values do not cross the
// RPC boundary.
func BuildAlertAuthorityScope(account, mode string) (string, error) {
	return risk.BuildAlertAuthorityScope(account, mode)
}

// ValidateAlertAuthorityScope rejects malformed or noncanonical scope values.
func ValidateAlertAuthorityScope(value string) error {
	return risk.ValidateAlertAuthorityScope(value)
}

// ValidateAlertCandidate validates a candidate against the shared risk contract.
func ValidateAlertCandidate(candidate AlertCandidate) error {
	return candidate.Validate()
}

// ValidateAlertCandidateSnapshot validates coverage, candidates, and snapshot
// coherence against the shared risk contract.
func ValidateAlertCandidateSnapshot(snapshot AlertCandidateSnapshot) error {
	return snapshot.Validate()
}
