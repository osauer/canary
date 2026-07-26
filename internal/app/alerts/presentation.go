package alerts

import "github.com/osauer/canary/v2/internal/rpc"

// Presentation is fixed app-authored copy selected only by the daemon's
// closed presentation code. It contains no producer free text or private
// account, symbol, order, or evidence data.
type Presentation struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var presentations = map[rpc.AlertPresentationCode]Presentation{
	rpc.AlertPresentationPortfolioStress:                  {Title: "Portfolio stress", Body: "Canary reports portfolio stress."},
	rpc.AlertPresentationRegimeMarketStress:               {Title: "Market stress", Body: "Broad-market stress conditions need attention."},
	rpc.AlertPresentationRulebookSingleNameExposure:       {Title: "Single-name exposure", Body: "A portfolio concentration rule needs attention."},
	rpc.AlertPresentationRulebookOptionLinePremium:        {Title: "Option premium limit", Body: "An option-line premium rule needs attention."},
	rpc.AlertPresentationRulebookCashSellOnly:             {Title: "Cash safeguard", Body: "The cash sell-only safeguard is active."},
	rpc.AlertPresentationRulebookExtrinsicBudget:          {Title: "Extrinsic-value budget", Body: "The extrinsic-value budget needs attention."},
	rpc.AlertPresentationRulebookExpiryRunway:             {Title: "Expiry runway", Body: "An option expiry-runway rule needs attention."},
	rpc.AlertPresentationRulebookCatalystCoverage:         {Title: "Catalyst coverage", Body: "A catalyst-coverage rule needs attention."},
	rpc.AlertPresentationRulebookOverwriteEarnings:        {Title: "Earnings overwrite", Body: "An earnings overwrite rule needs attention."},
	rpc.AlertPresentationRulebookEarningsSizeFreeze:       {Title: "Earnings size freeze", Body: "The earnings size-freeze rule is active."},
	rpc.AlertPresentationRulebookRedOnGreen:               {Title: "Red-on-green rule", Body: "The red-on-green discipline rule needs attention."},
	rpc.AlertPresentationRulebookWinnerTrim:               {Title: "Winner trim", Body: "A winner-trim rule needs attention."},
	rpc.AlertPresentationRulebookGreenDayAction:           {Title: "Green-day action", Body: "A green-day action rule needs attention."},
	rpc.AlertPresentationRulebookHedgeIntegrity:           {Title: "Hedge integrity", Body: "A hedge-integrity rule needs attention."},
	rpc.AlertPresentationRulebookExitDiscipline:           {Title: "Exit discipline", Body: "An exit-discipline rule needs attention."},
	rpc.AlertPresentationRulebookFXExposure:               {Title: "Currency exposure", Body: "A currency-exposure rule needs attention."},
	rpc.AlertPresentationProtectionOrphanedOrder:          {Title: "Orphaned protection order", Body: "A protection order no longer matches a held position."},
	rpc.AlertPresentationProtectionReconciliationRequired: {Title: "Protection check required", Body: "Protective orders require reconciliation."},
	rpc.AlertPresentationOrderIntegrityMismatch:           {Title: "Order mismatch", Body: "Open orders do not match the expected protection state."},
	rpc.AlertPresentationDataHealthGateway:                {Title: "Gateway data unavailable", Body: "Gateway evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthStorage:                {Title: "Storage data unavailable", Body: "Required stored evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthProposals:              {Title: "Proposal data unavailable", Body: "Proposal evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthOpportunities:          {Title: "Opportunity data unavailable", Body: "Opportunity evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthDataFarms:              {Title: "Market-data farm issue", Body: "A required market-data farm is unavailable."},
	rpc.AlertPresentationDataHealthRegime:                 {Title: "Regime data unavailable", Body: "Regime evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthGamma:                  {Title: "Gamma data unavailable", Body: "Gamma evidence is unavailable or stale."},
	rpc.AlertPresentationDataHealthQuality:                {Title: "Data quality issue", Body: "Required evidence is incomplete, stale, or unavailable."},
	rpc.AlertPresentationRiskPolicyLimitWouldBlock:        {Title: "Risk limit would block", Body: "A current position would be blocked by the active risk policy."},
	rpc.AlertPresentationRiskPolicyDrawdownLatched:        {Title: "Drawdown block active", Body: "The risk-policy drawdown block is active."},
	rpc.AlertPresentationRiskPolicyDrift:                  {Title: "Risk policy drift", Body: "The active risk policy differs from its required state."},
	rpc.AlertPresentationReconciliationDue:                {Title: "Reconciliation due", Body: "A broker reconciliation is due."},
	rpc.AlertPresentationReconciliationException:          {Title: "Reconciliation exception", Body: "A broker reconciliation has an unresolved exception."},
	rpc.AlertPresentationReconciliationConfirmedFlow:      {Title: "Broker flow confirmed", Body: "A broker-confirmed cash or position flow needs review."},
	rpc.AlertPresentationGovernanceMonthlyPulse:           {Title: "Monthly desk review", Body: "The monthly desk review has an exception."},
	rpc.AlertPresentationDeliveryHealth:                   {Title: "Alert delivery issue", Body: "Alert delivery is degraded or unavailable."},
	rpc.AlertPresentationRulebookLegacyCondition:          {Title: "Trading rule", Body: "A trading rule needs attention."},
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
