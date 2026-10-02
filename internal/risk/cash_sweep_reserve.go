package risk

import (
	"fmt"
	"math"
	"time"
)

// CashSweepFundingEvidence describes additional native funding needs after
// separately reserved working-order commitments. It is not marked portfolio
// loss or total broker margin. Only a calibrated observer may supply it.
type CashSweepFundingEvidence struct {
	AsOf, ValidUntil             time.Time
	ScenarioFingerprint          string
	FundingNative                map[string]float64
	NAVFloorPassed, MarginPassed bool
}

// CashSweepReserveAllocation allocates one EUR cushion by verified funding
// demand. Existing native floors overlap funding requirements through max;
// the one cushion is additional. Missing demand, FX, or scenario evidence
// never means zero.
func CashSweepReserveAllocation(cushionEUR float64, rates, floors map[string]float64, evidence *CashSweepFundingEvidence, now time.Time) (buffer, reserve map[string]float64, reason string) {
	if evidence == nil {
		return nil, nil, "reserve_calibration_required: operational funding, finite stress and exit horizon are not commissioned"
	}
	if evidence.AsOf.IsZero() || evidence.AsOf.After(now) || !evidence.ValidUntil.After(now) || evidence.ValidUntil.Before(evidence.AsOf) || evidence.ScenarioFingerprint == "" {
		return nil, nil, "reserve_evidence_unavailable: current calibrated funding evidence is required"
	}
	if !evidence.NAVFloorPassed || !evidence.MarginPassed {
		return nil, nil, "reserve_safety_hold: protected NAV or stressed margin check has not passed"
	}
	valid := func(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 }
	if !valid(cushionEUR) {
		return nil, nil, "reserve_configuration_invalid: cushion must be finite and nonnegative"
	}
	if rates["EUR"] != 1 {
		return nil, nil, "reserve_fx_unavailable: verified EUR valuation rates are required"
	}
	total := 0.0
	for ccy, need := range evidence.FundingNative {
		if _, observed := floors[ccy]; !observed && (!valid(need) || need != 0) {
			return nil, nil, fmt.Sprintf("reserve_funding_unavailable: funding currency %s has no observed cash reserve", ccy)
		}
	}
	for ccy, floor := range floors {
		need, known := evidence.FundingNative[ccy]
		rate := rates[ccy]
		if !known || !valid(need) || !valid(floor) || !valid(rate) || rate == 0 || !valid(need*rate) {
			return nil, nil, fmt.Sprintf("reserve_funding_unavailable: verified funding and EUR FX required for %s", ccy)
		}
		total += need * rate
	}
	if !valid(total) {
		return nil, nil, "reserve_funding_invalid: aggregate funding overflow"
	}
	buffer, reserve = map[string]float64{}, map[string]float64{}
	if total == 0 {
		if _, ok := floors["EUR"]; !ok {
			return nil, nil, "reserve_funding_unavailable: EUR cash is required for the zero-demand cushion"
		}
	}
	for ccy, floor := range floors {
		need, rate := evidence.FundingNative[ccy], rates[ccy]
		allocatedEUR := 0.0
		if total > 0 {
			allocatedEUR = cushionEUR * (need * rate / total)
		} else if ccy == "EUR" {
			allocatedEUR = cushionEUR
		}
		buffer[ccy] = allocatedEUR / rate
		reserve[ccy] = math.Max(floor, need) + buffer[ccy]
		if !valid(buffer[ccy]) || !valid(reserve[ccy]) {
			return nil, nil, "reserve_funding_invalid: native reserve overflow"
		}
	}
	return buffer, reserve, ""
}
