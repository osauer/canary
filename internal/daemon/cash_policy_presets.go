package daemon

import (
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Risk presets (owner decisions 2026-10-07 08:30 and 08:33 CEST, from
// output/presets/2026-10-07-presets-design-final.md in Desk): Cautious,
// Balanced and Aggressive size five keys across two files, the reserve pair
// of [cash.sweep] and three of the constitution's [order_limits]. A preset
// sizes; it never switches a feature on, never permits a short or a
// sell-to-open, never moves the 100,000 ceiling or a pre-authorisation.
// Nothing is stored as a label: policy.cash.get derives the stance from the
// values in the files, key by key, so Custom is any override of a covered
// key, and a fresh desk at Canary's defaults reads Balanced without a save.
//
// Balanced is Canary's defaults by construction: it reads the sweep's written
// defaults (cashSweepWrittenDefaults) and the compiled order-limit values
// (order_limits_policy_file.go) rather than copying them, and a test holds
// the equality against the owner's numbers.
//
// Aggressive loosens the cap on new orders to 20% of NLV with a 15,000 floor
// and 100 option contracts per order, and keeps 5% of NLV as cash. What still
// bounds cost and risk under it: the 100,000 ceiling on one order, unchanged
// under every preset, and the one-shot floor override's reach with it; every
// other gate (freeze, pins, WhatIf, preview tokens, journal integrity, origin
// gating, the drawdown brake, the Rulebook's rules, sell-only and the
// governor); the owner's approval of every order, a daemon-sent order
// existing only under a pre-authorised bucket, which no preset lists; the
// sweep's own bounds (same-currency bills, no_buy_while_borrowed, its own
// order size, which no preset covers); and the device for every cap change.
// Worst case at a synthetic 240,000 book: one wrong new order of 48,000
// instead of 24,000, one opening option order of 100 contracts instead of 10,
// idle cash down to 12,000 from 24,000. Delta-reducing exits pass the caps
// under the owner's 08:13 rule (delta_reduction.go), so no preset makes the
// book less able to cut risk.

// cashPolicyCoveredKeys are the keys a preset sizes, in screen order.
var cashPolicyCoveredKeys = []string{
	"cash.sweep.reserve_floor_base", "cash.sweep.reserve_pct_nlv",
	"order_limits.max_order_floor_base", "order_limits.max_order_pct_nlv", "order_limits.max_option_contracts",
}

// cashPolicyPresetDef is one preset of one table.
type cashPolicyPresetDef struct {
	id, label, explainer string
	values               map[string]any
}

// cashPolicyPresetTable is one dated table of the three presets. When an
// owner decision changes a number, the earlier table stays below the new one
// so a desk on the earlier values reads "Balanced (values of <date>)", never
// Custom, after an upgrade.
type cashPolicyPresetTable struct {
	revision string
	presets  []cashPolicyPresetDef
}

// cashPolicyPresetRevision dates the current table.
const cashPolicyPresetRevision = "2026-10-07"

// cashPolicyPresetTables lists the tables, the current one first.
func cashPolicyPresetTables() []cashPolicyPresetTable {
	return []cashPolicyPresetTable{{revision: cashPolicyPresetRevision, presets: []cashPolicyPresetDef{
		{id: rpc.CashPolicyPresetCautious, label: "Cautious", explainer: "More cash in reserve, lower caps on new orders.",
			values: map[string]any{
				"cash.sweep.reserve_floor_base": 15000.0, "cash.sweep.reserve_pct_nlv": 15.0,
				"order_limits.max_order_floor_base": 5000.0, "order_limits.max_order_pct_nlv": 5.0, "order_limits.max_option_contracts": 5,
			}},
		{id: rpc.CashPolicyPresetBalanced, label: "Balanced", explainer: "Canary's defaults for these sizes.",
			values: map[string]any{
				"cash.sweep.reserve_floor_base": cashPolicyWritten["cash.sweep.reserve_floor_base"], "cash.sweep.reserve_pct_nlv": cashPolicyWritten["cash.sweep.reserve_pct_nlv"],
				"order_limits.max_order_floor_base": orderLimitsWriteFloorBase, "order_limits.max_order_pct_nlv": orderLimitsWritePctNLV, "order_limits.max_option_contracts": orderLimitsWriteOptionQty,
			}},
		{id: rpc.CashPolicyPresetAggressive, label: "Aggressive", explainer: "Less cash in reserve, higher caps on new orders.",
			values: map[string]any{
				"cash.sweep.reserve_floor_base": 10000.0, "cash.sweep.reserve_pct_nlv": 5.0,
				"order_limits.max_order_floor_base": 15000.0, "order_limits.max_order_pct_nlv": 20.0, "order_limits.max_option_contracts": 100,
			}},
	}}}
}

// cashPolicyPresetByID finds a preset in the current table.
func cashPolicyPresetByID(id string) (cashPolicyPresetDef, bool) {
	for _, p := range cashPolicyPresetTables()[0].presets {
		if p.id == id {
			return p, true
		}
	}
	return cashPolicyPresetDef{}, false
}

// cashPolicyPresetIsPreset reports whether id names one of the three presets.
func cashPolicyPresetIsPreset(id string) bool {
	_, ok := cashPolicyPresetByID(id)
	return ok
}

// cashPolicyCoveredValues reads the five covered keys from a state: the value
// each file carries, nil for one it does not write.
func cashPolicyCoveredValues(st cashPolicyState) map[string]any {
	out := map[string]any{}
	for _, key := range cashPolicyCoveredKeys {
		sp, ccy, _ := cashPolicySpecFor(key)
		if v, ok := sp.get(st, ccy); ok {
			out[key] = v
		} else {
			out[key] = nil
		}
	}
	return out
}

// cashPolicyStance derives the preset from the covered values: the stance
// is the first table, current first, whose values every written covered key
// matches by value (15 and 15.0 are equal). While the sweep writes neither
// reserve key the stance follows the order caps alone and the note says the
// sweep is not set up yet (owner decision 2026-10-07 08:30 CEST: a fresh
// start reads Balanced). An unwritten order-limit key reads Custom with a
// note: every order preview is refused until it is written. notes are
// appended to the note, each a sentence about where the values were read.
func cashPolicyStance(tables []cashPolicyPresetTable, values map[string]any, notes ...string) rpc.CashPolicyPreset {
	out := rpc.CashPolicyPreset{ID: rpc.CashPolicyPresetCustom, Label: "Custom", Explainer: "Your own values.", Revision: cashPolicyPresetRevision}
	var parts []string
	compare := slices.Clone(cashPolicyCoveredKeys)
	if values["cash.sweep.reserve_floor_base"] == nil && values["cash.sweep.reserve_pct_nlv"] == nil {
		parts = append(parts, "cash sweep not set up yet")
		compare = slices.DeleteFunc(compare, func(k string) bool { return strings.HasPrefix(k, "cash.") })
	}
	for _, key := range compare {
		if values[key] == nil {
			if strings.HasPrefix(key, risk.OrderLimitsTable+".") {
				parts = append(parts, "the file does not write "+key+"; every order preview is refused until it does")
			} else {
				parts = append(parts, "the cash sweep does not write "+key+" yet")
			}
			out.Note = cashPolicyJoinNotes(append(parts, notes...))
			return out
		}
	}
	for i, table := range tables {
		for _, p := range table.presets {
			match := true
			for _, key := range compare {
				if !cashPolicySame(values[key], p.values[key]) {
					match = false
					break
				}
			}
			if !match {
				continue
			}
			out.ID, out.Label, out.Explainer, out.Revision = p.id, p.label, p.explainer, table.revision
			if i > 0 {
				out.Label = p.label + " (values of " + table.revision + ")"
			}
			out.Note = cashPolicyJoinNotes(append(parts, notes...))
			return out
		}
	}
	out.Note = cashPolicyJoinNotes(append(parts, notes...))
	return out
}

func cashPolicyJoinNotes(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return sentence(strings.Join(kept, "; ")) + "."
}

// cashPolicyPresetOptions lists the current table with what each preset
// comes to at today's NLV, under the file's ceiling (the compiled one when
// the file writes none).
func cashPolicyPresetOptions(st cashPolicyState, b cashPolicyBook) []rpc.CashPolicyPresetOption {
	ceiling := orderLimitsWriteCeilingBase
	if st.c != nil && st.c.OrderLimits != nil && st.c.OrderLimits.MaxOrderCeilingBase != nil && *st.c.OrderLimits.MaxOrderCeilingBase > 0 {
		ceiling = *st.c.OrderLimits.MaxOrderCeilingBase
	}
	table := cashPolicyPresetTables()[0]
	out := make([]rpc.CashPolicyPresetOption, 0, len(table.presets))
	for _, p := range table.presets {
		values := map[string]any{}
		maps.Copy(values, p.values)
		out = append(out, rpc.CashPolicyPresetOption{ID: p.id, Label: p.label, Explainer: p.explainer, Revision: table.revision, Values: values,
			Facts: cashPolicyPresetFacts(p, b, ceiling)})
	}
	return out
}

// cashPolicyPresetFacts works out a preset at today's NLV: the reserve kept
// as cash (the larger of the amount and the share) and the order cap on new
// orders (the share between the floor and the ceiling); without NLV the
// amounts that apply without it, the floors.
func cashPolicyPresetFacts(p cashPolicyPresetDef, b cashPolicyBook, ceiling float64) rpc.CashPolicyPresetFacts {
	num := func(key string) float64 { f, _ := cashPolicyNumber(p.values[key]); return f }
	reserveFloor, reservePct := num("cash.sweep.reserve_floor_base"), num("cash.sweep.reserve_pct_nlv")
	capFloor, capPct := num("order_limits.max_order_floor_base"), num("order_limits.max_order_pct_nlv")
	contracts := int(num("order_limits.max_option_contracts"))
	out := rpc.CashPolicyPresetFacts{Contracts: contracts}
	if b.nlv <= 0 {
		out.NLVUnknown = true
		out.ReserveBase, out.OrderCapBase = reserveFloor, min(ceiling, capFloor)
		out.ReserveText = cashPolicyMoney(out.ReserveBase, b.base) + " at least; NLV is unknown now"
		out.OrderCapText = cashPolicyMoney(out.OrderCapBase, b.base) + ": the floor, which applies while NLV is unknown"
		return out
	}
	share := reservePct / 100 * b.nlv
	if share > reserveFloor {
		out.ReserveBase, out.ReserveText = share, cashPolicyMoney(share, b.base)+": "+policyCheckNumber(reservePct)+"% of NLV"
	} else {
		out.ReserveBase, out.ReserveText = reserveFloor, cashPolicyMoney(reserveFloor, b.base)+": the amount, more than "+policyCheckNumber(reservePct)+"% of NLV"
	}
	capShare := capPct / 100 * b.nlv
	switch {
	case capShare >= ceiling:
		out.OrderCapBase, out.OrderCapText = ceiling, cashPolicyMoney(ceiling, b.base)+": the ceiling"
	case capShare <= capFloor:
		out.OrderCapBase, out.OrderCapText = capFloor, cashPolicyMoney(capFloor, b.base)+": the floor, more than "+policyCheckNumber(capPct)+"% of NLV"
	default:
		out.OrderCapBase, out.OrderCapText = capShare, cashPolicyMoney(capShare, b.base)+": "+policyCheckNumber(capPct)+"% of NLV"
	}
	return out
}

// cashPolicyOrderCapAt is the order cap in force for a constitution's
// [order_limits] at an NLV (the floor without one), under its own ceiling:
// what a cap change comes to today, for the consequence sentences.
func cashPolicyOrderCapAt(o *risk.ConstitutionOrderLimits, nlv float64) (float64, bool) {
	if o == nil || o.MaxOrderFloorBase == nil || o.MaxOrderPctNLV == nil || o.MaxOrderCeilingBase == nil {
		return 0, false
	}
	floor, ceiling := *o.MaxOrderFloorBase, *o.MaxOrderCeilingBase
	if nlv <= 0 || math.IsNaN(nlv) || math.IsInf(nlv, 0) {
		return min(ceiling, floor), true
	}
	return min(ceiling, max(floor, *o.MaxOrderPctNLV/100*nlv)), true
}

// cashPolicyPresetProvenance names the preset a save lands on, for the
// provenance comment beside each changed line: " from the Cautious preset".
func cashPolicyPresetProvenance(id string) string {
	if p, ok := cashPolicyPresetByID(id); ok {
		return " from the " + p.label + " preset"
	}
	return ""
}

// cashPolicyPresetContracts renders a contract count for a sentence.
func cashPolicyPresetContracts(n int) string {
	if n == 1 {
		return "1 contract"
	}
	return strconv.Itoa(n) + " contracts"
}
