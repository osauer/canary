package daemon

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// cashSweepInstrumentConvention is how the broker counts and prices one
// vocabulary instrument, and when its orders can fill. EVERY VALUE IS AN
// ASSUMPTION TO VERIFY on the post-install proof
// (internal-docs/design/cash-sweep.md, A5). The order path sizes and prices
// by them: an invest row's quantity is whole QuantityUnits, its limit is per
// 100 of face, and a held bill's quantity turns into face value for the
// ladder.
type cashSweepInstrumentConvention struct {
	// QuantityUnit is what one unit of order quantity counts
	// (rpc.BondQuantityUnit*); FacePerUnit is that unit in face value of
	// the instrument's currency (0 for ETF shares).
	QuantityUnit string
	FacePerUnit  float64
	// PriceConvention is how a quote is expressed (rpc.BondPriceConvention*).
	PriceConvention string
	// SecTypes are the IBKR security types a line of the instrument is
	// asked as, in order (A10).
	SecTypes []string
	// SessionLabel, TimeZone, Open and Close (local HHMM on weekdays) are the
	// assumed session a DAY order fills in, used only when the line's
	// contract details carry no liquid or trading hours. Holidays are not
	// modelled: on one the assumed session reads open and the preview's live
	// quote requirement refuses instead.
	SessionLabel string
	TimeZone     string
	Open, Close  int
}

// cashSweepInstrumentConventions: assumptions to verify, never facts.
//   - us_tbill: IBKR counts US Treasuries in bonds of 1,000 USD face and
//     quotes them per 100 of face; they trade 08:00–17:00 New York time.
//   - de_bubill, fr_btf, uk_tbill, ca_tbill: IBKR counts European, UK and
//     Canadian government bills in face value of their currency (one unit
//     is 1 of face) and quotes them per 100 of face; Bubills and BTFs trade
//     09:00–17:30 Frankfurt and Paris time, UK bills 08:00–16:30 London,
//     Canadian bills 08:00–17:00 Toronto.
//   - every bill: the contract's own min_size, size_increment and min_tick
//     bound an order, and its liquid hours (else trading hours) replace the
//     assumed session.
//   - etf: shares, quoted per share, on its exchange's calendar.
//   - security type (A10): IBKR lists US Treasury bills as secType BILL, so
//     us_tbill is asked as BILL only; a German, French, UK or Canadian bill
//     is asked as BILL first and BOND second, and the row records which one
//     resolved.
var cashSweepInstrumentConventions = map[string]cashSweepInstrumentConvention{
	cashSweepInstrumentUSTBill: {QuantityUnit: rpc.BondQuantityUnitFace1000, FacePerUnit: 1000, PriceConvention: rpc.BondPriceConventionPer100,
		SecTypes:     []string{ibkrlib.SecTypeBill},
		SessionLabel: "US Treasury bills", TimeZone: "America/New_York", Open: 800, Close: 1700},
	cashSweepInstrumentDEBubill: {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100,
		SecTypes:     []string{ibkrlib.SecTypeBill, ibkrlib.SecTypeBond},
		SessionLabel: "German Bubills", TimeZone: "Europe/Berlin", Open: 900, Close: 1730},
	cashSweepInstrumentFRBTF: {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100,
		SecTypes:     []string{ibkrlib.SecTypeBill, ibkrlib.SecTypeBond},
		SessionLabel: "French BTFs", TimeZone: "Europe/Paris", Open: 900, Close: 1730},
	cashSweepInstrumentUKTBill: {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100,
		SecTypes:     []string{ibkrlib.SecTypeBill, ibkrlib.SecTypeBond},
		SessionLabel: "UK Treasury bills", TimeZone: "Europe/London", Open: 800, Close: 1630},
	cashSweepInstrumentCATBill: {QuantityUnit: rpc.BondQuantityUnitFace1, FacePerUnit: 1, PriceConvention: rpc.BondPriceConventionPer100,
		SecTypes:     []string{ibkrlib.SecTypeBill, ibkrlib.SecTypeBond},
		SessionLabel: "Canadian Treasury bills", TimeZone: "America/Toronto", Open: 800, Close: 1700},
	cashSweepInstrumentETF: {QuantityUnit: rpc.BondQuantityUnitShares, PriceConvention: rpc.BondPriceConventionPerShare},
}

// cashSweepInstrumentSecTypes are the IBKR security types a vocabulary
// bill is asked as, in order; any other instrument is asked as BOND.
func cashSweepInstrumentSecTypes(instrument string) []string {
	if secTypes := cashSweepInstrumentConventions[instrument].SecTypes; len(secTypes) > 0 {
		return slices.Clone(secTypes)
	}
	return []string{ibkrlib.SecTypeBond}
}

// cashSweepHeldSecTypes are the types a held line is asked as by contract
// id: the position's own type first, then the instrument's.
func cashSweepHeldSecTypes(positionSecType, instrument string) []string {
	out := []string{ibkrlib.BillOrBondSecType(positionSecType)}
	for _, secType := range cashSweepInstrumentSecTypes(instrument) {
		if !slices.Contains(out, secType) {
			out = append(out, secType)
		}
	}
	return out
}

// cashSweepIdentifierInstrument names the vocabulary bill an identifier
// would be, or "": a US Treasury bill CUSIP (or its US ISIN) is us_tbill;
// another ISIN follows its issuer country. A line of that country may still
// be a bond; the lookup asks both types.
func cashSweepIdentifierInstrument(idType, id string) string {
	switch idType {
	case ibkrlib.BondIdentifierCUSIP:
		if usTreasuryBillCUSIP(id) {
			return cashSweepInstrumentUSTBill
		}
	case ibkrlib.BondIdentifierISIN:
		if len(id) != 12 {
			return ""
		}
		if id[:2] == "US" {
			if usTreasuryBillCUSIP(id[2:11]) {
				return cashSweepInstrumentUSTBill
			}
			return ""
		}
		return cashSweepISINCountryInstrument[id[:2]]
	}
	return ""
}

// cashSweepBondConvention is the convention of a BOND line in ccy that is
// not known to be a vocabulary bill: a working order or a fill Canary's
// journal holds for it. It assumes the currency's bill unit (A5), so a USD
// bond counts 1,000 of face and a EUR, GBP or CAD bond 1; any other currency
// has none and stays unvalued.
func cashSweepBondConvention(ccy string) (cashSweepInstrumentConvention, bool) {
	for _, instrument := range slices.Sorted(maps.Keys(cashSweepBillCurrency)) {
		if cashSweepBillCurrency[instrument] == normCcy(ccy) {
			return cashSweepInstrumentConventions[instrument], true
		}
	}
	return cashSweepInstrumentConvention{}, false
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
