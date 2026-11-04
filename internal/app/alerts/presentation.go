// Package alerts turns daemon-authored alert state into redacted app inbox
// records and Web Push work. It owns app-side observation, durable-before-send
// dispatch ordering, and presentation; it does not evaluate risk policy or own
// daemon delivery authority.
package alerts

import (
	"github.com/osauer/canary/v2/internal/rpc"
	"strings"
)

// Presentation is fixed app-authored copy selected only by the daemon's
// closed presentation code. It contains no producer free text or private
// account, symbol, order, or evidence data.
type Presentation struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

var presentations = map[rpc.AlertPresentationCode]Presentation{
	rpc.AlertPresentationPortfolioStress:                  {Title: "Portfolio risk", Body: "Portfolio exposure is at a watch or action level."},
	rpc.AlertPresentationMarginCushion:                    {Title: "Margin cushion", Body: "The account margin cushion is below its safety threshold."},
	rpc.AlertPresentationRegimeMarketStress:               {Title: "Market warning", Body: "Broad-market conditions crossed a warning level."},
	rpc.AlertPresentationRulebookSingleNameExposure:       {Title: "Worst-case loss on one issuer", Body: "One issuer's worst-case loss is above its Rulebook level."},
	rpc.AlertPresentationRulebookOptionLinePremium:        {Title: "Premium at risk", Body: "One option position holds more premium than its Rulebook level."},
	rpc.AlertPresentationRulebookCashSellOnly:             {Title: "Premium budget", Body: "Long option premium at risk is at or above the Rulebook budget; option buys are held until it is back under budget."},
	rpc.AlertPresentationRulebookExtrinsicBudget:          {Title: "Option time value at risk", Body: "The amount paid for time remaining in long options is above the Rulebook budget."},
	rpc.AlertPresentationRulebookExpiryRunway:             {Title: "Option nearing expiry", Body: "A long option is inside the Rulebook expiry window."},
	rpc.AlertPresentationRulebookCatalystCoverage:         {Title: "Earnings timing", Body: "An option expires before the next earnings announcement."},
	rpc.AlertPresentationRulebookOverwriteEarnings:        {Title: "Short option through earnings", Body: "A short option remains open through the next earnings announcement."},
	rpc.AlertPresentationRulebookEarningsSizeFreeze:       {Title: "Position size near earnings", Body: "A position on an issuer at its Rulebook watch level is near earnings."},
	rpc.AlertPresentationRulebookRedOnGreen:               {Title: "Holding falls while market rises", Body: "A held stock is falling while the broad market rises."},
	rpc.AlertPresentationRulebookWinnerTrim:               {Title: "Large winner today", Body: "A large holding is above its daily gain level."},
	rpc.AlertPresentationRulebookGreenDayAction:           {Title: "Positive day with urgent risks", Body: "The account is positive today while an urgent Rulebook item remains open."},
	rpc.AlertPresentationRulebookHedgeIntegrity:           {Title: "Index protection size", Body: "Index protection is missing or outside its Rulebook sizing range."},
	rpc.AlertPresentationRulebookExitDiscipline:           {Title: "Long option loss limit", Body: "A long option has crossed its premium-loss level."},
	rpc.AlertPresentationRulebookFXExposure:               {Title: "Foreign-currency exposure", Body: "Foreign-currency exposure is above its Rulebook level."},
	rpc.AlertPresentationRulebookNetExposure:              {Title: "Net market exposure", Body: "The book's net market exposure is above its Rulebook level."},
	rpc.AlertPresentationRulebookDeltaSwing:               {Title: "Delta swing on one issuer", Body: "One issuer's dollar delta is at its Rulebook watch level."},
	rpc.AlertPresentationRulebookClusterStress:            {Title: "Cluster falling together", Body: "A declared cluster's loss in a joint fall is at its Rulebook watch level."},
	rpc.AlertPresentationRulebookLossBudget:               {Title: "Issuer loss against risk capital", Body: "One issuer's worst-case loss is at its watch level against your risk capital."},
	rpc.AlertPresentationRulebookMarginHeadroom:           {Title: "Margin headroom", Body: "Excess liquidity is below its Rulebook level as a share of NLV."},
	rpc.AlertPresentationProtectionOrphanedOrder:          {Title: "Orphaned protection order", Body: "A protection order no longer matches a held position."},
	rpc.AlertPresentationProtectionReconciliationRequired: {Title: "Protection check required", Body: "Protective orders require reconciliation."},
	// Pre-authorised protection: the notice names the bucket and the timing;
	// the Protection row carries the symbol, quantity and exact time.
	rpc.AlertPresentationProtectionAutoTrailingStop:         {Title: "Trailing stop will be placed", Body: "Canary will place the trailing stop it proposed once the veto window closes. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoTrailingStopNow:      {Title: "Trailing stop being placed", Body: "The drawdown brake is latched, so Canary is placing the trailing stop it proposed now."},
	rpc.AlertPresentationProtectionAutoOptionLossExit:       {Title: "Option loss exit will be placed", Body: "Canary will place the option loss exit it proposed once the veto window closes. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoOptionLossExitNow:    {Title: "Option loss exit being placed", Body: "The drawdown brake is latched, so Canary is placing the option loss exit it proposed now."},
	rpc.AlertPresentationProtectionAutoOptionProfitTrail:    {Title: "Option profit trail will be placed", Body: "Canary will place the option profit trail it proposed once the veto window closes. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoOptionProfitTrailNow: {Title: "Option profit trail being placed", Body: "The drawdown brake is latched, so Canary is placing the option profit trail it proposed now."},
	rpc.AlertPresentationProtectionAutoBudgetReduction:      {Title: "Reduction to budget will be placed", Body: "Canary will place the reduction back to budget it proposed once the veto window closes. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoBudgetReductionNow:   {Title: "Reduction to budget being placed", Body: "The drawdown brake is latched, so Canary is placing the reduction back to budget it proposed now."},
	rpc.AlertPresentationProtectionAutoCashSweep:            {Title: "Cash sweep order will be placed", Body: "Canary will place the bill order the cash sweep proposed once the full veto window closes. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoMandateReduction:     {Title: "Reduction to the risk budget will be placed", Body: "The book's risk is over the mandate budget. Canary will place the reduction it proposed once the full veto window closes, largest risk first. Open Protection to veto it."},
	// A deferred or held submission is still pending: the copy says what it
	// waits for, never that the protection resolved.
	rpc.AlertPresentationProtectionAutoDeferred:        {Title: "Protection deferred", Body: "Deferred: trading frozen · resubmits when lifted. Canary will place the protection it proposed once trading.freeze is lifted, while the proposal is current. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoHeld:            {Title: "Protection held", Body: "Held: hand order working. Canary will place the protection it proposed once the order placed outside Canary fills or is cancelled, without a new veto window. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoHeldUnverified:  {Title: "Protection held", Body: "Held: open orders unreadable. Canary cannot rule out a working hand order, so it will place the protection it proposed once the broker's open orders can be read, without a new veto window. Open Protection to veto it."},
	rpc.AlertPresentationProtectionAutoHeldMandate:     {Title: "Reduction held", Body: "Held: risk not measurable. Canary will place the reduction to the risk budget it proposed once a fresh measurement confirms it, without a new veto window, or withdraw it once the risk is back within budget. Open Protection to veto it."},
	rpc.AlertPresentationOrderIntegrityMismatch:        {Title: "Order mismatch", Body: "Open orders do not match the expected protection state."},
	rpc.AlertPresentationDataHealthGateway:             {Title: "TWS connection issue", Body: "Canary cannot read current account or market data from TWS."},
	rpc.AlertPresentationDataHealthStorage:             {Title: "History needs attention", Body: "Canary's stored history is incomplete or too old for a current result."},
	rpc.AlertPresentationDataHealthProposals:           {Title: "Protection refresh failed", Body: "Canary could not refresh the protection suggestions."},
	rpc.AlertPresentationDataHealthOpportunities:       {Title: "Opportunity refresh failed", Body: "Canary could not refresh the opportunity scan."},
	rpc.AlertPresentationDataHealthDataFarms:           {Title: "Market-data farm issue", Body: "TWS explicitly reported a required farm as broken or disconnected; a missing informational connection notice alone does not light this alert."},
	rpc.AlertPresentationDataHealthRegime:              {Title: "Market stress inputs need attention", Body: "One or more market stress inputs are incomplete or too old."},
	rpc.AlertPresentationDataHealthGamma:               {Title: "Options positioning needs attention", Body: "The options positioning calculation is incomplete or too old."},
	rpc.AlertPresentationDataHealthQuality:             {Title: "Current result not ready", Body: "One or more inputs needed for a current result are incomplete or too old."},
	rpc.AlertPresentationDataHealthConfig:              {Title: "Config file needs attention", Body: "Part of config.toml could not be read and runs on Canary's defaults. Fix the file and restart the daemon."},
	rpc.AlertPresentationRiskPolicyLimitWouldBlock:     {Title: "Brake refused an order", Body: "The drawdown brake refused a risk-adding order under the active risk policy."},
	rpc.AlertPresentationRiskPolicyDrawdownLatched:     {Title: "Drawdown latch open", Body: "Account value crossed the limit. Canary has not confirmed whether the move came from trading or an external cash flow."},
	rpc.AlertPresentationRiskPolicyDrift:               {Title: "Risk policy drift", Body: "The active risk policy differs from its required state."},
	rpc.AlertPresentationReconciliationDue:             {Title: "Reconciliation due", Body: "A broker reconciliation is due."},
	rpc.AlertPresentationReconciliationException:       {Title: "Reconciliation exception", Body: "A broker reconciliation has an unresolved exception."},
	rpc.AlertPresentationReconciliationConfirmedFlow:   {Title: "Broker flow confirmed", Body: "A broker-confirmed cash or position flow needs review."},
	rpc.AlertPresentationGovernanceMonthlyPulse:        {Title: "Monthly desk review", Body: "The monthly desk review has an exception."},
	rpc.AlertPresentationDeliveryHealth:                {Title: "Alert delivery issue", Body: "Alert delivery is degraded or unavailable."},
	rpc.AlertPresentationRulebookLegacyCondition:       {Title: "Legacy trading-rule alert", Body: "This compatibility alert has been superseded by the current named Rulebook rows."},
	rpc.AlertPresentationRiskPolicyLegacyCondition:     {Title: "Risk policy", Body: "A risk-policy condition needs attention."},
	rpc.AlertPresentationReconciliationLegacyCondition: {Title: "Reconciliation", Body: "A reconciliation condition needs attention."},
	rpc.AlertPresentationGovernanceLegacyCondition:     {Title: "Desk process", Body: "A desk-process condition needs attention."},
}

// PresentationForBroker is PresentationFor with the copy that names a broker's
// process naming the broker the alert came from.
func PresentationForBroker(code rpc.AlertPresentationCode, state rpc.AlertEpisodeState, broker string) (Presentation, bool) {
	presentation, ok := PresentationFor(code, state)
	if !ok || broker != "opend" {
		return presentation, ok
	}
	presentation.Title = strings.ReplaceAll(presentation.Title, "TWS", "OpenD")
	presentation.Body = strings.ReplaceAll(presentation.Body, "TWS", "OpenD")
	return presentation, true
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
		// Escalated records that this occurrence once moved higher. The fixed
		// copy describes the current result, whose severity may since have
		// fallen, so do not present historical lifecycle state as current risk.
	case rpc.AlertEpisodeRecovered:
		presentation.Body = "Resolved: " + presentation.Body
	case rpc.AlertEpisodeOpen:
	default:
		return Presentation{}, false
	}
	return presentation, true
}
