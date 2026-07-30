import { $, money, readJSONOrText } from "./shared.js";
import { state } from "./state.js";

const attemptedStampFingerprints = new Set();
const pendingStampFingerprints = new Set();
const stampOutcomes = new Map();
const signoffOutcomes = new Map();
let visibilityBound = false;
let briefStampArmed = true;
let briefStampScheduled = false;
let briefStampInFlight = false;
let briefStampLook = 0;

function setupBriefVisibility() {
  if (visibilityBound) return;
  visibilityBound = true;
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") {
      briefStampLook += 1;
      briefStampArmed = false;
      return;
    }
    briefStampArmed = true;
    renderBriefCard(state.snapshot || {});
  });
  const briefTab = $("briefTab");
  if (briefTab && typeof MutationObserver !== "undefined") {
    const tabObserver = new MutationObserver(() => {
      if (!briefTab.hidden && state.activeTab === "brief") renderBriefCard(state.snapshot || {});
    });
    tabObserver.observe(briefTab, { attributes: true, attributeFilter: ["hidden"] });
  }
}

function renderBriefCard(snap = state.snapshot || {}) {
  const panel = $("briefPanel");
  if (!panel) return;
  panel.hidden = !state.authenticated;
  if (panel.hidden) return;

  renderBriefSource(snap.sources?.brief);
  const brief = snap.brief;
  if (!brief) {
    $("briefAsOf").textContent = "--";
    $("briefSections").replaceChildren(briefEmptyState());
    renderBriefAckStatus(null);
    return;
  }

  $("briefAsOf").textContent = dateTimeValue(brief.as_of);
  const sections = $("briefSections");
  // The daemon composes the narrative; an older daemon serves none and the
  // row render below stays the surface. Both paths keep the render stamp,
  // the sign-off flow, and the monthly-pulse gating identical.
  const narrative = servedNarrative(brief);
  sections.classList.toggle("brief-sections--narrative", Boolean(narrative));
  if (narrative) {
    sections.replaceChildren(...renderNarrative(narrative, brief));
  } else {
    sections.replaceChildren(
      renderReviewSection(brief.review || {}, brief),
      renderReadySection(brief.ready || {}, snap.sources || {}),
    );
  }
  renderBriefAckStatus(brief);
  scheduleBriefStamp(brief);
}

// Narrative render: the daemon's composed prose, rendered verbatim as typed
// runs. The SPA never composes a sentence and never re-derives a role.
const RUN_ROLE_CLASSES = { figure: "pd-fig", watch: "pd-wtint", act: "pd-atint" };

function servedNarrative(brief) {
  const narrative = brief?.narrative;
  if (!narrative) return null;
  const lead = runList(narrative.lead);
  const review = paragraphList(narrative.review);
  const ready = paragraphList(narrative.ready);
  if (lead.length === 0 && review.length === 0 && ready.length === 0) return null;
  return { lead, review, ready, coda: runList(narrative.coda) };
}

function runList(runs) {
  return Array.isArray(runs) ? runs.filter((run) => String(run?.text || "") !== "") : [];
}

function paragraphList(paragraphs) {
  if (!Array.isArray(paragraphs)) return [];
  return paragraphs.map((paragraph) => runList(paragraph?.runs)).filter((runs) => runs.length > 0);
}

function renderNarrative(narrative, brief) {
  const nodes = [briefPlacardRow(brief)];
  if (narrative.lead.length > 0) nodes.push(runsElement("div", "pd-brf-lead", narrative.lead));
  nodes.push(briefPlacard("Review \u00b7 last session"));
  for (const runs of narrative.review) nodes.push(runsElement("p", "pd-brf-para", runs));
  const signoff = renderNarrativeSignoff(brief);
  if (signoff) nodes.push(signoff);
  nodes.push(briefPlacard("Ready \u00b7 next open"));
  for (const runs of narrative.ready) nodes.push(runsElement("p", "pd-brf-para", runs));
  if (narrative.coda.length > 0) nodes.push(runsElement("p", "pd-brf-coda", narrative.coda));
  return nodes;
}

// Runs are text, never markup: each span is created and filled through
// textContent, so composed prose can never inject nodes.
function runsElement(tag, className, runs) {
  const el = document.createElement(tag);
  el.className = className;
  for (const run of runs) {
    const text = String(run?.text || "");
    const roleClass = RUN_ROLE_CLASSES[String(run?.role || "")];
    if (!roleClass) {
      el.append(document.createTextNode(text));
      continue;
    }
    const span = document.createElement(roleClass === "pd-fig" ? "b" : "span");
    span.className = roleClass;
    span.textContent = text;
    el.append(span);
  }
  return el;
}

function briefPlacard(text) {
  const placard = document.createElement("div");
  placard.className = "pd-placard";
  placard.textContent = text;
  return placard;
}

// The placard row carries the brief's own stamp line and the served stress
// severity, printed in the daemon's vocabulary. An unreadable stress row shows
// its status word instead of an implied calm.
function briefPlacardRow(brief) {
  const row = document.createElement("div");
  row.className = "pd-placard pd-placard--row";
  const label = document.createElement("span");
  label.textContent = joinValues("Briefing", dateValue(brief.as_of), timeValue(brief.as_of));
  row.append(label);
  const stress = brief.ready?.stress || {};
  const severity = String(stress.severity || "").trim() || String(stress.status || "").trim();
  if (severity) {
    const chip = document.createElement("span");
    chip.className = `pd-chip ${severityChipClass(severity)}`.trim();
    chip.textContent = severity;
    row.append(chip);
  }
  return row;
}

function severityChipClass(severity) {
  const normalized = String(severity || "").toLowerCase();
  if (normalized === "act") return "pd-chip--act";
  if (normalized === "watch") return "pd-chip--watch";
  return "";
}

// The sign-off control keeps its exact semantics in the narrative render; only
// its seat changes, from the One-tap row to the Review movement.
function renderNarrativeSignoff(brief) {
  const controls = signoffControls(brief.review?.one_tap || {}, brief, brief.review?.rules_delta || {});
  if (controls.length === 0) return null;
  const wrap = document.createElement("div");
  wrap.className = "brief-signoff-seat";
  wrap.append(...controls);
  return wrap;
}

function briefEmptyState() {
  const empty = document.createElement("p");
  empty.className = "brief-empty";
  empty.textContent = "Brief data is unavailable.";
  return empty;
}

function renderBriefSource(source = {}) {
  const banner = $("briefSourceBanner");
  const message = String(source?.error || "");
  banner.hidden = !message;
  if (!message) {
    banner.replaceChildren();
    return;
  }
  const label = document.createElement("span");
  label.textContent = "Degraded";
  const detail = document.createElement("b");
  detail.textContent = message;
  banner.replaceChildren(label, detail);
}

const SVG_NS = "http://www.w3.org/2000/svg";
// House-style 24×24 phase glyphs (stroke via CSS): a sunrise for the pre-trade
// Ready movement, a history clock for the post-trade Review movement.
const REVIEW_ICON = ["M12 7v5l3 2", "M5.2 8.5A7.5 7.5 0 1 1 4.5 12", "M4.5 7.5v3.2h3.2"];
const READY_ICON = ["M4 17h16", "M8.5 17a3.5 3.5 0 0 1 7 0", "M12 10V7", "M6.8 11.8 5 10", "M17.2 11.8 19 10"];

// Review — post-trade of the last completed session. Rows are rendered
// verbatim from the daemon-composed movement.
function renderReviewSection(section, brief) {
  const account = section.session_pnl || {};
  const currency = account.base_currency || "";
  const rows = [
    briefRow("Session P/L", account, joinValues(
      moneyValue(account, "equity_base", currency, "Equity"),
      moneyValue(account, "daily_pnl_base", currency, "Daily P/L"),
    )),
    briefRow("By underlying", section.attribution, moversValue(section.attribution, currency)),
    briefRow("Rules delta", section.rules_delta, rulesDeltaValue(section.rules_delta || {})),
    briefRow("Proposals", section.proposals, proposalsValue(section.proposals || {})),
    briefRow("Overrides used", section.overrides, (section.overrides?.rows || []).map((row) => joinValues(row.control, dateTimeValue(row.expires_at))).join(" · ")),
    briefRow("Capital events", section.capital_events, capitalEventsValue(section.capital_events || {})),
    briefRow("Reconcile", section.reconcile, joinValues(
      dateTimeValue(section.reconcile?.last_reconciled_at),
      section.reconcile?.source,
      section.reconcile?.deadline ? `due ${dateValue(section.reconcile.deadline)}` : "",
      integerValue(section.reconcile, "days_remaining", "Days remaining"),
    )),
    briefRow("Auto-extend", section.auto_extend, joinValues(section.auto_extend?.report_id, dateTimeValue(section.auto_extend?.at))),
    renderOneTapRow(section.one_tap || {}, brief, section.rules_delta || {}),
    briefRow("Working orders", section.working_orders, integerValue(section.working_orders, "count", "Count")),
  ];
  return briefSection(REVIEW_ICON, "Review", section, rows, "brief-section--review");
}

// Ready — pre-trade for today.
function renderReadySection(section, sources = {}) {
  const capital = section.capital || {};
  const rows = [
    briefRow("Regime", section.regime, joinValues(section.regime?.stage, section.regime?.verdict)),
    briefRow("Breadth", section.breadth, joinValues(
      percentValue(section.breadth, "pct_above_50dma", "50-DMA"),
      percentValue(section.breadth, "pct_above_200dma", "200-DMA"),
      percentValue(section.breadth, "net_new_highs_pct", "Net new highs"),
      fieldValue(section.breadth, "data_type", "Data"),
    )),
    briefRow("Dealer gamma", section.gamma, joinValues(
      numberValue(section.gamma, "spot", "Spot"),
      numberValue(section.gamma, "zero_gamma", "Zero gamma"),
      percentValue(section.gamma, "gap_pct", "Gap", true),
      fieldValue(section.gamma, "gamma_sign", "Sign"),
    )),
    briefRow("Stress", section.stress, joinValues(...stressHeadline(section.stress), section.stress?.summary)),
    briefRow("Session", section.session, joinValues(section.session?.market, section.session?.state)),
  ];
  const events = section.market_events || [];
  if (heldNameEventsUnavailable(sources)) {
    rows.push(briefRow("Held-name events", {
      status: "unavailable",
      detail: "Held-name events require an available positions snapshot.",
    }, "unavailable"));
  } else {
    for (const event of events) {
      rows.push(briefRow(`Event · ${event.kind || "--"}`, event, joinValues(
        integerValue(event, "count", "Count"),
        (event.symbols || []).join(", "),
      )));
    }
  }
  rows.push(
    briefRow("Capital", capital, joinValues(
      fieldValue(capital, "tier", "Tier"),
      fieldValue(capital, "enforcement", "Enforcement"),
      percentValue(capital, "consumed_pct", "Consumed"),
      moneyValue(capital, "drawdown_base", capital.base_currency, "Drawdown"),
      moneyValue(capital, "adjusted_peak_base", capital.base_currency, "Adjusted peak"),
      capital.peak_as_of ? `peak set ${dateTimeValue(capital.peak_as_of)}` : "",
    )),
    briefRow("Drawdown latch", section.latch, joinValues(
      hasField(section.latch, "latched") ? (section.latch.latched ? "latched" : "open") : "",
      latchAgeValue(section.latch),
      percentValue(section.latch, "consumed_pct_at_latch", "Engaged at"),
      dateValue(section.latch?.latched_at),
    )),
    briefRow("Premium at risk", section.premium_at_risk, moneyCoverageValue(section.premium_at_risk)),
    briefRow("Hedge cost / day", section.hedge_cost, moneyCoverageValue(section.hedge_cost)),
    briefRow("Policy drift", section.policy_drift, (section.policy_drift?.rows || []).map((row) => joinValues(row.policy, row.status, row.live_id, row.live_version)).join(" · ")),
  );
  if (Object.prototype.hasOwnProperty.call(section, "monthly_pulse") && section.monthly_pulse) {
    rows.push(renderMonthlyPulseRow(section.monthly_pulse));
  }
  rows.push(briefRow("Artefacts", section.artefacts, null));
  for (const artefact of section.artefacts?.rows || []) {
    rows.push(briefRow(`Artefact · ${artefact.kind || "--"}`, artefact, artefactValue(artefact), "brief-row--nested"));
  }
  return briefSection(READY_ICON, "Ready", section, rows, "brief-section--ready");
}

function heldNameEventsUnavailable(sources) {
  return Boolean(sources?.positions?.error);
}

// The daemon reports stress action and severity as separate fields that are
// usually the same word; printing "watch · watch" reads as a stutter, so the
// pair collapses when equal and labels itself when it genuinely differs.
function stressHeadline(stress = {}) {
  const action = String(stress?.action || "").trim();
  const severity = String(stress?.severity || "").trim();
  if (!action && !severity) return [];
  if (!action || !severity || action.toLowerCase() === severity.toLowerCase()) return [action || severity];
  return [action, `severity ${severity}`];
}

function proposalsValue(row = {}) {
  return joinValues(
    integerValue(row, "offered", "Offered"),
    integerValue(row, "acted", "Acted"),
    row.day || "",
  );
}

function capitalEventsValue(row = {}) {
  return joinValues(
    hasField(row, "latched") ? (row.latched ? "latched" : "no latch") : "",
    latchAgeValue(row, "latch_age_days"),
    percentValue(row, "consumed_pct_at_latch", "Engaged at"),
    moneyValue(row, "adjusted_peak_base", row.base_currency, "Adjusted peak"),
    row.peak_as_of ? `peak set ${dateTimeValue(row.peak_as_of)}` : "",
  );
}

function renderMonthlyPulseRow(monthly) {
  return briefRow("Monthly pulse", {}, monthlyPulseStatus(monthly));
}

function monthlyPulseStatus(monthly = {}) {
  switch (monthly.status) {
  case "not_due":
    return "not due";
  case "due":
    return "due";
  case "completed":
    return "completed this month";
  case "blocked":
    return "blocked by policy evidence";
  default:
    return "blocked by policy evidence";
  }
}

function artefactValue(artefact) {
  if (artefact?.declared !== true) return "not declared";
  if (artefact.completed === true) {
    const completedAt = dateTimeValue(artefact.completed_at);
    return completedAt ? `completed ${completedAt}` : "completed";
  }
  return artefact.cadence === "weekly" ? "not yet completed this week" : "not yet completed today";
}

function phaseIcon(paths) {
  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", "0 0 24 24");
  svg.setAttribute("aria-hidden", "true");
  svg.setAttribute("class", "brief-section__icon");
  for (const d of paths) {
    const path = document.createElementNS(SVG_NS, "path");
    path.setAttribute("d", d);
    svg.append(path);
  }
  return svg;
}

function briefSection(iconPaths, title, section, rows, className = "") {
  const el = document.createElement("section");
  el.className = `brief-section ${className}`.trim();
  const head = document.createElement("div");
  head.className = "brief-section__head";
  const heading = document.createElement("h3");
  heading.append(phaseIcon(iconPaths));
  const titleText = document.createElement("span");
  titleText.textContent = title;
  heading.append(titleText);
  head.append(heading, statusBadge(section?.status));
  const detail = document.createElement("p");
  detail.className = "brief-section__detail";
  detail.textContent = verbatimText(section?.detail);
  const list = document.createElement("div");
  list.className = "brief-rows";
  list.replaceChildren(...rows);
  el.append(head, detail, list);
  return el;
}

function briefRow(label, row = {}, value = "", className = "") {
  const el = document.createElement("div");
  el.className = `brief-row ${statusClass(row?.status)} ${className}`.trim();
  const head = document.createElement("div");
  head.className = "brief-row__head";
  const name = document.createElement("b");
  name.textContent = label;
  head.append(name, statusBadge(row?.status));
  const provided = document.createElement("p");
  provided.className = "brief-row__value";
  provided.textContent = rowText(value);
  const detail = document.createElement("p");
  detail.className = "brief-row__detail";
  detail.textContent = verbatimText(row?.detail);
  el.append(head);
  if (value !== null) el.append(provided);
  el.append(detail);
  return el;
}

function renderOneTapRow(row, brief, rulesDelta = {}) {
  const el = briefRow("One-tap sign-off", row, row.report_id || "--");
  const blockers = document.createElement("ul");
  blockers.className = "brief-blockers";
  for (const blocker of row.blockers || []) {
    const item = document.createElement("li");
    item.textContent = String(blocker);
    blockers.append(item);
  }
  if (blockers.childElementCount > 0) el.append(blockers);

  el.append(...signoffControls(row, brief, rulesDelta));
  return el;
}

// signoffControls builds the reconcile sign-off control, its rulebook caveat,
// and its receipt. Both renders seat the same elements, so the flow and its
// gating are one implementation, not two.
function signoffControls(row = {}, brief = {}, rulesDelta = {}) {
  const nodes = [];
  const reportID = String(row.report_id || "");
  const fingerprint = String(brief.brief_fingerprint || "");
  const outcome = signoffOutcome(fingerprint, reportID);
  if (row.signable === true && reportID && !outcome.result?.ok) {
    const button = document.createElement("button");
    button.id = "briefSignoffButton";
    button.type = "button";
    button.className = "primary brief-signoff";
    // Whichever seat renders it, the report is already named beside the
    // control (the row value, or the Review movement's prose); the tap target
    // speaks trader language, scopes the claim to what the report actually
    // attests (statement reconcile), and keeps the exact id in its tooltip.
    button.textContent = outcome.busy ? "Signing off the reconcile report" : "Sign off this reconcile report — statement clean";
    button.title = `Report ${reportID}`;
    button.disabled = outcome.busy;
    button.addEventListener("click", () => submitReconcileSignoff(fingerprint, reportID));
    nodes.push(button);
    // Signability is statement-scoped by design; when the rulebook changed
    // since the last stamped brief — or the delta cannot be verified at all —
    // the caveat sits on the control so the sign-off cannot borrow the
    // delta's ambiguity. Unknown is not clean.
    const deltaStatus = String(rulesDelta.status || "").toLowerCase();
    const deltaUnknowable = !rulesDeltaUnclean(rulesDelta) && deltaStatus !== "" && deltaStatus !== "ok";
    if (rulesDeltaUnclean(rulesDelta) || deltaUnknowable) {
      const caveat = document.createElement("p");
      caveat.className = "brief-action-message brief-signoff-caveat";
      caveat.textContent = deltaUnknowable
        ? "Note: the rulebook delta cannot be verified right now — unknown is not clean; review the Rules delta row before signing."
        : "Note: the rulebook changed since the last stamped brief — review the Rules delta row before signing.";
      nodes.push(caveat);
    }
  }
  const message = outcome.error || outcome.result?.message || (outcome.result?.ok ? `Report ${reportID} signed off.` : "");
  if (message) {
    const receipt = document.createElement("p");
    receipt.className = outcome.error ? "brief-action-message brief-action-message--error" : "brief-action-message";
    receipt.textContent = message;
    nodes.push(receipt);
  }
  return nodes;
}

async function submitReconcileSignoff(fingerprint, reportID) {
  const outcome = signoffOutcome(fingerprint, reportID);
  if (outcome.busy || outcome.result?.ok) return;
  outcome.busy = true;
  outcome.error = "";
  renderBriefCard(state.snapshot || {});
  try {
    const res = await fetch("/api/recon/signoff", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ report_id: reportID }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    outcome.result = body;
  } catch (err) {
    outcome.error = err.message;
  } finally {
    outcome.busy = false;
    renderBriefCard(state.snapshot || {});
  }
}

function signoffOutcome(fingerprint, reportID) {
  const key = `${fingerprint}\u0000${reportID}`;
  if (!signoffOutcomes.has(key)) signoffOutcomes.set(key, { busy: false, result: null, error: "" });
  return signoffOutcomes.get(key);
}

function scheduleBriefStamp(brief) {
  const fingerprint = String(brief?.brief_fingerprint || "");
  if (brief?.stamp_target === "monthly" && !brief?.ready?.monthly_pulse?.month) return;
  if (!briefStampArmed || briefStampScheduled || briefStampInFlight || !brief?.stamp_target || !fingerprint || attemptedStampFingerprints.has(fingerprint) || pendingStampFingerprints.has(fingerprint)) return;
  if (!briefStampVisible()) return;
  const look = briefStampLook;
  briefStampScheduled = true;
  pendingStampFingerprints.add(fingerprint);
  const afterRender = globalThis.requestAnimationFrame || ((callback) => globalThis.setTimeout(callback, 0));
  afterRender(() => {
    briefStampScheduled = false;
    pendingStampFingerprints.delete(fingerprint);
    if (!briefStampArmed || briefStampInFlight || look !== briefStampLook || !briefStampVisible() || state.snapshot?.brief?.brief_fingerprint !== fingerprint || attemptedStampFingerprints.has(fingerprint)) return;
    attemptedStampFingerprints.add(fingerprint);
    briefStampInFlight = true;
    acknowledgeBrief(brief, fingerprint, look);
  });
}

function briefStampVisible() {
  const panel = $("briefPanel");
  return state.authenticated === true && state.activeTab === "brief" && panel && !panel.hidden && document.visibilityState === "visible";
}

async function acknowledgeBrief(brief, fingerprint, look) {
  try {
    const res = await fetch("/api/brief/seen", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify(briefAckBody(brief, fingerprint)),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    if (look === briefStampLook) briefStampArmed = false;
    stampOutcomes.set(fingerprint, { result: body, error: "" });
  } catch (err) {
    stampOutcomes.set(fingerprint, { result: null, error: err.message });
  } finally {
    briefStampInFlight = false;
    renderBriefCard(state.snapshot || {});
  }
}

function briefAckBody(brief, fingerprint) {
  const body = { kind: brief.stamp_target, brief_fingerprint: fingerprint };
  if (brief.stamp_target === "monthly") {
    body.month = brief.ready?.monthly_pulse?.month || "";
    body.evidence = "render";
  }
  return body;
}

function renderBriefAckStatus(brief) {
  const target = $("briefAckStatus");
  const outcome = brief ? stampOutcomes.get(String(brief.brief_fingerprint || "")) : null;
  if (!outcome) {
    target.hidden = true;
    target.textContent = "";
    return;
  }
  target.hidden = false;
  target.classList.toggle("brief-receipt--error", Boolean(outcome.error));
  if (outcome.error) {
    target.textContent = brief?.stamp_target === "monthly" ? "Monthly foreground render unavailable." : `Render stamp failed: ${outcome.error}`;
    return;
  }
  const result = outcome.result || {};
  if (result.kind === "monthly") {
    target.textContent = result.already_stamped ? "foreground render already recorded" : "foreground render recorded";
    return;
  }
  target.textContent = result.already_stamped
    ? `${result.kind || "Brief"} artefact · ${result.day || "--"} · already stamped`
    : `${result.kind || "Brief"} artefact stamped · ${result.day || "--"}`;
}

function statusBadge(status) {
  const badge = document.createElement("span");
  badge.className = `brief-status ${statusClass(status)}`.trim();
  badge.textContent = verbatimText(status);
  return badge;
}

function statusClass(status) {
  const normalized = String(status || "").toLowerCase();
  return ["ok", "attention", "degraded", "unavailable"].includes(normalized) ? `brief-status--${normalized}` : "";
}

function latchAgeValue(latch = {}, key = "age_days") {
  if (!hasField(latch, key) || !Number.isFinite(latch[key])) return "";
  const days = Math.trunc(latch[key]);
  return `Age ${days} ${days === 1 ? "day" : "days"}`;
}

function moversValue(movers = {}, currency) {
  const parts = (movers?.rows || []).map((row) => `${row.symbol || "--"} ${money(row.daily_pnl_base, currency)}`);
  if (typeof movers?.other_daily_pnl_base === "number" && movers?.other_count > 0) {
    parts.push(`${movers.other_count} other${movers.other_count === 1 ? "" : "s"} ${money(movers.other_daily_pnl_base, currency)}`);
  }
  return parts.join(" · ");
}

function moneyCoverageValue(row = {}) {
  return joinValues(
    moneyValue(row, "amount_base", row.base_currency, "Amount"),
    integerValue(row, "included_legs", "Included legs"),
    integerValue(row, "excluded_legs", "Excluded legs"),
  );
}

function rulesDeltaUnclean(row = {}) {
  return row.rulebook_fingerprint_changed === true ||
    (row.transitions || []).length > 0 ||
    (row.added || []).length > 0 ||
    (row.removed || []).length > 0;
}

function rulesDeltaValue(row = {}) {
  const transitions = (row.transitions || []).map((item) => `${item.rule_id || "--"}: ${item.from || "--"} → ${item.to || "--"}`);
  const added = (row.added || []).map((item) => `added ${item}`);
  const removed = (row.removed || []).map((item) => `removed ${item}`);
  return joinValues(
    ...transitions,
    ...added,
    ...removed,
    row.rulebook_fingerprint_changed === true ? "fingerprint changed" : "",
  );
}

function moneyValue(object, key, currency, label) {
  if (!hasField(object, key) || typeof object[key] !== "number") return "";
  return `${label} ${money(object[key], currency || "")}`;
}

function fieldValue(object, key, label, suffix = "") {
  if (!hasField(object, key) || object[key] === null || object[key] === "") return "";
  return `${label} ${object[key]}${suffix}`;
}

function numberValue(object, key, label) {
  if (!hasField(object, key) || !Number.isFinite(object[key])) return "";
  return `${label} ${object[key].toFixed(2)}`;
}

function percentValue(object, key, label, signed = false) {
  if (!hasField(object, key) || !Number.isFinite(object[key])) return "";
  const prefix = signed && object[key] > 0 ? "+" : "";
  return `${label} ${prefix}${object[key].toFixed(1)}%`;
}

function integerValue(object, key, label, suffix = "") {
  if (!hasField(object, key) || !Number.isFinite(object[key])) return "";
  return `${label} ${Math.trunc(object[key])}${suffix}`;
}

function hasField(object, key) {
  return Boolean(object) && Object.prototype.hasOwnProperty.call(object, key);
}

function dateValue(value) {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return String(value);
  return `${at.getFullYear()}-${padDatePart(at.getMonth() + 1)}-${padDatePart(at.getDate())}`;
}

function dateTimeValue(value) {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return String(value);
  return `${dateValue(at)} ${padDatePart(at.getHours())}:${padDatePart(at.getMinutes())}`;
}

function timeValue(value) {
  if (!value) return "";
  const at = new Date(value);
  if (Number.isNaN(at.getTime())) return "";
  return `${padDatePart(at.getHours())}:${padDatePart(at.getMinutes())}`;
}

function padDatePart(value) {
  return String(value).padStart(2, "0");
}

function joinValues(...values) {
  return values.flat().map((value) => String(value || "").trim()).filter(Boolean).join(" · ");
}

function rowText(value) {
  const text = String(value || "").trim();
  return text || "--";
}

function verbatimText(value) {
  return value === undefined || value === null || value === "" ? "--" : String(value);
}

export { briefAckBody, monthlyPulseStatus, renderBriefCard, scheduleBriefStamp, setupBriefVisibility };
