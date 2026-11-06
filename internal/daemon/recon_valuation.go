package daemon

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/flexstmt"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// The capital document retains the broker inputs, not display-repaired FX.
// The original NLV remains the starting value: unexplained residuals are never
// discarded by replacing it with a sum of statement components.
func captureReconValuation(raw *ibkrlib.RawAccountSummary, positions []*ibkrlib.RawPosition, asOf time.Time, quotes map[string]reconFXQuote) *rpc.ReconRuntimeValuation {
	if raw == nil || raw.NetLiquidation == nil || !reconFinite(*raw.NetLiquidation) {
		return nil
	}
	v := &rpc.ReconRuntimeValuation{AsOf: asOf, BaseCurrency: normCcy(raw.BaseCurrency), EquityBase: *raw.NetLiquidation, Currencies: map[string]rpc.ReconNativeValuation{}}
	fail := func(reason string) *rpc.ReconRuntimeValuation { v.Reason = reason; return v }
	if !raw.BaseCurrencyProvenance.Proven() || asOf.IsZero() || !reconFinite(v.EquityBase) {
		return fail("runtime_basis_missing")
	}
	for c, row := range raw.CurrencyLedger {
		rate := row.ExchangeRate
		rateProven := row.ExchangeRateObserved && rate != 1
		var quote reconFXQuote
		switch {
		case c == v.BaseCurrency:
			rate, rateProven = 1, true
		case !rateProven && quotes[c].validAt(asOf):
			quote = quotes[c]
			rate, rateProven = quote.Rate, true
		}
		if !reconFinite(rate) || !reconFinite(row.NetLiquidationByCurrency) {
			v.Reason = "runtime_currency_basis_missing"
			continue
		}
		v.Currencies[c] = rpc.ReconNativeValuation{Equity: row.NetLiquidationByCurrency, FXRate: rate, FXSource: quote.Source, FXObservedAt: quote.At, StockValueReported: row.StockMarketValueText, OptionValueReported: row.OptionMarketValueText}
		// The account reader treats a foreign unit rate as unresolved. Without a
		// live quote of this moment, keep the broker value but do not promote it
		// into historical FX evidence.
		if !row.NetLiquidationObserved || !rateProven || rate <= 0 || !reconFinite(rate) || !reconFinite(row.NetLiquidationByCurrency) {
			v.Reason = "runtime_currency_basis_missing"
		}
	}
	if len(v.Currencies) == 0 {
		return fail("runtime_currency_basis_missing")
	}
	stocks, options := map[string]float64{}, map[string]float64{}
	seen := map[int]bool{}
	for _, p := range positions {
		if p == nil {
			return fail("runtime_position_basis_missing")
		}
		if p.Position == 0 {
			continue
		}
		if !reconFinite(p.Position) || !reconFinite(p.MarketPrice) || !reconFinite(p.MarketValue) {
			return fail("runtime_position_basis_missing")
		}
		c := p.Contract
		mult := c.Multiplier
		if c.SecType == "STK" && mult == 0 {
			mult = 1
		}
		v.Positions = append(v.Positions, rpc.ReconPositionValuation{ConID: int64(c.ConID), Currency: c.Currency, AssetClass: c.SecType, Quantity: p.Position, Multiplier: float64(mult), MarketValue: p.MarketValue, Mark: p.MarketPrice, ObservedAt: p.ValuationAt})
		if c.ConID <= 0 || seen[c.ConID] || !reconFinite(p.Position) || !reconFinite(p.MarketValue) || !reconFinite(p.MarketPrice) || mult <= 0 || p.ValuationAt.IsZero() || p.ValuationAt.After(asOf) || p.ValuationAt.UTC().Format("2006-01-02") != asOf.UTC().Format("2006-01-02") {
			return fail("runtime_position_basis_missing")
		}
		seen[c.ConID] = true
		if _, ok := v.Currencies[c.Currency]; !ok {
			return fail("runtime_currency_basis_missing")
		}
		switch c.SecType {
		case "STK":
			stocks[c.Currency] += p.MarketValue
		case "OPT":
			options[c.Currency] += p.MarketValue
		default:
			return fail("position_valuation_unsupported")
		}
	}
	// Both broker streams must describe the same marked book at the published
	// monetary precision. This is an accounting identity, not a risk tolerance.
	for c, row := range raw.CurrencyLedger {
		if !reconPublishedValueMatches(stocks[c], row.StockMarketValueText) || !reconPublishedValueMatches(options[c], row.OptionMarketValueText) {
			return fail("runtime_valuation_incoherent")
		}
	}
	slices.SortFunc(v.Positions, func(a, b rpc.ReconPositionValuation) int { return cmp.Compare(a.ConID, b.ConID) })
	return v
}

// Account-summary amounts may be whole units while portfolio rows carry cents.
// Use the numeric precision actually supplied by the broker, not a new tolerance.
func reconPublishedValueMatches(total float64, reported string) bool {
	if reported == "" {
		return total == 0
	}
	if strings.ContainsAny(reported, "eE") {
		return false
	}
	value, err := strconv.ParseFloat(reported, 64)
	if err != nil || !reconFinite(value) || !reconFinite(total) {
		return false
	}
	_, fraction, _ := strings.Cut(reported, ".")
	quantum := math.Pow10(-len(fraction))
	return quantum > 0 && reconFinite(total/quantum) && reconFinite(value/quantum) && math.Round(total/quantum) == math.Round(value/quantum)
}

func cloneReconValuation(v rpc.ReconRuntimeValuation) rpc.ReconRuntimeValuation {
	v.Currencies = maps.Clone(v.Currencies)
	v.Positions = slices.Clone(v.Positions)
	return v
}

func reconFinite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && math.Abs(v) != math.MaxFloat64
}

func compareReconValuation(row flexstmt.EquityRow, st flexstmt.Statement, sample rpc.ReconRuntimeValuation) *rpc.ReconEquityCheck {
	check := &rpc.ReconEquityCheck{StatementDate: row.ReportDate, StatementTotalBase: row.TotalBase, RuntimeEquityBase: new(sample.EquityBase), RuntimeAsOf: sample.AsOf, Comparability: "runtime_basis_missing"}
	day := row.ReportDate.UTC().Format("2006-01-02")
	check.SameDay = !sample.AsOf.IsZero() && sample.AsOf.UTC().Format("2006-01-02") == day
	if row.TotalBase != 0 && reconFinite(sample.EquityBase) && reconFinite(row.TotalBase) {
		check.RawDivergencePct = new((sample.EquityBase - row.TotalBase) / math.Abs(row.TotalBase) * 100)
	}
	data, _ := json.Marshal(st)
	fingerprint := sha256.Sum256(data)
	evidence := &rpc.ReconValuationEvidence{Basis: "statement_close_v1", Runtime: cloneReconValuation(sample), StatementGeneratedAt: st.WhenGenerated, StatementFingerprint: hex.EncodeToString(fingerprint[:])}
	check.Evidence = evidence
	if !check.SameDay || sample.BaseCurrency == "" || !reconFinite(sample.EquityBase) || sample.Reason != "" {
		if sample.Reason != "" {
			check.Comparability = sample.Reason
		}
		return check
	}
	f := st.FX
	check.Comparability = "statement_basis_missing"
	if f == nil || !f.SingleDay || f.Reason != "" || f.Day != day || f.BaseCurrency != sample.BaseCurrency || !reconFinite(row.TotalBase) || row.TotalBase == 0 || f.NAV != row.TotalBase || st.WhenGenerated.IsZero() {
		return check
	}
	evidence.StatementRates = maps.Clone(f.Rates)
	statementPositions := map[int64]rpc.ReconPositionValuation{}
	for _, p := range st.Positions {
		if p.LevelOfDetail != "SUMMARY" || p.Quantity == nil || p.MarkPrice == nil || p.Multiplier == nil || p.ConID <= 0 || !p.ReportDate.Equal(row.ReportDate) {
			return check
		}
		if *p.Quantity == 0 {
			continue
		}
		if p.AssetClass != "STK" && p.AssetClass != "OPT" {
			check.Comparability = "position_valuation_unsupported"
			return check
		}
		if _, duplicate := statementPositions[p.ConID]; duplicate {
			return check
		}
		if !reconFinite(*p.Quantity) || !reconFinite(*p.MarkPrice) || !reconFinite(*p.Multiplier) || *p.Multiplier <= 0 {
			return check
		}
		position := rpc.ReconPositionValuation{ConID: p.ConID, Currency: p.Currency, AssetClass: p.AssetClass, Quantity: *p.Quantity, Multiplier: *p.Multiplier, Mark: *p.MarkPrice, MarketValue: *p.Quantity * *p.Multiplier * *p.MarkPrice, ObservedAt: p.ReportDate}
		statementPositions[p.ConID] = position
		evidence.StatementPositions = append(evidence.StatementPositions, position)
	}
	check.Comparability = "position_inventory_changed"
	if len(statementPositions) != len(sample.Positions) {
		return check
	}
	seen := map[int64]bool{}
	for _, p := range sample.Positions {
		close, ok := statementPositions[p.ConID]
		if !ok || seen[p.ConID] || p.Quantity != close.Quantity || p.Currency != close.Currency || p.AssetClass != close.AssetClass || p.Multiplier != close.Multiplier {
			return check
		}
		seen[p.ConID] = true
		rate, ok := f.Rates[p.Currency]
		if !ok || rate <= 0 || !reconFinite(rate) || !reconFinite(p.MarketValue) {
			check.Comparability = "statement_currency_basis_missing"
			return check
		}
		evidence.MarketAdjustmentBase += (close.MarketValue - p.MarketValue) * rate
	}
	check.Comparability = "runtime_currency_basis_missing"
	if len(sample.Currencies) == 0 {
		return check
	}
	// Every currency in either closing book needs an observed runtime slice.
	// A missing slice is not an assumed zero balance. A closing balance that
	// rounds to zero at the cent, the broker's monetary precision, is FX dust
	// the live stream never publishes, not a balance.
	for c, value := range f.Book {
		if _, ok := sample.Currencies[c]; !ok && math.Round(value*100) != 0 {
			return check
		}
	}
	nativeTotal, fxAdjustment := 0.0, 0.0
	foreignUnitRate := false
	for _, c := range slices.Sorted(maps.Keys(sample.Currencies)) {
		native := sample.Currencies[c]
		rate, ok := f.Rates[c]
		if !ok || rate <= 0 || native.FXRate <= 0 || !reconFinite(rate) || !reconFinite(native.FXRate) || !reconFinite(native.Equity) {
			return check
		}
		if c == sample.BaseCurrency && (rate != 1 || native.FXRate != 1) {
			return check
		}
		foreignUnitRate = foreignUnitRate || (c != sample.BaseCurrency && native.FXRate == 1)
		nativeTotal += native.Equity * native.FXRate
		fxAdjustment += native.Equity * (rate - native.FXRate)
	}
	evidence.UnallocatedRuntimeBase = sample.EquityBase - nativeTotal
	// Legacy retained samples can lack the capture guard's Reason. Preserve
	// their raw residual, without using unproved FX to normalize it away.
	if foreignUnitRate {
		return check
	}
	evidence.FXAdjustmentBase = fxAdjustment
	compared := sample.EquityBase + evidence.FXAdjustmentBase + evidence.MarketAdjustmentBase
	if !reconFinite(compared) {
		return check
	}
	check.ComparedRuntimeBase = &compared
	check.DivergencePct = new((compared - row.TotalBase) / math.Abs(row.TotalBase) * 100)
	check.Comparability = "comparable"
	return check
}

// Retain each evaluated report's full evidence before it can extend a clock.
// The scoped append-only governance journal survives the rolling sample window.
func (st *riskCapitalStore) retainReconValuation(rep *rpc.ReconResult, scope brokerStateScope) error {
	if rep == nil || rep.ReportID == "" || rep.Equity == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.loadLocked()
	if st.core != nil {
		if err := st.selectScopeLocked(scope); err != nil {
			return err
		}
	}
	if st.state.LastValuationReportID == rep.ReportID {
		return nil
	}
	st.appendRiskPolicyJournal(map[string]any{"version": 1, "kind": "recon_valuation_evaluated", "at": st.now().UTC(), "report_id": rep.ReportID, "equity": rep.Equity})
	st.state.LastValuationReportID = rep.ReportID
	return st.persistLocked(true)
}
