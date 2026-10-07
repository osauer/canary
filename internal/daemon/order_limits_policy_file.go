package daemon

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/risk"
)

// Writing [order_limits] (owner decision 2026-10-05 19:56 CEST). The four
// per-order gates move from config.toml [trading] into the constitution.
// The ensure step (daemon start, canary policy ensure) writes the table from
// today's effective values: each key
// config.toml sets, else the compiled value [trading] used to fall back to,
// plus the new scaled-cap keys and the bond maturity limit (owner decision
// 2026-10-06 20:17 CEST). The compiled numbers below exist solely to be
// written into the file; the trading gate reads the file only.

// Compiled order-limit values, for writing only. They are also the Balanced
// preset by construction (cash_policy_presets.go): owner decision 2026-10-07
// 08:30 CEST ("Balanced is Canary's defaults", share of NLV 10%, option
// contracts 10) moved the share from 5.0 and the contracts from 5. A file
// that already writes its own values keeps them until the owner applies a
// preset; only a file that lacks the key, or a new file, gets these.
const (
	orderLimitsWriteFloorBase   = 10000.0
	orderLimitsWritePctNLV      = 10.0 // owner decision 2026-10-07 08:30 CEST; was 5.0
	orderLimitsWriteCeilingBase = 100000.0
	orderLimitsWriteOptionQty   = 10 // owner decision 2026-10-07 08:30 CEST; was 5
	// orderLimitsWriteBondMaturityYears is the owner's answer to decision B2
	// of internal-docs/design/bond-orders.md (2026-10-06 20:17 CEST, "30
	// years"): the longest maturity a bond or bill buy may have.
	orderLimitsWriteBondMaturityYears = 30
)

// orderLimitsWriteValue is one [order_limits] key with the value policy
// ensure writes and where it comes from.
type orderLimitsWriteValue struct {
	key, value, from string
}

// orderLimitsWriteValues lists every key in file order with the value to
// write. src is config.toml [trading] as written; srcRead is false when the
// file could not be read, so only compiled values are known.
func orderLimitsWriteValues(src config.Trading, srcRead bool) []orderLimitsWriteValue {
	compiled := "Canary's compiled default; config.toml [trading] does not set %s"
	if !srcRead {
		compiled = "Canary's compiled default; config.toml could not be read for %s"
	}
	fromConfig := "config.toml [trading].%s"
	decided := "owner decision 2026-10-05 19:56 CEST"
	pctDecided := decided + "; 10% since the owner decision 2026-10-07 08:30 CEST (the Balanced preset)"
	bondDecided := "owner decision 2026-10-06 20:17 CEST"

	floor, floorFrom := orderLimitsWriteFloorBase, fmt.Sprintf(compiled, "max_notional")
	if v := src.MaxNotional; v != nil && *v > 0 && !math.IsInf(*v, 0) && !math.IsNaN(*v) {
		floor, floorFrom = *v, fmt.Sprintf(fromConfig, "max_notional")
	}
	ceiling, ceilingFrom := orderLimitsWriteCeilingBase, decided
	if floor > ceiling {
		ceiling, ceilingFrom = floor, decided+", raised to the floor so the floor never exceeds it"
	}
	option, optionFrom := orderLimitsWriteOptionQty, fmt.Sprintf(compiled, "max_option_contracts")
	if v := src.MaxOptionContracts; v != nil && *v > 0 {
		option, optionFrom = *v, fmt.Sprintf(fromConfig, "max_option_contracts")
	}
	short, shortFrom := false, fmt.Sprintf(compiled, "allow_stock_short")
	if v := src.AllowStockShort; v != nil {
		short, shortFrom = *v, fmt.Sprintf(fromConfig, "allow_stock_short")
	}
	sto, stoFrom := false, fmt.Sprintf(compiled, "allow_option_sell_to_open")
	if v := src.AllowOptionSellToOpen; v != nil {
		sto, stoFrom = *v, fmt.Sprintf(fromConfig, "allow_option_sell_to_open")
	}
	return []orderLimitsWriteValue{
		{risk.OrderLimitMaxOrderFloorBase, tomlFloat(floor), floorFrom},
		{risk.OrderLimitMaxOrderPctNLV, tomlFloat(orderLimitsWritePctNLV), pctDecided},
		{risk.OrderLimitMaxOrderCeilingBase, tomlFloat(ceiling), ceilingFrom},
		{risk.OrderLimitMaxOptionContracts, strconv.Itoa(option), optionFrom},
		{risk.OrderLimitAllowStockShort, strconv.FormatBool(short), shortFrom},
		{risk.OrderLimitAllowOptionSellToOpen, strconv.FormatBool(sto), stoFrom},
		{risk.OrderLimitMaxBondMaturityYears, strconv.Itoa(orderLimitsWriteBondMaturityYears), bondDecided},
	}
}

// orderLimitsTableComment heads a newly written [order_limits] table.
var orderLimitsTableComment = []string{
	"# Per-order limits (owner decision 2026-10-05 19:56 CEST), moved here from",
	"# config.toml [trading]. The notional cap in force scales with the book:",
	"#   min(max_order_ceiling_base, max(max_order_floor_base, max_order_pct_nlv % of NLV)),",
	"# and is the floor whenever NLV cannot be read. Amounts are in the account",
	"# base currency. A missing key refuses every order preview. A one-shot",
	"# `canary policy override --control order_limits.max_order_floor_base` lifts",
	"# the floor to the ceiling until it expires. max_bond_maturity_years is the",
	"# longest maturity, in whole years from today, a bond or bill buy may have.",
}

// orderLimitsTemplateBlock renders the [order_limits] table of a new
// constitution file.
func orderLimitsTemplateBlock(src config.Trading, srcRead bool) string {
	var b strings.Builder
	b.WriteString("\n" + strings.Join(orderLimitsTableComment, "\n") + "\n[order_limits]\n")
	for _, v := range orderLimitsWriteValues(src, srcRead) {
		fmt.Fprintf(&b, "%s = %s  # %s\n", v.key, v.value, v.from)
	}
	return b.String()
}

// migrateConstitutionOrderLimits writes every [order_limits] key the file
// does not write, never changing one it does, and raises policy_version so
// the risk-policy manager adopts the revision. It returns the change lines.
func migrateConstitutionOrderLimits(doc *tomlDoc, c risk.Constitution, src config.Trading, srcRead bool) []string {
	missing := map[string]bool{}
	for _, k := range c.OrderLimits.MissingKeys() {
		missing[strings.TrimPrefix(k, risk.OrderLimitsTable+".")] = true
	}
	if len(missing) == 0 {
		return nil
	}
	if doc.headerLine(risk.OrderLimitsTable) < 0 {
		if n := len(doc.lines); n > 0 && strings.TrimSpace(doc.lines[n-1]) != "" {
			doc.lines = append(doc.lines, "")
		}
		doc.lines = append(doc.lines, orderLimitsTableComment...)
		doc.lines = append(doc.lines, "["+risk.OrderLimitsTable+"]")
	}
	var changes []string
	for _, v := range orderLimitsWriteValues(src, srcRead) {
		if !missing[v.key] {
			continue
		}
		doc.insert(risk.OrderLimitsTable, []string{v.key + " = " + v.value + "  # " + v.from})
		changes = append(changes, fmt.Sprintf("write [order_limits].%s = %s (%s)", v.key, v.value, v.from))
	}
	next := c.PolicyVersion + 1
	doc.set("", "policy_version", strconv.Itoa(next), nil)
	changes = append(changes, fmt.Sprintf("raise policy_version %d -> %d so the daemon adopts [order_limits]", c.PolicyVersion, next))
	return changes
}

// constitutionMaterialisationFileKey identifies every constitution setting
// except [order_limits], so the order-limits migration proves it changes
// nothing else.
func constitutionMaterialisationFileKey(_ string, data []byte) (string, error) {
	if err := parseConstitutionPolicy(data); err != nil {
		return "", err
	}
	var c risk.Constitution
	if _, err := toml.Decode(string(data), &c); err != nil {
		return "", err
	}
	c.OrderLimits = nil
	return c.EffectiveFingerprintKey(), nil
}

// orderLimitsPreserved reports whether every [order_limits] key before
// writes keeps its value after.
func orderLimitsPreserved(before, after *risk.ConstitutionOrderLimits) bool {
	if before == nil {
		return true
	}
	if after == nil {
		return false
	}
	sameF := func(a, b *float64) bool { return a == nil || (b != nil && *a == *b) }
	sameI := func(a, b *int) bool { return a == nil || (b != nil && *a == *b) }
	sameB := func(a, b *bool) bool { return a == nil || (b != nil && *a == *b) }
	return sameF(before.MaxOrderFloorBase, after.MaxOrderFloorBase) && sameF(before.MaxOrderPctNLV, after.MaxOrderPctNLV) &&
		sameF(before.MaxOrderCeilingBase, after.MaxOrderCeilingBase) && sameI(before.MaxOptionContracts, after.MaxOptionContracts) &&
		sameB(before.AllowStockShort, after.AllowStockShort) && sameB(before.AllowOptionSellToOpen, after.AllowOptionSellToOpen) &&
		sameI(before.MaxBondMaturityYears, after.MaxBondMaturityYears)
}
