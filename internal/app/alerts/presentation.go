// Package alerts turns daemon-authored alert state into redacted app inbox
// records and Web Push work. It owns app-side observation, durable-before-send
// dispatch ordering, and presentation; it does not evaluate risk policy or own
// daemon delivery authority.
package alerts

import (
	"github.com/osauer/canary/v2/internal/rpc"
)

// Presentation is fixed app-authored copy selected only by the daemon's
// closed presentation code. It contains no producer free text or private
// account, symbol, order, or evidence data.
type Presentation struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var presentations = map[rpc.AlertPresentationCode]Presentation{
	rpc.AlertPresentationPortfolioStress:                  {Title: "Portfolio stress", Body: "The current portfolio stress classification is watch or higher. Tap to see the measured drivers."},
	rpc.AlertPresentationMarginCushion:                    {Title: "Margin cushion", Body: "The account margin cushion is below its safety threshold."},
	rpc.AlertPresentationRegimeMarketStress:               {Title: "Market stress", Body: "Current broad-market evidence is at a stress threshold. Tap to see the contributing bands and evidence quality."},
	rpc.AlertPresentationRulebookSingleNameExposure:       {Title: "Single-name exposure", Body: "One underlying is approaching or above its share-of-NLV concentration threshold. Tap to see the measured value and cap."},
	rpc.AlertPresentationRulebookOptionLinePremium:        {Title: "Option-line exposure", Body: "One non-hedge option line is above its advisory share-of-NLV threshold. Tap to see the measured value and cap."},
	rpc.AlertPresentationRulebookCashSellOnly:             {Title: "Cash safeguard", Body: "The cash sell-only safeguard is active."},
	rpc.AlertPresentationRulebookExtrinsicBudget:          {Title: "Extrinsic-value budget", Body: "The extrinsic-value budget needs attention."},
	rpc.AlertPresentationRulebookExpiryRunway:             {Title: "Expiry runway", Body: "An option expiry-runway rule needs attention."},
	rpc.AlertPresentationRulebookCatalystCoverage:         {Title: "Catalyst evidence incomplete", Body: "Canary cannot confirm every required catalyst check. Tap to see which evidence is missing or unsupported."},
	rpc.AlertPresentationRulebookOverwriteEarnings:        {Title: "Earnings overwrite", Body: "An earnings overwrite rule needs attention."},
	rpc.AlertPresentationRulebookEarningsSizeFreeze:       {Title: "Earnings size freeze", Body: "The earnings size-freeze rule is active."},
	rpc.AlertPresentationRulebookRedOnGreen:               {Title: "Red-on-green rule", Body: "The red-on-green discipline rule needs attention."},
	rpc.AlertPresentationRulebookWinnerTrim:               {Title: "Winner trim", Body: "A winner-trim rule needs attention."},
	rpc.AlertPresentationRulebookGreenDayAction:           {Title: "Green-day action", Body: "A green-day action rule needs attention."},
	rpc.AlertPresentationRulebookHedgeIntegrity:           {Title: "Index-put sizing", Body: "Canary cannot confirm that the index-put position is proportionate protection. Tap to see whether its size reads as a hedge or a directional short."},
	rpc.AlertPresentationRulebookExitDiscipline:           {Title: "Exit discipline", Body: "An exit-discipline rule needs attention."},
	rpc.AlertPresentationRulebookFXExposure:               {Title: "Currency exposure", Body: "Non-base-currency exposure is approaching or above its share-of-NLV threshold. Tap to see the measured value and cap."},
	rpc.AlertPresentationProtectionOrphanedOrder:          {Title: "Orphaned protection order", Body: "A protection order no longer matches a held position."},
	rpc.AlertPresentationProtectionReconciliationRequired: {Title: "Protection check required", Body: "Protective orders require reconciliation."},
	rpc.AlertPresentationOrderIntegrityMismatch:           {Title: "Order mismatch", Body: "Open orders do not match the expected protection state."},
	rpc.AlertPresentationDataHealthGateway:                {Title: "Gateway data unavailable", Body: "Gateway evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthStorage:                {Title: "Storage data unavailable", Body: "Required stored evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthProposals:              {Title: "Proposal data unavailable", Body: "Proposal evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthOpportunities:          {Title: "Opportunity data unavailable", Body: "Opportunity evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthDataFarms:              {Title: "Market-data farm issue", Body: "TWS explicitly reported a required farm as broken or disconnected; a missing informational connection notice alone does not light this alert."},
	rpc.AlertPresentationDataHealthRegime:                 {Title: "Regime data unavailable", Body: "Regime evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthGamma:                  {Title: "Gamma data unavailable", Body: "Gamma evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthQuality:                {Title: "Data quality issue", Body: "Required evidence is incomplete, stale, or unavailable."},
	rpc.AlertPresentationRiskPolicyLimitWouldBlock:        {Title: "Risk limit would block", Body: "A current position would be blocked by the active risk policy."},
	rpc.AlertPresentationRiskPolicyDrawdownLatched:        {Title: "Drawdown latch open", Body: "A prior drawdown breach remains latched under the active policy. This is retained breach state, not a claim that current drawdown is still above the threshold or that runtime trading freeze is on."},
	rpc.AlertPresentationRiskPolicyDrift:                  {Title: "Risk policy drift", Body: "The active risk policy differs from its required state."},
	rpc.AlertPresentationReconciliationDue:                {Title: "Reconciliation due", Body: "A broker reconciliation is due."},
	rpc.AlertPresentationReconciliationException:          {Title: "Reconciliation exception", Body: "A broker reconciliation has an unresolved exception."},
	rpc.AlertPresentationReconciliationConfirmedFlow:      {Title: "Broker flow confirmed", Body: "A broker-confirmed cash or position flow needs review."},
	rpc.AlertPresentationGovernanceMonthlyPulse:           {Title: "Monthly desk review", Body: "The monthly desk review has an exception."},
	rpc.AlertPresentationDeliveryHealth:                   {Title: "Alert delivery issue", Body: "Alert delivery is degraded or unavailable."},
	rpc.AlertPresentationRulebookLegacyCondition:          {Title: "Legacy trading-rule alert", Body: "This compatibility alert has been superseded by the current named Rulebook rows."},
	rpc.AlertPresentationRiskPolicyLegacyCondition:        {Title: "Risk policy", Body: "A risk-policy condition needs attention."},
	rpc.AlertPresentationReconciliationLegacyCondition:    {Title: "Reconciliation", Body: "A reconciliation condition needs attention."},
	rpc.AlertPresentationGovernanceLegacyCondition:        {Title: "Desk process", Body: "A desk-process condition needs attention."},
}

// PresentationFor returns fixed copy for a closed code and lifecycle state.
// Recovery copy is retained for inbox history; recovered occurrences are never
// transport due.
func PresentationFor(code rpc.AlertPresentationCode, state rpc.AlertEpisodeState) (Presentation, bool) {
	presentation, ok := presentations[code]
	if !ok {
		return Presentation{}, false
	}
	switch state {
	case rpc.AlertEpisodeEscalated:
		presentation.Body = "Escalated: " + presentation.Body
	case rpc.AlertEpisodeRecovered:
		presentation.Body = "Resolved: " + presentation.Body
	case rpc.AlertEpisodeOpen:
	default:
		return Presentation{}, false
	}
	return presentation, true
}
