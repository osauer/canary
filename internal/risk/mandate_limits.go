package risk

import "strconv"

// MandateAdditionsOverrideControl is the one [mandate] key that takes a
// one-shot override (canary policy override --control mandate.vol_target_pct
// --reason TEXT --hours N): while it is active the mandate's additions gate
// (since 2026-10-27 the loss measure against the loss room and the sleeve
// allocations; before, the volatility budget) no longer refuses the owner's
// additions (owner decision 2026-10-23 18:22 CEST). What still bounds risk
// meanwhile: the drawdown brake at the 30% maximum loss and the mandate's
// own no-room hold, the owner sleeve's
// drawdown stop, every order cap, the premium limits on option additions
// (option_name_act_pct per name, the premium budget's act level for the
// book), the device confirmation of every order, and the override's own
// expiry under override.max_duration_hours. The other mandate keys change
// only by a policy revision.
const MandateAdditionsOverrideControl = "mandate.vol_target_pct"

// mandateLimits renders the [mandate] keys as constitution limits once the
// table is written, so `canary policy show --explain` names them and the
// override control is a known key. A file without the table lists none,
// as it carries no mandate.
func mandateLimits(c *Constitution, get func(key, value, source, meaning, enforcement string) ConstitutionLimit,
	pct func(v *float64) (string, string), money func(v *float64) (string, string)) []ConstitutionLimit {
	if c == nil || c.Mandate == nil {
		return nil
	}
	m := c.Mandate
	lossVal, lossSrc := pct(m.MaxLossPct)
	volVal, volSrc := pct(m.VolTargetPct)
	reserveVal, reserveSrc := money(m.TaxReserveBase)
	gapVal, gapSrc := pct(m.GapShockPct)
	roomVal, roomSrc := pct(m.GapRoomPct)
	startVal, startSrc := "unapproved", "unapproved"
	if m.StartedAt != "" {
		startVal, startSrc = strconv.Quote(m.StartedAt), "file"
	}
	return append([]ConstitutionLimit{
		get("mandate.max_loss_pct", lossVal, lossSrc,
			"The largest loss of the budget base the owner tolerates, in percent: room runs from 1 at a new high to 0 at this loss. The verdict's measure: the loss room (this percent of the budget base at room) bounds the loss measure, every bought option at its premium at risk plus the cash loss to the stop of every stopped share line (spec TM-04.12, owner decision 2026-10-27); over it the verdict reads reduce and additions are refused. Changes only by a policy revision.", "hard"),
		get(MandateAdditionsOverrideControl, volVal, volSrc,
			"The yearly volatility budget in percent of the budget base at full room. It sizes the sleeve allocations and the machine strategies, and is shown beside the loss measure as the 1-day swing; since 2026-10-27 it is no longer the verdict's headline. A sleeve over its allocation still refuses stock and ETF additions; option additions are gated by the premium limits instead. The one mandate key that takes a one-shot override: while one is active the volatility budget refuses no addition of yours; the maximum loss, the owner sleeve's stop, the order caps, the premium limits and the device confirmation still bind.", "hard"),
		get("mandate.tax_reserve_base", reserveVal, reserveSrc,
			"The part of equity that is not risk base, in the base currency. Changes only by a policy revision.", "advisory"),
		get("mandate.started_at", startVal, startSrc,
			"The date the high-water mark counts from; a change starts the series again.", "advisory"),
		get("mandate.gap_shock_pct", gapVal, gapSrc,
			"The one-name gap each underlying is stressed by, in percent of its price. Changes only by a policy revision.", "hard"),
		get("mandate.gap_room_pct", roomVal, roomSrc,
			"The share of the remaining loss room one name may lose in that gap, in percent. Changes only by a policy revision.", "hard"),
	}, append(ivFallbackLimits(m, get, pct), indexHedgeLimits(m, get)...)...)
}

// indexHedgeLimits lists [mandate.index_hedge] once the table is written.
func indexHedgeLimits(m *ConstitutionMandate, get func(key, value, source, meaning, enforcement string) ConstitutionLimit) []ConstitutionLimit {
	if m.IndexHedge == nil {
		return nil
	}
	val, src := "unapproved", "unapproved"
	if v := m.IndexHedge.MaxContracts; v != nil {
		val, src = strconv.Itoa(*v), "file"
	}
	return []ConstitutionLimit{get("mandate.index_hedge.max_contracts", val, src,
		"The most Micro E-mini S&P 500 contracts one Reduce hedge may hold after it is placed: a hard stop against a wrong size, never the target (the hedge is sized to neutral). The table's presence is the permission to place a hedge; without it, or without this key, a hedge is refused. Changes only by a policy revision.", "hard")}
}

// ivFallbackLimits lists [mandate.iv_fallback] once the table is written.
func ivFallbackLimits(m *ConstitutionMandate, get func(key, value, source, meaning, enforcement string) ConstitutionLimit, pct func(v *float64) (string, string)) []ConstitutionLimit {
	if m.IVFallback == nil {
		return nil
	}
	f := m.IVFallback
	band := "The share an option's fallback volatility (implied from its own mark when the position row carries none) is moved by, down and up, to bound the S&P-fall figure, for options "
	tier := " of the loss room the S&P-fall uncertainty from fallback volatilities may take"
	row := func(key string, v *float64, meaning string) ConstitutionLimit {
		val, src := pct(v)
		return get("mandate.iv_fallback."+key, val, src, meaning+" Changes only by a policy revision.", "advisory")
	}
	return []ConstitutionLimit{
		row("short_band_pct", f.ShortBandPct, band+"under 8 days to expiry."),
		row("month_band_pct", f.MonthBandPct, band+"8 to 35 days to expiry."),
		row("long_band_pct", f.LongBandPct, band+"beyond 35 days to expiry."),
		row("measured_pct", f.MeasuredPct, "The share"+tier+" while the verdict still reads measured."),
		row("range_pct", f.RangePct, "The share"+tier+" while the verdict reads measured only when it is the same at both edges; beyond it the verdict is unknown."),
	}
}
