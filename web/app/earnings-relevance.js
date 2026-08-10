function earningsLabel(value) {
  const words = String(value || "unknown").replaceAll("_", " ").trim().split(/\s+/).filter(Boolean);
  return words.map((word) => word.charAt(0).toUpperCase() + word.slice(1)).join(" ") || "Unknown";
}

function earningsSymbol(value) {
  return String(value || "").trim().toUpperCase();
}

// Cross-link live held-name catalyst evidence to the rules that govern it.
// Display-only: the daemon's typed status and rule verdict stay authoritative.
function unknownEventRuleNote(rules = {}) {
  const governing = { catalyst_coverage: true, earnings_size_freeze: true, overwrite_earnings: true };
  const unknownRules = (rules.rules || [])
    .filter((rule) => governing[rule.id] && rule.status === "unknown" && (rule.mode || "alert") === "alert")
    .map((rule) => (rule.number ? `rule ${rule.number} (${rule.title || earningsLabel(rule.id)})` : earningsLabel(rule.id)));
  if (unknownRules.length === 0) return "";

  const upcoming = (rules.earnings || [])
    .filter((entry) => entry.date && entry.source !== "unknown")
    .sort((a, b) => String(a.date).localeCompare(String(b.date)))
    .map((entry) => `${earningsSymbol(entry.symbol)} ${entry.date}`);
  const unresolved = (rules.earnings || [])
    .filter((entry) => entry.status !== "not_applicable" && entry.status !== "terminal_non_reporting")
    .filter((entry) => !entry.date || entry.source === "unknown" || (entry.status && entry.status !== "date"))
    .map((entry) => `${earningsSymbol(entry.symbol)} (${earningsLabel(entry.reason || entry.status)})`);
  if (unresolved.length > 0) {
    const knownContext = upcoming.length > 0 ? `; other dates ahead: ${upcoming.join(" · ")}` : "";
    return `Earnings dates need attention: ${unresolved.join(" · ")}${knownContext}. ${unknownRules.join(" and ")} ${unknownRules.length === 1 ? "is" : "are"} waiting for those dates.`;
  }
  if (upcoming.length === 0) return "";
  return `Earnings ahead: ${upcoming.join(" · ")}. ${unknownRules.join(" and ")} ${unknownRules.length === 1 ? "is" : "are"} waiting for a complete date.`;
}

export { earningsLabel, unknownEventRuleNote };
