import { state } from "./state.js";
import { calendarDate, calendarDateTime } from "./shared.js";

const $ = (id) => globalThis.document?.getElementById(id) || null;

const ALERT_SCHEMA = "alerts-v1";
const ALERT_VERSION = "alert-delivery-v4";
const SOURCES = new Set([
  "canary", "regime", "rulebook", "risk_policy", "protection", "order_integrity",
  "reconciliation", "governance", "data_health", "delivery",
]);
const SOURCE_LABELS = {
  canary: "Stress",
  regime: "Regime",
  rulebook: "Rulebook",
  risk_policy: "Risk policy",
  protection: "Protection",
  order_integrity: "Order integrity",
  reconciliation: "Reconciliation",
  governance: "Governance",
  data_health: "Data health",
  delivery: "Delivery",
};
const KINDS = new Set([
  "market_state", "portfolio_risk", "margin_safety", "drawdown", "protection_gap",
  "order_integrity", "reconciliation_exception", "governance", "policy_drift",
  "data_health", "delivery_health",
]);
const EPISODE_STATES = new Set(["open", "escalated"]);
const SEVERITIES = new Set(["observe", "watch", "act", "urgent"]);
const EVIDENCE_HEALTH = new Set(["current", "partial", "stale", "unavailable", "error"]);
const DESTINATIONS = new Set(["monitor", "alerts", "brief"]);
const COVERAGE_STATES = new Set(["complete", "partial", "unavailable"]);
const COVERAGE_FRESHNESS = new Set(["current", "stale", "unknown"]);
const CURRENT_STATES = new Set(["clear", "active", "unknown"]);
const DELIVERY_STATES = new Set(["healthy", "degraded", "unavailable", "overflow"]);
const DELIVERY_CLASSES = new Set([
  "", "retry_pending", "transport_rejected", "interrupted_uncertain", "state_write_failure",
  "capacity_overflow", "no_active_subscription", "signing_keys_unavailable", "sender_unavailable",
  "invalid_persisted_state", "retry_exhausted", "not_initialized", "producer_observation_rejected",
]);
const TOP_KEYS = [
  "schema_version", "version", "initialized", "generation", "as_of", "current_state",
  "coverage", "sources", "occurrences", "attention", "delivery_health",
];
const COVERAGE_KEYS = ["state", "freshness", "as_of", "expected_sources", "covered_sources"];
const SOURCE_KEYS = [
  "source", "status", "reason", "evidence_health", "input_as_of", "observed_at",
  "evidence_as_of", "fresh_until", "covered",
];
const OCCURRENCE_KEYS = [
  "display_id", "source", "kind", "presentation_code", "title", "body", "state", "severity",
  "evidence_health", "destination", "evidence_as_of", "state_changed_at", "first_seen_at",
  "last_seen_at", "ended_at", "end_reason", "attention_seq", "disposition",
];
const ATTENTION_KEYS = ["unread_count", "high_water_seq", "read_through_seq", "unread_refs"];
const ATTENTION_REF_KEYS = ["display_id", "source", "kind"];
const DELIVERY_KEYS = ["state", "class", "updated_at", "last_push_service_acceptance_at"];
const DISPLAY_ID = /^alert-(?:previous-)?[a-z0-9][a-z0-9-]{1,126}$/;
const CODE = /^[a-z0-9][a-z0-9_]{0,127}$/;
const RFC3339_UTC = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,9}))?Z$/;

class AlertContractError extends Error {
  constructor(message) {
    super(message);
    this.name = "AlertContractError";
  }
}

function fail(path, message) {
  throw new AlertContractError(`${path} ${message}`);
}

function exactObject(value, keys, path) {
  if (!value || typeof value !== "object" || Array.isArray(value)) fail(path, "must be an object");
  const actual = Object.keys(value).sort();
  const expected = [...keys].sort();
  if (actual.length !== expected.length || actual.some((key, index) => key !== expected[index])) {
    fail(path, "has unexpected or missing keys");
  }
  return value;
}

function arrayValue(value, path) {
  if (!Array.isArray(value)) fail(path, "must be an array");
  return value;
}

function unsigned(value, path, positive = false) {
  if (!Number.isSafeInteger(value) || value < (positive ? 1 : 0)) fail(path, "must be a safe unsigned integer");
  return value;
}

function enumValue(value, allowed, path) {
  if (typeof value !== "string" || !allowed.has(value)) fail(path, "has an invalid value");
  return value;
}

function codeValue(value, path, { empty = false } = {}) {
  if (typeof value !== "string" || (!empty && !CODE.test(value)) || (empty && value !== "" && !CODE.test(value))) {
    fail(path, "must be a safe code");
  }
  return value;
}

function textValue(value, path) {
  if (typeof value !== "string" || value.length === 0 || value.length > 500) fail(path, "must be bounded text");
  return value;
}

function timestamp(value, path, nullable = false) {
  if (nullable && value === null) return null;
  if (typeof value !== "string") fail(path, "must be an RFC3339 UTC timestamp");
  const match = RFC3339_UTC.exec(value);
  if (!match || !Number.isFinite(Date.parse(value))) fail(path, "must be an RFC3339 UTC timestamp");
  const [, y, m, d, h, min, s] = match.map((part, index) => index === 0 ? part : Number(part));
  const leap = y % 4 === 0 && (y % 100 !== 0 || y % 400 === 0);
  const days = [31, leap ? 29 : 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31];
  if (m < 1 || m > 12 || d < 1 || d > days[m - 1] || h > 23 || min > 59 || s > 59) {
    fail(path, "must be a real RFC3339 UTC timestamp");
  }
  return value;
}

function uniqueSources(values, path, nonempty = false) {
  arrayValue(values, path);
  if (nonempty && values.length === 0) fail(path, "must not be empty");
  const seen = new Set();
  values.forEach((value, index) => {
    enumValue(value, SOURCES, `${path}[${index}]`);
    if (seen.has(value)) fail(path, "must contain unique sources");
    seen.add(value);
  });
  return seen;
}

function validateAttention(value, occurrences = null) {
  exactObject(value, ATTENTION_KEYS, "attention");
  unsigned(value.unread_count, "attention.unread_count");
  unsigned(value.high_water_seq, "attention.high_water_seq");
  unsigned(value.read_through_seq, "attention.read_through_seq");
  if (value.read_through_seq > value.high_water_seq) fail("attention", "read cursor exceeds high water");
  arrayValue(value.unread_refs, "attention.unread_refs");
  if (value.unread_count !== value.unread_refs.length) fail("attention", "unread count does not match references");
  const byDisplay = occurrences ? new Map(occurrences.map((item) => [item.display_id, item])) : null;
  const seen = new Set();
  value.unread_refs.forEach((ref, index) => {
    const path = `attention.unread_refs[${index}]`;
    exactObject(ref, ATTENTION_REF_KEYS, path);
    if (typeof ref.display_id !== "string" || !DISPLAY_ID.test(ref.display_id)) fail(`${path}.display_id`, "is invalid");
    enumValue(ref.source, SOURCES, `${path}.source`);
    enumValue(ref.kind, KINDS, `${path}.kind`);
    if (seen.has(ref.display_id)) fail(path, "duplicates a display id");
    seen.add(ref.display_id);
    if (byDisplay) {
      const occurrence = byDisplay.get(ref.display_id);
      if (occurrence && (occurrence.source !== ref.source || occurrence.kind !== ref.kind ||
          occurrence.attention_seq <= value.read_through_seq || occurrence.attention_seq > value.high_water_seq)) {
        fail(path, "does not match a retained unread occurrence");
      }
    }
  });
  return value;
}

function validateCoverage(value, asOf) {
  exactObject(value, COVERAGE_KEYS, "coverage");
  enumValue(value.state, COVERAGE_STATES, "coverage.state");
  enumValue(value.freshness, COVERAGE_FRESHNESS, "coverage.freshness");
  timestamp(value.as_of, "coverage.as_of");
  if (Date.parse(value.as_of) > Date.parse(asOf)) fail("coverage.as_of", "must not be after as_of");
  const expected = uniqueSources(value.expected_sources, "coverage.expected_sources", true);
  const covered = uniqueSources(value.covered_sources, "coverage.covered_sources");
  for (const source of covered) if (!expected.has(source)) fail("coverage.covered_sources", "must be expected");
  if (value.state === "complete" && covered.size !== expected.size) fail("coverage", "complete coverage must include every source");
  if (value.state === "partial" && (covered.size === 0 || covered.size >= expected.size)) fail("coverage", "partial coverage must be a proper subset");
  if (value.state === "unavailable" && covered.size !== 0) fail("coverage", "unavailable coverage must be empty");
  return { expected, covered };
}

function validateSource(value, index, asOf) {
  const path = `sources[${index}]`;
  exactObject(value, SOURCE_KEYS, path);
  enumValue(value.source, SOURCES, `${path}.source`);
  codeValue(value.status, `${path}.status`);
  codeValue(value.reason, `${path}.reason`, { empty: true });
  enumValue(value.evidence_health, EVIDENCE_HEALTH, `${path}.evidence_health`);
  for (const key of ["input_as_of", "observed_at", "evidence_as_of", "fresh_until"]) timestamp(value[key], `${path}.${key}`, true);
  if (typeof value.covered !== "boolean") fail(`${path}.covered`, "must be boolean");
  for (const key of ["input_as_of", "observed_at", "evidence_as_of"]) {
    if (value[key] && Date.parse(value[key]) > Date.parse(asOf)) fail(`${path}.${key}`, "must not be after as_of");
  }
}

function validateOccurrence(value, index, asOf) {
  const path = `occurrences[${index}]`;
  exactObject(value, OCCURRENCE_KEYS, path);
  if (typeof value.display_id !== "string" || !DISPLAY_ID.test(value.display_id)) fail(`${path}.display_id`, "is invalid");
  enumValue(value.source, SOURCES, `${path}.source`);
  enumValue(value.kind, KINDS, `${path}.kind`);
  codeValue(value.presentation_code, `${path}.presentation_code`);
  textValue(value.title, `${path}.title`);
  textValue(value.body, `${path}.body`);
  enumValue(value.state, EPISODE_STATES, `${path}.state`);
  enumValue(value.severity, SEVERITIES, `${path}.severity`);
  enumValue(value.evidence_health, EVIDENCE_HEALTH, `${path}.evidence_health`);
  enumValue(value.destination, DESTINATIONS, `${path}.destination`);
  for (const key of ["evidence_as_of", "state_changed_at", "first_seen_at", "last_seen_at"]) timestamp(value[key], `${path}.${key}`);
  if (Date.parse(value.first_seen_at) > Date.parse(value.last_seen_at) || Date.parse(value.last_seen_at) > Date.parse(asOf)) {
    fail(path, "has incoherent lifecycle times");
  }
  timestamp(value.ended_at, `${path}.ended_at`, true);
  if (value.ended_at !== null || value.end_reason !== null) fail(path, "must be an active alert");
  unsigned(value.attention_seq, `${path}.attention_seq`, true);
  codeValue(value.disposition, `${path}.disposition`);
}

function validateDeliveryHealth(value, initialized) {
  exactObject(value, DELIVERY_KEYS, "delivery_health");
  enumValue(value.state, DELIVERY_STATES, "delivery_health.state");
  enumValue(value.class, DELIVERY_CLASSES, "delivery_health.class");
  timestamp(value.updated_at, "delivery_health.updated_at", true);
  timestamp(value.last_push_service_acceptance_at, "delivery_health.last_push_service_acceptance_at", true);
  if (value.state === "healthy" && value.class !== "") fail("delivery_health", "healthy state must have an empty class");
  if (value.state === "overflow" && value.class !== "capacity_overflow") fail("delivery_health", "overflow has an invalid class");
  if (!initialized && !["not_initialized", "invalid_persisted_state", "state_write_failure"].includes(value.class)) {
    fail("delivery_health", "must explain why alerts are unavailable");
  }
}

function validateAlerts(value) {
  exactObject(value, TOP_KEYS, "alerts");
  if (value.schema_version !== ALERT_SCHEMA) fail("schema_version", "is unsupported");
  if (typeof value.version !== "string") fail("version", "must be a string");
  if (typeof value.initialized !== "boolean") fail("initialized", "must be boolean");
  unsigned(value.generation, "generation");
  arrayValue(value.sources, "sources");
  arrayValue(value.occurrences, "occurrences");
  validateDeliveryHealth(value.delivery_health, value.initialized);
  if (!value.initialized) {
    validateAttention(value.attention);
    if (value.as_of !== null || value.current_state !== null || value.coverage !== null || value.sources.length !== 0 || value.occurrences.length !== 0 ||
        value.attention.unread_count !== 0 || value.attention.high_water_seq !== 0 || value.attention.read_through_seq !== 0) {
      fail("alerts", "contains authority data while unavailable");
    }
    return value;
  }
  if (value.version !== ALERT_VERSION) fail("version", "is unsupported");
  timestamp(value.as_of, "as_of");
  enumValue(value.current_state, CURRENT_STATES, "current_state");
  const coverage = validateCoverage(value.coverage, value.as_of);
  const sourceIDs = new Set();
  value.sources.forEach((source, index) => {
    validateSource(source, index, value.as_of);
    if (sourceIDs.has(source.source) || !coverage.expected.has(source.source)) fail(`sources[${index}].source`, "is duplicate or unexpected");
    sourceIDs.add(source.source);
  });
  if (sourceIDs.size !== coverage.expected.size) fail("sources", "must contain every expected source exactly once");
  const displayIDs = new Set();
  value.occurrences.forEach((occurrence, index) => {
    validateOccurrence(occurrence, index, value.as_of);
    if (displayIDs.has(occurrence.display_id)) fail("occurrences", "must have unique display ids");
    displayIDs.add(occurrence.display_id);
  });
  validateAttention(value.attention, value.occurrences);
  return value;
}

function clone(value) {
  return JSON.parse(JSON.stringify(value));
}

function equalJSON(left, right) {
  return JSON.stringify(left) === JSON.stringify(right);
}

function freshnessOnlyAdvance(previous, next) {
  const old = clone(previous);
  const aged = clone(next);
  if (!old.initialized || !aged.initialized || old.generation !== aged.generation) return false;
  if (old.current_state === "clear" && aged.current_state === "unknown") aged.current_state = "clear";
  if (old.coverage?.freshness === "current" && aged.coverage?.freshness === "stale") aged.coverage.freshness = "current";
  const expired = new Set();
  for (const source of aged.sources || []) {
    const prior = old.sources.find((item) => item.source === source.source);
    if (prior && prior.evidence_health === "current" && source.evidence_health === "stale" &&
        source.status === "stale" && source.reason === "freshness_expired") {
      source.status = prior.status;
      source.reason = prior.reason;
      source.evidence_health = prior.evidence_health;
      expired.add(source.source);
    }
  }
  for (const occurrence of aged.occurrences || []) {
    const prior = old.occurrences.find((item) => item.display_id === occurrence.display_id);
    if (prior && expired.has(occurrence.source) && prior.evidence_health === "current" && occurrence.evidence_health === "stale") {
      occurrence.evidence_health = "current";
    }
  }
  return equalJSON(old, aged);
}

function markInvalid(message) {
  state.alertsFeedValid = false;
  state.alertsFeedError = message;
}

function ingestAlerts(value) {
  const current = state.alerts;
  const generation = value?.generation;
  if (current && Number.isSafeInteger(generation)) {
    if (generation < current.generation) return { status: "ignored", value: current };
    if (generation === current.generation && equalJSON(value, current)) return { status: "noop", value: current };
  }
  try {
    validateAlerts(value);
  } catch (error) {
    markInvalid(error instanceof Error ? error.message : "invalid alerts feed");
    return { status: "rejected", value: current };
  }
  if (current && generation === current.generation && !freshnessOnlyAdvance(current, value)) {
    markInvalid("equal-generation alerts equivocation");
    return { status: "rejected", value: current };
  }
  const accepted = clone(value);
  state.alerts = accepted;
  state.alertsFeedValid = true;
  state.alertsFeedError = "";
  return { status: "applied", value: accepted };
}

function ingestAlertsEvent(raw) {
  try {
    const result = ingestAlerts(JSON.parse(raw));
    // An accepted push is the same evidence the recovery GET provides, so it
    // must retire the failure copy too. Without this the line outlives the
    // outage that set it: the feed reconnects and keeps delivering while the
    // panel still reads "retained state shown", and the only other clears —
    if (result.status !== "rejected" && state.attentionStatus.state === ALERTS_REFRESH_FAILED_COPY) {
      setAttentionStatus("");
    }
    return result;
  } catch {
    markInvalid("malformed alerts event");
    return { status: "rejected", value: state.alerts };
  }
}

function canAssertAlertClear(value = state.alerts, now = Date.now()) {
  if (!value || state.alertsFeedValid === false) return false;
  try { validateAlerts(value); } catch { return false; }
  if (!value.initialized || value.current_state !== "clear" || value.coverage.state !== "complete" || value.coverage.freshness !== "current") return false;
  const expected = new Set(value.coverage.expected_sources);
  const covered = new Set(value.coverage.covered_sources);
  if (expected.size === 0 || expected.size !== value.coverage.expected_sources.length || covered.size !== expected.size || value.sources.length !== expected.size) return false;
  for (const source of expected) if (!covered.has(source)) return false;
  const seen = new Set();
  for (const source of value.sources) {
    if (!expected.has(source.source) || seen.has(source.source) || !source.covered || source.evidence_health !== "current" ||
        !source.fresh_until || now > Date.parse(source.fresh_until)) return false;
    seen.add(source.source);
  }
  return seen.size === expected.size;
}

function timeLabel(value) {
  if (!value) return "not observed";
  const label = calendarDateTime(value, { timeZoneName: "short" });
  return label || "not observed";
}

function clockLabel(value) {
  if (!value) return "not observed";
  return calendarDateTime(value) || "not observed";
}

function alertSourceLabel(source) {
  return SOURCE_LABELS[source] || "Unknown source";
}

// The engraved source placard is the daemon's own vocabulary and nothing
// else: the display label for the served source, plus the served kind where
// the kind says something the source does not. No invented geography, no
function alertPlacard(occurrence) {
  const source = alertSourceLabel(occurrence.source);
  const kind = String(occurrence.kind || "").replaceAll("_", " ");
  return kind && kind.toLowerCase() !== source.toLowerCase() ? `${source} \u00b7 ${kind}` : source;
}

// The age line reads the served timestamps back as words. A lit annunciator
// the daemon's vocabulary only when they are not the nominal case, so a row
function alertAgeLine(occurrence) {
  const parts = [`Since ${clockLabel(occurrence.first_seen_at)}`];
  if (occurrence.presentation_code === "risk_policy_drawdown_latched") parts.push("latched");
  else if (occurrence.evidence_health !== "current") parts.push("retained");
  else parts.push(occurrence.severity);
  if (occurrence.evidence_health !== "current") parts.push(`evidence ${occurrence.evidence_health}`);
  return parts.filter(Boolean).join(" \u00b7 ");
}

function alertBodyCopy(occurrence) {
  return occurrence.body;
}

function setText(id, copy) {
  const element = $(id);
  if (element) element.textContent = copy;
}

function emptyRow(copy) {
  const row = document.createElement("div");
  row.className = "empty-row";
  row.textContent = copy;
  return row;
}

// One annunciator tile. Lit rows carry the lamp bar and the severity wash;
const SEVERITY_TINT = { urgent: "pd-tile--act", act: "pd-tile--act", watch: "pd-tile--watch" };

const RULE_ALERT_TARGETS = {
  rulebook_single_name_exposure: "single_name_exposure",
  rulebook_option_line_premium: "option_line_premium",
  rulebook_cash_sell_only: "cash_sell_only",
  rulebook_extrinsic_budget: "extrinsic_budget",
  rulebook_expiry_runway: "expiry_runway",
  rulebook_catalyst_coverage: "catalyst_coverage",
  rulebook_overwrite_earnings: "overwrite_earnings",
  rulebook_earnings_size_freeze: "earnings_size_freeze",
  rulebook_red_on_green: "red_on_green",
  rulebook_winner_trim: "winner_trim",
  rulebook_green_day_action: "green_day_action",
  rulebook_hedge_integrity: "hedge_integrity",
  rulebook_exit_discipline: "exit_discipline",
  rulebook_fx_exposure: "fx_exposure",
};

function alertEvidenceTarget(occurrence = {}) {
  const code = String(occurrence.presentation_code || "");
  if (RULE_ALERT_TARGETS[code]) return { kind: "rule", id: RULE_ALERT_TARGETS[code] };
  if (["regime_market_stress", "data_health_regime", "data_health_gamma"].includes(code)) return { kind: "regime" };
  if (code === "portfolio_stress" || code === "margin_cushion") return { kind: "stress" };
  if (code === "risk_policy_drawdown_latched" || code === "risk_policy_limit_would_block") return { kind: "brief" };
  if (code.startsWith("protection_") || code === "data_health_proposals" || code === "data_health_opportunities") return { kind: "protection" };
  if (code === "order_integrity_mismatch") return { kind: "orders" };
  if (code.startsWith("reconciliation_") || code.startsWith("governance_")) return { kind: "settings" };
  return { kind: occurrence.destination === "brief" ? "brief" : "monitor" };
}

function alertFactText(occurrence = {}, snapshot = state.snapshot || {}) {
  const target = alertEvidenceTarget(occurrence);
  if (target.kind === "rule") {
    const rule = (snapshot.rules?.rules || []).find((candidate) => String(candidate?.id || "") === target.id);
    return boundedFact(rule?.evidence || rule?.reason);
  }

  const stress = snapshot.stress || {};
  if (occurrence.presentation_code === "risk_policy_drawdown_latched") {
    const capital = snapshot.brief?.ready?.capital || {};
    const latch = snapshot.brief?.ready?.latch || {};
    const parts = [];
    if (Number.isFinite(capital.consumed_pct)) parts.push(`Current use ${formatAlertPercent(capital.consumed_pct)} of drawdown budget`);
    if (Number.isFinite(latch.consumed_pct_at_latch)) parts.push(`latched at ${formatAlertPercent(latch.consumed_pct_at_latch)}`);
    if (latch.report_coverage_to) parts.push(`broker report through ${formatAlertDate(latch.report_coverage_to)}`);
    if (latch.report_checked_at) parts.push(`checked ${formatAlertDateTime(latch.report_checked_at)}`);
    return boundedFact(parts.join(" · "));
  }
  if (occurrence.presentation_code === "regime_market_stress") {
    const indicators = stress.market_indicators || [];
    const indicator = indicators.find((item) => item?.status === "red") || indicators.find((item) => item?.status === "amber");
    const confirmation = stress.market_confirmation === "confirmed" ? "Confirmed" : stress.market_confirmation === "partial" ? "Not confirmed" : "";
    const provisionalAt = String(indicator?.comment || "").toLowerCase().indexOf("provisional:");
    const provisional = provisionalAt >= 0 ? String(indicator.comment).slice(provisionalAt).split(";")[0].trim() : "";
    return boundedFact([
      indicator?.name && indicator?.reading ? `${indicator.name}: ${indicator.reading}` : "",
      provisional,
      confirmation,
    ].filter(Boolean).join(" · "));
  }
  if (occurrence.presentation_code === "portfolio_stress") {
    const driver = stress.primary_drivers?.[0];
    const portfolio = stress.portfolio || {};
    const driverFacts = {
      gross_delta_high: ["Gross delta", portfolio.gross_delta_pct_nlv],
      net_delta_high: ["Net delta", portfolio.net_delta_pct_nlv],
      gross_exposure_high: ["Gross exposure", portfolio.gross_exposure_pct_nlv],
      single_name_delta_high: [`${portfolio.largest_delta_exposure || "Largest underlying"} delta`, portfolio.largest_delta_pct_nlv],
      single_name_exposure_high: [portfolio.largest_exposure || "Largest underlying", portfolio.largest_exposure_pct_nlv],
    }[driver];
    if (driverFacts && Number.isFinite(driverFacts[1])) return `${driverFacts[0]} ${formatAlertPercent(driverFacts[1])} of NLV`;
    const row = (stress.rows || []).find((candidate) => ["urgent", "act", "watch"].includes(String(candidate?.severity || "")) && candidate?.evidence);
    return boundedFact(row?.evidence || stress.summary);
  }
  if (occurrence.presentation_code === "margin_cushion") {
    const cushion = stress.portfolio?.cushion_pct;
    const trip = stress.portfolio?.cushion_trip_pct;
    return Number.isFinite(cushion)
      ? `Margin cushion ${formatAlertPercent(cushion)}${Number.isFinite(trip) ? ` · Rulebook level ${formatAlertPercent(trip)}` : ""}`
      : "";
  }
  if (target.kind === "brief") return boundedFact(snapshot.brief?.ready?.capital?.reason || snapshot.brief?.ready?.latch?.reason);
  if (target.kind === "protection") {
    const counts = snapshot.proposals?.counts || {};
    const total = Object.values(counts).filter(Number.isFinite).reduce((sum, value) => sum + value, 0);
    if (total > 0) return `${total} current ${total === 1 ? "suggestion" : "suggestions"}`;
  }
  if (target.kind === "settings") {
    const reconcile = snapshot.brief?.review?.reconcile || {};
    const parts = [];
    if (reconcile.last_reconciled_at) parts.push(`Last reconciled ${formatAlertDate(reconcile.last_reconciled_at)}`);
    if (Number.isFinite(reconcile.days_remaining)) parts.push(`${reconcile.days_remaining} days remaining`);
    if (parts.length > 0) return parts.join(" · ");
  }
  if (String(occurrence.presentation_code || "").startsWith("data_health_")) {
    const surface = String(occurrence.presentation_code || "").replace("data_health_", "");
    const qualityRows = snapshot.status?.data_quality || [];
    const quality = qualityRows.find((item) => String(item?.surface || "").toLowerCase() === surface) || qualityRows.find((item) => String(item?.status || "").toLowerCase() !== "ok");
    if (quality) {
      const clusters = [
        ...(quality.partial_clusters || []),
        ...(quality.degraded_clusters || []),
        ...(quality.stale_clusters || []),
      ];
      const subject = clusters.length > 0 ? clusters.map(humanAlertWord).join(", ") : humanAlertWord(quality.surface || "Input");
      return boundedFact(`${subject} inputs ${humanAlertWord(quality.status || "need attention").toLowerCase()}${quality.as_of ? ` · as of ${formatAlertDateTime(quality.as_of)}` : ""}`);
    }
    const indicator = (stress.market_indicators || []).find((item) => ["n/a", "context"].includes(String(item?.status || "").toLowerCase()));
    if (indicator) {
      return boundedFact(`${indicator.name}: ${indicator.comment || indicator.reading || "input unavailable"}${indicator.as_of ? ` · as of ${indicator.as_of}` : ""}`);
    }
    const degraded = Object.entries(snapshot.sources || {}).find(([, source]) => source && !["current", "ok"].includes(String(source.state || source.status || "").toLowerCase()));
    if (degraded) {
      const [name, source] = degraded;
      return boundedFact(`${humanAlertWord(name)}: ${humanAlertWord(source.reason || source.state || source.status || "needs attention")}${source.as_of ? ` · ${formatAlertDateTime(source.as_of)}` : ""}`);
    }
  }
  return occurrence.evidence_as_of ? `Evidence checked ${formatAlertDateTime(occurrence.evidence_as_of)}` : "";
}

function alertAffectedPositions(occurrence = {}, snapshot = state.snapshot || {}) {
  const target = alertEvidenceTarget(occurrence);
  if (target.kind !== "rule") return { total: 0, labels: [] };
  const rule = (snapshot.rules?.rules || []).find((candidate) => String(candidate?.id || "") === target.id);
  const labels = (rule?.offenders || [])
    .map((offender) => boundedFact(offender?.leg || offender?.symbol).slice(0, 120))
    .filter(Boolean);
  return { total: labels.length, labels: labels.slice(0, 3) };
}

function alertActionCopy(occurrence = {}) {
  switch (alertEvidenceTarget(occurrence).kind) {
    case "rule": return "Review rule details";
    case "regime": return "Review market evidence";
    case "stress": return "Review portfolio stress";
    case "protection": return "Review protection";
    case "brief": return "Review daily brief";
    case "orders": return "Review open orders";
    case "settings": return "Review workflow";
    default: return "Review on Monitor";
  }
}

function boundedFact(value) {
  const clean = String(value || "").replace(/\s+/g, " ").trim();
  if (!clean) return "";
  return clean.length > 280 ? `${clean.slice(0, 277).trimEnd()}…` : clean;
}

function formatAlertPercent(value) {
  return `${Number(value).toFixed(1)}%`;
}

function formatAlertDate(value) {
  return calendarDate(value);
}

function formatAlertDateTime(value) {
  return calendarDateTime(value);
}

function humanAlertWord(value) {
  return String(value || "").replace(/_/g, " ").replace(/^./, (letter) => letter.toUpperCase());
}

function evidenceElement(target) {
  if (target.kind === "rule") {
    return [...(document.querySelectorAll?.("[data-rule-id]") || [])]
      .find((candidate) => candidate.dataset.ruleId === target.id) || null;
  }
  const ids = {
    brief: "briefPanel",
    orders: "ordersPanel",
    settings: "settingsTab",
    regime: "regimeDetailPanel",
    stress: "stressDetailPanel",
    protection: "protectionPanel",
    monitor: "signalPanel",
  };
  return $(ids[target.kind]);
}

function markAlertEvidenceTarget(target) {
  state.alertEvidenceTarget = target;
  const element = evidenceElement(target);
  if (!element) return null;
  for (const previous of document.querySelectorAll?.(".is-alert-evidence-target") || []) {
    previous.classList.remove("is-alert-evidence-target");
    previous.removeAttribute("aria-current");
  }
  element.classList.add("is-alert-evidence-target");
  element.setAttribute("aria-current", "location");
  element.focus?.({ preventScroll: true });
  element.scrollIntoView?.({ block: target.kind === "rule" ? "center" : "nearest" });
  return element;
}

function scheduleAlertEvidenceMark(target) {
  markAlertEvidenceTarget(target);
  const frame = globalThis.requestAnimationFrame || ((callback) => setTimeout(callback, 0));
  frame(() => frame(() => markAlertEvidenceTarget(target)));
}

function openAlertEvidence(occurrence) {
  const target = alertEvidenceTarget(occurrence);
  if (target.kind === "brief") {
    $("tabMonitor")?.click();
    scheduleAlertEvidenceMark(target);
    return target;
  }
  if (["orders", "settings"].includes(target.kind)) {
    $(`tab${target.kind[0].toUpperCase()}${target.kind.slice(1)}`)?.click();
    scheduleAlertEvidenceMark(target);
    return target;
  }
  $("tabMonitor")?.click();
  if (target.kind === "rule") {
    $("stressRulesCard")?.click();
  } else if (target.kind === "regime" || target.kind === "stress") {
    const button = $(target.kind === "regime" ? "regimeDetailToggle" : "stressDetailToggle");
    if (button?.getAttribute("aria-expanded") !== "true") button?.click();
  } else if (target.kind === "protection") {
    $("protectionTile")?.click();
  }
  scheduleAlertEvidenceMark(target);
  return target;
}

function alertRowElement(occurrence) {
  const row = document.createElement("button");
  row.type = "button";
  const tint = SEVERITY_TINT[occurrence.severity] || "";
  row.className = `alert-row pd-tile pd-alert${tint ? ` ${tint}` : ""}`;
  row.dataset.displayId = occurrence.display_id;
  // A touch starts with pointerdown. Stop the panel-wide unread refresh here:
  // it replaces the alert rows and can otherwise remove this button before
  // the browser delivers the corresponding click.
  row.addEventListener("pointerdown", prepareAlertNavigation);
  row.addEventListener("click", () => {
    prepareAlertNavigation();
    openAlertEvidence(occurrence);
  });
  if (tint) {
    const bar = document.createElement("span");
    bar.className = "pd-tile__bar";
    bar.setAttribute("aria-hidden", "true");
    row.append(bar);
  }
  const placard = document.createElement("span");
  placard.className = "alert-row__source pd-alert__src";
  placard.textContent = alertPlacard(occurrence);
  const title = document.createElement("b");
  title.className = "pd-alert__title";
  title.textContent = occurrence.title;
  const body = document.createElement("p");
  body.className = "pd-alert__body";
  body.textContent = alertBodyCopy(occurrence);
  const factText = alertFactText(occurrence);
  const facts = document.createElement("small");
  facts.className = "pd-alert__facts";
  facts.textContent = factText;
  facts.hidden = !factText;
  const affected = alertAffectedPositions(occurrence);
  const affectedGroup = document.createElement("div");
  affectedGroup.className = "alert-row__affected";
  affectedGroup.hidden = affected.total === 0;
  if (affected.total > 0) {
    const affectedTitle = document.createElement("span");
    affectedTitle.textContent = `Affected positions · ${affected.total}`;
    const affectedList = document.createElement("span");
    affectedList.className = "alert-row__affected-list";
    for (const label of affected.labels) {
      const item = document.createElement("span");
      item.textContent = label;
      affectedList.append(item);
    }
    if (affected.total > affected.labels.length) {
      const more = document.createElement("span");
      more.textContent = `+${affected.total - affected.labels.length} more`;
      affectedList.append(more);
    }
    affectedGroup.append(affectedTitle, affectedList);
  }
  const age = document.createElement("span");
  age.className = "pd-alert__age";
  age.textContent = alertAgeLine(occurrence);
  const action = document.createElement("span");
  action.className = "alert-row__action";
  action.textContent = `${alertActionCopy(occurrence)} \u2192`;
  row.append(placard, title, body, facts, affectedGroup, age, action);
  return row;
}

// Process nudges enter the source-neutral registry as alert occurrences. The
// Alerts tab uses that current authority only; protection proposals and option
// exercise opportunities remain on their dedicated Monitor surfaces.
function activeAlertItems(activeAlerts = []) {
  return activeAlerts.map((alert) => ({ severity: alert.severity, alert })).sort(bySeverity);
}

// The engraved unlit poster: the quiet desk stated as a fact, and only when
// same, and only one of them means nothing is wrong.
function allDarkPoster(value) {
  const poster = document.createElement("div");
  poster.className = "pd-poster";
  const word = document.createElement("div");
  word.className = "pd-poster__word";
  word.textContent = "ALL DARK.";
  const sub = document.createElement("div");
  sub.className = "pd-poster__sub";
  sub.textContent = `No annunciators lit \u00b7 ${value.coverage.covered_sources.length}/${value.coverage.expected_sources.length} sources current \u00b7 ${clockLabel(value.coverage.as_of)}`;
  poster.append(word, sub);
  return poster;
}

// Act above watch, and the served order kept inside each band: the operator
const SEVERITY_ORDER = { urgent: 0, act: 1, watch: 2, observe: 3 };

function bySeverity(left, right) {
  return (SEVERITY_ORDER[left.severity] ?? 9) - (SEVERITY_ORDER[right.severity] ?? 9);
}

// Delivery classes are wire enums; the banner translates them into plain
// words so "no_active_subscription" never reaches the operator raw.
const DELIVERY_CLASS_COPY = {
  retry_pending: "a delivery attempt failed and will be retried",
  transport_rejected: "the push service rejected the delivery",
  interrupted_uncertain: "a delivery was interrupted; its outcome is unknown",
  state_write_failure: "delivery state could not be saved",
  capacity_overflow: "the inbox is full",
  no_active_subscription: "no device has phone notifications enabled",
  signing_keys_unavailable: "push signing keys are missing",
  sender_unavailable: "no push sender is configured",
  invalid_persisted_state: "stored delivery state is invalid",
  retry_exhausted: "delivery retries are exhausted",
  not_initialized: "delivery has not started yet",
  producer_observation_rejected: "the app refused the daemon's latest alert snapshot",
};

function deliveryCopy(health) {
  if (!health || health.state === "healthy") return "";
  if (health.state === "overflow") return "Alert delivery is blocked because the inbox is full.";
  const reason = DELIVERY_CLASS_COPY[health.class] || (health.class ? humanAlertWord(health.class).toLowerCase() : "reason unavailable");
  return `Alert delivery is ${health.state}: ${reason}.`;
}

function renderAttention() {
  const attention = state.alerts?.attention;
  const unread = attention?.unread_count;
  const feedInvalid = state.alertsFeedValid === false;
  const known = Number.isSafeInteger(unread) && unread >= 0 && !feedInvalid;
  const activeAlerts = feedInvalid ? [] : (state.alerts?.occurrences || []).filter((item) => item.ended_at === null);
  const alertCount = activeAlertItems(activeAlerts).length;
  const badge = $("alertUnreadBadge");
  const tab = $("tabAlerts");
  if (badge) {
    badge.hidden = alertCount === 0;
    badge.textContent = alertCount > 0 ? (alertCount > 99 ? "99+" : String(alertCount)) : "";
    badge.setAttribute("aria-hidden", "true");
    const severity = highestActiveAlertSeverity();
    badge.classList.toggle("bottom-tab__badge--act", severity === "act");
    badge.classList.toggle("bottom-tab__badge--watch", severity === "watch");
  }
  // An invalidated feed makes the unread state unknown, not zero: never
  if (tab) tab.setAttribute("aria-label", alertCount > 0 ? `Alerts, ${alertCount} open` : feedInvalid ? "Alerts, state unknown" : "Alerts, none open");
  if (!feedInvalid) syncAppIconBadge(known ? unread : 0);
}

// The tab badge follows the loudest ACTIVE alert, read straight off the
function highestActiveAlertSeverity() {
  if (state.alertsFeedValid === false) return "";
  const active = (state.alerts?.occurrences || []).filter((item) => item.ended_at === null);
  if (active.some((item) => item.severity === "act" || item.severity === "urgent")) return "act";
  return active.some((item) => item.severity === "watch") ? "watch" : "";
}

function syncAppIconBadge(unread) {
  if (typeof navigator === "undefined" || typeof navigator.setAppBadge !== "function") return;
  const update = unread > 0 ? navigator.setAppBadge(unread) : typeof navigator.clearAppBadge === "function" ? navigator.clearAppBadge() : navigator.setAppBadge(0);
  Promise.resolve(update).catch(() => {});
}

function renderSources(value) {
  const list = $("alertSourceList");
  if (!list) return;
  if (!value?.initialized) {
    list.replaceChildren(emptyRow("Source status is unavailable."));
    return;
  }
  const rows = value.sources.map((source) => {
    const row = document.createElement("div");
    // A source row reads lit only while its own served evidence says so; the
    // dark class is a tint, never a verdict the daemon did not state.
    row.className = source.covered && source.evidence_health === "current" ? "alert-source-row" : "alert-source-row alert-source-row--dark";
    const name = document.createElement("b");
    name.textContent = alertSourceLabel(source.source);
    const status = document.createElement("span");
    status.textContent = `${source.status}${source.reason ? ` · ${source.reason}` : ""}`;
    const timing = document.createElement("small");
    timing.textContent = `Evidence ${timeLabel(source.evidence_as_of)} · current until ${timeLabel(source.fresh_until)}`;
    row.append(name, status, timing);
    return row;
  });
  list.replaceChildren(...rows);
}

function renderDelivery(value) {
  const health = value?.delivery_health;
  const banner = $("alertsDeliveryBanner");
  const warning = deliveryCopy(health);
  if (banner) {
    banner.hidden = !warning;
    banner.textContent = warning;
  }
  setText("alertDeliveryHealth", health ? `${health.state}${health.class ? ` · ${health.class}` : ""}` : "unavailable");
  setText("alertDeliveryAcceptance", health?.last_push_service_acceptance_at
    ? `Push service accepted at ${timeLabel(health.last_push_service_acceptance_at)}. This does not prove the phone displayed it or that it was read.`
    : "No push-service acceptance is recorded. Phone display and reading are not known.");
}

function renderAlerts() {
  const value = state.alerts;
  const valid = state.alertsFeedValid !== false;
  const currentList = $("currentSignalList");
  const placard = $("currentSignalPlacard");
  if (!value || !valid || !value.initialized) {
    const alerts = activeAlertItems([]);
    state.renderedAlertAttention = null;
    if (placard) placard.hidden = false;
    setText("alertCount", alerts.length > 0 ? `${alerts.length} Open` : "Unknown");
    setText("currentSignalCount", String(alerts.length));
    setText("alertAuthorityState", "Unknown");
    setText("alertCoverageSummary", valid ? "Alert authority is not initialized." : "The latest alert update was rejected; retained evidence is not a current verdict.");
    if (currentList) currentList.replaceChildren(...(alerts.length > 0 ? alerts.map((item) => alertRowElement(item.alert)) : [emptyRow("Current alert state is unavailable.")]));
    renderSources(null);
    // Delivery health shares the feed's authority: an invalid or
    // uninitialized feed must not keep presenting the retained health as
    renderDelivery(null);
    renderAttention();
    return { state: "unknown", active: [], ended: [] };
  }

  const active = value.occurrences.filter((item) => item.ended_at === null);
  const alerts = activeAlertItems(active);
  const clear = canAssertAlertClear(value);
  const completeCurrent = value.coverage.state === "complete" && value.coverage.freshness === "current";
  const authorityState = clear ? "Clear" : value.current_state === "active" && completeCurrent ? "Active" : value.current_state === "active" ? "Degraded" : "Unknown";
  setText("alertCount", alerts.length > 0 ? `${alerts.length} Open` : authorityState);
  setText("currentSignalCount", String(alerts.length));
  setText("alertAuthorityState", authorityState);
  setText("alertCoverageSummary", `${value.coverage.state} coverage · ${value.coverage.freshness} · ${value.coverage.covered_sources.length}/${value.coverage.expected_sources.length} sources · ${timeLabel(value.coverage.as_of)}`);
  // The poster is the count: an engraved ALL DARK under an "ACTIVE 0" legend
  const posted = alerts.length === 0 && clear;
  if (placard) placard.hidden = posted;
  if (currentList) {
    currentList.replaceChildren(...(alerts.length > 0
      ? alerts.map((item) => alertRowElement(item.alert))
      : [posted ? allDarkPoster(value) : emptyRow("No active alert can be confirmed because source coverage is incomplete or stale.")]));
  }
  renderSources(value);
  renderDelivery(value);
  renderAttention();

  const rendered = new Map(value.occurrences.map((item) => [item.display_id, item]));
  const allUnreadRendered = value.attention.unread_refs.every((ref) => {
    const item = rendered.get(ref.display_id);
    return item && item.source === ref.source && item.kind === ref.kind;
  });
  state.renderedAlertAttention = allUnreadRendered
    ? { high_water_seq: value.attention.high_water_seq, refs: clone(value.attention.unread_refs) }
    : null;
  return { state: authorityState.toLowerCase(), active, ended: [] };
}

function attentionViewReady() {
  const panel = $("alertsTab");
  return state.authenticated === true && state.activeTab === "alerts" && panel && !panel.hidden && document.visibilityState === "visible";
}

function sameAttention(left, right) {
  return equalJSON(left, right);
}

function setAttentionStatus(copy, error = false) {
  state.attentionStatus.state = copy;
  state.attentionStatus.error = error;
  renderAttention();
}

const ALERTS_REFRESH_MIN_INTERVAL_MS = 15000;
const ALERTS_FETCH_DEADLINE_MS = 10000;
const ALERTS_REFRESH_FAILED_COPY = "Couldn't refresh alerts. Showing the last verified list; Canary will retry automatically.";

function alertsFetchDeadlineMs() {
  return Number.isSafeInteger(state.alertsFetchDeadlineMs) && state.alertsFetchDeadlineMs > 0
    ? state.alertsFetchDeadlineMs
    : ALERTS_FETCH_DEADLINE_MS;
}

function scheduleAlertsRefresh(options = {}) {
  if (!state.authenticated) return false;
  const delayMs = Math.max(0, Number(options.delayMs) || 0);
  const minIntervalMs = options.minIntervalMs === undefined
    ? ALERTS_REFRESH_MIN_INTERVAL_MS
    : Math.max(0, Number(options.minIntervalMs) || 0);
  const now = Date.now();
  const throttleDelay = Math.max(0, minIntervalMs - (now - state.alertsLastRefreshAt));
  const dueAt = now + Math.max(delayMs, throttleDelay);
  const ensureTrailing = options.ensureTrailing === true;
  let timerEnsureTrailing = ensureTrailing;
  if (state.alertsRefreshTimer) {
    timerEnsureTrailing ||= state.alertsRefreshTimerEnsureTrailing;
    state.alertsRefreshTimerEnsureTrailing = timerEnsureTrailing;
    if (state.alertsRefreshDueAt <= dueAt) return true;
    clearTimeout(state.alertsRefreshTimer);
  }
  state.alertsRefreshDueAt = dueAt;
  state.alertsRefreshTimerEnsureTrailing = timerEnsureTrailing;
  state.alertsRefreshTimer = setTimeout(() => {
    const trailing = state.alertsRefreshTimerEnsureTrailing;
    state.alertsRefreshTimer = null;
    state.alertsRefreshDueAt = 0;
    state.alertsRefreshTimerEnsureTrailing = false;
    refreshAlerts({ ensureTrailing: trailing });
  }, Math.max(0, dueAt - Date.now()));
  return true;
}

async function refreshAlerts(options = {}) {
  if (!state.authenticated) return false;
  if (state.alertsRefreshInFlight) {
    if (options.ensureTrailing === true) state.alertsRefreshAfterFlight = true;
    return state.alertsRefreshInFlight;
  }
  state.alertsLastRefreshAt = Date.now();
  state.alertsRefreshInFlight = (async () => {
    try {
      const response = await fetch("/api/alerts", { credentials: "include", signal: AbortSignal.timeout(alertsFetchDeadlineMs()) });
      if (!response.ok) throw new Error("alerts unavailable");
      const result = ingestAlerts(await response.json());
      if (result.status === "rejected") throw new Error("alerts malformed");
      if (state.attentionStatus.state === ALERTS_REFRESH_FAILED_COPY) setAttentionStatus("");
      renderAlerts();
      return true;
    } catch {
      // A failed or timed-out recovery GET is weaker evidence than the
      // retained validated feed: keep the last accepted authority (the
      // malformed or equivocating) and surface the failure as status.
      setAttentionStatus(ALERTS_REFRESH_FAILED_COPY, true);
      renderAlerts();
      return false;
    } finally {
      state.alertsRefreshInFlight = null;
      if (state.alertsRefreshAfterFlight) {
        state.alertsRefreshAfterFlight = false;
        scheduleAlertsRefresh({ minIntervalMs: 0 });
      }
    }
  })();
  return state.alertsRefreshInFlight;
}

async function acknowledgeAttention(options = {}) {
  if (!attentionViewReady()) return false;
  if (state.attentionReadInFlight) return state.attentionReadInFlight;
  state.attentionReadInFlight = (async () => {
    const epoch = (state.attentionEpoch || 0) + 1;
    state.attentionEpoch = epoch;
    try {
      const attentionResponse = await fetch("/api/alerts/attention", { credentials: "include", signal: AbortSignal.timeout(alertsFetchDeadlineMs()) });
      if (!attentionResponse.ok) throw new Error("attention unavailable");
      const attention = validateAttention(await attentionResponse.json());
      if (state.attentionEpoch !== epoch) return false;
      const alertsResponse = await fetch("/api/alerts", { credentials: "include", signal: AbortSignal.timeout(alertsFetchDeadlineMs()) });
      if (!alertsResponse.ok) throw new Error("alerts unavailable");
      const alerts = await alertsResponse.json();
      if (state.attentionEpoch !== epoch) return false;
      const accepted = ingestAlerts(alerts);
      if (accepted.status === "rejected") throw new Error("alerts malformed");
      renderAlerts();
      // An alert tap intentionally leaves this tab to show its evidence. That
      // navigation cancels acknowledgement; it is not a render failure and
      // must not leave a false unread-error banner behind.
      if (!attentionViewReady()) return false;
      if (!sameAttention(attention, state.alerts.attention) || !state.renderedAlertAttention ||
          state.renderedAlertAttention.high_water_seq !== attention.high_water_seq ||
          !sameAttention(state.renderedAlertAttention.refs, attention.unread_refs)) {
        throw new Error("unread alerts were not all rendered");
      }
      if (attention.unread_count === 0) return true;
      const readResponse = await fetch("/api/alerts/attention/read", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "include",
        body: JSON.stringify({ through_seq: attention.high_water_seq }),
        signal: AbortSignal.timeout(alertsFetchDeadlineMs()),
      });
      if (!readResponse.ok) throw new Error("attention read unavailable");
      const readResult = ingestAlerts(await readResponse.json());
      if (state.attentionEpoch !== epoch || readResult.status === "rejected") return false;
      setAttentionStatus("");
      renderAlerts();
      return true;
    } catch {
      if (state.attentionEpoch !== epoch) return false;
      if (!attentionViewReady()) return false;
      setAttentionStatus(ATTENTION_RENDER_FAILED_COPY, true);
      if (options.retry !== false) scheduleAttentionRetry();
      return false;
    } finally {
      state.attentionReadInFlight = null;
    }
  })();
  return state.attentionReadInFlight;
}

const ATTENTION_DWELL_MS = 2000;
const ATTENTION_RETRY_MS = 1500;
const ATTENTION_RENDER_FAILED_COPY = "Canary could not verify that every unread alert is visible. They remain unread, and Canary will retry automatically.";
let attentionDwellTimer = null;
let attentionVisibilityBound = false;

function cancelAttentionDwell() {
  if (attentionDwellTimer) clearTimeout(attentionDwellTimer);
  attentionDwellTimer = null;
}

function alertRowContains(target) {
  for (let element = target; element; element = element.parentElement) {
    if (element.classList?.contains("alert-row")) return true;
  }
  return false;
}

function prepareAlertNavigation(event) {
  event?.stopPropagation?.();
  cancelAttentionDwell();
  if (state.attentionRetryTimer) clearTimeout(state.attentionRetryTimer);
  state.attentionRetryTimer = null;
  // Any acknowledgement already waiting on the network must lose authority
  // before it can re-render the row being touched.
  state.attentionEpoch = (state.attentionEpoch || 0) + 1;
  if (state.attentionStatus.state === ATTENTION_RENDER_FAILED_COPY) setAttentionStatus("");
  return true;
}

function scheduleAttentionRetry() {
  if (!attentionViewReady() || state.attentionRetryTimer) return false;
  state.attentionRetryTimer = setTimeout(() => {
    state.attentionRetryTimer = null;
    acknowledgeAttention({ retry: false });
  }, ATTENTION_RETRY_MS);
  return true;
}

function handleAttentionContextChange() {
  if (!attentionViewReady()) {
    cancelAttentionDwell();
    // The typed SSE feed owns passive authority; a context change away
    // from the alerts view only needs a coalesced recovery read through
    // the scheduler, never a per-event direct GET (stress bursts used to
    return scheduleAlertsRefresh();
  }
  if (attentionDwellTimer) return true;
  const delay = Number.isSafeInteger(state.attentionDwellMs) && state.attentionDwellMs >= 0 ? state.attentionDwellMs : ATTENTION_DWELL_MS;
  attentionDwellTimer = setTimeout(() => {
    attentionDwellTimer = null;
    if (attentionViewReady()) acknowledgeAttention();
  }, delay);
  return true;
}

function acknowledgeAttentionNow() {
  cancelAttentionDwell();
  return attentionViewReady() ? acknowledgeAttention() : false;
}

function handleAttentionPointerDown(event) {
  // Alert rows own their tap. Acknowledgement is handled by dwell, scrolling,
  // or a touch on the non-actionable panel background.
  if (alertRowContains(event?.target)) return false;
  return acknowledgeAttentionNow();
}

function setupAttentionVisibility() {
  if (attentionVisibilityBound) return;
  attentionVisibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") cancelAttentionDwell();
    else handleAttentionContextChange();
  });
  const panel = $("alertsTab");
  panel?.addEventListener("pointerdown", handleAttentionPointerDown);
  panel?.addEventListener("scroll", acknowledgeAttentionNow, { capture: true, passive: true });
}

export {
  AlertContractError,
  acknowledgeAttention,
  acknowledgeAttentionNow,
  activeAlertItems,
  alertActionCopy,
  alertAffectedPositions,
  alertEvidenceTarget,
  alertFactText,
  alertRowElement,
  alertSourceLabel,
  attentionViewReady,
  canAssertAlertClear,
  handleAttentionContextChange,
  handleAttentionPointerDown,
  ingestAlerts,
  ingestAlertsEvent,
  markAlertEvidenceTarget,
  openAlertEvidence,
  refreshAlerts,
  renderAlerts,
  renderAttention,
  scheduleAlertsRefresh,
  setupAttentionVisibility,
  validateAlerts,
  validateAttention,
};
