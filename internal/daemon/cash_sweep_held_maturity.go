package daemon

import (
	"fmt"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// cashSweepHeldMaturity binds public dates only to an exact held USD Treasury
// BILL. Every sibling frame must agree; a malformed or contradictory broker
// date never becomes an omitted date. Other currencies require broker dates.
func cashSweepHeldMaturity(src cashSweepBillSource, lines []ibkrlib.BondContractDetails, line ibkrlib.BondContractDetails, now time.Time) (string, string, time.Time, error) {
	if !cashSweepBondIdentifierChannelsAgree(line) {
		return "", "", time.Time{}, fmt.Errorf("the held bill's broker identifier channels contradict each other")
	}
	maturity, hasDate := line.MaturityDate()
	if !hasDate && strings.TrimSpace(line.Maturity) != "" {
		return "", "", time.Time{}, fmt.Errorf("the held bill's broker maturity is malformed")
	}
	source, at := rpc.CashSweepMaturitySourceBroker, time.Time{}
	var candidate cashSweepBillCandidate
	if !hasDate {
		if src == nil || bondLineInstrument(line) != cashSweepInstrumentUSTBill || line.SecType != ibkrlib.SecTypeBill || normCcy(line.Currency) != "USD" {
			return "", "", at, fmt.Errorf("the held bill has no broker maturity or supported public source")
		}
		bills, fetchedAt, reason := src.usBills(now)
		if reason != "" || fetchedAt.IsZero() || fetchedAt.After(now) || now.Sub(fetchedAt) > treasuryBillUniverseMaxAge {
			return "", "", at, fmt.Errorf("the held bill needs a current dated TreasuryDirect list")
		}
		found := false
		for _, bill := range bills {
			if bill.CUSIP != line.CUSIP() {
				continue
			}
			issue, okIssue := treasuryDirectDate(bill.IssueDate)
			date, okDate := treasuryDirectDate(bill.MaturityDate)
			if !okIssue || !okDate || issue.After(cashSweepDay(now)) || !issue.Before(date) || !date.After(cashSweepDay(now)) || (found && !maturity.Equal(date)) {
				return "", "", at, fmt.Errorf("the exact public Treasury bill dates are unavailable or contradictory")
			}
			maturity, found = date, true
		}
		if !found {
			return "", "", at, fmt.Errorf("the held CUSIP is absent from TreasuryDirect's issued bill list")
		}
		at = fetchedAt.UTC()
		candidate = cashSweepBillCandidate{id: line.CUSIP(), instrument: cashSweepInstrumentUSTBill,
			source: rpc.CashSweepBillSourceTreasuryDirect, maturity: maturity, publicFetchedAt: at}
		source = rpc.CashSweepMaturitySourceTreasuryDirect
	}
	for _, sibling := range lines {
		if sibling.ConID != line.ConID {
			continue
		}
		if !cashSweepBondIdentifierChannelsAgree(sibling) {
			return "", "", at, fmt.Errorf("the held contract's broker identifier channels contradict each other")
		}
		if normCcy(sibling.Currency) != normCcy(line.Currency) || !strings.EqualFold(sibling.SecType, line.SecType) || sibling.CUSIP() != line.CUSIP() || sibling.ISIN() != line.ISIN() || sibling.Coupon != line.Coupon {
			return "", "", at, fmt.Errorf("the held contract's identity frames contradict each other")
		}
		if !hasDate {
			matchedSource, err := cashSweepUSBillMaturity(candidate, sibling, now)
			if err != nil {
				return "", "", at, err
			}
			if matchedSource == rpc.CashSweepMaturitySourceBrokerTreasuryDirect {
				source = matchedSource
			}
		} else {
			date, ok := sibling.MaturityDate()
			if !ok || !date.Equal(maturity) {
				return "", "", at, fmt.Errorf("the held contract's maturity frames contradict each other")
			}
		}
	}
	return maturity.Format(time.DateOnly), source, at, nil
}

// cashSweepCheckReviewedMaturity rechecks semantic identity against fresh
// preview-session frames. A missing date needs the same public provenance;
// broker dates must match the review exactly. Receipt-time refresh is harmless.
func cashSweepCheckReviewedMaturity(src cashSweepBillSource, lines []ibkrlib.BondContractDetails, line ibkrlib.BondContractDetails, terms rpc.OrderBondTerms, now time.Time) error {
	if terms.Maturity == "" {
		return nil
	}
	if terms.CUSIP != "" && terms.CUSIP != line.CUSIP() || terms.ISIN != "" && terms.ISIN != line.ISIN() {
		return fmt.Errorf("the bill's identifier changed after proposal review")
	}
	date, _, _, err := cashSweepHeldMaturity(src, lines, line, now)
	if err != nil {
		return err
	}
	if date != terms.Maturity {
		return fmt.Errorf("the bill's maturity changed after proposal review")
	}
	return nil
}

// Identifier selection precedence cannot erase another valid issuer identity.
func cashSweepBondIdentifierChannelsAgree(line ibkrlib.BondContractDetails) bool {
	if cusip, isin := line.CUSIP(), line.ISIN(); usTreasuryBillCUSIP(cusip) && isin != "" && (!strings.HasPrefix(isin, "US") || isin[2:11] != cusip) {
		return false
	}
	for _, raw := range []string{line.CUSIPField, line.SecIDs[ibkrlib.BondIdentifierCUSIP], line.SecIDs[ibkrlib.BondIdentifierISIN]} {
		id := strings.ToUpper(strings.TrimSpace(raw))
		if ibkrlib.ValidCUSIP(id) && id != line.CUSIP() || ibkrlib.ValidISIN(id) && id != line.ISIN() {
			return false
		}
	}
	return true
}
