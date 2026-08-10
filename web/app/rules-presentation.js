function ruleStatusLabel(status, reason = "") {
  if (status === "not_evaluated") {
    if (reason === "rule_off") return "off";
    if (reason === "no_index_protection") return "no protection position";
    if (reason === "broker_nonissuer") return "broker nonissuer";
    if (reason === "terminal_non_reporting") return "terminal/non-reporting";
    if (reason === "earnings_not_applicable") return "issuer earnings not applicable";
    return "not evaluated";
  }
  return status || "--";
}

function rulesCountSummary(rules = {}) {
  const rows = Array.isArray(rules.rules) ? rules.rules : [];
  const bits = [];
  for (const [status, label] of [["act", "act"], ["watch", "watch"], ["unknown", "data note"]]) {
    const value = rows.filter((row) => (row?.mode || "alert") === "alert" && row?.status === status).length;
    if (value) bits.push(`${value} ${label}`);
  }
  const tracked = rows.filter((row) => row?.mode === "track" && !["pass", "not_evaluated"].includes(row?.status)).length;
  if (tracked) bits.push(`${tracked} tracked`);
  return bits.length ? bits.join(" · ") : "no alerts";
}

function earningsApplicabilitySummary(rules = {}) {
  const earnings = Array.isArray(rules.earnings) ? rules.earnings : [];
  const broker = earnings.filter((item) => item?.source === "broker_identity" && item?.status === "not_applicable").length;
  const terminal = earnings.filter((item) => item?.source === "verified_terminal" && item?.status === "terminal_non_reporting").length;
  const parts = [];
  if (broker) parts.push(`${broker} broker-proven nonissuer`);
  if (terminal) parts.push(`${terminal} terminal/non-reporting`);
  return parts.length ? `Issuer earnings not applicable: ${parts.join(" · ")}.` : "";
}

function earningsHealthNotes(rules = {}) {
  const health = (rules.input_health || []).find((item) => item?.source === "earnings" && item?.status === "ok");
  if (!health || !Array.isArray(health.notes) || health.notes.length === 0) return "";
	return `Earnings source note: ${health.notes.join("; ")}.`;
}

function wshEntitlementNotice(rules = {}) {
  const earnings = Array.isArray(rules.earnings) ? rules.earnings : [];
  const unavailable = earnings.some((entry) => (entry?.providers || []).some((provider) => {
    const failure = provider?.last_failure;
    return provider?.provider === "ibkr_wsh" && failure?.code === "not_entitled" &&
      (failure?.stage === "wsh_metadata" || failure?.stage === "wsh_event") && failure?.retryable === false;
  }));
  return unavailable
    ? "The optional Wall Street Horizon earnings feed is unavailable because this account lacks the WSH research subscription. Nasdaq remains active; names without a usable date stay unknown, never pass."
    : "";
}

export { earningsApplicabilitySummary, earningsHealthNotes, ruleStatusLabel, rulesCountSummary, wshEntitlementNotice };
