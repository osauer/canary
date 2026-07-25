package stress

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Stress action, input-health, and portfolio-fit tokens shared by adapters.
const (
	ActionWatch         = stressActionWatch
	ActionDefend        = stressActionDefend
	ActionRebalance     = stressActionRebalance
	ActionDeploy        = stressActionDeploy
	ActionConfirmInputs = stressActionConfirmInputs

	InputOK       = stressInputOK
	InputDegraded = stressInputDegraded

	PortfolioFitLow     = stressPortfolioFitLow
	PortfolioFitUnknown = stressPortfolioFitUnknown
)

// PolicyName returns the active Stress risk-policy profile name.
func PolicyName() string {
	return stressPolicy.Name
}

// SummarizeMarket converts a typed regime snapshot into the market summary used
// by Stress evaluation.
func SummarizeMarket(r rpc.RegimeSnapshotResult, now time.Time) rpc.StressMarketSummary {
	return summarizeStressMarket(r, now)
}

// SeverityAtLeast reports whether got ranks at or above want in Stress's
// severity ordering.
func SeverityAtLeast(got, want risk.SignalSeverity) bool {
	return severityRankAtLeast(got, want)
}

// GammaDegraded reports whether the gamma input is unsuitable for an
// undegraded Stress assessment.
func GammaDegraded(g rpc.RegimeGammaZero) bool {
	return stressGammaDegraded(g)
}

// MarketEvidence formats the redacted market evidence used in Stress output.
func MarketEvidence(m rpc.StressMarketSummary) string {
	return stressMarketEvidence(m)
}

// PortfolioEvidence formats the redacted portfolio evidence used in Stress
// output.
func PortfolioEvidence(p rpc.StressPortfolioSummary) string {
	return stressPortfolioEvidence(p)
}

// AmbiguityEvidence formats evidence explaining incomplete or ambiguous market
// confirmation.
func AmbiguityEvidence(m rpc.StressMarketSummary) string {
	return stressAmbiguityEvidence(m)
}

// FormatProtectionCoverageEvidence formats a protection-coverage summary for a
// Stress evidence row.
func FormatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	return formatProtectionCoverageEvidence(c)
}

// AppendUniqueString appends a non-duplicate string using Stress's canonical
// equality rules.
func AppendUniqueString(values []string, value string) []string {
	return appendUniqueString(values, value)
}
