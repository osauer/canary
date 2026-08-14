import { stressProtectionCoverageFor, protectionCoverageBaseCurrency, protectionCoverageHasData, protectionCoverageHeadline, protectionCoverageLargestText, protectionCoverageStaleText } from "./protection-coverage.js";
import { unknownEventRuleNote } from "./earnings-relevance.js";
import { earningsApplicabilitySummary, earningsHealthNotes, ruleStatusLabel, wshEntitlementNotice } from "./rules-presentation.js";
import { $, cleanDetail, firstNumber, labelize, normalizeSymbol, numberRead, parseDate, pct, quoteTimestamp, renderFreshnessTimestamp, shortTimeWithZone, signedClass, signedPct, wholePct } from "./shared.js";
import { state } from "./state.js";

const RULE_TONES = { act: "risk", watch: "warn", pass: "ok", info: "neutral", unknown: "neutral", not_evaluated: "neutral" };

function ruleTone(status, mode = "alert") {
  if (mode === "track" || mode === "off") return "neutral";
  return RULE_TONES[status] || "neutral";
}

function trackedRuleStatus(status, reason = "") {
  if (status === "pass") return "within level";
  if (status === "watch" || status === "act") return "above level";
  if (status === "unknown") return "waiting for input";
  return ruleStatusLabel(status, reason);
}

// Rules card: advisory 14-rule daily checklist from snapshot.rules
// grid shows all rows. Read-only by design — no order actions here.
function renderRulesCard(rules) {
  const card = $("stressRulesCard");
  const strip = $("stressRulesStrip");
  const detail = $("stressRulesDetailPanel");
  const tally = $("rulesSheetTally");
  if (!card || !strip || !detail) return;
  if (!rules || rules.enabled === false || !Array.isArray(rules.rules) || rules.rules.length === 0) {
    card.hidden = true;
    strip.hidden = true;
    detail.hidden = true;
    if (tally) tally.textContent = "--";
    return;
  }
  card.hidden = false;
  strip.hidden = false;
  $("stressRulesCounts").textContent = rulesTileFigure(rules);
  // The sheet opens on its own tally: the same served breach_counts figure
  if (tally) tally.textContent = rulesTileFigure(rules);
  renderRulesProvenance(rules);

  const order = Array.isArray(rules.ranked) && rules.ranked.length === rules.rules.length
    ? rules.ranked
    : rules.rules.map((_, i) => i);
  renderRulesTileState(rules, order);
  const brief = $("stressRulesBrief");
  brief.replaceChildren();
  let shown = 0;
  for (const ix of order) {
    const r = rules.rules[ix];
    if (!r || r.status === "pass") continue;
    if (shown >= 3) break;
    shown++;
    const pill = document.createElement("span");
    pill.className = `severity-pill stress-rules__pill ${ruleTone(r.status, r.mode)}`;
    const stateLabel = r.mode === "track" ? `Track · ${trackedRuleStatus(r.status, r.reason)}` : r.mode === "off" ? "Off" : ruleStatusLabel(r.status, r.reason);
    pill.textContent = `${r.number} · ${r.title} · ${stateLabel}`;
    pill.title = r.evidence || "";
    brief.appendChild(pill);
  }
  const noteParts = [];
  const eventNote = unknownEventRuleNote(rules);
  if (eventNote) noteParts.push(eventNote);
  const applicability = earningsApplicabilitySummary(rules);
  if (applicability) noteParts.push(applicability);
  const evidenceInfo = earningsHealthNotes(rules);
  if (evidenceInfo) noteParts.push(evidenceInfo);
  const wshNotice = wshEntitlementNotice(rules);
  if (wshNotice) noteParts.push(wshNotice);
  const degraded = (rules.input_health || []).filter((h) => h.status && h.status !== "ok");
  if (rules.status === "degraded" && degraded.length) {
    noteParts.push(`Inputs degraded (${degraded.map((h) => `${h.source}: ${h.status}`).join(", ")}) — unknown rows are not passes.`);
  }
  renderRulesNotes(noteParts, Boolean(eventNote));

  const button = $("stressRulesToggle");
  button.setAttribute("aria-expanded", state.rulesDetailOpen ? "true" : "false");
  button.textContent = state.rulesDetailOpen ? "Hide rules" : "Show rules";
  detail.hidden = !state.rulesDetailOpen;
  if (state.rulesDetailOpen) {
    renderRulesGrid(rules, order);
  }
}

// The sheet's provenance line: which Rulebook policy produced these rows and
// when. Served values verbatim; the line hides when the daemon serves none.
function renderRulesProvenance(rules = {}) {
  const el = $("rulesSheetProvenance");
  if (!el) return;
  const parts = [];
  if (rules.policy_id) {
    // Print the served id verbatim, adding "Rulebook" and the version only
    // when the id does not already carry them ("rulebook-v2" stays as is).
    const id = String(rules.policy_id);
    const v = rules.policy_version ? `v${rules.policy_version}` : "";
    const name = /rulebook/i.test(id) ? id : `Rulebook ${id}`;
    parts.push(v && id.toLowerCase().endsWith(v.toLowerCase()) ? name : `${name}${v ? ` ${v}` : ""}`);
  }
  const at = parseDate(rules.as_of);
  if (at) parts.push(`evaluated ${shortTimeWithZone(at.toISOString())}`);
  el.hidden = parts.length === 0;
  el.textContent = parts.join(" · ");
}

// The tile figure keeps notification policy separate from findings and data
// gaps. Track-mode rows remain visible but cannot create alert episodes.
function rulesTileFigure(rules = {}) {
  const rows = Array.isArray(rules.rules) ? rules.rules : [];
  const alerts = rows.filter((row) => (row?.mode || "alert") === "alert" && ["act", "watch"].includes(row?.status)).length;
  const monitorOnly = rows.filter((row) => row?.mode === "track" && ["act", "watch", "info"].includes(row?.status)).length;
  const dataGaps = rows.filter((row) => row?.mode !== "off" && row?.status === "unknown").length;
  const parts = [];
  if (alerts > 0) parts.push(`${alerts} alert-mode ${alerts === 1 ? "finding" : "findings"}`);
  if (monitorOnly > 0) parts.push(`${monitorOnly} monitor-only`);
  if (dataGaps > 0) parts.push(`${dataGaps} data ${dataGaps === 1 ? "gap" : "gaps"}`);
  return parts.join(" · ") || "No active findings";
}


// The Rules window states the worst flagged rule the daemon ranked — its own
// title, verbatim — over the served tally. An info-only day lamps nothing and
// shows the advisory dot instead, so "info" never reads as a breach.
function renderRulesTileState(rules, order) {
  const card = $("stressRulesCard");
  const caption = $("stressRulesState");
  const dot = $("stressRulesInfoDot");
  if (!card || !caption || !dot) return;
  const worst = order.map((ix) => rules.rules[ix]).find((rule) => rule && (rule.mode || "alert") === "alert" && ["act", "watch"].includes(rule.status));
  const status = String(worst?.status || "").toLowerCase();
  const active = order.map((ix) => rules.rules[ix]).filter((rule) => rule && rule.mode !== "off");
  const dataGaps = active.filter((rule) => rule.status === "unknown").length;
  const monitorOnly = active.filter((rule) => rule.mode === "track" && ["act", "watch", "info"].includes(rule.status)).length;
  const infoOnly = !worst && (dataGaps > 0 || monitorOnly > 0);
  applyTileSeverity(card, status === "act" ? "act" : status === "watch" ? "watch" : "");
  dot.hidden = !infoOnly;
  caption.textContent = worst ? cleanDetail(worst.title) : dataGaps > 0 ? "Data gaps" : monitorOnly > 0 ? "Monitor only" : "No findings";
  caption.title = worst
    ? `${worst.number} · ${worst.title} · ${ruleStatusLabel(worst.status, worst.reason)}`
    : dataGaps > 0
      ? `${dataGaps} active rule ${dataGaps === 1 ? "has" : "have"} unavailable input`
      : monitorOnly > 0
        ? `${monitorOnly} track-mode ${monitorOnly === 1 ? "finding is" : "findings are"} visible but cannot create alerts`
        : "No active Rulebook findings";
}


// The note builders return five independent degradation notices; joining them
function renderRulesNotes(parts, attention) {
  const trigger = $("stressRulesNotesToggle");
  const list = $("stressRulesNotesList");
  const dialog = $("stressRulesNotesDialog");
  if (!trigger || !list || !dialog) return;
  trigger.hidden = parts.length === 0;
  if (parts.length === 0) {
    if (dialog.open) dialog.close();
    list.replaceChildren();
    return;
  }
  $("stressRulesNotesLabel").textContent = `Data notes · ${parts.length}`;
  trigger.classList.toggle("stress-rules__notes-trigger--attention", attention);
  list.replaceChildren(...parts.map((part) => {
    const p = document.createElement("p");
    p.textContent = part;
    return p;
  }));
}

// The Rules sheet is a checklist, not a card deck: one dot-leader line per
// dot so it never reads as a breach. One element per rule, keeping the tone
// class, so "unknown" can still never render with the pass tone.
function renderRulesGrid(rules, order) {
  const grid = $("stressRulesGrid");
  if (!grid) return;
  const rows = [];
  for (const ix of order) {
    const r = rules.rules[ix];
    if (!r) continue;
    rows.push(ruleChecklistRow(r));
  }
  grid.replaceChildren(...rows);
}

function ruleChecklistRow(r) {
  const status = String(r.status || "").toLowerCase();
  const row = document.createElement("div");
  row.className = `pd-row rules-row ${ruleTone(r.status, r.mode)}`;
  row.dataset.ruleId = String(r.id || "");
  row.tabIndex = -1;
  if (state.alertEvidenceTarget?.kind === "rule" && state.alertEvidenceTarget.id === row.dataset.ruleId) {
    row.classList.add("is-alert-evidence-target");
    row.setAttribute("aria-current", "location");
  }
  if ((r.mode || "alert") === "alert" && (status === "act" || status === "watch")) row.classList.add(`rules-row--${status}`);
  if (status === "info") row.classList.add("rules-row--info");
  const line = document.createElement("span");
  line.className = "rules-row__line";
  const number = document.createElement("span");
  number.className = "rules-row__number";
  number.textContent = String(r.number ?? "--");
  const title = document.createElement("span");
  title.className = "rules-row__title";
  title.textContent = cleanDetail(r.title);
  const leader = document.createElement("span");
  leader.className = "rules-row__leader";
  leader.setAttribute("aria-hidden", "true");
  line.append(number, title, leader);
  if (status === "info") {
    const dot = document.createElement("i");
    dot.className = "rules-row__dot";
    dot.setAttribute("aria-hidden", "true");
    line.append(dot);
  }
  if (status === "pass") {
    const tick = document.createElement("i");
    tick.className = "rules-row__tick";
    tick.setAttribute("aria-hidden", "true");
    line.append(tick);
  }
  const statusWord = document.createElement("b");
  statusWord.className = "rules-row__status";
  statusWord.textContent = r.mode === "off" ? "off" : r.mode === "track" ? `track · ${trackedRuleStatus(r.status, r.reason)}` : ruleStatusLabel(r.status, r.reason);
  line.append(statusWord);
  row.append(line);
  if (status !== "pass") {
    const evidence = document.createElement("p");
    evidence.className = "rules-row__evidence";
    let text = r.evidence || "--";
    if (typeof r.observed === "number" && typeof r.threshold === "number") {
      text += ` (observed ${r.observed} vs ${r.threshold}${r.unit ? " " + r.unit : ""})`;
    }
    evidence.textContent = text;
    row.append(evidence);
    const meter = ruleMeasureMeter(r);
    if (meter) row.append(meter);
  }
  const offenders = (r.offenders || []).slice(0, 3);
  if (offenders.length) {
    const list = document.createElement("p");
    list.className = "stress-rules__offenders";
    list.textContent = offenders.map((o) => (o.leg || o.symbol) + (o.note ? ` — ${o.note}` : "")).join(" · ");
    row.append(list);
  }
  return row;
}

// ruleMeasureMeter draws observed against the rule's own served threshold: a
// thin track scaled to max(2× threshold, observed), with a tick at the
// threshold. It renders only served numbers — no invented policy — and only
// for non-pass rows, so the sheet answers "how far past the line?" at a
// glance without adding noise to clean rows.
function ruleMeasureMeter(r = {}) {
  const observed = r.observed;
  const threshold = r.threshold;
  if (typeof observed !== "number" || typeof threshold !== "number" || threshold <= 0 || observed < 0) return null;
  const scale = Math.max(threshold * 2, observed * 1.05);
  const meter = document.createElement("div");
  meter.className = "rules-row__meter";
  // The evidence line above already speaks the numbers; the meter is the
  // picture of them, so screen readers must not hear the pair twice.
  meter.setAttribute("aria-hidden", "true");
  const fill = document.createElement("span");
  fill.className = "rules-row__meter-fill";
  fill.style.width = `${Math.min(100, (observed / scale) * 100)}%`;
  const tick = document.createElement("span");
  tick.className = "rules-row__meter-tick";
  tick.style.left = `${(threshold / scale) * 100}%`;
  meter.append(fill, tick);
  return meter;
}

function renderStressDetail(stress, snap = state.snapshot || {}) {
  const panel = $("stressDetailPanel");
  const button = $("stressDetailToggle");
  panel.hidden = !state.stressDetailOpen;
  button.textContent = state.stressDetailOpen ? "Hide detail" : "Show detail";
  button.setAttribute("aria-expanded", String(state.stressDetailOpen));
  if (!state.stressDetailOpen) return;

  $("stressDetailGrid").replaceChildren(...stressExplanationCards(stress, snap).map(detailCard));
  renderHeldStress(stress);

  const rows = stressDriverRows(stress);
  $("stressDrivers").replaceChildren(...(rows.length > 0 ? rows.map(stressDriverRow) : [stressEmptyDriverRow()]));
}

function stressDriverRows(stress) {
  const rows = Array.isArray(stress.rows) ? stress.rows : [];
  const detailRows = rows.filter((row) => cleanDetail(row.title).toLowerCase() !== "portfolio stress");
  const active = detailRows
    .filter(stressRowNeedsAttention)
    .map((row, index) => ({ row, index }))
    .sort((a, b) => stressDriverPriority(a.row) - stressDriverPriority(b.row) || a.index - b.index)
    .map((item) => item.row);
  return (active.length > 0 ? active : detailRows).slice(0, 5);
}

function stressRowNeedsAttention(row = {}) {
  const severity = String(row.severity || "").toLowerCase();
  const direction = String(row.direction || "").toLowerCase();
  return ["urgent", "act", "watch"].includes(severity) ||
    ["defensive", "rebalance", "data_quality"].includes(direction);
}

function stressDriverPriority(row = {}) {
  const severity = String(row.severity || "").toLowerCase();
  const direction = String(row.direction || "").toLowerCase();
  const title = cleanDetail(row.title).toLowerCase();
  if (severity === "urgent") return 0;
  if (severity === "act") return 1;
  if (direction === "data_quality" || title.includes("ambiguity") || title.includes("data quality")) return 2;
  if (title.includes("exposure") || title.includes("concentration")) return 3;
  if (severity === "watch") return 4;
  return 9;
}

function stressDriverRow(row = {}) {
  const item = document.createElement("div");
  item.className = "driver-row " + stressDriverTone(row);
  const label = document.createElement("span");
  label.textContent = stressDriverLabel(row);
  const title = document.createElement("b");
  title.textContent = row.title || "Stress driver";
  const body = document.createElement("p");
  body.textContent = [row.guidance, row.evidence ? `Evidence: ${row.evidence}` : ""].filter(Boolean).join(" ");
  item.append(label, title, body);
  return item;
}

function stressEmptyDriverRow() {
  const item = document.createElement("div");
  item.className = "driver-row neutral";
  const label = document.createElement("span");
  label.textContent = "Context";
  const title = document.createElement("b");
  title.textContent = "No active stress drivers";
  const body = document.createElement("p");
  body.textContent = "The current snapshot has no warning, action, or data-quality rows to review.";
  item.append(label, title, body);
  return item;
}

function stressDriverTone(row = {}) {
  const severity = String(row.severity || "").toLowerCase();
  const direction = String(row.direction || "").toLowerCase();
  if (["urgent", "act"].includes(severity)) return "risk";
  if (severity === "watch" || ["defensive", "rebalance", "data_quality"].includes(direction)) return "warn";
  if (severity === "observe") return "neutral";
  return "neutral";
}

function stressDriverLabel(row = {}) {
  const severity = String(row.severity || "").toLowerCase();
  const direction = String(row.direction || "").toLowerCase();
  if (direction === "data_quality") return "Data quality";
  if (direction === "rebalance") return "Rebalance";
  if (severity === "urgent") return "Urgent";
  if (severity === "act") return "Act";
  if (severity === "watch") return "Watch";
  return "Context";
}

function stressExplanationCards(stress, snap = state.snapshot || {}) {
  return [
    marketExplanation(stress),
    portfolioExplanation(stress, snap),
  ];
}

// The Stress window: served severity as the caption, served action and the
function renderStressStatus(stress, snap = state.snapshot || {}) {
  const fault = sourceTransportFault(snap, "stress");
  applyTileSeverity($("stressHero"), fault ? "stale" : String(stress.severity || "").toLowerCase());
  const severityLabel = fault ? faultCaption(fault) : labelize(stress.severity || "--");
  $("stressSeverity").textContent = severityLabel;
  // The action slot earns its line only when the stage differs from the
  // severity caption above it ("Defend", "Rebalance"); a repeated "Watch"
  // under "Watch" is a stutter, not information.
  const stage = stressStageLabel(stress);
  const action = $("stressAction");
  action.textContent = stage.toLowerCase() === severityLabel.toLowerCase() ? "" : stage;
  action.hidden = action.textContent === "";
  const summary = $("stressSummary");
  const full = stressSummaryText(stress, snap);
  if (fault) {
    summary.textContent = staleFigure(stressCushionFigure(stress), fault.asOf);
    summary.title = `${full} (${fault.reason}; last good ${indicatorAsOfLabel(fault.asOf)})`;
    return;
  }
  const figure = stressHeroFigure(stress);
  const cushion = stressCushionFigure(stress);
  summary.textContent = figure;
  summary.title = figure === cushion ? full : `${full} — ${cushion}`;
}


// The figure names the served binding driver: primary_drivers is the daemon's
function stressHeroFigure(stress = {}) {
  return stressLeadDriverFigure(stress) || stressCushionFigure(stress);
}

function stressLeadDriverFigure(stress = {}) {
  const p = stress.portfolio || {};
  const readings = {
    gross_delta_high: () => stressDriverReading("gross delta", p.gross_delta_pct_nlv),
    net_delta_high: () => stressDriverReading("net delta", p.net_delta_pct_nlv),
    gross_exposure_high: () => stressDriverReading("gross", p.gross_exposure_pct_nlv),
    single_name_delta_high: () => stressDriverReading(`${normalizeSymbol(p.largest_delta_exposure) || "top name"} delta`, p.largest_delta_pct_nlv),
    single_name_exposure_high: () => stressDriverReading(normalizeSymbol(p.largest_exposure) || "top name", p.largest_exposure_pct_nlv),
    margin_cushion_low: () => stressCushionFigure(stress),
    lookahead_cushion_low: () => stressCushionFigure(stress),
  };
  for (const id of stress.primary_drivers || []) {
    const reading = readings[String(id || "").trim().toLowerCase()]?.();
    if (reading) return reading;
  }
  return "";
}

function stressDriverReading(label, value) {
  if (typeof value !== "number") return "";
  return `${label} ${wholePct(value)} NLV`;
}


// One fault vocabulary for every dead window: "Fault" plus the served word
// that explains it, and never a word this renderer invented.
function faultCaption(fault = null) {
  const reason = cleanDetail(fault?.reason);
  return reason === "--" ? "No data" : `Fault · ${reason}`;
}


// A dead window keeps its last-good figure and stamps it with the served
// as-of, so an old number can never be mistaken for a current one.
function staleFigure(reading, asOf) {
  const at = indicatorAsOfLabel(asOf);
  if (!reading || reading === "--") return at === "--" ? "Reading pending" : `Last ${at}`;
  return at === "--" ? reading : `${reading} · last ${at}`;
}


// sourceTransportFault reads the app snapshot's served per-source transport
// unavailable. The lamp-test stamp names exactly these faults, so a window
function sourceTransportFault(snap = {}, name) {
  const meta = snap.sources?.[name];
  if (!meta) return null;
  const transport = String(meta.state || "").trim().toLowerCase();
  if (!meta.error && transport !== "stale" && transport !== "unavailable") return null;
  return {
    reason: cleanDetail(meta.reason || transport || "unavailable"),
    asOf: meta.last_success_at || "",
  };
}


// The cushion reading against its served trip: the stress policy's own watch
// threshold this renderer invented would be policy the daemon never published.
function stressCushionFigure(stress = {}) {
  const cushion = stress.portfolio?.cushion_pct;
  if (typeof cushion !== "number") return "cushion pending";
  const trip = stress.portfolio?.cushion_trip_pct;
  const reading = `cushion ${wholePct(cushion)}`;
  return typeof trip === "number" ? `${reading} · trips <${wholePct(trip)}` : reading;
}

function stressStageLabel(stress) {
  const action = String(stress.action || "").toLowerCase();
  if (action === "defend") return "Defend";
  if (action === "rebalance") return "Rebalance";
  if (action === "confirm_inputs") return "Check data";
  if (action === "stand_down") return "Stand down";
  const severity = String(stress.severity || "").toLowerCase();
  if (severity === "act") return "Defend";
  if (severity === "watch") return "Watch";
  if (severity === "observe") return "Stand down";
  return labelize(stress.action || "--");
}


// First sentence or semicolon-clause of a summary, with terminal punctuation
function firstClause(text) {
  const s = String(text || "").trim();
  const m = s.match(/^[^.;]*[.;]/);
  if (!m) return s;
  return m[0].replace(/;$/, ".");
}

function stressSummaryText(stress, snap = {}) {
  const fallback = stress.summary || "Waiting for stress snapshot.";
  if (stressHasProvisionalOnlyMarketWarning(stress)) {
    const fit = String(stress.portfolio_fit || "").toLowerCase();
    const exposure = ["high", "medium"].includes(fit) ? " and portfolio exposure is elevated" : "";
    return `Early market warning, not confirmed yet${exposure}; review evidence before treating this as confirmed stress.`;
  }
  if (!stressInputCheckBlocksAction(stress)) return fallback;

  const verdict = cleanDetail(stress.market?.regime_posture?.label || stress.market?.regime_verdict);
  const prefix = verdict === "--" ? "Market read" : verdict;
  const issues = stressInputIssueSummary(stress, snap);
  const issueLine = issues ? `check ${issues}` : "check input health";
  const confirmation = String(stress.market_confirmation || "").toLowerCase();
  const actionLine = confirmation === "confirmed"
    ? "verify before escalation."
    : "no market-stress action.";
  return `${prefix}; ${issueLine} before treating the stress read as a market signal; ${actionLine}`;
}

function stressGovernedHold(stress = {}) {
  const posture = stress.market?.regime_posture || {};
  return String(posture.stage || "").toLowerCase() === "confirmed_stress" &&
    String(posture.severity || "").toLowerCase() === "watch";
}

function stressHasProvisionalOnlyMarketWarning(stress) {
  const market = stress.market || {};
  return String(stress.market_confirmation || "").toLowerCase() === "partial" &&
    Number(market.eligible_red_clusters || 0) === 0 &&
    Array.isArray(market.unconfirmed_red_cluster_names) &&
    market.unconfirmed_red_cluster_names.length > 0;
}

function stressNeedsInputCheck(stress) {
  const inputHealth = String(stress.input_health || "").toLowerCase();
  return stressInputCheckBlocksAction(stress) ||
    ["warming", "degraded", "failed"].includes(inputHealth);
}

function stressInputCheckBlocksAction(stress) {
  const action = String(stress.action || "").toLowerCase();
  const direction = String(stress.direction || "").toLowerCase();
  const planner = String(stress.planner_mode_hint || "").toLowerCase();
  const readiness = String(stress.planner_readiness || "").toLowerCase();
  return action === "confirm_inputs" ||
    planner === "confirm_data" ||
    direction === "data_quality" ||
    readiness === "blocked";
}

function marketExplanation(stress) {
  const confirmation = String(stress.market_confirmation || "").toLowerCase();
  if (confirmation === "confirmed") {
    return {
      label: "Market",
      title: "Stress is confirmed",
      body: "Independent market signals agree. Treat this as real pressure, not one noisy input.",
      tone: "risk",
    };
  }
  if (confirmation === "partial") {
    if (stressHasProvisionalOnlyMarketWarning(stress)) {
      const names = humanList((stress.market?.unconfirmed_red_cluster_names || []).map(clusterInputLabel), 3);
      return {
        label: "Market",
        title: "Provisional warning",
        body: `${names || "One market signal"} needs confirmation or fresher data. Treat this as watch context, not confirmed stress.`,
        tone: "warn",
      };
    }
    if (stressGovernedHold(stress)) {
      // Served fact, not reinterpretation: stage confirmed_stress with
      // must agree with the amber Regime panel above it.
      return {
        label: "Market",
        title: cleanDetail(stress.market?.regime_posture?.label) !== "--" ? stress.market.regime_posture.label : "Confirmed stress held at watch",
        body: "Regime policy holds this confirmed stage at watch; treat it as watch-grade pressure, not act-grade stress.",
        tone: "warn",
      };
    }
    return {
      label: "Market",
      title: "Pressure is developing",
      body: "Some signals are warning, but confirmation is incomplete. Watch before taking major action.",
      tone: "warn",
    };
  }
  const posture = normalizeRegimePosture(stress.market?.regime_posture) || {
    label: cleanDetail(stress.market?.regime_verdict),
    tone: legacyRegimeTone(stress.market?.regime_verdict),
  };
  const verdict = cleanDetail(posture.label || stress.market?.regime_verdict);
  // Trust the server's posture.tone outright — same pattern renderMarketWeather
  // to "warn" locally whenever it saw a data gap, which is exactly the kind
  const tone = regimePostureDetailTone(posture);
  const hasGaps = marketHasDataGaps(stress.market || {}) ||
    ["blocked", "degraded", "failed", "partial", "warming"].includes(String(posture.readiness || "").toLowerCase()) ||
    String(posture.tone || "").toLowerCase() === "data_quality";
  const body = tone === "warn" || hasGaps
    ? "Market stress is not confirmed, but the regime read has watch or data-quality warnings."
    : "The broad-market regime is not giving a fully confirmed stress signal.";
  return {
    label: "Market",
    title: verdict === "--" ? "No clear market stress" : verdict,
    body,
    tone,
  };
}

function regimePostureDetailTone(posture = {}) {
  switch (regimeWeatherClass(posture.tone)) {
    case "red":
      return "risk";
    case "amber":
      return "warn";
    case "green":
      return "ok";
    default:
      return "neutral";
  }
}

function portfolioExplanation(stress, snap = state.snapshot || {}) {
  const fit = String(stress.portfolio_fit || "").toLowerCase();
  const heldStress = heldStressItems(stress);
  const heldStressLine = heldStress.length > 0 ? ` Held stress: ${heldStressSummary(heldStress, 2)}.` : "";
  const protectionLine = protectionCoverageStressLine(stress, snap);
  if (fit === "high") {
    const confirmed = String(stress.market_confirmation || "").toLowerCase() === "confirmed";
    const severity = String(stress.severity || "").toLowerCase();
    return {
      label: "Portfolio",
      title: "Portfolio is exposed",
      body: confirmed
        ? "The current portfolio shape is vulnerable to the confirmed market stress." + heldStressLine + protectionLine
        : "The portfolio is vulnerable if the warning firms; this is exposure context until market stress confirms." + heldStressLine + protectionLine,
      tone: confirmed && ["act", "urgent"].includes(severity) ? "risk" : "warn",
    };
  }
  if (fit === "medium") {
    return {
      label: "Portfolio",
      title: "Exposure is meaningful",
      body: "The portfolio has some sensitivity to the current stress. Size changes carefully." + heldStressLine + protectionLine,
      tone: "warn",
    };
  }
  if (heldStress.length > 0) {
    return {
      label: "Portfolio",
      title: "Held-name stress",
      body: heldStressSummary(heldStress, 2) + protectionLine,
      tone: "warn",
    };
  }
  return {
    label: "Portfolio",
    title: fit === "low" ? "Exposure looks contained" : cleanDetail(stress.portfolio?.largest_exposure),
    body: "The current portfolio shape is not the main reason for a defensive stress action." + protectionLine,
    tone: "ok",
  };
}

function protectionCoverageStressLine(stress = {}, snap = state.snapshot || {}) {
  const coverage = stressProtectionCoverageFor(snap, stress);
  if (!protectionCoverageHasData(coverage)) return "";
  const baseCurrency = protectionCoverageBaseCurrency(coverage, snap.account?.base_currency || "");
  const headline = protectionCoverageHeadline(coverage, baseCurrency, { sensitive: true });
  const largest = protectionCoverageLargestText(coverage, baseCurrency, { sensitive: true });
  const stale = protectionCoverageStaleText(coverage);
  const parts = [`Protection coverage: ${headline}`];
  if (largest) parts.push(`largest unprotected ${largest}`);
  if (stale) parts.push(stale.replace(/\.$/, ""));
  return ` ${parts.join("; ")}.`;
}

function renderStressTimestamp(stress) {
  renderFreshnessTimestamp("stressAsOf", stress.as_of, { staleMinutes: 5, compact: true, quietWhenFresh: true });
  reconcileSignalPanelTimes();
}


// The Market & Portfolio head shows two freshness spans (regime + stress).
function reconcileSignalPanelTimes() {
  const regime = $("regimeAsOf");
  const stress = $("stressAsOf");
  if (!regime || !stress) return;
  const duplicate = regime.textContent === stress.textContent;
  stress.hidden = stress.hidden || duplicate;
  // The separator only earns ink when both sides render text (quiet-when-
  const sep = stress.parentElement?.querySelector(".panel-time-sep");
  if (sep) sep.hidden = duplicate || regime.hidden || stress.hidden || !regime.textContent;
}

function renderMarketContext(snap) {
  const stress = snap.stress || {};
  const market = stress.market || {};
  const quotes = snap.market_quotes?.quotes || {};
  const strip = $("marketQuoteStrip");
  const symbols = ["SPY", "VIX", "QQQ"];
  strip.replaceChildren(...symbols.map((symbol) => marketQuoteCell(symbol, quoteBySymbol(quotes, symbol), market, snap.market_quotes, snap.market_calendar)));
}

function marketQuoteCell(symbol, quote, market, marketQuotes, marketCalendar) {
  const fallback = marketQuoteFallback(symbol, market);
  const price = quotePrice(quote) ?? fallback.price;
  const change = quoteChangePct(quote) ?? fallback.changePct;
  const error = marketQuotes?.errors?.[symbol] || "";
  const closed = Boolean(error) && marketQuoteSessionClosed(marketCalendar);
  const hasPrice = typeof price === "number";
  const cell = document.createElement("div");
  cell.className = "market-quote-cell";
  cell.classList.toggle("market-quote-cell--missing", !hasPrice);
  if (error && !closed) cell.classList.add("market-quote-cell--error");
  cell.setAttribute("aria-label", `${symbol} ${hasPrice ? numberRead(price) : "price pending"} ${typeof change === "number" ? signedPct(change) : "change pending"}`);

  const head = document.createElement("div");
  head.className = "market-quote-cell__head";
  const label = document.createElement("b");
  label.textContent = symbol;
  head.append(label);

  const valueLine = document.createElement("div");
  valueLine.className = "market-quote-cell__value";
  const value = document.createElement("strong");
  value.textContent = hasPrice ? numberRead(price) : "--";
  const changeEl = document.createElement("span");
  changeEl.className = "market-change " + marketQuoteChangeClass(symbol, change);
  changeEl.textContent = typeof change === "number" ? signedPct(change) : "--";
  valueLine.append(value, changeEl);

  const source = document.createElement("small");
  source.className = "market-quote-cell__source" + (error && !closed ? " error" : "");
  source.textContent = error
    ? closed ? "Closed" : marketQuoteInterruptedLine(quote, marketQuotes, hasPrice)
    : marketQuoteSourceLine(quote, marketQuotes, fallback.source);
  source.title = error
    ? closed ? "Selected market session is closed" : `${marketQuoteErrorLabel(error)}; ${hasPrice ? "showing last available quote" : "no frozen quote available yet"}`
    : source.textContent;
  cell.append(head, valueLine, source);
  return cell;
}

function marketQuoteSessionClosed(calendar) {
  const session = calendar?.session;
  const sessionState = String(session?.state || "").toLowerCase();
  return Boolean(session) && Boolean(sessionState) && session.is_open === false && sessionState !== "unknown";
}

function marketQuoteChangeClass(symbol, change) {
  return signedClass(normalizeSymbol(symbol) === "VIX" && typeof change === "number" ? -change : change);
}

function marketQuoteInterruptedLine(quote, marketQuotes, hasPrice) {
  const at = quoteTimestamp(quote) || marketQuotes?.as_of || "";
  const atLabel = at ? ` · ${quoteTime(at)}` : "";
  return hasPrice ? `Frozen${atLabel}` : "Feed issue";
}

function marketQuoteFallback(symbol, market = {}) {
  switch (symbol) {
    case "SPY":
      return { price: market.spy_price, changePct: market.spy_change_pct, source: "stress read" };
    case "QQQ":
      return {
        price: firstNumber(market.qqq_price, market.ndx_price, market.nasdaq_price, market.nasdaq_100_price),
        changePct: firstNumber(market.qqq_change_pct, market.ndx_change_pct, market.nasdaq_change_pct, market.nasdaq_100_change_pct),
        source: "stress read",
      };
    case "VIX":
      return { price: market.vix, changePct: market.vix_change_pct, source: "stress read" };
    default:
      return { price: null, changePct: null, source: "IBKR quote pending" };
  }
}

function marketQuoteSourceLine(quote, marketQuotes, fallback) {
  const parts = [];
  const quality = String(quote?.quote_quality || "").trim();
  const dataType = String(quote?.data_type || "").trim();
  if (quality && quality !== "firm") parts.push(labelize(quality));
  if (dataType && dataType !== "live") parts.push(labelize(dataType));
  const uniqueParts = [...new Set(parts)];
  // A healthy live quote is the default state; naming the source 6× across
  // the rail is noise. The label only appears when there is no quote yet;
  if (uniqueParts.length === 0 && !quote) uniqueParts.push(fallback || "Quote pending");
  const at = quote?.quote_price_at || quote?.price_at || quote?.as_of || marketQuotes?.as_of;
  if (at) uniqueParts.push(quoteTime(at));
  return uniqueParts.join(" · ");
}

function quoteTime(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
}

function renderRegimePanel(snap) {
  const stress = snap.stress || {};
  const market = stress.market || {};
  const indicators = stress.market_indicators || [];
  const posture = regimePosture(snap, stress, market);
  const authority = regimeAuthorityView(snap);
  const regimeStatus = marketRegimeStatusLine(snap, stress, market, indicators);
  $("marketRegime").textContent = regimeAuthorityLabel(posture, authority);
  const summary = $("marketRegimeSummary");
  const subline = masterSubline(snap, stress);
  summary.textContent = subline;
  // The subline clamps; its title keeps the full text plus the freshness or
  // authority explanation behind it, so nothing disclosed here is truncated
  summary.title = [subline, regimeStatus.title || regimeStatus.detail || regimeStatus.summary]
    .filter(Boolean).join(" — ");
  applyTileSeverity($("masterAnnunciator"), masterSeverity(snap, stress));
  renderLampTest(snap, stress);
  // marketRegimeMix now lives in the expanded detail deck and shows only the
  const governedNote = regimeGovernedNote(snap, market);
  const mixNote = $("marketRegimeMix");
  mixNote.hidden = !governedNote;
  if (governedNote) {
    mixNote.textContent = governedNote;
    mixNote.title = governedNote;
  }
  renderRegimeAuthorityTimestamp(snap, latestRegimeTimestamp(stress, indicators));
  reconcileSignalPanelTimes();
  renderMarketWeather(regimePresentationPosture(posture, authority));
  renderRegimeGrid(snap, stress);
  renderRegimeDetail(indicators, snap, stress);
}


// Severity ranking for the annunciator lamps. Only the daemon's own severity
const SEVERITY_RANK = { observe: 1, watch: 2, act: 3, urgent: 4 };

function severityRank(value) {
  return SEVERITY_RANK[String(value || "").trim().toLowerCase()] || 0;
}

function worstSeverity(...values) {
  return values.reduce((worst, value) => (severityRank(value) > severityRank(worst) ? value : worst), "");
}


// masterSeverity is the worst severity the daemon serves across the two
// would be a lie, so the master takes the maximum; it never derives a
function masterSeverity(snap = {}, stress = {}) {
  const regime = snap.regime || {};
  return String(worstSeverity(
    stress.severity,
    regime.posture?.severity,
    regime.lifecycle?.severity,
  ) || "").toLowerCase();
}


// applyTileSeverity is the one place a severity word becomes a lamp class.
// The nominal lamp is opt-in. Every other caller here passes the severity
// vocabulary (observe/watch/act/urgent), which has no healthy word in it, so a
// tile that cannot positively measure health must keep reading unlit rather
// than acquire a green lamp from a band word that drifts into its input.
function applyTileSeverity(el, severity, { nominal = false } = {}) {
  if (!el) return;
  el.classList.remove("pd-tile--ok", "pd-tile--watch", "pd-tile--act", "pd-tile--info", "pd-tile--stale");
  const key = String(severity || "").trim().toLowerCase();
  if (key === "watch" || key === "yellow") el.classList.add("pd-tile--watch");
  else if (key === "act" || key === "urgent" || key === "red") el.classList.add("pd-tile--act");
  else if (key === "info") el.classList.add("pd-tile--info");
  else if (key === "stale") el.classList.add("pd-tile--stale");
  else if (nominal && key === "green") el.classList.add("pd-tile--ok");
}


// masterSubline answers the operator's immediate question: what decision is
// still allowed, and what evidence may not be used. Diagnostic lifecycle words
// stay in the detail and lamp-test surfaces rather than becoming a word salad
// under the master verdict.
function masterSubline(snap = {}, stress = {}) {
  const action = stressStageLabel(stress);
  const severity = masterSeverity(snap, stress);
  const dark = REGIME_CLUSTERS
    .filter((cluster) => regimeClusterBand(cluster, snap, stress) === "stale");
  const dataQualityDecision = masterDataQualityDecision(snap, stress, action, dark);
  // Action and severity often share a word ("Watch"/"watch"); printing both
  // reads as a stutter, so the severity only appears when it adds information.
  const governed = regimeGovernedNote(snap, stress.market || {});
  const parts = dataQualityDecision
    ? [dataQualityDecision]
    : governed
      ? [governed]
      : [action === "--" ? "" : action, severity.toLowerCase() === action.toLowerCase() ? "" : severity];
  // Every cluster the daemon ranks now has a window, so this clause fires
  // only for a red the panel genuinely cannot show: a cluster name the served
  // appear must still be named rather than silently dropped.
  const reds = offPanelRedClusters(stress);
  if (reds.length > 0) parts.push(`${reds.length} red: ${reds.join(", ")}`);
  // A dead window under a quiet master is the same silent disagreement as a
  if (!dataQualityDecision && dark.length > 0) {
    parts.push(`${dark.map((cluster) => cluster.legend.toLowerCase()).join(", ")} dark`);
  }
  const timing = cleanDetail(snap.regime?.lifecycle?.timing);
  if (!dataQualityDecision && timing !== "--") parts.push(labelize(timing).toLowerCase());
  return parts.filter(Boolean).join(" · ");
}


// When market evidence is incomplete but the portfolio planner remains ready,
// the two authorities must not collapse into one ambiguous "watch". The user
// may follow the portfolio action; the unavailable market signal may not be
// used to time, escalate, or dismiss it.
function masterDataQualityDecision(snap = {}, stress = {}, action = "", dark = []) {
  const lifecycle = snap.regime?.lifecycle || {};
  const posture = snap.regime?.posture || stress.market?.regime_posture || {};
  const dataQuality = [lifecycle.stage, lifecycle.timing, posture.stage, posture.tone]
    .some((value) => String(value || "").trim().toLowerCase() === "data_quality");
  if (!dataQuality || dark.length === 0) return "";
  const sources = humanList(dark.map((cluster) => cluster.legend), 3);
  if (["Rebalance", "Defend"].includes(action) && !stressInputCheckBlocksAction(stress)) {
    return `${action} based on portfolio risk · Market signal unavailable until ${sources} recover`;
  }
  if (stressInputCheckBlocksAction(stress)) {
    return `No market-stress action · Wait for ${sources} to recover`;
  }
  return "";
}


// offPanelRedClusters lists served red cluster names that no window on this
function offPanelRedClusters(stress = {}) {
  return (stress.market?.red_cluster_names || []).filter((name) => {
    const key = String(name || "").trim().toLowerCase();
    return key && !REGIME_CLUSTERS.some((cluster) => cluster.sources.includes(key));
  });
}


// The lamp test is the panel's own self-report: how many served feeds are
// stress source-health entries (the instrument's feeds); app-transport
// failures are named as faults but never silently change the feed count.
function renderLampTest(snap = {}, stress = {}) {
  const stamp = $("lampTestStamp");
  const line = $("lampTest");
  if (!stamp || !line) return;
  const health = lampTestSources(snap, stress);
  line.hidden = health.faults.length === 0 && health.inherited.length === 0;
  if (line.hidden) {
    const dialog = $("lampTestDialog");
    if (dialog?.open) dialog.close();
  }
  const at = parseDate(snap.updated_at);
  const when = at ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false }) : "--";
  stamp.replaceChildren();
  const lead = document.createElement("span");
  lead.textContent = `Last snapshot ${when} · `;
  const count = document.createElement("b");
  count.className = health.faults.length > 0 ? "pd-dimcount" : "";
  count.textContent = `${health.ok}/${health.total} sources ok`;
  stamp.append(lead, count);
  const noteParts = health.faults.length > 0 ? health.faults : health.inherited;
  if (noteParts.length > 0) {
    const note = document.createElement("span");
    note.className = "pd-stale-note";
    note.textContent = ` · ${humanList(noteParts, 2)}`;
    stamp.append(note);
  }
  const detail = [...health.faults, ...health.inherited];
  stamp.title = detail.length > 0
    ? `Served source health: ${health.ok} of ${health.total} ok; ${detail.join(", ")}`
    : `Served source health: ${health.ok} of ${health.total} ok`;
}

function lampTestSources(snap = {}, stress = {}) {
  const seen = new Map();
  for (const source of [...(snap.regime?.source_health || []), ...(stress.source_health || [])]) {
    const name = String(source?.source || "").trim().toLowerCase();
    if (!name || seen.has(name)) continue;
    seen.set(name, source);
  }
  const faults = [];
  const inherited = [];
  let ok = 0;
  let total = 0;
  for (const [name, source] of seen) {
    const status = String(source?.status || "").trim().toLowerCase();
    const derivedFrom = Array.isArray(source?.derived_from) ? source.derived_from : [];
    if (derivedFrom.length > 0) {
      // A served aggregate row inheriting its inputs' health is not an
      // extra failed source: name the leaves it inherits instead of
      // counting one cause twice.
      if (status && status !== "ok") {
        inherited.push(`${clusterInputLabel(name)} ${status} — inherits ${humanList(derivedFrom.map(clusterInputLabel), 3)}`);
      }
      continue;
    }
    total++;
    if (!status || status === "ok") {
      ok++;
      continue;
    }
    faults.push(`${clusterInputLabel(name)} ${status}`);
  }
  // App-transport failures arrive two ways: a served error string, or a served
  // and brief report a failed poll). Reading only the first left a dead window
  for (const [name, meta] of Object.entries(snap.sources || {})) {
    const transport = String(meta?.state || "").trim().toLowerCase();
    if (!meta?.error && transport !== "unavailable" && transport !== "stale") continue;
    faults.push(`${snapshotSourceName(name)} ${meta?.error || transport === "unavailable" ? "unavailable" : "stale"}`);
  }
  return { ok, total, faults: [...new Set(faults)], inherited: [...new Set(inherited)] };
}

function snapshotSourceName(name) {
  const key = String(name || "").trim().toLowerCase();
  return key === "market_quotes" ? "market quotes" : clusterInputLabel(key);
}


// The six regime windows, in fixed positions — one per cluster the daemon
const REGIME_CLUSTERS = [
  { key: "breadth", legend: "Breadth", sources: ["breadth"], match: ["breadth"] },
  { key: "vol", legend: "Volatility", sources: ["vol", "volatility"], match: ["vix", "vol"] },
  { key: "credit", legend: "Credit", sources: ["credit"], match: ["credit", "hyg", "oas", "hy/ig"] },
  { key: "gamma", legend: "Dealer gamma", sources: ["gamma"], match: ["gamma", "γ-zero"] },
  { key: "funding", legend: "Funding", sources: ["funding"], match: ["funding"] },
  { key: "fx", legend: "FX", sources: ["fx"], match: ["usd/jpy", "jpy"] },
];

function renderRegimeGrid(snap = {}, stress = {}) {
  const grid = $("regimeSummaryCard");
  if (!grid) return;
  grid.replaceChildren(...REGIME_CLUSTERS.map((cluster) => regimeClusterTile(cluster, snap, stress)));
}

function regimeClusterTile(cluster, snap = {}, stress = {}) {
  const band = regimeClusterBand(cluster, snap, stress);
  const fault = band === "stale" ? clusterFault(cluster, snap, stress) : null;
  const lead = clusterLeadIndicator(cluster, stress);
  const tile = document.createElement("div");
  tile.className = "pd-tile";
  // A window the daemon measured and called green is a state, not the absence
  // of one, so this is the one grid that asks for the nominal lamp. Muted now
  // means exactly one thing: no band could be read.
  applyTileSeverity(tile, band, { nominal: true });
  const bar = document.createElement("span");
  bar.className = "pd-tile__bar";
  bar.setAttribute("aria-hidden", "true");
  const legend = document.createElement("span");
  legend.className = "pd-tile__legend";
  legend.textContent = cluster.legend;
  const cap = document.createElement("b");
  cap.className = "pd-tile__cap";
  cap.textContent = clusterCaption(lead, band, fault);
  const fig = document.createElement("div");
  fig.className = "pd-tile__fig";
  fig.textContent = clusterFigure(lead, band, fault);
  tile.append(bar, legend, cap, fig);
  // Third line: the served trip anchor, engraved beneath the reading so the
  // serves no trip stays figure-only — this renderer never supplies a cutoff.
  const trip = clusterTrip(lead, band);
  if (trip) {
    const anchor = document.createElement("div");
    anchor.className = "pd-tile__trip";
    anchor.textContent = trip;
    tile.append(anchor);
  }
  tile.title = [lead.name, lead.reading, trip, lead.comment, indicatorAsOfLabel(lead.as_of || fault?.asOf)]
    .map((part) => humanizeStalenessSeconds(cleanDetail(part)))
    .filter((part) => part && part !== "--")
    .join(" · ");
  tile.setAttribute("aria-label", `${cluster.legend} ${clusterCaption(lead, band, fault)}`);
  return tile;
}


// The trip anchor is served text: the daemon's own compact trigger wording
function clusterTrip(lead = {}, band = "") {
  if (band === "stale") return "";
  const trip = cleanDetail(lead.trip);
  return trip === "--" ? "" : trip;
}


// The caption is served text: the indicator's own worded comment, falling
// the daemon never published.
function clusterCaption(lead = {}, band = "", fault = null) {
  if (band === "stale") return faultCaption(fault);
  for (const source of [lead.comment, lead.reading]) {
    const clause = leadingClause(source);
    if (clause) return clause;
  }
  return labelize(band || "no read");
}


// The caption is one clause of served text, without the sentence punctuation
function leadingClause(value) {
  const text = firstClause(humanizeStalenessSeconds(cleanDetail(value))).replace(/[.;,]+$/, "").trim();
  return text === "--" ? "" : text;
}

function clusterFigure(lead = {}, band = "", fault = null) {
  const reading = humanizeStalenessSeconds(cleanDetail(lead.reading));
  // Dead window: keep the last-good figure and stamp it with the served
  if (band === "stale") return staleFigure(reading, lead.as_of || fault?.asOf);
  if (reading !== "--") return reading;
  return "Reading pending";
}


// The daemon's own data-quality cluster lists, each with the word it puts on
// the window when the cluster is named in it. The word is the daemon's, never
const CLUSTER_FAULT_LISTS = [
  ["stale_clusters", "stale"],
  ["degraded_clusters", "degraded"],
  ["partial_clusters", "partial"],
  ["computing_clusters", "computing"],
  ["ambiguous_clusters", "ambiguous"],
];


// clusterFault is the served reason a window cannot be read: the stress
// market summary's data-quality lists first, then the per-cluster source
function clusterFault(cluster, snap = {}, stress = {}) {
  const market = stress.market || {};
  for (const [field, word] of CLUSTER_FAULT_LISTS) {
    if (clusterNameListed(market[field], cluster)) return { reason: word, asOf: clusterSourceAsOf(cluster, snap, stress) };
  }
  return clusterSourceFault(cluster, snap, stress);
}


// The served source-health rows for this cluster's inputs, deduplicated the
// same way the lamp-test stamp deduplicates them (first served row per source
function clusterSourceRows(cluster, snap = {}, stress = {}) {
  const rows = new Map();
  for (const source of [...(snap.regime?.source_health || []), ...(stress.source_health || [])]) {
    const name = String(source?.source || "").trim().toLowerCase();
    if (!name || rows.has(name) || !cluster.sources.includes(name)) continue;
    rows.set(name, source);
  }
  return [...rows.values()];
}


// A source is faulted whenever its served status is anything but ok — exactly
// the predicate lampTestSources counts. Reading the same field twice is what
function clusterSourceFault(cluster, snap = {}, stress = {}) {
  for (const source of clusterSourceRows(cluster, snap, stress)) {
    const status = String(source?.status || "").trim().toLowerCase();
    if (!status || status === "ok") continue;
    return { reason: status, asOf: source.as_of || "" };
  }
  return null;
}

function clusterSourceAsOf(cluster, snap = {}, stress = {}) {
  return clusterSourceRows(cluster, snap, stress).map((source) => source?.as_of).find(Boolean) || "";
}


// The band is the daemon's own cluster verdict, in served-authority order:
// summary's cluster-name lists, and only then the worst served indicator
// status inside the cluster. Served data-quality wins over all of them — a
// window whose input is stale must read as a dead window, not as calm.
function regimeClusterBand(cluster, snap = {}, stress = {}) {
  const market = stress.market || {};
  if (clusterFault(cluster, snap, stress)) return "stale";
  if (clusterNameListed(market.unconfirmed_red_cluster_names, cluster) && !clusterNameListed(market.eligible_red_cluster_names, cluster)) return "yellow";
  for (const item of snap.regime?.lifecycle?.evidence || []) {
    if (String(item?.signal || "").toLowerCase() !== "cluster") continue;
    if (!cluster.sources.includes(String(item?.source || "").trim().toLowerCase())) continue;
    const bucket = String(item.bucket || "").trim().toLowerCase();
    if (bucket) return bucket;
  }
  if (clusterNameListed(market.red_cluster_names, cluster)) return "red";
  if (clusterNameListed(market.yellow_cluster_names, cluster)) return "yellow";
  // The daemon serves every cluster's band (lifecycle evidence or the name
  // lists above). A cluster absent from all of them has no served verdict,
  // and this renderer computes none of its own.
  return "stale";
}

function clusterNameListed(names, cluster) {
  return (names || []).some((name) => cluster.sources.includes(String(name || "").trim().toLowerCase()));
}

function clusterIndicators(cluster, stress = {}) {
  return (stress.market_indicators || []).filter((indicator) => {
    const name = String(indicator?.name || "").toLowerCase();
    return cluster.match.some((needle) => name.includes(needle));
  });
}


// The lead indicator is the worst-reading window inside the cluster: the one
function clusterLeadIndicator(cluster, stress = {}) {
  const indicators = clusterIndicators(cluster, stress);
  let lead = indicators[0] || {};
  for (const indicator of indicators) {
    if (bandRank(indicatorBand(indicator)) > bandRank(indicatorBand(lead))) lead = indicator;
  }
  return lead;
}

function indicatorBand(indicator = {}) {
  const status = String(indicator.band || indicator.status || "").trim().toLowerCase();
  if (status === "amber") return "yellow";
  return status;
}

function indicatorPresentationStatus(indicator = {}, stress = {}) {
  const status = String(indicator.status || "").trim().toLowerCase();
  if (status !== "red") return status;
  const name = String(indicator.name || "").toLowerCase();
  const cluster = REGIME_CLUSTERS.find((candidate) => candidate.match.some((needle) => name.includes(needle)));
  if (!cluster) return status;
  const market = stress.market || {};
  return clusterNameListed(market.unconfirmed_red_cluster_names, cluster) && !clusterNameListed(market.eligible_red_cluster_names, cluster)
    ? "amber"
    : status;
}

function bandRank(band) {
  switch (String(band || "").trim().toLowerCase()) {
    case "red":
      return 3;
    case "yellow":
      return 2;
    case "green":
      return 1;
    default:
      return 0;
  }
}


// Regime authority health is response/cache metadata, not market evidence.
// The SPA therefore preserves the daemon-authored verdict and changes only
// its data-quality treatment when either the authority or the app transport
function regimeAuthorityView(snap = {}) {
  const health = snap.regime?.authority_health || {};
  const source = snap.sources?.regime || {};
  const authorityStatus = String(health.status || "").toLowerCase();
  const sourceState = String(source.state || "").toLowerCase();
  let status = "legacy";
  if (authorityStatus === "unavailable" || ["unavailable", "not_observed"].includes(sourceState) || source.error) {
    status = "unavailable";
  } else if (authorityStatus === "stale" || sourceState === "stale") {
    status = "stale";
  } else if (authorityStatus === "fresh" || sourceState === "current") {
    status = "fresh";
  }
  const reasons = [];
  if (["stale", "unavailable"].includes(authorityStatus)) {
    reasons.push(regimeAuthorityReasonLabel(health.failure_code, "", authorityStatus));
  }
  if (["stale", "unavailable", "not_observed"].includes(sourceState) || source.error) {
    reasons.push(regimeAuthorityReasonLabel("", source.reason, sourceState === "stale" ? "stale" : "unavailable"));
  }
  const reason = [...new Set(reasons.filter(Boolean))].join("; ") || regimeAuthorityReasonLabel("", "", status);
  return {
    status,
    degraded: status === "stale" || status === "unavailable",
    refreshing: health.refreshing === true,
    lastSuccessAt: health.last_success_at || source.last_success_at || "",
    reason,
  };
}

function regimeAuthorityReasonLabel(failureCode, sourceReason, status) {
  switch (String(failureCode || "")) {
    case "no_last_good":
      return "no last-good Regime read";
    case "refresh_timeout":
      return "refresh timed out";
    case "refresh_incomplete":
      return "refresh incomplete";
    case "refresh_failed":
      return "refresh failed";
    case "publish_failed":
      return "publication failed";
    case "invalid_persisted_state":
      return "persisted authority is invalid";
    case "clock_invalid":
      return "daemon clock is behind the last successful Regime commit";
  }
  switch (String(sourceReason || "")) {
    case "poll_stale":
      return "app observation is stale";
    case "transport_unavailable":
      return "daemon transport is unavailable";
    case "producer_unavailable":
      return "Regime producer is unavailable";
    case "persistence_unavailable":
      return "Regime persistence is unavailable";
    case "not_observed":
      return "authority has not been observed yet";
  }
  if (status === "stale") return "last complete read is outside its freshness window";
  if (status === "unavailable") return "current authority is unavailable";
  return "";
}

function regimePresentationPosture(posture = {}, authority = {}) {
  if (!authority.degraded) return posture;
  return { ...posture, tone: "data_quality" };
}

function regimeAuthorityLabel(posture = {}, authority = {}) {
  const canonical = marketRegimeLabel(posture);
  if (!authority.degraded) return canonical;
  if (canonical !== "--") return `Last known · ${canonical}`;
  return authority.status === "stale" ? "Regime stale" : "Regime unavailable";
}

function regimeAuthorityStatusLine(snap = {}, posture = {}) {
  const authority = regimeAuthorityView(snap);
  if (!authority.degraded) return null;
  const hasVerdict = marketRegimeLabel(posture) !== "--";
  const refresh = authority.refreshing ? "; refresh in progress" : "";
  if (authority.status === "stale") {
    return {
      summary: `${hasVerdict ? "Last-known regime" : "Regime read"} · stale`,
      detail: `The canonical last-good verdict is retained as context; ${authority.reason}${refresh}.`,
      title: `Regime authority stale: ${authority.reason}${refresh}`,
    };
  }
  return {
    summary: `${hasVerdict ? "Last-known regime" : "Regime"} · authority unavailable`,
    detail: hasVerdict
      ? `The canonical last-known verdict is context only; ${authority.reason}${refresh}.`
      : `No current Regime verdict is available; ${authority.reason}${refresh}.`,
    title: `Regime authority unavailable: ${authority.reason}${refresh}`,
  };
}

function renderRegimeAuthorityTimestamp(snap = {}, fallbackTimestamp = null) {
  const authority = regimeAuthorityView(snap);
  const timestamp = authority.lastSuccessAt || fallbackTimestamp;
  renderFreshnessTimestamp("regimeAsOf", timestamp, {
    staleMinutes: regimeStaleBudgetMinutes(snap),
    compact: true,
    quietWhenFresh: true,
  });
  if (!authority.degraded) return;
  const el = $("regimeAsOf");
  if (!el) return;
  const parsed = parseDate(timestamp);
  const last = parsed ? ` · last ${shortTimeWithZone(parsed.toISOString())}` : "";
  el.hidden = false;
  el.textContent = `${authority.status}${last}`;
  el.classList.add("stale");
  el.title = `Market regime freshness · ${authority.reason}`;
}


// The stale-badge threshold derives from the SERVED per-cluster staleness
// policy (regime.source_health[].max_age_seconds) — same no-hardcoded-twins
function regimeStaleBudgetMinutes(snap) {
  const entries = snap.regime?.source_health || [];
  let tightest = null;
  for (const src of entries) {
    const secs = Number(src?.max_age_seconds);
    if (Number.isFinite(secs) && secs > 0 && (tightest === null || secs < tightest)) {
      tightest = secs;
    }
  }
  if (tightest === null) return 60;
  return Math.max(60, Math.round(tightest / 60));
}


// regimeGovernedNote surfaces the confirmation-policy detail: provisional
// so the panel never shows an unqualified red while the engine itself is
function regimeGovernedNote(snap, market) {
  const parts = [];
  const unconfirmed = market?.unconfirmed_red_cluster_names || [];
  if (unconfirmed.length > 0) {
    // Name the actual signal, not its cluster key ("HYG 50-DMA", not
    // "credit"), and say that confirmation is Canary's job with both
    // outcomes stated: the signal earns its co-sign or clears, no operator
    // step. "Provisional" names the evidence state without asking the trader
    // to perform a confirmation ritual.
    const subject = humanList(unconfirmed.map(clusterInputLabel), 2);
    const verb = unconfirmed.length === 1 ? "is provisional" : "are provisional";
    parts.push(`${subject} ${verb}; Canary will confirm or clear ${unconfirmed.length === 1 ? "it" : "them"} on the next fresh read`);
  }
  for (const g of snap.regime?.lifecycle?.governors || []) {
    if (g?.action === "severity_capped") {
      parts.push(`severity held at ${g.to} — ${regimeGovernorReasonLabel(g.reason)}`);
    }
  }
  return parts.join(" · ");
}

function regimeGovernorReasonLabel(reason) {
  if (reason === "pending_backtest_no_tape_cosign") return "thresholds pending backtest, no tape co-sign";
  if (reason === "confirming_cluster_quality") return "confirming data quality impaired";
  return reason || "governed";
}

function marketSourceIssueLabels(snap = {}) {
  const labels = [];
  const add = (label) => {
    label = String(label || "").trim();
    if (label && !labels.includes(label)) labels.push(label);
  };

  // A refused subscription is the cause behind the vaguer per-symbol quote
  const refused = marketAccessBySymbol(snap);
  for (const label of refused.values()) add(label);

  for (const [symbol, error] of Object.entries(snap.market_quotes?.errors || {})) {
    const name = normalizeSymbol(symbol);
    if (refused.has(name)) continue;
    add(`${name} ${marketQuoteErrorLabel(error)}`);
  }

  const marketSourceError = String(snap.sources?.market_quotes?.error || "").trim();
  if (marketSourceError) {
    for (const part of marketSourceError.split("|")) {
      const label = marketSourceErrorLabel(part);
      if (refused.has(label.split(" ")[0])) continue;
      add(label);
    }
  }

  const regimeAuthority = regimeAuthorityView(snap);
  if (regimeAuthority.degraded) {
    add(`Regime authority ${regimeAuthority.status} (${regimeAuthority.reason})`);
  }

  return labels;
}

// marketAccessBySymbol maps each symbol the gateway is currently refusing
// This is an observation with a retry window, not a record of the account's
// nothing asked for never appears, and a data-farm outage can list a symbol
// the account does hold. Panels holding a cached result keep rendering it.
function marketAccessBySymbol(snap = {}) {
  const out = new Map();
  for (const row of snap.status?.market_data_access || []) {
    const name = normalizeSymbol(row.symbol || row.route_key);
    if (!name || out.has(name)) continue;
    out.set(name, `${name} ${marketAccessReasonLabel(row)}`);
  }
  return out;
}

// marketAccessReasonLabel renders the daemon's typed reason. The broker's own
// free text never reaches the wire, so the code is the only classification
// input, and 354 — the account is not subscribed — is the one a user can act
function marketAccessReasonLabel(row = {}) {
  const reason = String(row.reason || "").toLowerCase();
  const phrase = reason === "not_subscribed" ? "not subscribed" : "market data refused";
  const code = Number(row.code || 0);
  return code > 0 ? `${phrase} (IBKR ${code})` : phrase;
}

function marketSourceErrorLabel(error) {
  const text = String(error || "").trim();
  const match = text.match(/^([A-Za-z0-9._-]+):\s*(.+)$/);
  if (!match) return marketQuoteErrorLabel(text);
  return `${normalizeSymbol(match[1])} ${marketQuoteErrorLabel(match[2])}`;
}

function marketQuoteErrorLabel(error) {
  const text = String(error || "").trim();
  if (!text) return "";
  const withoutPrefix = text.replace(/^quote\.snapshot:\s*/i, "").trim();
  const lower = withoutPrefix.toLowerCase();
  if (lower.includes("gateway_unavailable") || lower.includes("connection unavailable") || lower.includes("ibkr connection unavailable")) return "feed interrupted";
  if (lower.includes("symbol_inactive")) return "quote unavailable";
  if (lower.includes("timeout")) return "quote timeout";
  return withoutPrefix;
}

function quoteBySymbol(quotes, symbol) {
  if (!quotes) return null;
  return quotes[symbol] || quotes[symbol.toLowerCase()] || null;
}

function quotePrice(quote) {
  if (!quote) return null;
  return firstNumber(quote.quote_price, quote.price, quote.last, quote.mark);
}

function quotePrevClose(quote) {
  if (!quote) return null;
  return firstNumber(quote.prev_close, quote.regular_close, quote.prior_regular_close);
}

function quoteChangePct(quote) {
  if (!quote) return null;
  const explicit = firstNumber(quote.quote_change_pct, quote.change_pct, quote.regular_change_pct);
  if (typeof explicit === "number") return explicit;
  const price = quotePrice(quote);
  const prev = quotePrevClose(quote);
  if (typeof price === "number" && typeof prev === "number" && prev !== 0) {
    return (price - prev) / prev * 100;
  }
  return null;
}

function quoteChange(quote) {
  if (!quote) return null;
  const explicit = firstNumber(quote.quote_change, quote.change, quote.regular_change);
  if (typeof explicit === "number") return explicit;
  const price = quotePrice(quote);
  const prev = quotePrevClose(quote);
  if (typeof price === "number" && typeof prev === "number") {
    return price - prev;
  }
  return null;
}

function regimePosture(snap = {}, stress = {}, market = {}) {
  for (const candidate of [snap.regime?.posture, market.regime_posture, stress.market?.regime_posture]) {
    const normalized = normalizeRegimePosture(candidate);
    if (normalized) return normalized;
  }
  const label = cleanDetail(snap.regime?.summary?.label || snap.regime?.composite?.verdict || market.regime_verdict);
  if (label === "--") return { label: "--", tone: "na" };
  return { label, tone: legacyRegimeTone(label) };
}

function normalizeRegimePosture(candidate) {
  if (!candidate || typeof candidate !== "object") return null;
  const label = cleanDetail(candidate.label);
  const tone = String(candidate.tone || "").trim().toLowerCase();
  if (label === "--" && !tone) return null;
  return {
    label,
    tone: tone || legacyRegimeTone(label),
    stage: candidate.stage || "",
    severity: candidate.severity || "",
    readiness: candidate.readiness || "",
    confidence: candidate.confidence || "",
    evidence: candidate.evidence || "",
  };
}

function legacyRegimeTone(label) {
  const lower = String(label || "").toLowerCase();
  if (!lower || lower === "--") return "na";
  if (lower.includes("full risk-off")) return "risk_off";
  if (lower.includes("broad stress")) return "stress";
  if (lower.includes("stress signal") || lower.includes("elevated stress") || lower.includes("watch")) return "watch";
  if (lower.includes("insufficient") || lower.includes("no usable") || lower.includes("no ranked")) return "data_quality";
  if (lower.includes("normal") || lower.includes("constructive")) return "normal";
  return "watch";
}

function marketRegimeLabel(posture = {}) {
  const label = cleanDetail(posture.label);
  return label === "--" ? "--" : labelize(label);
}

function marketRegimeStatusLine(snap, stress, market, indicators) {
  const authorityStatus = regimeAuthorityStatusLine(snap, regimePosture(snap, stress, market));
  if (authorityStatus) return authorityStatus;
  const latest = latestRegimeRead(stress, indicators);
  const ranked = Number(market.ranked_clusters || 0);
  const unranked = Number(market.unranked_clusters || 0);
  const total = ranked + unranked;
  if (!stressNeedsInputCheck(stress) && !marketHasDataGaps(market)) {
    const governed = regimeGovernedNote(snap, market);
    if (governed) {
      return { summary: "Regime read", detail: governed, title: `${governed}; updated ${latest}` };
    }
    return { summary: "Regime read", detail: latest, title: latest };
  }

  const issues = stressInputIssueSummary(stress, snap);
  const coverage = total > 0 ? `${ranked}/${total} ranked` : "ranked inputs pending";
  const summary = issues ? `${coverage}; data gaps` : `${coverage}; degraded`;
  const gateway = gatewayDataStatus(snap);
  const detail = issues ? `${gateway}; check ${issues}` : `${gateway}; check regime sources`;
  return { summary, detail, title: `${detail}; regime updated ${latest}` };
}

function latestRegimeRead(stress, indicators) {
  const latest = latestRegimeTimestamp(stress, indicators);
  if (latest) return shortTimeWithZone(latest.toISOString());
  return latestRegimeTimestampFallback(stress, indicators) || "Waiting for regime timestamp";
}

function latestRegimeTimestamp(stress, indicators) {
  const sourceAsOf = stress.source_as_of || {};
  const candidates = [
    sourceAsOf.regime,
    sourceAsOf.market_regime,
    stress.regime_as_of,
    stress.market?.regime_as_of,
    stress.as_of,
    ...indicators.map((indicator) => indicator.as_of),
  ].filter(Boolean);
  let latest = null;
  for (const candidate of candidates) {
    const parsed = parseDate(candidate);
    if (parsed && (!latest || parsed > latest)) {
      latest = parsed;
    }
  }
  return latest;
}

function latestRegimeTimestampFallback(stress, indicators) {
  const sourceAsOf = stress.source_as_of || {};
  return [
    sourceAsOf.regime,
    sourceAsOf.market_regime,
    stress.regime_as_of,
    stress.market?.regime_as_of,
    stress.as_of,
    ...indicators.map((indicator) => indicator.as_of),
  ].map((candidate) => String(candidate || "").trim()).find(Boolean) || "";
}

function renderMarketWeather(posture = {}) {
  const tone = regimeWeatherClass(posture.tone);
  const card = $("regimeSummaryCard");
  card.classList.remove("weather-green", "weather-amber", "weather-red", "weather-na");
  card.classList.add("weather-" + tone);
}

function regimeWeatherClass(tone) {
  switch (String(tone || "").toLowerCase()) {
    case "normal":
      return "green";
    case "stress":
    case "risk_off":
      return "red";
    case "watch":
    case "data_quality":
      return "amber";
    default:
      return "na";
  }
}

function marketHasDataGaps(market = {}) {
  const lists = [
    market.ambiguous_clusters,
    market.partial_clusters,
    market.computing_clusters,
    market.degraded_clusters,
    market.stale_clusters,
  ];
  return lists.some((items) => Array.isArray(items) && items.length > 0) ||
    Number(market.unranked_clusters || 0) > 0;
}

function stressInputCheckSentence(stress) {
  const issues = stressInputIssueSummary(stress, state.snapshot || {});
  return issues
    ? `Refresh or verify ${issues} before treating the stress read as a market signal.`
    : "Use the detail rows before acting.";
}

function stressInputIssueSummary(stress, snap = {}) {
  return humanList(stressInputIssueLabels(stress, snap), 4);
}

function stressInputIssueLabels(stress, snap = {}) {
  const labels = [];
  const add = (label) => {
    label = String(label || "").trim();
    if (label && !labels.includes(label)) labels.push(label);
  };

  const market = stress.market || {};
  for (const cluster of [
    ...(market.partial_clusters || []),
    ...(market.ambiguous_clusters || []),
    ...(market.computing_clusters || []),
    ...(market.degraded_clusters || []),
    ...(market.stale_clusters || []),
  ]) {
    add(clusterInputLabel(cluster));
  }

  for (const item of snap.status?.data_quality || []) {
    for (const cluster of [
      ...(item.partial_clusters || []),
      ...(item.degraded_clusters || []),
      ...(item.stale_clusters || []),
    ]) {
      add(clusterInputLabel(cluster));
    }
  }

  for (const source of stress.source_health || []) {
    const status = String(source.status || "").toLowerCase();
    if (!status || status === "ok") continue;
    switch (String(source.source || "").toLowerCase()) {
      case "account":
        add("account snapshot");
        break;
      case "positions":
        add("positions snapshot");
        break;
      case "regime": {
        // A derived row names its leaf causes; those cluster labels are
        // already added above, so a second "regime snapshot" entry would
        // make one cause read as two.
        const derivedFrom = Array.isArray(source.derived_from) ? source.derived_from : [];
        if (derivedFrom.length > 0) {
          for (const cluster of derivedFrom) add(clusterInputLabel(cluster));
        } else if (sourceHealthMentions(source, "gamma")) {
          add("gamma cache");
        } else {
          add("regime snapshot");
        }
        break;
      }
      case "market_events":
        add("market-event sources");
        break;
      default:
        add(labelize(source.source));
        break;
    }
  }

  for (const warning of stress.warnings || []) {
    const text = String(warning || "").toLowerCase();
    if (text.includes("hyg") || text.includes("50dma") || text.includes("50-day")) add("HYG 50-DMA");
    if (text.includes("usd.jpy") || text.includes("usd/jpy") || text.includes("weekly") || text.includes("7d")) add("USD/JPY baseline");
    if (text.includes("gamma")) add("gamma cache");
  }
  return labels;
}

function sourceHealthMentions(source, needle) {
  const text = [
    source.source,
    source.status,
    ...(Array.isArray(source.notes) ? source.notes : []),
  ].join(" ").toLowerCase();
  return text.includes(String(needle || "").toLowerCase());
}

function clusterInputLabel(cluster) {
  switch (String(cluster || "").trim().toLowerCase()) {
    case "credit":
      return "HYG 50-DMA";
    case "fx":
      return "USD/JPY baseline";
    case "gamma":
      return "gamma cache";
    case "breadth":
      return "breadth compute";
    case "vol":
    case "volatility":
      return "volatility feed";
    case "funding":
      return "funding series";
    default:
      return labelize(cluster);
  }
}

function gatewayDataStatus(snap = {}) {
  const status = snap.status || {};
  const mode = String(status.account_mode || snap.trading?.mode || "").toLowerCase();
  const quoteReady = (status.subsystems || []).some((subsystem) =>
    String(subsystem.name || "").toLowerCase() === "quote" &&
    String(subsystem.status || "").toLowerCase() === "ready"
  );
  // A refused subscription leaves the socket connected and the quote
  const refused = [...marketAccessBySymbol(snap).values()];
  const qualify = (reading) => refused.length === 0 ? reading : `${reading}; ${humanList(refused, 2)}`;
  if (status.connected && quoteReady && mode.includes("paper")) return qualify("Paper gateway live quotes OK");
  if (status.connected && quoteReady) return qualify("Gateway live quotes OK");
  if (status.connected) return qualify("Gateway connected");
  return "Gateway status pending";
}

function humanList(items, limit = 3) {
  items = (items || []).filter(Boolean);
  if (items.length === 0) return "";
  const shown = items.slice(0, limit);
  if (items.length > limit) {
    shown[shown.length - 1] = `${shown[shown.length - 1]} +${items.length - limit} more`;
  }
  if (shown.length === 1) return shown[0];
  if (shown.length === 2) return `${shown[0]} and ${shown[1]}`;
  return `${shown.slice(0, -1).join(", ")}, and ${shown[shown.length - 1]}`;
}

function renderSignedPercent(id, value, positiveIsRisk) {
  const el = $(id);
  el.classList.remove("signed", "ok", "risk", "neutral", "is-empty");
  if (typeof value !== "number") {
    el.textContent = "";
    el.classList.add("is-empty");
    return "neutral";
  }
  el.textContent = signedPct(value);
  el.classList.add("signed");
  const isRisk = positiveIsRisk ? value > 0 : value < 0;
  const isOk = positiveIsRisk ? value < 0 : value > 0;
  if (isRisk) el.classList.add("risk");
  if (isOk) el.classList.add("ok");
  if (!isRisk && !isOk) el.classList.add("neutral");
  return isRisk ? "risk" : isOk ? "ok" : "neutral";
}

function renderRegimeDetail(indicators, snap = {}, stress = {}) {
  const panel = $("regimeDetailPanel");
  const button = $("regimeDetailToggle");
  panel.hidden = !state.regimeDetailOpen;
  button.textContent = state.regimeDetailOpen ? "Hide detail" : "Show detail";
  button.setAttribute("aria-expanded", String(state.regimeDetailOpen));
  if (!state.regimeDetailOpen) return;
  const rows = indicators.length > 0 ? indicators : regimeFallbackIndicators(snap, stress);
  $("regimeIndicators").replaceChildren(...rows.map((indicator) => {
    const row = document.createElement("div");
    row.className = "indicator-row";
    const dot = document.createElement("span");
    dot.className = "indicator-status " + indicatorStatusClass(indicatorPresentationStatus(indicator, stress));
    const body = document.createElement("div");
    body.className = "indicator-body";
    const head = document.createElement("div");
    head.className = "indicator-head";
    const title = document.createElement("b");
    title.textContent = indicator.name || "Indicator";
    const at = document.createElement("span");
    at.textContent = indicatorAsOfLabel(indicator.as_of);
    if (indicator.as_of) at.title = indicator.as_of;
    head.append(title, at);
    const reading = document.createElement("p");
    reading.textContent = humanizeStalenessSeconds(indicator.reading || "--");
    body.append(head, reading);
    if (indicator.comment) {
      const comment = document.createElement("small");
      comment.textContent = humanizeStalenessSeconds(indicator.comment);
      body.append(comment);
    }
    row.append(dot, body);
    return row;
  }));
  renderRegimeQualityRemarks(snap, stress);
}


// Indicator cards all carry an as-of date; "today" is the expected state and
// a full ISO date restates it eight times, so only older reads keep the date.
function indicatorAsOfLabel(value) {
  if (!value) return "--";
  const at = parseDate(value);
  if (!at) return String(value);
  const now = new Date();
  const dayMS = 24 * 60 * 60 * 1000;
  const days = Math.floor((new Date(now.getFullYear(), now.getMonth(), now.getDate()) - new Date(at.getFullYear(), at.getMonth(), at.getDate())) / dayMS);
  if (days <= 0) return "today";
  if (days === 1) return "yesterday";
  return String(value);
}


// Daemon staleness estimates arrive as raw seconds ("est 68519s"); render
function humanizeStalenessSeconds(text) {
  return String(text).replace(/\b(\d{3,})s\b/g, (all, secs) => {
    const s = Number(secs);
    if (!Number.isFinite(s)) return all;
    if (s < 5400) return `~${Math.round(s / 60)}m`;
    if (s < 129600) return `~${Math.round(s / 3600)}h`;
    return `~${Math.round(s / 86400)}d`;
  });
}

function regimeFallbackIndicators(snap = {}, stress = {}) {
  const market = stress.market || {};
  const status = marketRegimeStatusLine(snap, stress, market, []);
  const tone = regimeWeatherClass(regimePosture(snap, stress, market).tone);
  const rows = [{
    name: "Regime status",
    status: tone === "red" ? "red" : tone === "green" ? "green" : tone === "amber" ? "amber" : "na",
    as_of: latestRegimeRead(stress, []),
    reading: status.summary,
    comment: status.detail,
  }, {
    name: "Gateway",
    status: state.connectionOK ? "green" : "amber",
    as_of: snap.updated_at ? shortTimeWithZone(snap.updated_at) : "--",
    reading: gatewayDataStatus(snap),
    comment: state.connectionOK ? "Live app stream connected." : "App stream is reconnecting.",
  }];
  const issues = [...marketSourceIssueLabels(snap), ...stressInputIssueLabels(stress, snap)];
  if (issues.length > 0) {
    rows.push({
      name: "Data quality",
      status: "amber",
      as_of: stress.as_of ? shortTimeWithZone(stress.as_of) : "--",
      reading: humanList([...new Set(issues)], 4),
      comment: "Fine-print data gaps are kept inside the Regime panel.",
    });
  }
  return rows;
}

function renderRegimeQualityRemarks(snap = {}, stress = {}) {
  const panel = $("regimeQualityRemarks");
  const text = $("regimeQualityText");
  if (!panel || !text) return;
  const issues = [...marketSourceIssueLabels(snap), ...stressInputIssueLabels(stress, snap)];
  const unique = [...new Set(issues.filter(Boolean))];
  panel.hidden = unique.length === 0;
  text.textContent = unique.length === 0 ? "--" : humanList(unique, 4);
}

function indicatorStatusClass(status) {
  status = String(status || "").toLowerCase();
  if (["green", "amber", "red", "context"].includes(status)) return status;
  return "na";
}

function detailCard(card) {
  const item = document.createElement("div");
  item.className = "detail-card " + (card.tone || "neutral");
  const labelEl = document.createElement("span");
  labelEl.textContent = card.label;
  const valueEl = document.createElement("b");
  valueEl.textContent = card.title || "--";
  const body = document.createElement("p");
  body.textContent = card.body || "";
  item.append(labelEl, valueEl, body);
  return item;
}

function renderHeldStress(stress) {
  const panel = $("heldStressPanel");
  if (!panel) return;
  const stresses = heldStressItems(stress);
  panel.hidden = stresses.length === 0;
  if (stresses.length === 0) {
    $("heldStressSummary").textContent = "--";
    $("heldStressList").replaceChildren();
    return;
  }
  $("heldStressSummary").textContent = heldStressSummary(stresses, 2);
  $("heldStressList").replaceChildren(...stresses.slice(0, 5).map(heldStressRow));
}

function heldStressRow(stress) {
  const row = document.createElement("div");
  row.className = "held-stress-row " + heldStressTone(stress);
  const title = document.createElement("b");
  title.textContent = stress.underlying || "Held name";
  const body = document.createElement("p");
  body.textContent = heldStressEvidence(stress);
  const reasons = document.createElement("div");
  reasons.className = "held-stress-row__reasons";
  for (const reason of heldStressReasonLabels(stress)) {
    const pill = document.createElement("span");
    pill.textContent = reason;
    reasons.append(pill);
  }
  row.append(title, body, reasons);
  return row;
}

function heldStressItems(stress) {
  const items = stress?.portfolio?.held_stress;
  return Array.isArray(items) ? items : [];
}

function heldStressTone(stress) {
  const daily = stress.daily_pnl_pct_nlv;
  if (typeof daily === "number" && daily <= -2) return "risk";
  if ((stress.liquidity_flags || []).length > 0 || typeof stress.near_expiry_delta_pct_nlv === "number") return "warn";
  return "neutral";
}

function heldStressSummary(stresses, limit) {
  const shown = stresses.slice(0, limit).map((stress) => {
    const evidence = heldStressEvidence(stress);
    return `${stress.underlying || "Held name"} ${evidence}`;
  });
  if (stresses.length > shown.length) {
    shown.push(`+${stresses.length - shown.length} more`);
  }
  return shown.join("; ");
}

function heldStressEvidence(stress) {
  const parts = [];
  if (typeof stress.daily_pnl_pct_nlv === "number") {
    parts.push(`daily P/L ${signedPct(stress.daily_pnl_pct_nlv)} NLV`);
  }
  if (typeof stress.near_expiry_delta_pct_nlv === "number") {
    let text = `near-expiry delta ${pct(stress.near_expiry_delta_pct_nlv)} NLV`;
    if (typeof stress.near_expiry_min_dte === "number") {
      text += ` at ${stress.near_expiry_min_dte} DTE`;
    }
    parts.push(text);
  }
  if ((stress.liquidity_flags || []).length > 0) {
    parts.push("liquidity " + stress.liquidity_flags.map(heldStressFlagLabel).join(", "));
  }
  if (typeof stress.market_value_pct_nlv === "number") {
    parts.push(`market value ${pct(stress.market_value_pct_nlv)} NLV`);
  }
  if (typeof stress.delta_pct_nlv === "number") {
    parts.push(`delta ${pct(stress.delta_pct_nlv)} NLV`);
  }
  if (parts.length === 0 && (stress.material_reasons || []).length > 0) {
    parts.push(stress.material_reasons.map(labelize).join(", "));
  }
  return parts.join(" / ") || "Material held-name stress";
}

function heldStressReasonLabels(stress) {
  const labels = (stress.material_reasons || []).map(heldStressReasonLabel);
  if ((stress.liquidity_flags || []).length > 0) labels.push("Liquidity");
  if (labels.length === 0 && (stress.signal_ids || []).length > 0) {
    labels.push(...stress.signal_ids.map(heldStressReasonLabel));
  }
  return [...new Set(labels)].slice(0, 4);
}

function heldStressReasonLabel(value) {
  const key = String(value || "").toLowerCase();
  if (key === "daily_pnl" || key === "held_underlying_pnl_shock") return "Daily P/L";
  if (key === "near_expiry_option_delta" || key === "held_option_expiry_concentration") return "Near-expiry options";
  if (key === "market_value") return "Market value";
  if (key === "delta") return "Delta";
  if (key === "held_liquidity_degraded") return "Liquidity";
  return labelize(value);
}

function heldStressFlagLabel(value) {
  const key = String(value || "").toLowerCase();
  if (key === "mark_outside_bid_ask") return "mark outside bid/ask";
  if (key === "options_closed") return "options closed";
  if (key === "stale_quote") return "stale quote";
  if (key === "wide_spread") return "wide spread";
  return cleanDetail(value);
}

export { applyTileSeverity, bandRank, CLUSTER_FAULT_LISTS, clusterCaption, clusterFault, clusterFigure, clusterIndicators, clusterInputLabel, clusterLeadIndicator, clusterNameListed, clusterSourceAsOf, clusterSourceFault, clusterSourceRows, clusterTrip, detailCard, earningsApplicabilitySummary, earningsHealthNotes, faultCaption, firstClause, gatewayDataStatus, heldStressEvidence, heldStressFlagLabel, heldStressItems, heldStressReasonLabel, heldStressReasonLabels, heldStressRow, heldStressSummary, heldStressTone, humanizeStalenessSeconds, humanList, indicatorAsOfLabel, indicatorBand, indicatorStatusClass, lampTestSources, latestRegimeRead, latestRegimeTimestamp, latestRegimeTimestampFallback, leadingClause, legacyRegimeTone, marketAccessBySymbol, marketAccessReasonLabel, marketExplanation, marketHasDataGaps, marketQuoteCell, marketQuoteChangeClass, marketQuoteErrorLabel, marketQuoteFallback, marketQuoteInterruptedLine, marketQuoteSessionClosed, marketQuoteSourceLine, marketRegimeLabel, marketRegimeStatusLine, marketSourceErrorLabel, marketSourceIssueLabels, masterSeverity, masterSubline, normalizeRegimePosture, offPanelRedClusters, portfolioExplanation, protectionCoverageStressLine, quoteBySymbol, quoteChange, quoteChangePct, quotePrevClose, quotePrice, quoteTime, reconcileSignalPanelTimes, REGIME_CLUSTERS, regimeAuthorityLabel, regimeAuthorityReasonLabel, regimeAuthorityStatusLine, regimeAuthorityView, regimeClusterBand, regimeClusterTile, regimeFallbackIndicators, regimeGovernedNote, regimeGovernorReasonLabel, regimePosture, regimePostureDetailTone, regimePresentationPosture, regimeStaleBudgetMinutes, regimeWeatherClass, renderHeldStress, renderLampTest, renderMarketContext, renderMarketWeather, renderRegimeAuthorityTimestamp, renderRegimeDetail, renderRegimeGrid, renderRegimePanel, renderRegimeQualityRemarks, renderRulesCard, renderRulesGrid, renderRulesProvenance, renderRulesTileState, renderSignedPercent, renderStressDetail, renderStressStatus, renderStressTimestamp, RULE_TONES, ruleChecklistRow, ruleMeasureMeter, ruleStatusLabel, rulesTileFigure, ruleTone, severityRank, snapshotSourceName, sourceHealthMentions, sourceTransportFault, staleFigure, stressCushionFigure, stressDriverLabel, stressDriverPriority, stressDriverRow, stressDriverRows, stressDriverTone, stressEmptyDriverRow, stressExplanationCards, stressHasProvisionalOnlyMarketWarning, stressInputCheckBlocksAction, stressInputCheckSentence, stressInputIssueLabels, stressInputIssueSummary, stressNeedsInputCheck, stressRowNeedsAttention, stressStageLabel, stressSummaryText, unknownEventRuleNote, worstSeverity };
