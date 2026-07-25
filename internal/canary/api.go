package canary

import (
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
	"github.com/osauer/ibkr/v2/internal/rpc"
)

// Canary action, input-health, and portfolio-fit tokens shared by adapters.
const (
	ActionWatch         = canaryActionWatch
	ActionDefend        = canaryActionDefend
	ActionRebalance     = canaryActionRebalance
	ActionDeploy        = canaryActionDeploy
	ActionConfirmInputs = canaryActionConfirmInputs

	InputOK       = canaryInputOK
	InputDegraded = canaryInputDegraded

	PortfolioFitLow     = canaryPortfolioFitLow
	PortfolioFitUnknown = canaryPortfolioFitUnknown
)

// PolicyName returns the active Canary risk-policy profile name.
func PolicyName() string {
	return canaryPolicy.Name
}

// SummarizeMarket converts a typed regime snapshot into the market summary used
// by Canary evaluation.
func SummarizeMarket(r rpc.RegimeSnapshotResult, now time.Time) rpc.StressMarketSummary {
	return summarizeCanaryMarket(r, now)
}

// SeverityAtLeast reports whether got ranks at or above want in Canary's
// severity ordering.
func SeverityAtLeast(got, want risk.SignalSeverity) bool {
	return severityRankAtLeast(got, want)
}

// GammaDegraded reports whether the gamma input is unsuitable for an
// undegraded Canary assessment.
func GammaDegraded(g rpc.RegimeGammaZero) bool {
	return canaryGammaDegraded(g)
}

// MarketEvidence formats the redacted market evidence used in Canary output.
func MarketEvidence(m rpc.StressMarketSummary) string {
	return canaryMarketEvidence(m)
}

// PortfolioEvidence formats the redacted portfolio evidence used in Canary
// output.
func PortfolioEvidence(p rpc.StressPortfolioSummary) string {
	return canaryPortfolioEvidence(p)
}

// AmbiguityEvidence formats evidence explaining incomplete or ambiguous market
// confirmation.
func AmbiguityEvidence(m rpc.StressMarketSummary) string {
	return canaryAmbiguityEvidence(m)
}

// FormatProtectionCoverageEvidence formats a protection-coverage summary for a
// Canary evidence row.
func FormatProtectionCoverageEvidence(c *rpc.ProtectionCoverageSummary) string {
	return formatProtectionCoverageEvidence(c)
}

// AppendUniqueString appends a non-duplicate string using Canary's canonical
// equality rules.
func AppendUniqueString(values []string, value string) []string {
	return appendUniqueString(values, value)
}
