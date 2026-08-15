package risk

import (
	"fmt"
	"strconv"
	"time"
)

// ConstitutionLimit is one row of the generated human view (`canary policy
// show --explain`): a limit key, its effective value, what it means in
// plain English, where the value came from, and its enforcement class.
// Observation and override columns are overlaid by the daemon; this layer
// is pure policy so every surface renders identical meanings.
type ConstitutionLimit struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Meaning     string `json:"meaning"`
	Source      string `json:"source"`      // file | default | unapproved
	Enforcement string `json:"enforcement"` // advisory | shadow | structural
}

// ConstitutionLimits renders every material limit and governance field of
// the constitution. A nil policy (no file) yields the same rows with every
// value unapproved, so the view never hides what is missing.
func ConstitutionLimits(c *Constitution) []ConstitutionLimit {
	get := func(key, value, source, meaning, enforcement string) ConstitutionLimit {
		return ConstitutionLimit{Key: key, Value: value, Meaning: meaning, Source: source, Enforcement: enforcement}
	}
	str := func(present bool, render func() string) (string, string) {
		if !present {
			return "unapproved", "unapproved"
		}
		return render(), "file"
	}

	var (
		cur      = ""
		floor    *float64
		declared *float64
		eqAge    *int
		recAge   *int
		warn     *float64
		block    *float64
		enfc     = EnforcementShadow
		enfSrc   = "unapproved"
		ovh      *int
		rTolP    *float64
		rTolM    *float64
		rDateW   *int
		rAge     *int
		rEqDiv   *float64
	)
	if c != nil {
		cur = c.Capital.BaseCurrency
		floor = c.Capital.ProtectedFloor
		declared = c.Capital.DeclaredRiskCapital
		eqAge = c.Capital.MaxEquityAgeMinutes
		recAge = c.Capital.MaxUnreconciledDays
		warn = c.Drawdown.WarnConsumedPct
		block = c.Drawdown.BlockConsumedPct
		enfc = c.EffectiveBlockEnforcement()
		if c.Drawdown.BlockEnforcement == "" {
			enfSrc = "default"
		} else {
			enfSrc = "file"
		}
		ovh = c.Override.MaxDurationHours
		rTolP = c.Recon.AmountTolerancePct
		rTolM = c.Recon.AmountToleranceMin
		rDateW = c.Recon.DateWindowBusinessDays
		rAge = c.Recon.MaxReportAgeDays
		rEqDiv = c.Recon.MaxEquityDivergencePct
	}

	money := func(v *float64) (string, string) {
		return str(v != nil, func() string { return fmt.Sprintf("%.2f %s", *v, nonEmpty(cur, "base")) })
	}
	pct := func(v *float64) (string, string) {
		return str(v != nil, func() string { return strconv.FormatFloat(*v, 'f', -1, 64) + "%" })
	}
	num := func(v *int, unit string) (string, string) {
		return str(v != nil, func() string { return strconv.Itoa(*v) + " " + unit })
	}

	curVal, curSrc := str(cur != "", func() string { return cur })
	floorVal, floorSrc := money(floor)
	declVal, declSrc := money(declared)
	eqVal, eqSrc := num(eqAge, "minutes")
	recVal, recSrc := num(recAge, "days")
	warnVal, warnSrc := pct(warn)
	blockVal, blockSrc := pct(block)
	ovhVal, ovhSrc := num(ovh, "hours")

	reconcileMeaning := "How long the declared capital-event ledger may go without a human reconcile attestation before the state counts as unreconciled. Same posture as equity staleness."
	if c != nil && c.PolicyVersion >= 3 {
		reconcileMeaning = "How many days may pass without reconcile evidence — either an automatic clean-report extension or a human sign-off — before the state counts as unreconciled. Same posture as equity staleness."
	}
	rows := []ConstitutionLimit{
		get("capital.base_currency", curVal, curSrc,
			"Currency every capital number in this policy is stated in; must match the account base currency.", "structural"),
		get("capital.protected_floor", floorVal, floorSrc,
			"Account equity that is never risk capital. A policy boundary inside the account, not a broker segregation: it bounds authorized risk, it cannot stop a gap.", "advisory"),
		get("capital.declared_risk_capital", declVal, declSrc,
			"The money you have deliberately authorized to be at risk. Effective risk capital = min(this, equity − protected floor). Deposits and profits never raise it; only a fingerprinted policy revision does.", "advisory"),
		get("capital.max_equity_age_minutes", eqVal, eqSrc,
			"How old the last equity observation may be before the capital state counts as stale. Stale never passes; once the block control is promoted to hard, stale fails closed for risk increases.", "advisory"),
		get("capital.max_unreconciled_days", recVal, recSrc,
			reconcileMeaning, "advisory"),
		get("drawdown.warn_consumed_pct", warnVal, warnSrc,
			"Advisory tier: when losses from the cash-flow-adjusted peak consume this share of declared risk capital, surfaces warn and risk-increasing previews carry an advisory cause. Self-clearing on recovery.", "advisory"),
		get("drawdown.block_consumed_pct", blockVal, blockSrc,
			"Block tier: at this consumed share the breach latches in daemon state. Risk-increasing orders are the target; reductions, closes, cancels, and policy-classified hedges stay exempt. Clears only by journaled human reset that re-bases the peak.", enfc),
		get("drawdown.block_enforcement", enfc, enfSrc,
			"Enforcement class of the block tier. v1 accepts shadow (journal what would block) or advisory (warn loudly); promotion to hard is a later human policy revision after the shadow period.", "structural"),
		get("override.max_duration_hours", ovhVal, ovhSrc,
			"Longest lifetime of a one-shot override. Overrides are human-only, name one control, require a reason, are journaled with the policy fingerprint, and expire on their own.", "advisory"),
	}
	rTolPVal, rTolPSrc := pct(rTolP)
	rTolMVal, rTolMSrc := money(rTolM)
	rDateWVal, rDateWSrc := num(rDateW, "business days")
	rAgeVal, rAgeSrc := num(rAge, "days")
	rows = append(rows,
		get("recon.amount_tolerance_pct", rTolPVal, rTolPSrc,
			"How far a statement flow and a declared event may differ in amount and still match, as a share of the statement amount. Differences beyond max(this, the minimum below) become exceptions.", "advisory"),
		get("recon.amount_tolerance_min", rTolMVal, rTolMSrc,
			"Absolute floor of the amount tolerance, so small flows are not held to sub-cent precision while FX conversion noise stays inside the match.", "advisory"),
		get("recon.date_window_business_days", rDateWVal, rDateWSrc,
			"How many weekdays the statement value date and the declared effective date may sit apart before the pair is a date exception. Guards the late-deposit peak correction against wrong dates.", "advisory"),
		get("recon.max_report_age_days", rAgeVal, rAgeSrc,
			"How old the newest ingested statement may be for a recon report to back a reconcile sign-off. Older data means the sign-off would attest to a week nobody has seen.", "advisory"),
	)
	if c == nil || c.PolicyVersion >= 3 {
		rEqDivVal, rEqDivSrc := pct(rEqDiv)
		rows = append(rows, get("recon.max_equity_divergence_pct", rEqDivVal, rEqDivSrc,
			"Largest absolute same-day difference allowed between broker statement equity and the runtime observation before a clean report may extend the reconcile clock automatically.", "advisory"))
	}
	for _, a := range []struct {
		key   string
		class string
	}{
		{"cadence.morning", artefactClass(c, func(cc *Constitution) string { return cc.Cadence.Morning.Class })},
		{"cadence.eod", artefactClass(c, func(cc *Constitution) string { return cc.Cadence.EOD.Class })},
		{"cadence.weekly", artefactClass(c, func(cc *Constitution) string { return cc.Cadence.Weekly.Class })},
	} {
		val, src := "undeclared", "unapproved"
		if a.class != "" {
			val, src = a.class, "file"
		}
		rows = append(rows, get(a.key+".class", val, src,
			"Legacy v2 cadence declaration retained for policy-file compatibility. Canary v3 has no manual completion or acknowledgement surface.", "advisory"))
	}
	if c == nil || c.PolicyVersion >= 4 {
		var timezone, monthlyClass, monthlyTime *string
		var warningDays, monthlyDay *int
		if c != nil {
			if c.Cadence.Nudges != nil {
				timezone = c.Cadence.Nudges.Timezone
				warningDays = c.Cadence.Nudges.ReconcileWarningDays
			}
			if c.Cadence.Monthly != nil {
				monthlyClass = c.Cadence.Monthly.Class
				monthlyDay = c.Cadence.Monthly.DayOfMonth
				monthlyTime = c.Cadence.Monthly.NudgeAtLocal
			}
		}
		stringValue := func(value *string) (string, string) {
			return str(value != nil, func() string { return *value })
		}
		timezoneValue, timezoneSource := stringValue(timezone)
		if timezone == nil {
			timezoneValue, timezoneSource = time.Local.String(), "machine"
		}
		warningValue, warningSource := num(warningDays, "days")
		if warningDays == nil {
			warningValue, warningSource = fmt.Sprintf("%d days", DefaultReconcileWarningDays), "default"
		}
		classValue, classSource := stringValue(monthlyClass)
		if monthlyClass == nil {
			classValue, classSource = EnforcementAdvisory, "default"
		}
		dayValue, daySource := num(monthlyDay, "working day of month")
		if monthlyDay == nil {
			dayValue, daySource = fmt.Sprintf("%d working day of month", DefaultMonthlyPulseWorkingDay), "default"
		}
		timeValue, timeSource := stringValue(monthlyTime)
		if monthlyTime == nil {
			timeValue, timeSource = DefaultMonthlyPulseAtLocal, "default"
		}
		signoffValue, signoffSource := "false", "default"
		if c != nil && c.Inventory.RequireSignoff != nil {
			signoffValue, signoffSource = strconv.FormatBool(*c.Inventory.RequireSignoff), "file"
		}
		rows = append(rows,
			get("inventory.require_signoff", signoffValue, signoffSource,
				"Whether a sibling-policy version change (rulebook, protection, stress) blocks governance evidence until the [inventory] pin is updated. Off: pins are informational; a changed sibling is disclosed but neither flags attention nor blocks the monthly pulse. Regulated desks set true to require the explicit pin sign-off.", "advisory"),
			get("cadence.nudges.timezone", timezoneValue, timezoneSource,
				"Clock cadence months and scheduled nudge instants are derived in. The machine's local timezone unless the file authors an override.", "structural"),
			get("cadence.nudges.reconcile_warning_days", warningValue, warningSource,
				"Rolling 24-hour days before the reconciliation deadline when the due-soon advisory begins. Overdue begins only after the deadline. Defaults in code; the file key overrides.", "advisory"),
			get("cadence.monthly.class", classValue, classSource,
				"Enforcement class of the monthly pulse. Policy version 4 accepts advisory only.", "structural"),
			get("cadence.monthly.day_of_month", dayValue, daySource,
				"Nth working day of the month (Monday through Friday, weeks starting Monday) when the monthly pulse becomes due; limited to [1, 20] so the day exists in every month.", "advisory"),
			get("cadence.monthly.nudge_at_local", timeValue, timeSource,
				"Local HH:MM when the monthly pulse becomes due.", "advisory"),
		)
	}
	return rows
}

func artefactClass(c *Constitution, pick func(*Constitution) string) string {
	if c == nil {
		return ""
	}
	return pick(c)
}
