import assert from "node:assert/strict";
import test from "node:test";

globalThis.localStorage = { getItem() { return null; }, setItem() {} };

class Element {
  constructor(id = "") {
    this.id = id;
    this.hidden = false;
    this.textContent = "";
    this.className = "";
    this.dataset = {};
    this.children = [];
    this.attributes = {};
    this.classList = {
      toggle: (name, on) => {
        const names = new Set(this.className.split(/\s+/).filter(Boolean));
        if (on) names.add(name); else names.delete(name);
        this.className = [...names].join(" ");
      },
    };
  }
  addEventListener() {}
  append(...items) { this.children.push(...items); }
  appendChild(item) { this.children.push(item); }
  replaceChildren(...items) { this.children = items; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
}

const ids = [
  "alertCount", "currentSignalCount", "currentSignalPlacard", "alertAuthorityState", "alertCoverageSummary", "currentSignalList",
  "alertHistoryList", "alertsHistorySection", "alertHistoryCount", "alertSourceList", "alertsDeliveryBanner",
  "alertDeliveryHealth", "alertDeliveryAcceptance", "alertUnreadBadge", "tabAlerts", "attentionStatus", "alertsTab",
  "selectedAlertPanel", "selectedAlertTitle", "selectedAlertBody", "selectedAlertTime",
];
const elements = new Map(ids.map((id) => [id, new Element(id)]));
globalThis.document = {
  visibilityState: "visible",
  addEventListener() {},
  createElement() { return new Element(); },
  getElementById(id) { return elements.get(id) || null; },
};
Object.defineProperty(globalThis, "navigator", { value: {}, configurable: true });

const {
  acknowledgeAttention,
  canAssertAlertClear,
  handleAttentionContextChange,
  ingestAlerts,
  ingestAlertsEvent,
  refreshAlerts,
  renderAlerts,
  renderSelectedAlert,
  scheduleAlertsRefresh,
  validateAlerts,
} = await import("../alert-inbox.js");
const { state } = await import("../state.js");

const sourceNames = [
  "canary", "regime", "rulebook", "risk_policy", "protection", "order_integrity",
  "reconciliation", "governance", "data_health", "delivery",
];
const at = "2026-07-22T12:00:00Z";
const freshUntil = "2026-07-22T12:10:00Z";

function source(name, overrides = {}) {
  return {
    source: name,
    status: "current",
    reason: "authoritative",
    evidence_health: "current",
    input_as_of: at,
    observed_at: at,
    evidence_as_of: at,
    fresh_until: freshUntil,
    covered: true,
    ...overrides,
  };
}

function occurrence(overrides = {}) {
  return {
    display_id: "alert-0123456789abcdef",
    source: "canary",
    kind: "portfolio_risk",
    presentation_code: "portfolio_stress",
    title: "Portfolio stress",
    body: "Portfolio stress needs attention.",
    state: "open",
    severity: "act",
    evidence_health: "current",
    destination: "alerts",
    evidence_as_of: at,
    state_changed_at: at,
    first_seen_at: at,
    last_seen_at: at,
    ended_at: null,
    end_reason: null,
    attention_seq: 4,
    disposition: "push_service_accepted",
    ...overrides,
  };
}

function dto(overrides = {}) {
  const active = occurrence();
  return {
    schema_version: "alerts-v1",
    version: "alert-delivery-v4",
    initialized: true,
    generation: 9,
    as_of: at,
    current_state: "active",
    coverage: {
      state: "complete",
      freshness: "current",
      as_of: at,
      expected_sources: [...sourceNames],
      covered_sources: [...sourceNames],
    },
    sources: sourceNames.map((name) => source(name)),
    occurrences: [active],
    attention: {
      unread_count: 1,
      high_water_seq: 4,
      read_through_seq: 3,
      unread_refs: [{ display_id: active.display_id, source: active.source, kind: active.kind }],
    },
    delivery_health: {
      state: "healthy",
      class: "",
      updated_at: at,
      last_push_service_acceptance_at: at,
    },
    ...overrides,
  };
}

function reset() {
  state.alerts = null;
  state.alertsFeedValid = null;
  state.alertsFeedError = "";
  state.alertsRefreshInFlight = null;
  if (state.alertsRefreshTimer) clearTimeout(state.alertsRefreshTimer);
  state.alertsRefreshTimer = null;
  state.alertsRefreshDueAt = 0;
  state.alertsRefreshTimerEnsureTrailing = false;
  state.alertsRefreshAfterFlight = false;
  state.alertsLastRefreshAt = 0;
  state.alertsFetchDeadlineMs = null;
  state.renderedAlertAttention = null;
  state.attentionEpoch = 0;
  state.attentionReadInFlight = null;
  state.attentionRetryTimer = null;
  state.attentionStatus = { state: "", error: false };
  state.selectedAlertID = null;
  state.authenticated = true;
  state.activeTab = "alerts";
  for (const element of elements.values()) {
    element.hidden = false;
    element.textContent = "";
    element.children = [];
  }
}

function visibleText(element) {
  return `${element?.textContent || ""} ${(element?.children || []).map(visibleText).join(" ")}`.trim();
}

function wait(ms) {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

test("the active DTO accepts current and previous display ids with a reused attention sequence", () => {
  const value = dto();
  value.occurrences.push(occurrence({
    display_id: "alert-previous-abcdef0123456789",
    ended_at: at,
    end_reason: "authority_scope_changed",
  }));
  assert.equal(validateAlerts(value), value);
});

test("clear requires every exact source to be covered, current, and not expired", () => {
  const clear = dto({
    current_state: "clear",
    occurrences: [],
    attention: { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] },
  });
  assert.equal(canAssertAlertClear(clear, Date.parse("2026-07-22T12:05:00Z")), true);
  assert.equal(canAssertAlertClear(clear, Date.parse("2026-07-22T12:10:00.001Z")), false);
  clear.sources[0].covered = false;
  assert.equal(canAssertAlertClear(clear, Date.parse("2026-07-22T12:05:00Z")), false);
});

test("same-generation freshness aging applies and any other equivocation is rejected", () => {
  reset();
  const current = dto();
  assert.equal(ingestAlerts(current).status, "applied");
  const aged = structuredClone(current);
  aged.coverage.freshness = "stale";
  aged.sources[0].status = "stale";
  aged.sources[0].reason = "freshness_expired";
  aged.sources[0].evidence_health = "stale";
  aged.occurrences[0].evidence_health = "stale";
  assert.equal(ingestAlertsEvent(JSON.stringify(aged)).status, "applied");
  assert.equal(state.alerts.sources[0].reason, "freshness_expired");
  const equivocation = structuredClone(aged);
  equivocation.occurrences[0].title = "Client-invented copy";
  assert.equal(ingestAlerts(equivocation).status, "rejected");
  assert.match(state.alertsFeedError, /equivocation/);
  assert.equal(state.alerts.occurrences[0].title, "Portfolio stress");
});

test("render uses only API title and body, and records the exact unread set rendered", () => {
  reset();
  const value = dto();
  ingestAlerts(value);
  const view = renderAlerts();
  assert.equal(view.active.length, 1);
  const row = elements.get("currentSignalList").children[0];
  assert.match(visibleText(row), /Portfolio stress/);
  assert.deepEqual(state.renderedAlertAttention, {
    high_water_seq: 4,
    refs: [{ display_id: "alert-0123456789abcdef", source: "canary", kind: "portfolio_risk" }],
  });
  // The engraved placard is the served source label plus the served kind, and
  // never the retained backend sensor id.
  assert.match(visibleText(row), /Stress · portfolio risk/);
  assert.doesNotMatch(visibleText(row), /canary/i);
  // A lit annunciator is the Monitor's tile, tinted by the served severity,
  // with the served timestamps read back as words.
  assert.match(row.className, /\balert-row\b/);
  assert.match(row.className, /\bpd-alert\b/);
  assert.match(row.className, /\bpd-tile--act\b/);
  assert.match(visibleText(row), /Lit \w/);
  assert.match(elements.get("alertDeliveryAcceptance").textContent, /does not prove the phone displayed it or that it was read/i);
  assert.equal(elements.get("alertSourceList").children[0].children[0].textContent, "Stress");
  assert.match(elements.get("alertSourceList").children[0].children[1].textContent, /current · authoritative/);
  assert.equal(elements.get("alertSourceList").children[0].className, "alert-source-row");
});

test("the log lamps act above watch, and an extinguished row reads unlit with its burn time", () => {
  reset();
  const value = dto({
    occurrences: [
      occurrence({ display_id: "alert-watch0123456789", source: "regime", kind: "market_state", severity: "watch", title: "Watch lamp", attention_seq: 5 }),
      occurrence({ display_id: "alert-act01234567890", severity: "act", title: "Act lamp", attention_seq: 6 }),
      occurrence({
        display_id: "alert-previous-abcdef0123456789", severity: "act", state: "recovered", title: "Out lamp",
        first_seen_at: "2026-07-22T11:00:00Z", last_seen_at: "2026-07-22T11:38:00Z",
        ended_at: "2026-07-22T11:38:00Z", end_reason: "recovered", attention_seq: 4,
      }),
    ],
    attention: { unread_count: 0, high_water_seq: 6, read_through_seq: 6, unread_refs: [] },
  });
  assert.equal(validateAlerts(value), value);
  ingestAlerts(value);
  renderAlerts();
  const active = elements.get("currentSignalList").children;
  assert.equal(active.length, 2);
  assert.match(visibleText(active[0]), /Act lamp/);
  assert.match(visibleText(active[1]), /Watch lamp/);
  assert.match(active[1].className, /\bpd-tile--watch\b/);
  const out = elements.get("alertHistoryList").children;
  assert.equal(out.length, 1);
  assert.match(out[0].className, /\bpd-alert--out\b/);
  assert.doesNotMatch(out[0].className, /pd-tile--(act|watch)/);
  assert.match(visibleText(out[0]), /Lit .*, out .*38 min lit · recovered/);
  assert.equal(elements.get("alertHistoryCount").textContent, "1");
  assert.equal(elements.get("alertsHistorySection").hidden, false);
});

test("the age line names the weekday inside the week and the date beyond it", () => {
  reset();
  const nowMs = Date.now();
  const nowISO = new Date(nowMs).toISOString();
  const litAt = new Date(nowMs - 3 * 60 * 60 * 1000);
  ingestAlerts(dto({
    as_of: nowISO,
    occurrences: [occurrence({ first_seen_at: litAt.toISOString(), last_seen_at: nowISO })],
    attention: { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] },
  }));
  renderAlerts();
  const recent = visibleText(elements.get("currentSignalList").children[0]);
  assert.ok(recent.includes(`Lit ${litAt.toLocaleDateString([], { weekday: "short" })} `), recent);

  reset();
  const oldLit = new Date(nowMs - 20 * 24 * 60 * 60 * 1000);
  const oldOut = new Date(oldLit.getTime() + 40 * 60 * 1000);
  const dated = oldLit.toLocaleDateString([], { day: "numeric", month: "short" });
  const outDated = oldOut.toLocaleDateString([], { day: "numeric", month: "short" });
  const stale = occurrence({
    display_id: "alert-previous-abcdef0123456789", severity: "watch", state: "recovered", title: "Old lamp",
    first_seen_at: oldLit.toISOString(), last_seen_at: oldOut.toISOString(),
    ended_at: oldOut.toISOString(), end_reason: "recovered",
  });
  ingestAlerts(dto({
    as_of: nowISO,
    occurrences: [stale],
    attention: {
      unread_count: 1,
      high_water_seq: 4,
      read_through_seq: 3,
      unread_refs: [{ display_id: stale.display_id, source: stale.source, kind: stale.kind }],
    },
  }));
  renderAlerts();
  // A weekday twenty days out names two different days and the operator
  // cannot tell which, so a retained lamp states its date.
  const old = visibleText(elements.get("alertHistoryList").children[0]);
  assert.ok(old.includes(`Lit ${dated} `), old);
  assert.ok(old.includes(`, out ${outDated} `), old);
  assert.ok(old.includes("40 min lit"), old);
});

test("a confirmed quiet desk posts ALL DARK, and an unconfirmed one keeps the honest sentence", () => {
  reset();
  const now = new Date();
  const nowISO = now.toISOString();
  const fresh = new Date(now.getTime() + 600_000).toISOString();
  const quiet = dto({
    as_of: nowISO,
    current_state: "clear",
    coverage: { state: "complete", freshness: "current", as_of: nowISO, expected_sources: [...sourceNames], covered_sources: [...sourceNames] },
    sources: sourceNames.map((name) => source(name, { input_as_of: nowISO, observed_at: nowISO, evidence_as_of: nowISO, fresh_until: fresh })),
    occurrences: [],
    attention: { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] },
  });
  assert.equal(canAssertAlertClear(quiet), true);
  ingestAlerts(quiet);
  renderAlerts();
  const poster = elements.get("currentSignalList").children[0];
  assert.equal(poster.className, "pd-poster");
  assert.match(visibleText(poster), /ALL DARK\./);
  assert.match(visibleText(poster), new RegExp(`${sourceNames.length}/${sourceNames.length} sources current`));
  // The poster is the count: no "ACTIVE 0" legend stands over it.
  assert.equal(elements.get("currentSignalPlacard").hidden, true);

  reset();
  const unconfirmed = dto({
    current_state: "clear",
    occurrences: [],
    attention: { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] },
  });
  assert.equal(canAssertAlertClear(unconfirmed), false);
  ingestAlerts(unconfirmed);
  renderAlerts();
  const empty = elements.get("currentSignalList").children[0];
  assert.equal(empty.className, "empty-row");
  assert.match(empty.textContent, /source coverage is incomplete or stale/);
  // A sentence is not a poster: the legend and its count stay.
  assert.equal(elements.get("currentSignalPlacard").hidden, false);
  assert.equal(elements.get("currentSignalCount").textContent, "0");
});

test("an unread occurrence outside the seven-day window stays in the extinguished register", () => {
  reset();
  const stale = occurrence({
    display_id: "alert-previous-abcdef0123456789", severity: "watch", state: "recovered", title: "Old lamp",
    first_seen_at: "2026-07-10T09:00:00Z", last_seen_at: "2026-07-10T09:40:00Z",
    ended_at: "2026-07-10T09:40:00Z", end_reason: "recovered", attention_seq: 4,
  });
  const unread = dto({
    occurrences: [stale],
    attention: {
      unread_count: 1,
      high_water_seq: 4,
      read_through_seq: 3,
      unread_refs: [{ display_id: stale.display_id, source: stale.source, kind: stale.kind }],
    },
  });
  assert.equal(validateAlerts(unread), unread);
  ingestAlerts(unread);
  renderAlerts();
  // The acknowledge guard requires every unread reference to have been
  // rendered: a display window must never satisfy it with an invisible row.
  assert.equal(elements.get("alertHistoryList").children.length, 1);
  assert.equal(elements.get("alertHistoryCount").textContent, "1");
  assert.deepEqual(state.renderedAlertAttention, { high_water_seq: 4, refs: unread.attention.unread_refs });

  reset();
  const read = dto({
    occurrences: [stale],
    attention: { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] },
  });
  ingestAlerts(read);
  renderAlerts();
  assert.equal(elements.get("alertHistoryList").children.length, 0);
  assert.equal(elements.get("alertsHistorySection").hidden, true);
});

test("an unread row outside bounded history keeps the inbox visible but cannot be acknowledged", async () => {
  reset();
  const value = dto();
  value.attention = {
    unread_count: 1,
    high_water_seq: 5,
    read_through_seq: 3,
    unread_refs: [{ display_id: "alert-previous-fedcba9876543210", source: "regime", kind: "market_state" }],
  };
  assert.equal(validateAlerts(value), value);
  ingestAlerts(value);
  renderAlerts();
  assert.equal(state.renderedAlertAttention, null);
  let posted = false;
  globalThis.fetch = async (url) => {
    if (url === "/api/alerts/attention") return { ok: true, async json() { return structuredClone(value.attention); } };
    if (url === "/api/alerts") return { ok: true, async json() { return structuredClone(value); } };
    posted = true;
    throw new Error("read must not be posted");
  };
  assert.equal(await acknowledgeAttention({ retry: false }), false);
  assert.equal(posted, false);
  assert.equal(elements.get("currentSignalList").children.length, 1);
});

test("read acknowledgement uses the active routes and accepts a full DTO receipt", async () => {
  reset();
  const value = dto();
  ingestAlerts(value);
  renderAlerts();
  const read = structuredClone(value);
  read.generation = 10;
  read.attention = { unread_count: 0, high_water_seq: 4, read_through_seq: 4, unread_refs: [] };
  const calls = [];
  globalThis.fetch = async (url, init = {}) => {
    calls.push({ url, init });
    if (url === "/api/alerts/attention") return { ok: true, async json() { return structuredClone(value.attention); } };
    if (url === "/api/alerts" && !init.method) return { ok: true, async json() { return structuredClone(value); } };
    if (url === "/api/alerts/attention/read") return { ok: true, async json() { return structuredClone(read); } };
    throw new Error(`unexpected route ${url}`);
  };
  assert.equal(await acknowledgeAttention({ retry: false }), true);
  assert.deepEqual(calls.map((call) => call.url), ["/api/alerts/attention", "/api/alerts", "/api/alerts/attention/read"]);
  assert.deepEqual(JSON.parse(calls[2].init.body), { through_seq: 4 });
  assert.equal(state.alerts.attention.unread_count, 0);
});

test("malformed or hidden unread state never advances the read cursor", async () => {
  reset();
  elements.get("alertsTab").hidden = true;
  let called = false;
  globalThis.fetch = async () => { called = true; throw new Error("must not fetch"); };
  assert.equal(await acknowledgeAttention({ retry: false }), false);
  assert.equal(called, false);
});

test("burst refresh triggers coalesce into one GET and a stale in-flight fetch gets one trailing refresh", async () => {
  reset();
  let calls = 0;
  let releaseFirst;
  const first = new Promise((resolve) => { releaseFirst = resolve; });
  globalThis.fetch = async () => {
    calls++;
    if (calls === 1) {
      await first;
      return { ok: true, async json() { return dto(); } };
    }
    return { ok: true, async json() { return dto({ generation: 10 }); } };
  };
  const inFlight = refreshAlerts();
  scheduleAlertsRefresh({ delayMs: 1, minIntervalMs: 0, ensureTrailing: true });
  scheduleAlertsRefresh({ delayMs: 1, minIntervalMs: 0, ensureTrailing: true });
  await wait(5);
  assert.equal(calls, 1, "concurrent delayed triggers must not start a second in-flight GET");
  releaseFirst();
  await inFlight;
  await wait(10);
  assert.equal(calls, 2, "the first stale GET must be followed by exactly one trailing refresh");
  assert.equal(state.alerts.generation, 10);
});

test("the default min interval defers a refresh scheduled right after a completed fetch", async () => {
  reset();
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return { ok: true, async json() { return dto(); } };
  };
  assert.equal(await refreshAlerts(), true);
  assert.equal(calls, 1);
  assert.equal(scheduleAlertsRefresh({ delayMs: 1 }), true);
  await wait(10);
  assert.equal(calls, 1, "a schedule inside the min interval must not fetch immediately");
  assert.ok(state.alertsRefreshTimer, "the deferred trailing refresh must stay scheduled");
  reset();
});

test("a failed recovery GET keeps the retained unread authority instead of clearing it", async () => {
  reset();
  ingestAlerts(dto());
  renderAlerts();
  const badge = elements.get("alertUnreadBadge");
  const tab = elements.get("tabAlerts");
  assert.equal(badge.hidden, false);
  assert.equal(badge.textContent, "1");
  globalThis.fetch = async () => { throw new Error("network down"); };
  assert.equal(await refreshAlerts(), false);
  assert.notEqual(state.alertsFeedValid, false, "a transport failure must not invalidate the validated feed");
  assert.equal(badge.hidden, false, "the unread badge must survive a failed refresh");
  assert.equal(badge.textContent, "1");
  assert.equal(tab.attributes["aria-label"], "Alerts, 1 unread");
  assert.equal(state.attentionStatus.error, true);
  assert.match(state.attentionStatus.state, /retained state/i);
  globalThis.fetch = async () => ({ ok: true, async json() { return dto({ generation: 10 }); } });
  assert.equal(await refreshAlerts(), true);
  assert.equal(state.attentionStatus.state, "", "a successful refresh must clear the failure note");
  reset();
});

test("a delivered alerts event clears the failure note the recovery GET set", async () => {
  reset();
  ingestAlerts(dto());
  globalThis.fetch = async () => { throw new Error("network down"); };
  assert.equal(await refreshAlerts(), false);
  assert.match(state.attentionStatus.state, /retained state/i);
  // The feed itself comes back before any recovery GET succeeds — on a closed
  // session the other clear-triggers can stay silent for hours.
  assert.equal(ingestAlertsEvent(JSON.stringify(dto({ generation: 11 }))).status, "applied");
  assert.equal(state.attentionStatus.state, "", "a delivered event must retire the failure note");
  assert.equal(state.attentionStatus.error, false);
  reset();
});

test("a malformed alerts event leaves the failure note standing", async () => {
  reset();
  ingestAlerts(dto());
  globalThis.fetch = async () => { throw new Error("network down"); };
  assert.equal(await refreshAlerts(), false);
  assert.match(state.attentionStatus.state, /retained state/i);
  assert.equal(ingestAlertsEvent("{not json").status, "rejected");
  assert.match(state.attentionStatus.state, /retained state/i, "a rejected event proves nothing about the feed");
  reset();
});

test("a hung GET aborts at the deadline and later refreshes are not wedged", async () => {
  reset();
  state.alertsFetchDeadlineMs = 25;
  let deadlineSignal;
  globalThis.fetch = (url, init = {}) => new Promise((resolve, reject) => {
    deadlineSignal = init.signal;
    // AbortSignal.timeout() deliberately uses an unreferenced timer in Node.
    // Keep this mocked fetch alive long enough to observe the real deadline;
    // otherwise node:test may cancel the pending promise when the event loop
    // becomes empty before the abort event is delivered.
    const watchdog = setTimeout(() => reject(new Error("deadline did not abort")), 250);
    init.signal?.addEventListener("abort", () => {
      clearTimeout(watchdog);
      reject(new Error("aborted"));
    }, { once: true });
  });
  assert.equal(await refreshAlerts(), false);
  assert.equal(deadlineSignal?.aborted, true, "the fetch deadline must abort the request");
  assert.equal(state.alertsRefreshInFlight, null, "the aborted refresh must clear the in-flight slot");
  assert.notEqual(state.alertsFeedValid, false);
  globalThis.fetch = async () => ({ ok: true, async json() { return dto(); } });
  assert.equal(await refreshAlerts(), true, "a later refresh must run after the abort");
  assert.equal(state.alerts.generation, 9);
  reset();
});

test("a context change away from the alerts view schedules a coalesced refresh instead of a direct GET", async () => {
  reset();
  state.activeTab = "monitor";
  state.alertsLastRefreshAt = Date.now();
  let calls = 0;
  globalThis.fetch = async () => {
    calls++;
    return { ok: true, async json() { return dto(); } };
  };
  handleAttentionContextChange();
  handleAttentionContextChange();
  handleAttentionContextChange();
  await wait(10);
  assert.equal(calls, 0, "SSE-driven context changes must not fetch outside the scheduler min interval");
  assert.ok(state.alertsRefreshTimer, "a coalesced scheduler timer must be pending");
  reset();
});

test("a refused producer observation is accepted by the contract and named in delivery health", () => {
  reset();
  const refused = dto({
    delivery_health: {
      state: "degraded",
      class: "producer_observation_rejected",
      updated_at: at,
      last_push_service_acceptance_at: at,
    },
  });
  assert.equal(validateAlerts(refused), refused);
  assert.equal(ingestAlerts(refused).status, "applied");
  renderAlerts();
  assert.equal(elements.get("alertDeliveryHealth").textContent, "degraded · producer_observation_rejected");
  reset();
});

test("an invalidated feed quarantines delivery health and the selected-alert panel", () => {
  reset();
  const value = dto();
  ingestAlerts(value);
  state.selectedAlertID = value.occurrences[0].display_id;
  renderAlerts();
  renderSelectedAlert();
  assert.equal(elements.get("selectedAlertPanel").hidden, false);
  assert.match(elements.get("alertDeliveryHealth").textContent, /healthy/);
  const equivocation = structuredClone(value);
  equivocation.occurrences[0].title = "Client-invented copy";
  assert.equal(ingestAlerts(equivocation).status, "rejected");
  renderAlerts();
  renderSelectedAlert();
  assert.equal(elements.get("selectedAlertPanel").hidden, true, "stale selected-alert detail must not present as current");
  assert.equal(elements.get("alertDeliveryHealth").textContent, "unavailable");
  assert.equal(elements.get("tabAlerts").attributes["aria-label"], "Alerts, unread state unknown");
  reset();
});

test("selected alert distinguishes retained evidence and a drawdown latch from current breaches", () => {
  reset();
  const retained = occurrence({
    presentation_code: "rulebook_catalyst_coverage",
    evidence_health: "partial",
    body: "Catalyst coverage needs attention.",
  });
  ingestAlerts(dto({ occurrences: [retained] }));
  state.selectedAlertID = retained.display_id;
  renderSelectedAlert();
  assert.match(elements.get("selectedAlertBody").textContent, /Retained: current evidence is partial/);
  assert.match(elements.get("selectedAlertBody").textContent, /not a fresh breach/);
  assert.match(elements.get("selectedAlertTime").textContent, /retained; recovery unconfirmed/);

	reset();
  const latched = occurrence({
    presentation_code: "risk_policy_drawdown_latched",
    source: "risk_policy",
    kind: "drawdown",
    body: "A prior drawdown breach remains latched.",
  });
  ingestAlerts(dto({
    occurrences: [latched],
    attention: {
      unread_count: 1,
      high_water_seq: latched.attention_seq,
      read_through_seq: latched.attention_seq - 1,
      unread_refs: [{ display_id: latched.display_id, source: latched.source, kind: latched.kind }],
    },
  }));
  state.selectedAlertID = latched.display_id;
  renderSelectedAlert();
  assert.match(elements.get("selectedAlertTime").textContent, /latched prior breach/);
  reset();
});
