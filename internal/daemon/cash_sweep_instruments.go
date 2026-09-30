package daemon

import (
	"fmt"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// cashSweepInstrumentConvention is how the broker counts and prices one
// vocabulary instrument. EVERY VALUE IS AN ASSUMPTION TO VERIFY on the
// post-install proof (internal-docs/design/cash-sweep.md, A5): no order uses
// them yet. Today they only label a row's resolved bill and turn a held
// bill's quantity into face value for the ladder.
type cashSweepInstrumentConvention struct {
	// QuantityUnit is what one unit of order quantity counts
	// (rpc.BondQuantityUnit*); FacePerUnit is that unit in face value of
	// the instrument's currency (0 for ETF shares).
	QuantityUnit string
	FacePerUnit  float64
	// PriceConvention is how a quote is expressed (rpc.BondPriceConvention*).
	PriceConvention string
}

// cashSweepInstrumentConventions: assumptions to verify, never facts.
//   - us_tbill: IBKR counts US Treasuries in bonds of 1,000 USD face and
//     quotes them per 100 of face.
//   - de_bubill, fr_btf, uk_tbill, ca_tbill: IBKR counts European, UK and
//     Canadian government bills in face value of their currency (one unit
//     is 1 of face) and quotes them per 100 of face; the contract's own
//     min_size and size_increment bound an order.
//   - etf: shares, quoted per share.
var cashSweepInstrumentConventions = map[string]cashSweepInstrumentConvention{
	cashSweepInstrumentUSTBill:  {QuantityUnit: rpc.BondQuantityUnitFace1000, FacePerUnit: 1000, PriceConvention: rpc.BondPriceConventionPer100},
	cashSweepInstrumentDEBubill: {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100},
	cashSweepInstrumentFRBTF:    {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100},
	cashSweepInstrumentUKTBill:  {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100},
	cashSweepInstrumentCATBill:  {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100},
	cashSweepInstrumentETF:      {QuantityUnit: rpc.BondQuantityUnitShares, PriceConvention: rpc.BondPriceConventionPerShare},
}

// cashSweepISINCountryInstrument maps an ISIN's issuer country to the bill
// instrument of that country. It identifies an owner-listed ISIN's
// instrument and a held bill's issuer; the country prefix is an assumption
// the post-install proof checks (a zero-coupon line of another issuer in the
// same country would read as that country's bill).
var cashSweepISINCountryInstrument = map[string]string{
	"US": cashSweepInstrumentUSTBill,
	"DE": cashSweepInstrumentDEBubill,
	"FR": cashSweepInstrumentFRBTF,
	"GB": cashSweepInstrumentUKTBill,
	"CA": cashSweepInstrumentCATBill,
}

// usTreasuryBillCUSIPPrefixes are the CUSIP issuer numbers the US Treasury
// has issued bills under. A CUSIP outside them is not a T-bill.
var usTreasuryBillCUSIPPrefixes = []string{"912794", "912795", "912796", "912797"}

func usTreasuryBillCUSIP(cusip string) bool {
	return slices.ContainsFunc(usTreasuryBillCUSIPPrefixes, func(p string) bool { return strings.HasPrefix(cusip, p) })
}

// cashSweepMaxISINs bounds an owner's list: each entry costs one contract
// lookup per day.
const cashSweepMaxISINs = 24

// validateCashSweepISINs checks a currency's isins: well-formed ISINs with a
// valid check digit, each belonging to a bill instrument the currency
// declares, listed once. USD bills come from TreasuryDirect, so a USD list
// is refused rather than silently ignored.
func validateCashSweepISINs(prefix, ccy string, c protectionCashSweepCurrency) error {
	if len(c.ISINs) == 0 {
		return nil
	}
	if ccy == "USD" {
		return fmt.Errorf("%s.isins: USD bills come from TreasuryDirect's outstanding list; remove isins", prefix)
	}
	if len(c.ISINs) > cashSweepMaxISINs {
		return fmt.Errorf("%s.isins lists %d; at most %d", prefix, len(c.ISINs), cashSweepMaxISINs)
	}
	for i, isin := range c.ISINs {
		if !ibkrlib.ValidISIN(isin) {
			return fmt.Errorf("%s.isins[%d] %q is not an ISIN (12 characters in capitals with a valid check digit)", prefix, i, isin)
		}
		instrument := cashSweepISINCountryInstrument[isin[:2]]
		if instrument == "" || cashSweepBillCurrency[instrument] != ccy || !slices.Contains(c.Instruments, instrument) {
			return fmt.Errorf("%s.isins[%d] %q is not a bill of an instrument declared for %s (%s)", prefix, i, isin, ccy, strings.Join(cashSweepDeclaredBills(c), ", "))
		}
		if slices.Contains(c.ISINs[:i], isin) {
			return fmt.Errorf("%s.isins lists %q twice", prefix, isin)
		}
	}
	return nil
}

// cashSweepDeclaredBills lists the bill instruments a currency declares.
func cashSweepDeclaredBills(c protectionCashSweepCurrency) []string {
	var out []string
	for _, instrument := range c.Instruments {
		if cashSweepIsBill(instrument) {
			out = append(out, instrument)
		}
	}
	if len(out) == 0 {
		return []string{"no bill instrument"}
	}
	return out
}

// cashSweepHeldBillInstrument names the vocabulary instrument a classified
// bill belongs to, or "" when it is not a vocabulary bill of ccy: a US bill
// by its Treasury CUSIP, any other by its ISIN's issuer country.
func cashSweepHeldBillInstrument(b rpc.PositionBond, ccy string) string {
	if b.Class != rpc.BondClassBill {
		return ""
	}
	instrument := ""
	switch {
	case ccy == "USD":
		if usTreasuryBillCUSIP(b.CUSIP) || (strings.HasPrefix(b.ISIN, "US") && usTreasuryBillCUSIP(b.ISIN[2:])) {
			instrument = cashSweepInstrumentUSTBill
		}
	case len(b.ISIN) == 12:
		instrument = cashSweepISINCountryInstrument[b.ISIN[:2]]
	}
	if instrument == "" || cashSweepBillCurrency[instrument] != ccy {
		return ""
	}
	return instrument
}
