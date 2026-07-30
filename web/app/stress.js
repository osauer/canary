import { stressProtectionCoverageFor, protectionCoverageBaseCurrency, protectionCoverageHasData, protectionCoverageHeadline, protectionCoverageLargestText, protectionCoverageStaleText } from "./protection-coverage.js";
import { unknownEventRuleNote } from "./earnings-relevance.js";
import { earningsApplicabilitySummary, earningsHealthNotes, ruleStatusLabel, rulesCountSummary, wshEntitlementNotice } from "./rules-presentation.js";
import { $, cleanDetail, firstNumber, labelize, normalizeSymbol, numberRead, parseDate, pct, quoteTimestamp, renderFreshnessTimestamp, shortTimeWithZone, signedClass, signedPct, wholePct } from "./shared.js";
import { state } from "./state.js";

const RULE_TONES = { act: "risk", watch: "warn", pass: "ok", info: "neutral", unknown: "neutral", not_evaluated: "neutral" };

function ruleTone(status) {
  return RULE_TONES[status] || "neutral";
}

// Rules card: advisory 14-rule daily checklist from snapshot.rules
// (daemon-owned verdicts and ranking; this renderer adds no policy). The
// brief row shows the worst three non-pass rows hardest-first; the detail
// grid shows all rows. Read-only by design — no order actions here.
function renderRulesCard(rules) {
  const card = $("stressRulesCard");
  const strip = $("stressRulesStrip");
  const detail = $("stressRulesDetailPanel");
  if (!card || !strip || !detail) return;
  if (!rules || rules.enabled === false || !Array.isArray(rules.rules) || rules.rules.length === 0) {
    card.hidden = true;
    strip.hidden = true;
    detail.hidden = true;
    return;
  }
  card.hidden = false;
  strip.hidden = false;
  $("stressRulesCounts").textContent = rulesTileFigure(rules);

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
    pill.className = `severity-pill stress-rules__pill ${ruleTone(r.status)}`;
    pill.textContent = `${r.number} · ${r.title} · ${ruleStatusLabel(r.status, r.reason)}`;
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

// The tile figure states the served tally over its denominator, so "2 watch"
// is read against the size of the daemon's checklist rather than alone.
function rulesTileFigure(rules = {}) {
  const rows = Array.isArray(rules.rules) ? rules.rules : [];
  const pass = typeof rules.breach_counts?.pass === "number"
    ? rules.breach_counts.pass
    : rows.filter((row) => row?.status === "pass").length;
  const summary = rulesCountSummary(rules);
  return summary === "all pass" ? `${pass} pass` : `${pass} pass · ${summary}`;
}


// The Rules window states the worst flagged rule the daemon ranked — its own
// title, verbatim — over the served tally. An info-only day lamps nothing and
// shows the advisory dot instead, so "info" never reads as a breach.
function renderRulesTileState(rules, order) {
  const card = $("stressRulesCard");
  const caption = $("stressRulesState");
  const dot = $("stressRulesInfoDot");
  if (!card || !caption || !dot) return;
  const worst = order.map((ix) => rules.rules[ix]).find((rule) => rule && rule.status !== "pass");
  const status = String(worst?.status || "").toLowerCase();
  const infoOnly = Boolean(worst) && status === "info";
  applyTileSeverity(card, status === "act" ? "act" : status === "watch" ? "watch" : "");
  dot.hidden = !infoOnly;
  caption.textContent = worst ? cleanDetail(worst.title) : "No breaches";
  caption.title = worst ? `${worst.number} · ${worst.title} · ${ruleStatusLabel(worst.status, worst.reason)}` : "Every evaluated rule passes";
}


// The note builders return five independent degradation notices; joining them
// into one paragraph is what made them unreadable. Each keeps its own block,
// behind an info affordance so the ranked rule chips keep the strip.
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

function renderRulesGrid(rules, order) {
  const grid = $("stressRulesGrid");
  if (!grid) return;
  grid.replaceChildren();
  for (const ix of order) {
    const r = rules.rules[ix];
    if (!r) continue;
    const cardEl = document.createElement("div");
    cardEl.className = `detail-card ${ruleTone(r.status)}`;
    const label = document.createElement("span");
    label.textContent = `Rule ${r.number} · ${ruleStatusLabel(r.status, r.reason)}`;
    const title = document.createElement("b");
    title.textContent = r.title;
    const body = document.createElement("p");
    let text = r.evidence || "--";
    if (typeof r.observed === "number" && typeof r.threshold === "number") {
      text += ` (observed ${r.observed} vs ${r.threshold}${r.unit ? " " + r.unit : ""})`;
    }
    body.textContent = text;
    cardEl.append(label, title, body);
    const offenders = (r.offenders || []).slice(0, 3);
    if (offenders.length) {
      const list = document.createElement("p");
      list.className = "stress-rules__offenders";
      list.textContent = offenders.map((o) => (o.leg || o.symbol) + (o.note ? ` — ${o.note}` : "")).join(" · ");
      cardEl.appendChild(list);
    }
    grid.appendChild(cardEl);
  }
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
// portfolio cushion as the figure. The full served summary stays one hover
// (and one tap into the detail panel) away rather than crowding the tile.
function renderStressStatus(stress, snap = state.snapshot || {}) {
  applyTileSeverity($("stressHero"), String(stress.severity || "").toLowerCase());
  $("stressSeverity").textContent = labelize(stress.severity || "--");
  $("stressAction").textContent = stressStageLabel(stress);
  const summary = $("stressSummary");
  const full = stressSummaryText(stress, snap);
  summary.textContent = stressCushionFigure(stress);
  summary.title = full;
}


// No trip anchor: the stress payload serves the cushion reading but no
// cushion threshold, so the figure states what was measured and stops there.
function stressCushionFigure(stress = {}) {
  const cushion = stress.portfolio?.cushion_pct;
  if (typeof cushion !== "number") return "cushion pending";
  return `cushion ${wholePct(cushion)}`;
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
// normalized to a period.
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
  // uses for the Regime panel's own weather chip. This card used to escalate
  // to "warn" locally whenever it saw a data gap, which is exactly the kind
  // of client-side reinterpretation that let closed-session gamma staleness
  // read amber here even when the canonical posture read normal.
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
// When both render the same text, showing the pair reads as a stutter
// ("now · now"), so collapse to the regime span alone.
function reconcileSignalPanelTimes() {
  const regime = $("regimeAsOf");
  const stress = $("stressAsOf");
  if (!regime || !stress) return;
  const duplicate = regime.textContent === stress.textContent;
  stress.hidden = stress.hidden || duplicate;
  // The separator only earns ink when both sides render text (quiet-when-
  // fresh can blank either side independently).
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
  // degraded states (stale/frozen/delayed) keep their explicit words.
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
  // out of reach.
  summary.title = [subline, regimeStatus.title || regimeStatus.detail || regimeStatus.summary]
    .filter(Boolean).join(" — ");
  applyTileSeverity($("masterAnnunciator"), masterSeverity(snap, stress));
  renderLampTest(snap, stress);
  // marketRegimeMix now lives in the expanded detail deck and shows only the
  // governed-severity note (a real policy downgrade disclosure) — not a
  // repeat of the freshness badge or the itemized data-gap list, both of
  // which already have their own home (regimeAsOf and regimeQualityRemarks).
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
// words appear here; the rank exists so the master can light on the worst
// SERVED verdict rather than inventing one.
const SEVERITY_RANK = { observe: 1, watch: 2, act: 3, urgent: 4 };

function severityRank(value) {
  return SEVERITY_RANK[String(value || "").trim().toLowerCase()] || 0;
}

function worstSeverity(...values) {
  return values.reduce((worst, value) => (severityRank(value) > severityRank(worst) ? value : worst), "");
}


// masterSeverity is the worst severity the daemon serves across the two
// verdicts this panel shows — portfolio stress and the regime posture. A
// master annunciator that stayed dark while a subordinate window was lit
// would be a lie, so the master takes the maximum; it never derives a
// severity of its own.
function masterSeverity(snap = {}, stress = {}) {
  const regime = snap.regime || {};
  return String(worstSeverity(
    stress.severity,
    regime.posture?.severity,
    regime.lifecycle?.severity,
  ) || "").toLowerCase();
}


// applyTileSeverity is the one place a severity word becomes a lamp class.
// "stale" is the dead-window treatment: a darker face with no severity tint,
// used when the served cluster health says the reading cannot be trusted.
function applyTileSeverity(el, severity) {
  if (!el) return;
  el.classList.remove("pd-tile--watch", "pd-tile--act", "pd-tile--info", "pd-tile--stale");
  const key = String(severity || "").trim().toLowerCase();
  if (key === "watch" || key === "yellow") el.classList.add("pd-tile--watch");
  else if (key === "act" || key === "urgent" || key === "red") el.classList.add("pd-tile--act");
  else if (key === "info") el.classList.add("pd-tile--info");
  else if (key === "stale") el.classList.add("pd-tile--stale");
}


// masterSubline states the action first, then anything the panel would
// otherwise disclose only in a subordinate window, then the served timing
// word. The red-window clause is the master law in code: when cluster
// windows are lit red while the master is not at act grade, the master says
// so instead of letting the two surfaces disagree in silence.
function masterSubline(snap = {}, stress = {}) {
  const action = stressStageLabel(stress);
  const parts = [action === "--" ? "" : action, masterSeverity(snap, stress)];
  // The daemon ranks more clusters than this panel gives windows to, so the
  // reds are named, not just counted: a red cluster without a window of its
  // own has nowhere else on the Monitor face to appear.
  const reds = stress.market?.red_cluster_names || [];
  if (reds.length > 0) parts.push(`${reds.length} red: ${reds.join(", ")}`);
  // A dead window under a quiet master is the same silent disagreement as a
  // lit one, so the master names the windows it cannot read.
  const dark = REGIME_CLUSTERS
    .filter((cluster) => regimeClusterBand(cluster, snap, stress) === "stale")
    .map((cluster) => cluster.legend.toLowerCase());
  if (dark.length > 0) parts.push(`${dark.join(", ")} dark`);
  parts.push(regimeGovernedNote(snap, stress.market || {}));
  const timing = cleanDetail(snap.regime?.lifecycle?.timing);
  if (timing !== "--") parts.push(labelize(timing).toLowerCase());
  return parts.filter(Boolean).join(" · ");
}


// The lamp test is the panel's own self-report: how many served feeds are
// healthy, and which ones are not. The count covers the daemon's regime and
// stress source-health entries (the instrument's feeds); app-transport
// failures are named as faults but never silently change the feed count.
function renderLampTest(snap = {}, stress = {}) {
  const stamp = $("lampTestStamp");
  if (!stamp) return;
  const health = lampTestSources(snap, stress);
  const at = parseDate(snap.updated_at);
  const when = at ? at.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false }) : "--";
  stamp.replaceChildren();
  const lead = document.createElement("span");
  lead.textContent = `Last snapshot ${when} · `;
  const count = document.createElement("b");
  count.className = health.faults.length > 0 ? "pd-dimcount" : "";
  count.textContent = `${health.ok}/${health.total} sources ok`;
  stamp.append(lead, count);
  if (health.faults.length > 0) {
    const note = document.createElement("span");
    note.className = "pd-stale-note";
    note.textContent = ` · ${humanList(health.faults, 2)}`;
    stamp.append(note);
  }
  stamp.title = health.faults.length > 0
    ? `Served source health: ${health.ok} of ${health.total} ok; ${health.faults.join(", ")}`
    : `Served source health: ${health.ok} of ${health.total} ok`;
}

function lampTestSources(snap = {}, stress = {}) {
  const seen = new Map();
  for (const source of [...(snap.regime?.source_health || []), ...(stress.source_health || [])]) {
    const name = String(source?.source || "").trim().toLowerCase();
    if (!name || seen.has(name)) continue;
    seen.set(name, String(source?.status || "").trim().toLowerCase());
  }
  const faults = [];
  let ok = 0;
  for (const [name, status] of seen) {
    if (!status || status === "ok") {
      ok++;
      continue;
    }
    faults.push(`${clusterInputLabel(name)} ${status}`);
  }
  for (const [name, meta] of Object.entries(snap.sources || {})) {
    if (!meta?.error) continue;
    faults.push(`${snapshotSourceName(name)} unavailable`);
  }
  return { ok, total: seen.size, faults: [...new Set(faults)] };
}

function snapshotSourceName(name) {
  const key = String(name || "").trim().toLowerCase();
  return key === "market_quotes" ? "market quotes" : clusterInputLabel(key);
}


// The four regime windows, in fixed positions. Nothing here is reordered by
// severity: an instrument that moves cannot be read by muscle memory, and
// the lamps already say which window needs attention.
const REGIME_CLUSTERS = [
  { key: "breadth", legend: "Breadth", sources: ["breadth"], match: ["breadth"] },
  { key: "vol", legend: "Volatility", sources: ["vol", "volatility"], match: ["vix", "vol"] },
  { key: "credit", legend: "Credit", sources: ["credit"], match: ["credit", "hyg", "oas", "hy/ig"] },
  { key: "gamma", legend: "Dealer gamma", sources: ["gamma"], match: ["gamma", "γ-zero"] },
];

function renderRegimeGrid(snap = {}, stress = {}) {
  const grid = $("regimeSummaryCard");
  if (!grid) return;
  grid.replaceChildren(...REGIME_CLUSTERS.map((cluster) => regimeClusterTile(cluster, snap, stress)));
}

function regimeClusterTile(cluster, snap = {}, stress = {}) {
  const band = regimeClusterBand(cluster, snap, stress);
  const lead = clusterLeadIndicator(cluster, stress);
  const tile = document.createElement("div");
  tile.className = "pd-tile";
  applyTileSeverity(tile, band);
  const bar = document.createElement("span");
  bar.className = "pd-tile__bar";
  bar.setAttribute("aria-hidden", "true");
  const legend = document.createElement("span");
  legend.className = "pd-tile__legend";
  legend.textContent = cluster.legend;
  const cap = document.createElement("b");
  cap.className = "pd-tile__cap";
  cap.textContent = clusterCaption(lead, band);
  const fig = document.createElement("div");
  fig.className = "pd-tile__fig";
  fig.textContent = clusterFigure(lead, band);
  tile.append(bar, legend, cap, fig);
  // No trip anchor: the app snapshot's indicators carry no served threshold
  // bands, so the tile shows the reading the daemon did serve and keeps the
  // rest of that evidence in the title rather than inventing a cutoff.
  tile.title = [lead.name, lead.reading, lead.comment, indicatorAsOfLabel(lead.as_of)]
    .map((part) => humanizeStalenessSeconds(cleanDetail(part)))
    .filter((part) => part && part !== "--")
    .join(" · ");
  tile.setAttribute("aria-label", `${cluster.legend} ${clusterCaption(lead, band)}`);
  return tile;
}


// The caption is served text: the indicator's own worded comment, falling
// back to its reading and then to the served band word. Nothing is composed
// from the numbers here — a caption this renderer wrote would be analysis
// the daemon never published.
function clusterCaption(lead = {}, band = "") {
  for (const source of [lead.comment, lead.reading]) {
    const clause = leadingClause(source);
    if (clause) return clause;
  }
  return band === "stale" ? "Fault" : labelize(band || "no read");
}


// The caption is one clause of served text, without the sentence punctuation
// that a caps annunciator legend would carry as visual noise.
function leadingClause(value) {
  const text = firstClause(humanizeStalenessSeconds(cleanDetail(value))).replace(/[.;,]+$/, "").trim();
  return text === "--" ? "" : text;
}

function clusterFigure(lead = {}, band = "") {
  const reading = humanizeStalenessSeconds(cleanDetail(lead.reading));
  if (reading !== "--") return reading;
  const at = indicatorAsOfLabel(lead.as_of);
  if (band === "stale" && at !== "--") return `Last ${at}`;
  return "Reading pending";
}


// The band is the daemon's own cluster verdict, in served-authority order:
// the regime lifecycle's per-cluster evidence, then the stress market
// summary's cluster-name lists, and only then the worst served indicator
// status inside the cluster. Data-quality cluster lists win over all of them
// — a window whose input is stale must read as a dead window, not as calm.
function regimeClusterBand(cluster, snap = {}, stress = {}) {
  const market = stress.market || {};
  const degraded = [
    market.stale_clusters, market.degraded_clusters, market.partial_clusters,
    market.computing_clusters, market.ambiguous_clusters,
  ];
  if (degraded.some((names) => clusterNameListed(names, cluster))) return "stale";
  for (const item of snap.regime?.lifecycle?.evidence || []) {
    if (String(item?.signal || "").toLowerCase() !== "cluster") continue;
    if (!cluster.sources.includes(String(item?.source || "").trim().toLowerCase())) continue;
    const bucket = String(item.bucket || "").trim().toLowerCase();
    if (bucket) return bucket;
  }
  if (clusterNameListed(market.red_cluster_names, cluster)) return "red";
  if (clusterNameListed(market.yellow_cluster_names, cluster)) return "yellow";
  const indicators = clusterIndicators(cluster, stress);
  if (indicators.length === 0) return "stale";
  return indicators.map(indicatorBand).reduce((worst, band) => (bandRank(band) > bandRank(worst) ? band : worst), "green");
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
// whose evidence explains why the cluster lamp is lit. Ties keep served
// order.
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
// says that the retained result is stale or unavailable.
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
// pattern as renderProtectionTimestamp. The timestamp shown is the freshest
// indicator read, so its budget is the tightest served max-age, floored at
// 60 minutes so an intraday tick lull doesn't flap the badge. Fallback when
// no policy is served: the historical 60m.
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
// (visible-but-unconfirmed) stress signals and any severity-governor caps,
// so the panel never shows an unqualified red while the engine itself is
// withholding confirmation.
function regimeGovernedNote(snap, market) {
  const parts = [];
  const unconfirmed = market?.unconfirmed_red_cluster_names || [];
  if (unconfirmed.length > 0) {
    const plural = unconfirmed.length === 1 ? "" : "s";
    parts.push(`${unconfirmed.length} stress signal${plural} pending confirmation (${unconfirmed.join(", ")})`);
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

  for (const [symbol, error] of Object.entries(snap.market_quotes?.errors || {})) {
    add(`${normalizeSymbol(symbol)} ${marketQuoteErrorLabel(error)}`);
  }

  const marketSourceError = String(snap.sources?.market_quotes?.error || "").trim();
  if (marketSourceError) {
    for (const part of marketSourceError.split("|")) {
      add(marketSourceErrorLabel(part));
    }
  }

  const regimeAuthority = regimeAuthorityView(snap);
  if (regimeAuthority.degraded) {
    add(`Regime authority ${regimeAuthority.status} (${regimeAuthority.reason})`);
  }

  return labels;
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
      case "regime":
        if (sourceHealthMentions(source, "gamma")) add("gamma cache");
        else add("regime snapshot");
        break;
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
  if (status.connected && quoteReady && mode.includes("paper")) return "Paper gateway live quotes OK";
  if (status.connected && quoteReady) return "Gateway live quotes OK";
  if (status.connected) return "Gateway connected";
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
    dot.className = "indicator-status " + indicatorStatusClass(indicator.status);
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
// them as approximate human durations.
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

export { applyTileSeverity, bandRank, clusterCaption, clusterFigure, clusterIndicators, clusterInputLabel, clusterLeadIndicator, clusterNameListed, detailCard, earningsApplicabilitySummary, earningsHealthNotes, firstClause, gatewayDataStatus, heldStressEvidence, heldStressFlagLabel, heldStressItems, heldStressReasonLabel, heldStressReasonLabels, heldStressRow, heldStressSummary, heldStressTone, humanizeStalenessSeconds, humanList, indicatorAsOfLabel, indicatorBand, indicatorStatusClass, lampTestSources, latestRegimeRead, latestRegimeTimestamp, latestRegimeTimestampFallback, leadingClause, legacyRegimeTone, marketExplanation, marketHasDataGaps, marketQuoteCell, marketQuoteChangeClass, marketQuoteErrorLabel, marketQuoteFallback, marketQuoteInterruptedLine, marketQuoteSourceLine, marketRegimeLabel, marketRegimeStatusLine, marketSourceErrorLabel, marketSourceIssueLabels, masterSeverity, masterSubline, normalizeRegimePosture, portfolioExplanation, protectionCoverageStressLine, quoteBySymbol, quoteChange, quoteChangePct, quotePrevClose, quotePrice, quoteTime, reconcileSignalPanelTimes, REGIME_CLUSTERS, regimeAuthorityLabel, regimeAuthorityReasonLabel, regimeAuthorityStatusLine, regimeAuthorityView, regimeClusterBand, regimeClusterTile, regimeFallbackIndicators, regimeGovernedNote, regimeGovernorReasonLabel, regimePosture, regimePostureDetailTone, regimePresentationPosture, regimeStaleBudgetMinutes, regimeWeatherClass, renderHeldStress, renderLampTest, renderMarketContext, renderMarketWeather, renderRegimeAuthorityTimestamp, renderRegimeDetail, renderRegimeGrid, renderRegimePanel, renderRegimeQualityRemarks, renderRulesCard, renderRulesGrid, renderRulesTileState, renderSignedPercent, renderStressDetail, renderStressStatus, renderStressTimestamp, RULE_TONES, rulesCountSummary, ruleStatusLabel, rulesTileFigure, ruleTone, severityRank, snapshotSourceName, sourceHealthMentions, stressCushionFigure, stressDriverLabel, stressDriverPriority, stressDriverRow, stressDriverRows, stressDriverTone, stressEmptyDriverRow, stressExplanationCards, stressHasProvisionalOnlyMarketWarning, stressInputCheckBlocksAction, stressInputCheckSentence, stressInputIssueLabels, stressInputIssueSummary, stressNeedsInputCheck, stressRowNeedsAttention, stressStageLabel, stressSummaryText, unknownEventRuleNote, worstSeverity };
