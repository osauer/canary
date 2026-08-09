import { state } from "./state.js";

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
const EPISODE_STATES = new Set(["open", "escalated", "recovered"]);
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

function validateOccurrence(value, index, asOf, endedSeen) {
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
  if (value.ended_at === null) {
    if (endedSeen || value.end_reason !== null || value.state === "recovered") fail(path, "has invalid active ordering or state");
  } else {
    if (typeof value.end_reason !== "string" || !CODE.test(value.end_reason)) fail(`${path}.end_reason`, "must be a safe code");
    if (Date.parse(value.ended_at) > Date.parse(asOf)) fail(`${path}.ended_at`, "must not be after as_of");
  }
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
  let endedSeen = false;
  let endedCount = 0;
  value.occurrences.forEach((occurrence, index) => {
    validateOccurrence(occurrence, index, value.as_of, endedSeen);
    if (displayIDs.has(occurrence.display_id)) fail("occurrences", "must have unique display ids");
    displayIDs.add(occurrence.display_id);
    if (occurrence.ended_at !== null) {
      endedSeen = true;
      endedCount += 1;
    }
  });
  if (endedCount > 100) fail("occurrences", "contains more than 100 ended items");
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
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "not observed";
  return parsed.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit", timeZoneName: "short" });
}

// Panel clock: the day and the minute, on the same 24-hour register the
// earns its place; the seconds never do. Inside the week the weekday reads
const WEEKDAY_CLOCK_MS = 6 * 24 * 60 * 60 * 1000;

function clockLabel(value) {
  if (!value) return "not observed";
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return "not observed";
  const day = Math.abs(Date.now() - parsed.getTime()) <= WEEKDAY_CLOCK_MS
    ? parsed.toLocaleDateString([], { weekday: "short" })
    : parsed.toLocaleDateString([], { day: "numeric", month: "short" });
  const time = parsed.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  return `${day} ${time}`;
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
  const parts = occurrence.ended_at === null
    ? [`Lit ${clockLabel(occurrence.first_seen_at)}`]
    : [`Lit ${clockLabel(occurrence.first_seen_at)}, out ${clockLabel(occurrence.ended_at)}`, litDuration(occurrence.first_seen_at, occurrence.ended_at)];
  if (occurrence.ended_at === null) {
    if (occurrence.presentation_code === "risk_policy_drawdown_latched") parts.push("latched");
    else if (occurrence.evidence_health !== "current") parts.push("retained");
    else parts.push(occurrence.state);
  } else {
    parts.push(occurrence.state);
  }
  if (occurrence.evidence_health !== "current") parts.push(`evidence ${occurrence.evidence_health}`);
  return parts.filter(Boolean).join(" \u00b7 ");
}

function alertBodyCopy(occurrence) {
  if (occurrence.ended_at === null && occurrence.evidence_health !== "current") {
    return `${occurrence.body} Retained: current evidence is ${occurrence.evidence_health}, so recovery is unconfirmed; this is not a fresh breach.`;
  }
  return occurrence.body;
}

function litDuration(from, to) {
  const start = Date.parse(from);
  const end = Date.parse(to);
  if (!Number.isFinite(start) || !Number.isFinite(end) || end < start) return "";
  const minutes = Math.round((end - start) / 60000);
  if (minutes < 1) return "under a minute lit";
  if (minutes < 60) return `${minutes} min lit`;
  const hours = Math.floor(minutes / 60);
  const remainder = minutes % 60;
  if (hours < 24) return remainder === 0 ? `${hours} h lit` : `${hours} h ${remainder} min lit`;
  const days = Math.round(hours / 24);
  return days === 1 ? "1 day lit" : `${days} days lit`;
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

function alertRowElement(occurrence) {
  const row = document.createElement("button");
  row.type = "button";
  const out = occurrence.ended_at !== null;
  const tint = out ? "" : SEVERITY_TINT[occurrence.severity] || "";
  row.className = `alert-row pd-tile pd-alert${out ? " pd-alert--out" : ""}${tint ? ` ${tint}` : ""}`;
  row.dataset.displayId = occurrence.display_id;
  row.classList.toggle("active", occurrence.display_id === state.selectedAlertID);
  row.addEventListener("click", () => {
    state.selectedAlertID = occurrence.display_id;
    renderAlerts();
    renderSelectedAlert();
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
  const age = document.createElement("span");
  age.className = "pd-alert__age";
  age.textContent = alertAgeLine(occurrence);
  row.append(placard, title, body, age);
  return row;
}

// The queue is a presentation over existing authorities, not a new policy
function actionQueueItems(activeAlerts = []) {
  const snapshot = state.snapshot || {};
  const items = activeAlerts.map((alert) => ({ kind: "alert", severity: alert.severity, at: alert.last_seen_at, alert }));
  for (const proposal of snapshot.proposals?.proposals || []) {
    items.push({
      kind: "protection",
      severity: String(proposal.state || "").toLowerCase() === "blocked" ? "watch" : "act",
      title: `${proposal.symbol || proposal.contract?.symbol || "Position"} · ${String(proposal.bucket || "protection").replaceAll("_", " ")}`,
      body: "Review the daemon-authored protection action and its warnings.",
      at: snapshot.proposals?.as_of || "",
    });
  }
  for (const opportunity of snapshot.opportunities?.opportunities || []) {
    items.push({
      kind: "exercise",
      severity: (opportunity.blockers || []).length > 0 ? "watch" : "act",
      title: `${opportunity.symbol || opportunity.contract?.symbol || "Option"} · option exercise`,
      body: "Review warnings and the fresh exercise preflight before confirmation.",
      at: snapshot.opportunities?.as_of || "",
    });
  }
  for (const nudge of snapshot.nudges?.candidates || []) {
    items.push({ kind: "process", severity: nudge.severity || "watch", title: nudge.title, body: nudge.body, at: nudge.occurred_at || nudge.due_at || "" });
  }
  return items.sort(bySeverity);
}

function actionQueueRowElement(item) {
  if (item.kind === "alert") return alertRowElement(item.alert);
  const row = document.createElement("button");
  row.type = "button";
  const tint = SEVERITY_TINT[item.severity] || "";
  row.className = `alert-row pd-tile pd-alert${tint ? ` ${tint}` : ""}`;
  if (tint) {
    const bar = document.createElement("span");
    bar.className = "pd-tile__bar";
    bar.setAttribute("aria-hidden", "true");
    row.append(bar);
  }
  const source = document.createElement("span");
  source.className = "alert-row__source pd-alert__src";
  source.textContent = item.kind === "protection" ? "Protection" : item.kind === "exercise" ? "Option exercise" : "Process";
  const title = document.createElement("b");
  title.className = "pd-alert__title";
  title.textContent = item.title || "Review required";
  const body = document.createElement("p");
  body.className = "pd-alert__body";
  body.textContent = item.body || "Review the current evidence.";
  const age = document.createElement("span");
  age.className = "pd-alert__age";
  age.textContent = item.at ? `Current ${clockLabel(item.at)}` : "Current";
  row.append(source, title, body, age);
  row.addEventListener("click", () => {
    if (item.kind === "process") {
      $("tabBrief")?.click();
      return;
    }
    $("tabMonitor")?.click();
    state.protectionOpen = true;
    state.opportunitiesOpen = item.kind === "exercise";
    const sheet = $("protectionSheet");
    if (sheet && !sheet.open) sheet.showModal();
  });
  return row;
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

const EXTINGUISHED_WINDOW_MS = 7 * 24 * 60 * 60 * 1000;

// The extinguished register is the last seven days off the SERVED clock. An
// reference to have been rendered, and a display window must never quietly
function extinguishedRegister(value, ended) {
  const asOf = Date.parse(value.as_of);
  const unread = new Set(value.attention.unread_refs.map((ref) => ref.display_id));
  return ended.filter((item) => {
    if (unread.has(item.display_id)) return true;
    const endedAt = Date.parse(item.ended_at);
    return !Number.isFinite(asOf) || !Number.isFinite(endedAt) || asOf - endedAt <= EXTINGUISHED_WINDOW_MS;
  });
}

function deliveryCopy(health) {
  if (!health || health.state === "healthy") return "";
  if (health.state === "overflow") return "Alert delivery is blocked because the inbox is full.";
  return `Alert delivery is ${health.state}: ${health.class || "reason unavailable"}.`;
}

function renderAttention() {
  const attention = state.alerts?.attention;
  const unread = attention?.unread_count;
  const feedInvalid = state.alertsFeedValid === false;
  const known = Number.isSafeInteger(unread) && unread >= 0 && !feedInvalid;
  const activeAlerts = feedInvalid ? [] : (state.alerts?.occurrences || []).filter((item) => item.ended_at === null);
  const queueCount = actionQueueItems(activeAlerts).length;
  const badge = $("alertUnreadBadge");
  const tab = $("tabAlerts");
  if (badge) {
    badge.hidden = queueCount === 0;
    badge.textContent = queueCount > 0 ? (queueCount > 99 ? "99+" : String(queueCount)) : "";
    badge.setAttribute("aria-hidden", "true");
    const severity = highestActiveAlertSeverity();
    badge.classList.toggle("bottom-tab__badge--act", severity === "act");
    badge.classList.toggle("bottom-tab__badge--watch", severity === "watch");
  }
  // An invalidated feed makes the unread state unknown, not zero: never
  if (tab) tab.setAttribute("aria-label", queueCount > 0 ? `Action queue, ${queueCount} open` : feedInvalid ? "Action queue, alert state unknown" : "Action queue, empty");
  if (!feedInvalid) syncAppIconBadge(known ? unread : 0);
  const status = $("attentionStatus");
  if (status) {
    status.textContent = state.attentionStatus.state;
    status.classList.toggle("governance-action-status--error", state.attentionStatus.error);
  }
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
  const historyList = $("alertHistoryList");
  const placard = $("currentSignalPlacard");
  if (!value || !valid || !value.initialized) {
    const actions = actionQueueItems([]);
    state.renderedAlertAttention = null;
    if (placard) placard.hidden = false;
    setText("alertCount", actions.length > 0 ? `${actions.length} Open` : "Unknown");
    setText("currentSignalCount", String(actions.length));
    setText("alertAuthorityState", "Unknown");
    setText("alertCoverageSummary", valid ? "Alert authority is not initialized." : "The latest alert update was rejected; retained evidence is not a current verdict.");
    if (currentList) currentList.replaceChildren(...(actions.length > 0 ? actions.map(actionQueueRowElement) : [emptyRow("Current alert state is unavailable.")]));
    if (historyList) historyList.replaceChildren();
    const history = $("alertsHistorySection");
    if (history) history.hidden = true;
    renderSources(null);
    // Delivery health shares the feed's authority: an invalid or
    // uninitialized feed must not keep presenting the retained health as
    renderDelivery(null);
    renderAttention();
    return { state: "unknown", active: [], ended: [] };
  }

  const active = value.occurrences.filter((item) => item.ended_at === null);
  const queue = actionQueueItems(active);
  const ended = value.occurrences.filter((item) => item.ended_at !== null);
  const clear = canAssertAlertClear(value);
  const completeCurrent = value.coverage.state === "complete" && value.coverage.freshness === "current";
  const authorityState = clear ? "Clear" : value.current_state === "active" && completeCurrent ? "Active" : value.current_state === "active" ? "Degraded" : "Unknown";
  setText("alertCount", queue.length > 0 ? `${queue.length} Open` : authorityState);
  setText("currentSignalCount", String(queue.length));
  setText("alertAuthorityState", authorityState);
  setText("alertCoverageSummary", `${value.coverage.state} coverage · ${value.coverage.freshness} · ${value.coverage.covered_sources.length}/${value.coverage.expected_sources.length} sources · ${timeLabel(value.coverage.as_of)}`);
  const extinguished = extinguishedRegister(value, ended);
  // The poster is the count: an engraved ALL DARK under an "ACTIVE 0" legend
  const posted = queue.length === 0 && clear;
  if (placard) placard.hidden = posted;
  if (currentList) {
    currentList.replaceChildren(...(queue.length > 0
      ? queue.map(actionQueueRowElement)
      : [posted ? allDarkPoster(value) : emptyRow("No active alert can be confirmed because source coverage is incomplete or stale.")]));
  }
  if (historyList) historyList.replaceChildren(...extinguished.map(alertRowElement));
  const history = $("alertsHistorySection");
  if (history) history.hidden = extinguished.length === 0;
  setText("alertHistoryCount", String(extinguished.length));
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
  return { state: authorityState.toLowerCase(), active, ended };
}

function renderSelectedAlert() {
  // An invalid feed quarantines the detail panel with the other alert
  const occurrence = state.alertsFeedValid === false
    ? null
    : state.alerts?.occurrences?.find((item) => item.display_id === state.selectedAlertID);
  const panel = $("selectedAlertPanel");
  if (!panel) return;
  panel.hidden = !occurrence;
  if (!occurrence) return;
  setText("selectedAlertTitle", occurrence.title);
  setText("selectedAlertBody", alertBodyCopy(occurrence));
  const lifecycle = occurrence.ended_at !== null
    ? "extinguished"
    : occurrence.presentation_code === "risk_policy_drawdown_latched"
      ? "latched prior breach"
      : occurrence.evidence_health !== "current"
        ? "retained; recovery unconfirmed"
        : occurrence.state;
  setText("selectedAlertTime", `${alertSourceLabel(occurrence.source)} · ${lifecycle} · evidence ${occurrence.evidence_health} · ${timeLabel(occurrence.last_seen_at)}`);
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
const ALERTS_REFRESH_FAILED_COPY = "Alert refresh unavailable; retained state shown";

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
      renderSelectedAlert();
      return true;
    } catch {
      // A failed or timed-out recovery GET is weaker evidence than the
      // retained validated feed: keep the last accepted authority (the
      // malformed or equivocating) and surface the failure as status.
      setAttentionStatus(ALERTS_REFRESH_FAILED_COPY, true);
      renderAlerts();
      renderSelectedAlert();
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
      renderSelectedAlert();
      if (!attentionViewReady()) throw new Error("view not visible");
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
      renderSelectedAlert();
      return true;
    } catch {
      if (state.attentionEpoch !== epoch) return false;
      setAttentionStatus("Alerts stayed unread because the current server set was not fully rendered.", true);
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
let attentionDwellTimer = null;
let attentionVisibilityBound = false;

function cancelAttentionDwell() {
  if (attentionDwellTimer) clearTimeout(attentionDwellTimer);
  attentionDwellTimer = null;
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

function setupAttentionVisibility() {
  if (attentionVisibilityBound) return;
  attentionVisibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") cancelAttentionDwell();
    else handleAttentionContextChange();
  });
  const panel = $("alertsTab");
  panel?.addEventListener("pointerdown", acknowledgeAttentionNow);
  panel?.addEventListener("scroll", acknowledgeAttentionNow, { capture: true, passive: true });
}

export {
  AlertContractError,
  acknowledgeAttention,
  acknowledgeAttentionNow,
  actionQueueItems,
  alertRowElement,
  alertSourceLabel,
  attentionViewReady,
  canAssertAlertClear,
  handleAttentionContextChange,
  ingestAlerts,
  ingestAlertsEvent,
  refreshAlerts,
  renderAlerts,
  renderAttention,
  renderSelectedAlert,
  scheduleAlertsRefresh,
  setupAttentionVisibility,
  validateAlerts,
  validateAttention,
};
