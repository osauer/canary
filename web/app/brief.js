import { setActiveTab, setProtectionSheetOpen, setRulesSheetOpen } from "./chrome.js";
import { $, calendarDate, calendarDateTime, money, privacyMask, weekdayName } from "./shared.js";
import { state } from "./state.js";

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
    return;
  }

  $("briefAsOf").textContent = calendarDateTime(brief.as_of);
  const sections = $("briefSections");
  // The daemon composes the narrative; an older daemon serves none and the
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
  nodes.push(briefPlacard("Review"));
  for (const runs of narrative.review) nodes.push(runsElement("p", "pd-brf-para", runs));
  nodes.push(briefPlacard("Ready"));
  for (const runs of narrative.ready) nodes.push(runsElement("p", "pd-brf-para", runs));
  if (narrative.coda.length > 0) nodes.push(runsElement("p", "pd-brf-coda", narrative.coda));
  return nodes;
}

// Runs are text, never markup: each span is created and filled through
// textContent, so composed prose can never inject nodes. A run carrying a
// served topic slug renders as a tap target that navigates to the surface
// owning that row — navigation only, never an order action.
function runsElement(tag, className, runs) {
  const el = document.createElement(tag);
  el.className = className;
  for (const run of runs) {
    const hidden = Boolean(run?.account_sensitive) && !state.accountValueVisible;
    const text = hidden ? privacyMask() : String(run?.text || "");
    const roleClass = RUN_ROLE_CLASSES[String(run?.role || "")];
    if (!roleClass) {
      el.append(document.createTextNode(text));
      continue;
    }
    const topic = String(run?.topic || "");
    if (topic) {
      const link = document.createElement("button");
      link.type = "button";
      link.className = `${roleClass} brief-topic-link`;
      link.textContent = text;
      link.title = "Open this topic's panel";
      link.addEventListener("click", () => openBriefTopic(topic));
      el.append(link);
      continue;
    }
    const span = document.createElement(roleClass === "pd-fig" ? "b" : "span");
    span.className = roleClass;
    span.classList.toggle("is-private", hidden);
    span.textContent = text;
    el.append(span);
  }
  return el;
}

// openBriefTopic routes a flagged topic to its owning surface. Rules and
// protection are sheets; capital, latch, and reconcile rows live with the
// daily broker report card in the lamp-test dialog, which carries the one
// latch-related control (Check again).
function openBriefTopic(topic) {
  if (topic.startsWith("held_name")) {
    setActiveTab("positions");
    return;
  }
  switch (topic) {
    case "policy_adherence":
    case "premium_at_risk":
      setRulesSheetOpen(true);
      return;
    case "proposals":
    case "protection_proposals":
    case "index_put_theta":
      setProtectionSheetOpen(true);
      return;
    case "attribution":
      setActiveTab("positions");
      return;
    case "capital":
    case "capital_events":
    case "drawdown_latch":
    case "reconcile":
    case "auto_extend":
      $("lampTestDialog")?.showModal();
      return;
    case "working_orders":
      setActiveTab("orders");
      return;
    case "policy_drift":
    case "monthly_pulse":
      setActiveTab("settings");
      return;
    default:
      setActiveTab("monitor");
  }
}

function briefPlacard(text) {
  const placard = document.createElement("div");
  placard.className = "pd-placard";
  placard.textContent = text;
  return placard;
}

// The placard row names the last-session bridge and carries the served stress
function briefPlacardRow(brief) {
  const row = document.createElement("div");
  row.className = "pd-placard pd-placard--row";
  const label = document.createElement("span");
  const weekday = weekdayName(brief.review?.last_session?.session_date || "");
  label.textContent = weekday ? `${weekday}'s close → next open` : "Last close → next open";
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
const REVIEW_ICON = ["M12 7v5l3 2", "M5.2 8.5A7.5 7.5 0 1 1 4.5 12", "M4.5 7.5v3.2h3.2"];
const READY_ICON = ["M4 17h16", "M8.5 17a3.5 3.5 0 0 1 7 0", "M12 10V7", "M6.8 11.8 5 10", "M17.2 11.8 19 10"];

// Review — post-trade since the last regular close. Rows are rendered
function renderReviewSection(section, brief) {
  const account = section.session_pnl || {};
  const currency = account.base_currency || "";
  const rows = [
    briefRow("Session P/L", account, joinValues(
      moneyValue(account, "equity_base", currency, "Equity"),
      moneyValue(account, "daily_pnl_base", currency, "Daily P/L"),
    )),
    // Older daemons serve no last_session field; a labeled empty row would
    // read as a data gap, so the row renders only once the daemon serves it.
    ...(section.last_session
      ? [briefRow("Last session close", section.last_session, lastSessionValue(section.last_session))]
      : []),
    briefRow("By underlying", section.attribution, moversValue(section.attribution, currency)),
    briefRow("Policy adherence", section.rules, rulesValue(section.rules || {})),
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
      section.latch?.report_coverage_to ? `Report through ${dateValue(section.latch.report_coverage_to)}` : "",
      section.latch?.report_checked_at ? `Checked ${dateTimeValue(section.latch.report_checked_at)}` : "",
    )),
    briefRow("Premium at risk", section.premium_at_risk, moneyCoverageValue(section.premium_at_risk)),
    briefRow("Index-put theta / day", section.hedge_cost, moneyCoverageValue(section.hedge_cost)),
    briefRow("Protection proposals", section.proposals, readyProposalsValue(section.proposals || {})),
  );
  // Pins are operator material only under required sign-off; otherwise the
  // row appears solely when something is actually wrong (non-ok status).
  const policyDrift = section.policy_drift;
  if (policyDrift && (policyDrift.signoff_required || (policyDrift.status && policyDrift.status !== "ok"))) {
    rows.push(briefRow("Policy drift", policyDrift, (policyDrift.rows || []).map((row) => joinValues(row.policy, row.status, row.live_id, row.live_version)).join(" · ")));
  }
  if (Object.prototype.hasOwnProperty.call(section, "monthly_pulse") && section.monthly_pulse) {
    rows.push(renderMonthlyPulseRow(section.monthly_pulse));
  }
  return briefSection(READY_ICON, "Ready", section, rows, "brief-section--ready");
}

function heldNameEventsUnavailable(sources) {
  return Boolean(sources?.positions?.error);
}

// The daemon reports stress action and severity as separate fields that are
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


// Ready-side proposals: how much protection work is staged for the session
// ahead. Counts only — the served facts, never an action affordance.
function readyProposalsValue(row = {}) {
  return joinValues(
    integerValue(row, "actionable", "Ready to act"),
    integerValue(row, "blocked", "Blocked"),
    integerValue(row, "total", "Staged"),
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
  case "completed":
    return "completed this month";
  case "blocked":
    return "blocked by policy evidence";
  default:
    return "blocked by policy evidence";
  }
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

function rulesValue(row = {}) {
  return joinValues(
    integerValue(row, "pass", "Pass"),
    integerValue(row, "watch", "Watch"),
    integerValue(row, "act", "Act"),
    integerValue(row, "track", "Tracked"),
    integerValue(row, "unknown", "Unknown"),
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
  return calendarDate(value);
}

// Close-captured last-session Daily P/L. A populated session date without a
// value is an explicit "not captured", never a substituted running number.
function lastSessionValue(row) {
  if (!row.session_date) return "";
  if (typeof row.daily_pnl_base !== "number") {
    return joinValues(dateValue(row.session_date), "not captured");
  }
  return joinValues(
    dateValue(row.session_date),
    moneyValue(row, "daily_pnl_base", row.base_currency || "", "Daily P/L"),
    row.captured_at ? `captured ${timeValue(row.captured_at)}` : "",
  );
}

function dateTimeValue(value) {
  return calendarDateTime(value);
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

export { monthlyPulseStatus, renderBriefCard };
