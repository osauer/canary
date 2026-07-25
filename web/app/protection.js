import { renderAll } from "./app.js";
import { quoteBySymbol } from "./canary.js";
import { marketEventFlagVisible, marketEventHealthItems, marketEventIDLabel, marketEventTone, marketFlagRow, protectionEffectiveBlockers, protectionEffectiveMarketFlags, renderMarketFlagRail } from "./market-events.js";
import { refreshOpenOrders } from "./orders.js";
import { applyProtectionSnapshot, currentProtectionCoverage, protectionCoverageBaseCurrency, protectionEmptyRow, protectionHiddenRowsText, protectionNoStopExposureSummary, protectionNotProtectableText, protectionVisibleRows } from "./protection-coverage.js";
import { $, blockerText, cleanDetail, compactMoney, compactWholeMoney, firstNumber, hasNumericValue, labelize, money, normalizeCurrency, normalizeSymbol, numberRead, pct, protectionWriteConfirmation, protectionWriteConfirmationLabel, protectionWriteUnavailableReason, readJSONOrText, renderFreshnessTimestamp, setMetricTone, shortPreviewMessage, shortPreviewTokenID, shortTimeWithZone, signedMoneyRead } from "./shared.js";
import { currentMarketCalendar, marketSessionLabel } from "./shell.js";
import { state } from "./state.js";

function renderProtectionPanel(proposals = {}, autoTrade = {}, marketEvents = state.snapshot?.market_events || {}) {
  const panel = $("protectionPanel");
  const detail = $("protectionDetailPanel");
  const toggle = $("protectionToggle");
  const rows = proposals.proposals || [];
  const counts = proposals.counts || {};
  panel.dataset.open = String(state.protectionOpen);
  detail.hidden = !state.protectionOpen;
  toggle.textContent = state.protectionOpen ? "Hide proposals" : "Show proposals";
  toggle.setAttribute("aria-expanded", String(state.protectionOpen));
  renderProtectionTimestamp(proposals);
  const theta = protectionThetaSummary(proposals, rows);
  const thetaEl = $("protectionTheta");
  thetaEl.textContent = hasNumericValue(theta.value) ? money(theta.value, theta.currency) : theta.mixed ? "Mixed" : "--";
  thetaEl.title = theta.title;
  setMetricTone(thetaEl, hasNumericValue(theta.value) && theta.value > 0 ? "alert" : "neutral");
  const riskExcessEl = $("protectionRiskExcess");
  const riskExcess = protectionRiskExcessSummary(counts);
  riskExcessEl.textContent = riskExcess.text;
  riskExcessEl.title = riskExcess.title;
  setMetricTone(riskExcessEl, riskExcess.risk ? "risk" : "neutral");
  const noStop = protectionNoStopExposureSummary(rows, marketEvents, currentProtectionCoverage());
  const noStopEl = $("protectionNoStopExposure");
  noStopEl.textContent = noStop.text;
  noStopEl.title = noStop.title;
  setMetricTone(noStopEl, noStop.risk ? "alert" : "neutral");
  // Sits above the proposals fold: it explains a row deliberately left out of
  // the metric, so it must be readable without expanding anything.
  const defunctEl = $("protectionDataNote");
  const defunctNote = protectionNotProtectableText(currentProtectionCoverage());
  defunctEl.textContent = defunctNote;
  defunctEl.hidden = !defunctNote;
  $("protectionActions").textContent = String(counts.actionable ?? rows.length ?? 0);
  renderProtectionExposure();
  renderMarketFlagRail("protectionFlagRail", protectionHeroMarketFlags(rows, marketEvents));
  const autoButton = $("protectionAutoButton");
  autoButton.disabled = true;
  autoButton.title = "Manual confirmation required";
  const reason = protectionReason(proposals, autoTrade);
  const reasonEl = $("protectionReason");
  const refreshReason = protectionSnapshotRefreshReason();
  const hiddenReason = protectionHiddenRowsText(rows, marketEvents);
  const reasonText = [reason, hiddenReason, refreshReason].filter(Boolean).join(" · ");
  reasonEl.textContent = reasonText;
  reasonEl.hidden = !reasonText;
  // The de-risk control lives in the always-visible header, so it renders
  // before the fold early-return below.
  renderProtectionDerisk();
  if (!state.protectionOpen) return;
  const visibleRows = protectionVisibleRows(rows, marketEvents);
  $("protectionRows").replaceChildren(...(visibleRows.length > 0
    ? visibleRows.map(protectionRow)
    : [protectionEmptyRow("No protection proposals requiring action.")]));
  if (protectionNeedsSnapshotSync(proposals, autoTrade)) {
    queueProtectionSnapshotSync();
  }
}


// reduceEligibleHoldings lists the positions the discretionary trim could
// touch: stocks/ETFs (long or short) and long options, matching the daemon's
// reduceEligible scope. This is a cheap "is the sweep worth offering"
// precondition only — it does not replicate the daemon's net-delta
// direction/sign-matching logic, which is the real source of truth for which
// legs actually trim. A Preview can still legitimately return zero candidates
// (e.g. net delta is immaterial) even when this list is non-empty.
function reduceEligibleHoldings() {
  const positions = state.snapshot?.positions || {};
  const stocks = Array.isArray(positions.stocks) ? positions.stocks : [];
  const options = Array.isArray(positions.options) ? positions.options : [];
  const out = [];
  for (const row of [...stocks, ...options]) {
    if (!row || !row.con_id) continue;
    const qty = Number(row.quantity || 0);
    if (qty === 0) continue;
    // Skip defunct rows the enricher flagged stale (delisted/zero-value): they
    // are position truth but not tradable. Stale is the deliberate signal — a
    // live row can carry a zero/absent mark off-hours and is still tradable.
    if (row.stale) continue;
    const isOption = reduceIsOption(row);
    if (isOption && qty <= 0) continue; // long options only
    out.push(row);
  }
  return out;
}

function reduceIsOption(row = {}) {
  const secType = String(row.sec_type || "").toUpperCase();
  return secType === "OPT" || secType === "OPTION";
}


// renderProtectionDerisk draws the always-visible header "Trim delta-adjusted
// risk" control: a percentage + a Preview button, plus the basket once
// previewed. It lives outside the foldable detail panel so it is reachable
// whether the panel is open or not. The daemon computes the basket legs; this
// is display only.
function renderProtectionDerisk() {
  syncDeriskValidityTicker();
  const section = $("protectionDerisk");
  if (!section) return;
  const d = state.protectionDerisk;
  // Offer the sweep only when something is scope-eligible to trim, so the
  // header stays calm for an empty book. The daemon's net-delta direction
  // logic — which decides which of these actually trim — runs at Preview.
  const eligible = reduceEligibleHoldings();
  section.hidden = eligible.length === 0;
  if (eligible.length === 0) return;
  // The trim sizes by Δ-adjusted risk; when the portfolio delta itself is
  // unavailable the control would be flying blind, so it greys out and says
  // why on the control instead of offering a preview that cannot size.
  const portfolio = state.snapshot?.positions?.portfolio || {};
  const deltaUnavailable = !hasNumericValue(portfolio.dollar_delta_base ?? portfolio.dollar_delta_ccy);
  const percentPicker = $("protectionDeriskPercent");
  percentPicker.value = String(d.percent);
  percentPicker.disabled = deltaUnavailable;
  const previewBtn = $("protectionDeriskPreview");
  previewBtn.disabled = d.busy !== "" || deltaUnavailable;
  previewBtn.title = deltaUnavailable ? "Portfolio delta is unavailable — the trim needs portfolio Greeks to size a basket." : "";
  if (d.busy === "preview") previewBtn.textContent = "Previewing…";
  else previewBtn.textContent = deriskPreviewExpired() ? "Preview again" : "Preview";
  if (deltaUnavailable && d.busy === "" && !d.result && !d.submitted) {
    $("protectionDeriskState").textContent = "Delta unavailable — trimming needs portfolio Greeks, which are missing in this snapshot.";
    renderProtectionDeriskBasket();
    const cancelHidden = $("protectionDeriskCancel");
    cancelHidden.hidden = true;
    return;
  }
  const cancelBtn = $("protectionDeriskCancel");
  cancelBtn.hidden = !(d.busy === "preview" || (d.busy === "" && (d.result || d.submitted)));
  cancelBtn.textContent = d.submitted ? "Dismiss" : "Cancel";
  renderProtectionDeriskBasket();
  $("protectionDeriskState").textContent = protectionDeriskStateText();
}


// The preview's quotes/WhatIf numbers are already sweep-duration old (tens of
// seconds for a wide basket) when the basket first renders, and Submit makes
// the daemon re-run the whole sweep fresh anyway — so displayed numbers and
// placed orders diverge as the market moves. The validity window is honest-UI:
// after it lapses the human must re-anchor on fresh numbers before submitting.
// It is a behavioral nudge, not a safety gate (the daemon is the gate).
const DERISK_PREVIEW_VALID_MS = 10_000;


// Remaining validity in ms, or null when no countdown applies (no preview,
// already submitted, busy, or nothing submit-eligible to protect).
function deriskPreviewRemainingMs() {
  const d = state.protectionDerisk;
  if (!d.result || d.submitted || d.busy !== "" || !d.previewedAt) return null;
  if ((d.result.eligible_count || 0) === 0) return null;
  return Math.max(0, d.previewedAt + DERISK_PREVIEW_VALID_MS - Date.now());
}

function deriskPreviewExpired() {
  return deriskPreviewRemainingMs() === 0;
}


// Ticker re-renders once per second while a live countdown is showing.
// Remaining time is always derived from the previewedAt timestamp — never a
// decremented counter — so background-tab interval throttling cannot make the
// display lie after a tab switch. Rendering is state-derived and idempotent,
// so the SSE-driven re-renders and this ticker can interleave freely.
let deriskValidityTicker = 0;

function syncDeriskValidityTicker() {
  const remaining = deriskPreviewRemainingMs();
  const active = remaining !== null && remaining > 0;
  if (active && !deriskValidityTicker) {
    deriskValidityTicker = window.setInterval(renderProtectionDerisk, 1000);
  } else if (!active && deriskValidityTicker) {
    window.clearInterval(deriskValidityTicker);
    deriskValidityTicker = 0;
  }
}

function protectionDeriskStateText() {
  const d = state.protectionDerisk;
  if (d.busy === "preview") return "Previewing each leg; no orders placed";
  if (d.busy === "submit") return "Submitting the basket; fresh broker WhatIf per leg";
  const res = d.submitted || d.result;
  if (!res) return "Choose a percentage, then Preview to see the basket.";
  if ((res.blockers || []).length > 0) return blockerText(res.blockers[0]);
  const verb = d.submitted ? "placed" : "eligible";
  let line = `${res.eligible_count || 0} ${verb} · ${res.blocked_count || 0} blocked`;
  if (res.target_dollar_delta) {
    const ccy = res.base_currency || "";
    const achievedPct = res.achieved_pct_of_target != null ? Math.round(res.achieved_pct_of_target) : null;
    line += ` · removing ${money(res.achieved_dollar_delta || 0, ccy)} of ${money(res.target_dollar_delta, ccy)} targeted net delta`;
    if (achievedPct != null) line += ` (${achievedPct}%)`;
  }
  if (res.net_delta_incomplete) line += " · net delta is a partial-book estimate (some Greeks unavailable)";
  if (res.total_notional) {
    line += ` · ≈ ${money(res.total_notional, res.base_currency || "")}`;
    if (res.fx_incomplete) line += " (partial FX)";
  }
  if (!d.submitted && (res.eligible_count || 0) > 0) {
    if (deriskPreviewExpired()) {
      line += " — preview expired; the market moved on. Preview again for fresh numbers.";
    } else {
      line += ` — Submit sends to ${protectionWriteConfirmationLabel()}`;
      const remaining = deriskPreviewRemainingMs();
      if (remaining !== null) line += ` · numbers valid ${Math.ceil(remaining / 1000)}s`;
    }
  }
  return line;
}

function renderProtectionDeriskBasket() {
  const box = $("protectionDeriskBasket");
  const d = state.protectionDerisk;
  const res = d.submitted || d.result;
  const legs = (res && res.legs) || [];
  const basketBlockers = (res && res.blockers) || [];
  if (!res || (legs.length === 0 && basketBlockers.length === 0)) {
    box.hidden = true;
    box.replaceChildren();
    return;
  }
  box.hidden = false;
  box.classList.toggle("protection-derisk__basket--expired", deriskPreviewExpired());
  const children = [];
  for (const b of basketBlockers) children.push(deriskBasketLine(blockerText(b), "blocked"));
  // Only render a leg that will be/was trimmed (reduce_quantity > 0, no
  // blocker) or that carries a disclosed problem (a blocker — e.g.
  // delta_unavailable, wide_spread, preview_failed). The daemon already omits
  // candidates that round to zero, so this is mostly defense-in-depth against
  // a leg that should never reach the basket in the first place.
  for (const leg of legs) {
    const hasBlocker = (leg.blockers || []).length > 0;
    const willTrim = Number(leg.reduce_quantity || 0) > 0 && !hasBlocker;
    if (!willTrim && !hasBlocker) continue;
    children.push(deriskLegRow(leg, Boolean(d.submitted), res.base_currency || ""));
  }
  // Two-gesture flow: the header Preview never writes. The Submit button only
  // appears after a preview that surfaced eligible legs, and is minted with a
  // literal id so the contract test can pin it. An expired preview withdraws
  // Submit entirely: the path back to a broker write is a fresh Preview.
  if (!d.submitted && (res.eligible_count || 0) > 0 && !deriskPreviewExpired()) {
    const submit = document.createElement("button");
    submit.type = "button";
    submit.id = "protectionDeriskSubmit";
    submit.className = "protection-submit protection-derisk__submit";
    let submitLabel = `Submit ${res.eligible_count} order${res.eligible_count === 1 ? "" : "s"}`;
    const remaining = deriskPreviewRemainingMs();
    if (remaining !== null) submitLabel += ` · ${Math.ceil(remaining / 1000)}s`;
    submit.textContent = submitLabel;
    submit.disabled = d.busy !== "";
    submit.addEventListener("click", submitProtectionDerisk);
    children.push(submit);
  }
  box.replaceChildren(...children);
}

function deriskBasketLine(text, kind = "") {
  const line = document.createElement("p");
  line.className = "protection-derisk__note" + (kind ? ` protection-derisk__note--${kind}` : "");
  line.textContent = text;
  return line;
}

function deriskLegRow(leg = {}, submitted = false, baseCurrency = "") {
  const row = document.createElement("div");
  row.className = "protection-derisk__leg";
  const hasBlockers = (leg.blockers || []).length > 0;
  let badge = "eligible";
  if (submitted) badge = leg.placed ? "placed" : "blocked";
  else if (!leg.submit_eligible || hasBlockers) badge = "blocked";
  const action = leg.action === "BUY" ? "Buy to cover" : "Sell";
  const unit = leg.sec_type === "OPT" ? "ct" : "sh";
  const label = document.createElement("span");
  label.className = "protection-derisk__leg-label";
  label.textContent = `${action} ${leg.reduce_quantity || 0} ${unit} ${leg.symbol}`;
  const tag = document.createElement("span");
  tag.className = `protection-derisk__badge protection-derisk__badge--${badge}`;
  tag.textContent = badge;
  row.append(label, tag);
  if (hasNumericValue(leg.notional) && leg.notional !== 0) {
    const n = document.createElement("span");
    n.className = "protection-derisk__leg-notional";
    n.textContent = money(leg.notional, leg.notional_currency || "");
    row.append(n);
  }
  if (hasNumericValue(leg.risk_contribution_cut) && leg.risk_contribution_cut !== 0) {
    const risk = document.createElement("span");
    risk.className = "protection-derisk__leg-risk";
    // risk_contribution_cut is delta-adjusted risk in the sweep's base
    // currency (TradeProposalReduceLeg), not the leg's contract currency.
    risk.textContent = `cuts ~${money(leg.risk_contribution_cut, baseCurrency)} of risk`;
    row.append(risk);
  }
  const blocker = (leg.blockers || [])[0];
  if (blocker) {
    const why = document.createElement("span");
    why.className = "protection-derisk__leg-why";
    why.textContent = blocker.message;
    row.append(why);
  }
  if (leg.order_ref) {
    const ref = document.createElement("span");
    ref.className = "protection-derisk__leg-ref";
    ref.textContent = `ref ${leg.order_ref}`;
    row.append(ref);
  }
  return row;
}


// deriskRequestRef is a per-preview idempotency key so a double-tapped Submit
// (or a client retry) replays the daemon's basket result instead of placing it
// twice. Not cryptographic — uniqueness within a session is all that is needed,
// and crypto.randomUUID is unavailable on non-secure LAN origins.
function deriskRequestRef() {
  return `derisk-${Date.now()}-${Math.random().toString(36).slice(2, 10)}`;
}

async function previewProtectionDerisk() {
  const d = state.protectionDerisk;
  d.busy = "preview";
  d.result = null;
  d.submitted = null;
  d.previewedAt = 0;
  d.requestRef = deriskRequestRef();
  const abort = new AbortController();
  d.abort = abort;
  renderProtectionDerisk();
  try {
    const res = await fetch("/api/proposals/reduce-portfolio/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({ percent: d.percent, timeout_ms: 5000 }),
      signal: abort.signal,
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    if (d.abort !== abort) return; // cancelled/superseded while in flight
    d.result = body;
    // The validity clock starts when the basket lands, not when the sweep
    // started — the earliest legs are already sweep-duration old here.
    d.previewedAt = Date.now();
  } catch (err) {
    if (d.abort === abort && !abort.signal.aborted) {
      d.result = { blockers: [{ code: "preview_failed", message: err.message }] };
    }
  } finally {
    if (d.abort === abort) {
      d.abort = null;
      if (d.busy === "preview") d.busy = "";
    }
    renderProtectionDerisk();
  }
}


// cancelProtectionDerisk returns the panel to idle from any non-write state:
// it aborts an in-flight preview fetch, discards a rendered basket, or
// dismisses a submitted result. Aborting the fetch only frees the UI and the
// app-layer wait — the daemon dispatch loop is synchronous per connection, so
// an already-running sweep quietly finishes server-side (read-only, no
// orders). A busy submit is never cancellable from here.
function cancelProtectionDerisk() {
  const d = state.protectionDerisk;
  if (d.busy === "submit") return;
  if (d.busy === "preview" && d.abort) d.abort.abort();
  d.abort = null;
  d.busy = "";
  d.result = null;
  d.submitted = null;
  d.previewedAt = 0;
  renderProtectionDerisk();
}

async function submitProtectionDerisk() {
  const d = state.protectionDerisk;
  if (!d.result || (d.result.eligible_count || 0) === 0) return;
  // Belt-and-braces against stale DOM: the Submit button is withdrawn at
  // expiry by the next render, but a click racing that render must not slip
  // through on lapsed numbers.
  if (deriskPreviewExpired()) {
    renderProtectionDerisk();
    return;
  }
  const confirmation = protectionWriteConfirmation();
  if (!confirmation) {
    d.submitted = { blockers: [{ code: "confirmation_cancelled", message: "broker submit confirmation was cancelled" }] };
    renderProtectionDerisk();
    return;
  }
  d.busy = "submit";
  renderProtectionDerisk();
  try {
    const res = await fetch("/api/proposals/reduce-portfolio/submit", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        percent: d.percent,
        timeout_ms: 5000,
        request_ref: d.requestRef || deriskRequestRef(),
        confirm_account: confirmation.account,
        confirm_mode: confirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    d.submitted = body;
    d.result = null;
    await refreshOpenOrders();
  } catch (err) {
    d.submitted = { blockers: [{ code: "submit_failed", message: err.message }] };
  } finally {
    if (d.busy === "submit") d.busy = "";
    renderProtectionDerisk();
  }
}


// Theta/day prefers the daemon's base-currency aggregate so the panel's
// money metrics share one currency. Every branch that returns a numeric
// value also names its currency — back when money(value, "") coerced to
// USD, "$729.87" sat next to "€12K" in one panel; money() now renders
// unknown currencies bare, but naming the currency per branch stays the
// contract.
function protectionThetaSummary(proposals = {}, rows = []) {
  const counts = proposals.counts || {};
  const baseCurrency = normalizeCurrency(counts.base_currency || "");
  if (hasNumericValue(counts.theta_per_day_base) && baseCurrency) {
    return {
      value: counts.theta_per_day_base,
      currency: baseCurrency,
      title: "Theta/day represented by theta-hygiene proposals that crossed policy thresholds, converted to the account base currency; zero means no theta-hygiene action is pending.",
    };
  }
  const servedCurrency = normalizeCurrency(counts.theta_per_day_currency || "");
  if (hasNumericValue(counts.theta_per_day) && servedCurrency) {
    return {
      value: counts.theta_per_day,
      currency: servedCurrency,
      title: "Theta/day represented by theta-hygiene proposals that crossed policy thresholds; zero means no theta-hygiene action is pending.",
    };
  }
  const thetaRows = rows.filter((row) => row.bucket === "theta_hygiene" && hasNumericValue(row.theta_per_day));
  const rowCurrencies = [...new Set(thetaRows.map((row) => normalizeCurrency(row.contract?.currency)).filter(Boolean))];
  if (thetaRows.length > 0 && rowCurrencies.length === 1) {
    return {
      value: thetaRows.reduce((sum, row) => sum + Math.abs(row.theta_per_day), 0),
      currency: rowCurrencies[0],
      title: "Theta/day summed from visible theta-hygiene proposal rows.",
    };
  }
  if (thetaRows.length > 0) {
    return {
      value: null,
      mixed: true,
      title: "Theta-hygiene proposals span multiple currencies and no base-currency conversion is available in this snapshot.",
    };
  }
  if ((proposals.blockers || []).length === 0 && counts.theta_hygiene === 0) {
    // "€0.00 pending" is only honest when the engine could actually see the
    // book's Greeks; with partial coverage a zero means "could not evaluate",
    // which must render as unavailable rather than as comfort.
    const portfolio = state.snapshot?.positions?.portfolio || {};
    const total = Number(portfolio.greeks_total || 0);
    const covered = Number(portfolio.greeks_coverage || 0);
    if (total > 0 && covered < total) {
      return {
        value: null,
        title: `Greeks are unavailable for ${total - covered} of ${total} option legs, so theta-hygiene proposals cannot be fully evaluated in this snapshot.`,
      };
    }
    return {
      value: 0,
      currency: baseCurrency || protectionCoverageBaseCurrency(currentProtectionCoverage() || {}),
      title: "No theta-hygiene action is above policy threshold.",
    };
  }
  if (hasNumericValue(counts.theta_per_day) && counts.theta_per_day !== 0) {
    return {
      value: null,
      mixed: true,
      title: "Theta-hygiene exposure exists but is not summable to a single currency in this snapshot.",
    };
  }
  return {
    value: null,
    title: "Actionable theta proposal exposure is unavailable in this snapshot.",
  };
}

function renderProtectionTimestamp(proposals = {}) {
  // The badge threshold derives from the served refresh cadence
  // ([auto_trade].proposal_cadence rides inside the proposal snapshot), so
  // the SPA never hardcodes a twin of daemon policy: one full cycle plus
  // grace — max(3m, cadence/3) — keeps a healthy panel out of "stale"
  // while a genuinely missed cycle flags within minutes. 30s cadence → 4m
  // threshold; a 15m override keeps the historical 20m.
  const cadence = goDurationMinutes(proposals.auto_trade?.proposal_cadence) ?? 0.5;
  const staleMinutes = Math.ceil(cadence + Math.max(3, cadence / 3));
  renderFreshnessTimestamp("protectionAsOf", proposals.as_of, { staleMinutes, quietWhenFresh: true });
}


// goDurationMinutes parses a Go time.Duration string ("2m0s", "1h2m3s",
// "90s") into minutes; null when unparseable so callers keep a fallback.
function goDurationMinutes(value) {
  const raw = String(value || "").trim();
  if (!raw) return null;
  let seconds = 0;
  let matched = false;
  for (const [, num, unit] of raw.matchAll(/(\d+(?:\.\d+)?)(h|ms|m|s)/g)) {
    matched = true;
    const v = Number(num);
    if (unit === "h") seconds += v * 3600;
    else if (unit === "m") seconds += v * 60;
    else if (unit === "ms") seconds += v / 1000;
    else seconds += v;
  }
  return matched && seconds > 0 ? seconds / 60 : null;
}


// Single-name "% of NLV" figures are concentration vs equity — under
// margin they legitimately sum past 100%. Surfacing the account's served
// gross exposure beside them lets the panel arithmetic reconcile on
// sight instead of looking like broken math.
function renderProtectionExposure() {
  const el = $("protectionExposure");
  if (!el) return;
  const gross = state.snapshot?.canary?.portfolio?.gross_exposure_pct_nlv;
  if (typeof gross !== "number") {
    el.hidden = true;
    el.replaceChildren();
    return;
  }
  const label = document.createElement("span");
  label.textContent = "Portfolio context";
  const value = document.createElement("b");
  value.textContent = `Gross exposure ${pct(gross)} of NLV`;
  el.replaceChildren(label, value);
  el.title = "Source: canary portfolio snapshot. Gross market value can exceed NLV under margin, so per-name % of NLV figures can sum past 100%.";
  el.hidden = false;
}

function protectionReason(proposals = {}, autoTrade = {}) {
  const blocker = (proposals.blockers || autoTrade.blockers || [])[0];
  if (blocker) return blockerText(blocker);
  if (autoTrade.policy?.status && autoTrade.policy.status !== "active" && autoTrade.policy.status !== "default") {
    return `Policy ${autoTrade.policy.status}`;
  }
  return autoTrade.fast_path_enabled === false ? "Fast path disabled" : "";
}

function protectionSnapshotRefreshReason() {
  if (state.protectionSnapshotBusy) return "Refreshing proposals";
  return state.protectionSnapshotNotice || "";
}

function protectionNeedsSnapshotSync(proposals = {}, autoTrade = {}) {
  if (!state.protectionOpen || state.protectionSnapshotBusy) return false;
  if (autoTrade.proposals_enabled === false) return false;
  const policyStatus = String(proposals.policy_status?.status || autoTrade.policy?.status || "").toLowerCase();
  if (policyStatus === "disabled") return false;
  const revision = String(proposals.revision || "");
  const blockerCodes = [
    ...(proposals.blockers || []),
    ...(autoTrade.blockers || []),
  ].map((blocker) => String(blocker.code || ""));
  if (blockerCodes.some(protectionTransientSnapshotBlocker)) return true;
  if ((proposals.proposals || []).length > 0) return false;
  return !revision || revision === "empty";
}

function protectionTransientSnapshotBlocker(code = "") {
  return [
    "account_unavailable",
    "positions_unavailable",
    "positions_pending",
    "trading_status_unavailable",
    "market_events_unavailable",
  ].includes(code);
}

function queueProtectionSnapshotSync() {
  const now = Date.now();
  if (state.protectionSnapshotBusy || now - state.protectionSnapshotLastAt < 10000) return;
  state.protectionSnapshotBusy = true;
  state.protectionSnapshotLastAt = now;
  state.protectionSnapshotNotice = "";
  setTimeout(() => {
    syncProtectionSnapshot();
  }, 0);
}

async function syncProtectionSnapshot() {
  try {
    const res = await fetch("/api/proposals", { credentials: "include", cache: "no-store" });
    if (!res.ok) throw new Error(await res.text());
    const proposals = await res.json();
    applyProtectionSnapshot(proposals);
    const proposalCount = proposals.counts?.total ?? (proposals.proposals || []).length;
    state.protectionSnapshotNotice = proposalCount > 0 ? "" : "No protection proposals available yet";
  } catch (err) {
    state.protectionSnapshotNotice = "Proposal refresh failed: " + shortPreviewMessage(err.message);
  } finally {
    state.protectionSnapshotBusy = false;
    renderAll();
  }
}

function protectionRow(proposal) {
  const row = document.createElement("div");
  row.className = "protection-row";
  const marketEvents = state.snapshot?.market_events || {};
  const effectiveBlockers = protectionEffectiveBlockers(proposal, marketEvents);
  const blocked = effectiveBlockers.length > 0;
  const previewFlow = protectionUsesPreviewFlow(proposal);
  const tradability = previewFlow ? protectionPreviewGate(proposal) : protectionSubmitGate(proposal);
  const previewKey = protectionPreviewStateKey(proposal);
  const previewBusy = state.protectionPreviewBusy === previewKey;
  const previewResult = state.protectionPreviews[previewKey] || null;
  const finalSubmitGate = previewFlow ? protectionPreviewSubmitGate(proposal, previewResult) : null;
  const submitBusy = state.protectionSubmitBusy === previewKey;
  const submitResult = state.protectionSubmits[previewKey] || null;
  const copy = document.createElement("div");
  copy.className = "protection-row__copy";
  const bucket = document.createElement("span");
  bucket.className = "protection-row__bucket";
  bucket.textContent = protectionBucketLabel(proposal);
  const title = document.createElement("b");
  title.className = "protection-row__title";
  title.textContent = protectionProposalTitle(proposal);
  copy.append(bucket, title);
  const stepper = protectionQuantityStepper(proposal);
  if (stepper) copy.append(stepper);
  const quoteLine = protectionQuoteLine(proposal);
  if (quoteLine) copy.append(quoteLine);
  const positionLine = protectionPositionLine(proposal);
  if (positionLine) copy.append(positionLine);
  const metricText = protectionMetricText(proposal);
  const riskTicket = protectionRiskTicket(proposal, metricText);
  if (riskTicket) {
    copy.append(riskTicket);
    const ladder = protectionStopLadder(proposal);
    if (ladder) copy.append(ladder);
  } else if (metricText) {
    const metric = document.createElement("small");
    metric.className = "protection-row__trail";
    if (protectionTrailSizingFallback(proposal)) {
      metric.classList.add("protection-row__trail--fallback");
    }
    metric.textContent = metricText;
    copy.append(metric);
  }
  const blockerText = blocked ? protectionBlockerText({ ...proposal, blockers: effectiveBlockers }) : "";
  if (blockerText) {
    const blocker = document.createElement("small");
    blocker.className = "protection-row__blocker";
    blocker.textContent = blockerText;
    copy.append(blocker);
  }
  const previewText = protectionPreviewText(previewResult, proposal);
  if (previewText) {
    const preview = document.createElement("small");
    preview.className = "protection-row__preview";
    preview.textContent = previewText;
    copy.append(preview);
  }
  const submitStateText = previewFlow ? protectionSubmitStateText({
    result: submitResult,
    gate: finalSubmitGate,
    busy: submitBusy,
    previewResult,
    proposal,
  }) : "";
  if (submitStateText) {
    const submitState = document.createElement("small");
    submitState.className = protectionSubmitStateClass({ result: submitResult, gate: finalSubmitGate, busy: submitBusy });
    submitState.textContent = submitStateText;
    copy.append(submitState);
  }
  const reasonText = protectionReasonText(proposal, { metricShown: Boolean(metricText) });
  if (reasonText) {
    const reason = document.createElement("small");
    reason.className = "protection-row__reason";
    reason.textContent = reasonText;
    copy.append(reason);
  }
  const flagRow = marketFlagRow(protectionDecisionFlags(proposal, marketEvents));
  if (flagRow) copy.append(flagRow);
  const actions = document.createElement("div");
  actions.className = "protection-row__actions";
  const primary = document.createElement("button");
  primary.type = "button";
  primary.className = previewFlow ? "protection-preview" : proposal.action === "BUY" ? "protection-buy" : "protection-sell";
  primary.textContent = previewBusy ? "Previewing" : protectionSubmitLabel(proposal);
  primary.disabled = blocked || previewBusy || submitBusy || !tradability.ready;
  primary.title = protectionButtonTitle(proposal, { blocked, previewBusy, tradability });
  primary.addEventListener("click", () => {
    if (previewFlow) {
      previewProtectionProposal(proposal);
      return;
    }
    submitProtectionProposal(proposal);
  });
  actions.append(primary);
  if (previewFlow && (submitResult || submitBusy || (previewResult && !previewResult.pending))) {
    const finalSubmit = document.createElement("button");
    finalSubmit.type = "button";
    finalSubmit.className = "protection-submit";
    finalSubmit.textContent = submitBusy ? "Submitting" : protectionFinalSubmitLabel(proposal);
    finalSubmit.disabled = blocked || previewBusy || submitBusy || !finalSubmitGate.ready;
    finalSubmit.title = protectionSubmitButtonTitle({ blocked, previewBusy, submitBusy, gate: finalSubmitGate });
    finalSubmit.addEventListener("click", () => submitProtectionProposal(proposal));
    actions.append(finalSubmit);
  }
  const ignore = document.createElement("button");
  ignore.type = "button";
  ignore.className = "protection-ignore";
  ignore.textContent = "Ignore";
  ignore.title = "Ignore this proposal; no market order is sent";
  ignore.addEventListener("click", () => ignoreProtectionProposal(proposal));
  actions.append(ignore);
  row.append(copy, actions);
  return row;
}

function protectionProposalTitle(proposal = {}) {
  return [
    protectionSideLabel(proposal),
    protectionEffectiveQuantity(proposal) || 0,
    proposal.symbol || "--",
    protectionContractLabel(proposal.contract || {}),
  ].filter(Boolean).join(" ");
}

function protectionSubmitLabel(proposal = {}) {
  if (proposal.bucket === "trailing_stop") return "Preview stop";
  return "Preview";
}

function protectionUsesPreviewFlow(proposal = {}) {
  return true;
}

function protectionFinalSubmitLabel(proposal = {}) {
  if (proposal.bucket === "trailing_stop") return "Submit stop";
  return "Submit order";
}

function protectionButtonTitle(proposal = {}, gate = {}) {
  if (gate.blocked) return protectionBlockerText(proposal);
  if (gate.previewBusy) return "Broker WhatIf preview is running; no order has been placed";
  if (!gate.tradability?.ready) return gate.tradability?.reason || "Protection action is unavailable";
  return protectionActionTitle(proposal, gate.tradability.reason);
}

function protectionSideLabel(proposal = {}) {
  if (proposal.bucket !== "trailing_stop") return protectionActionLabel(proposal);
  if (proposalIsBuyToCover(proposal)) return "Buy to cover stop";
  return String(proposal.action || "--").toUpperCase() === "BUY" ? "Buy stop" : "Sell stop";
}

function protectionBucketLabel(proposal = {}) {
  if (proposal.bucket === "trailing_stop") return "Broker stop";
  return labelize(proposal.bucket || "--");
}

function protectionActionLabel(proposal = {}) {
  if (proposalIsBuyToCover(proposal)) {
    // "Buy to cover" is stock-borrow terminology; a reducing BUY on a
    // short option is a buy-to-close.
    const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
    return secType === "OPT" || secType === "OPTION" ? "Buy to close" : "Buy to cover";
  }
  return String(proposal.action || "--").toUpperCase() === "BUY" ? "Buy" : "Sell";
}

function protectionActionTitle(proposal = {}, fallback = "") {
  if (proposal.bucket === "trailing_stop" && String(proposal.action || "").toUpperCase() === "SELL") {
    return [
      "Preview a broker trailing stop sell order. Once submitted, IBKR maintains the stop and raises it as the instrument price rises above the submission reference.",
      protectionMarketStateHint(proposal),
    ].filter(Boolean).join(" ");
  }
  if (proposal.bucket === "trailing_stop" && String(proposal.action || "").toUpperCase() === "BUY") {
    return [
      "Preview a broker trailing stop buy-to-close order. Once submitted, IBKR maintains the stop as the instrument price moves in favor of the short position.",
      protectionMarketStateHint(proposal),
    ].filter(Boolean).join(" ");
  }
  return fallback || "Preview this protection proposal";
}

function protectionMarketStateHint(proposal = {}) {
  const calendar = protectionMarketCalendar(proposal);
  const marketName = proposalMarketLabel(proposalMarketKey(proposal));
  const session = calendar?.session;
  if (!session) {
    return `${marketName} calendar unavailable; broker WhatIf remains the submit authority.`;
  }
  if (session.is_open) {
    return `${marketName} is currently tradable.`;
  }
  const label = marketSessionLabel(calendar);
  const market = label.phase || label.text || `${marketName} is closed`;
  return `${market}; broker may queue after fresh WhatIf.`;
}


// protectionReasonText keeps reason prose off rows where it restates what
// the row already shows: trailing-stop reasons were constant boilerplate
// (the mechanics live in the action-button titles), and the theta/risk
// reason sentences duplicate the metric line. The prose remains as the
// fallback when the typed fields behind the metric line are missing.
function protectionReasonText(proposal = {}, { metricShown = false } = {}) {
  if (proposal.bucket === "trailing_stop") return "";
  if (metricShown) return "";
  return proposal.reason || "";
}


// protectionMetricText renders the one decision number per bucket:
// trailing stop → live stop level + offset + TIF; theta hygiene → daily
// theta burn + DTE; risk reduction → concentration vs NLV + excess to
// trim. Parts vanish individually when their typed field is missing —
// never fabricated.
function protectionMetricText(proposal = {}) {
  if (proposal.bucket === "trailing_stop") {
    const trail = proposal.trail || null;
    if (!trail) return "";
    const parts = [];
    const live = protectionLiveTrailStop(proposal, trail);
    if (live && protectionStopChanged(trail.initial_stop_price, live.stop)) {
      parts.push(`stop ${numberRead(live.stop)}`);
    } else if (hasNumericValue(trail.initial_stop_price) && trail.initial_stop_price > 0) {
      parts.push(`stop ${numberRead(trail.initial_stop_price)}`);
    } else if (live) {
      parts.push(`stop ${numberRead(live.stop)}`);
    }
    const offset = protectionTrailOffsetLabel(trail);
    if (offset) parts.push(offset);
    const sizing = protectionTrailSizingLabel(proposal.trail_sizing);
    if (sizing) parts.push(sizing);
    if (hasNumericValue(trail.limit_offset)) {
      parts.push(`limit offset ${numberRead(trail.limit_offset)}`);
    }
    const tif = String(proposal.tif || "").trim().toUpperCase();
    if (tif) parts.push(tif);
    return parts.join(" · ");
  }
  if (proposal.bucket === "theta_hygiene") {
    const parts = [];
    if (hasNumericValue(proposal.theta_per_day) && proposal.theta_per_day > 0) {
      const thetaCurrency = normalizeCurrency(proposal.contract?.currency || "");
      parts.push(`theta ${thetaCurrency ? money(proposal.theta_per_day, thetaCurrency) : numberRead(proposal.theta_per_day)}/day`);
    }
    const dte = protectionProposalDTE(proposal);
    if (dte !== null) parts.push(`${dte} DTE`);
    return parts.join(" · ");
  }
  if (proposal.bucket === "risk_reduction") {
    const parts = [];
    if (typeof proposal.market_value_pct_nlv === "number") {
      parts.push(`${pct(Math.abs(proposal.market_value_pct_nlv))} of NLV`);
    }
    if (hasNumericValue(proposal.risk_excess_notional) && proposal.risk_excess_notional > 0) {
      parts.push(`${compactWholeMoney(proposal.risk_excess_notional, proposal.risk_excess_currency || "")} over target`);
    }
    return parts.join(" · ");
  }
  return "";
}

function protectionRiskTicket(proposal = {}, metricText = "") {
  if (proposal.bucket !== "trailing_stop") return null;
  const parts = protectionRiskTicketParts(proposal, metricText);
  if (parts.length === 0) return null;
  const ticket = document.createElement("small");
  ticket.className = "protection-row__trail protection-row__risk-ticket";
  if (protectionTrailSizingFallback(proposal)) {
    ticket.classList.add("protection-row__trail--fallback");
  }
  parts.forEach((part, index) => {
    if (index > 0) ticket.append(document.createTextNode(" · "));
    const item = document.createElement("span");
    item.textContent = part;
    ticket.append(item);
  });
  const title = protectionRiskTicketTitle(proposal);
  if (title) ticket.title = title;
  return ticket;
}

function protectionRiskTicketParts(proposal = {}, metricText = "") {
  const parts = [];
  if (metricText) parts.push(metricText);
  const trigger = protectionExecutionTriggerLabel(proposal.execution_semantics);
  if (trigger) parts.push(`trigger ${trigger}`);
  const loss = protectionStopRiskLossLabel(proposal.stop_risk);
  if (loss) parts.push(`est. loss ${loss}`);
  const gap = protectionStopRiskGapLabel(proposal.stop_risk);
  if (gap) parts.push(`${protectionStopRiskGapName(proposal.stop_risk?.gap_scenario)} ${gap}`);
  const warning = protectionExecutionWarningLabel(proposal.execution_semantics);
  if (warning) parts.push(warning);
  return parts;
}

function protectionExecutionTriggerLabel(semantics = {}) {
  if (!semantics) return "";
  const side = String(semantics.reference_side || "").trim().toLowerCase();
  const trigger = String(semantics.trigger_method_label || semantics.trigger_source || "").trim();
  return [side, trigger].filter(Boolean).join(" / ");
}


// protectionLossCurrency labels an estimated-loss amount: base-converted
// losses carry the risk block's base currency (account base as fallback),
// contract-currency losses carry the row currency. Never coerce a
// base-converted amount to the contract currency, or anything to USD —
// unknown stays "" and renders as a bare number.
function protectionLossCurrency(usedBase, risk = {}) {
  if (usedBase) return risk.base_currency || state.snapshot?.account?.base_currency || "";
  return risk.currency || "";
}

function protectionStopRiskLossLabel(risk = {}) {
  if (!risk) return "";
  const value = firstNumber(risk.estimated_loss_base, risk.estimated_loss_ccy);
  if (!hasNumericValue(value)) return "";
  const currency = protectionLossCurrency(risk.estimated_loss_base !== undefined, risk);
  const pctNLV = hasNumericValue(risk.estimated_loss_pct_nlv) ? ` (${pct(risk.estimated_loss_pct_nlv)} NLV)` : "";
  return `${compactWholeMoney(Math.abs(value), currency)}${pctNLV}`;
}

function protectionStopRiskGapLabel(risk = {}) {
  const gap = risk?.gap_scenario || null;
  if (!gap) return "";
  const value = firstNumber(gap.estimated_loss_base, gap.estimated_loss_ccy);
  if (!hasNumericValue(value)) return "";
  const currency = protectionLossCurrency(gap.estimated_loss_base !== undefined, risk);
  const pctNLV = hasNumericValue(gap.estimated_loss_pct_nlv) ? ` (${pct(gap.estimated_loss_pct_nlv)} NLV)` : "";
  return `${compactWholeMoney(Math.abs(value), currency)}${pctNLV}`;
}

function protectionStopRiskGapName(gap = {}) {
  if (hasNumericValue(gap?.gap_pct)) return `${pct(gap.gap_pct)} gap`;
  const label = cleanDetail(String(gap?.label || "").replaceAll("_", " "));
  return label && label !== "--" ? label : "gap scenario";
}

function protectionExecutionWarningLabel(semantics = {}) {
  const guarantee = String(semantics?.price_guarantee || "").toLowerCase();
  if (guarantee === "stop_limit_can_leave_position_unfilled") return "limit may not fill";
  if (guarantee === "stop_price_is_not_execution_price") return "trigger becomes market";
  return "";
}

function protectionRiskTicketTitle(proposal = {}) {
  const semantics = proposal.execution_semantics || {};
  const parts = [];
  if (semantics.trigger_effect === "market_order_when_triggered") {
    parts.push("TRAIL converts to a market order when triggered; stop price is not the execution price.");
  } else if (semantics.trigger_effect === "limit_order_when_triggered") {
    parts.push("TRAIL LIMIT converts to a limit order when triggered; the position can remain open if the limit does not fill.");
  }
  const ladder = protectionStopLadderLabel(proposal.stop_ladder || [], proposal.stop_risk);
  if (ladder) parts.push(`Stop ladder: ${ladder}`);
  return parts.join(" ");
}

function protectionStopLadder(proposal = {}) {
  if (proposal.bucket !== "trailing_stop") return null;
  const steps = protectionStopLadderDisplaySteps(proposal.stop_ladder || []);
  if (steps.length === 0) return null;
  const wrap = document.createElement("div");
  wrap.className = "protection-row__ladder";
  wrap.setAttribute("aria-label", "Stop ladder comparison");
  const heading = document.createElement("span");
  heading.className = "protection-row__ladder-label";
  heading.textContent = "Stop ladder";
  wrap.append(heading);
  for (const step of steps) {
    const item = document.createElement("span");
    item.className = `protection-row__ladder-step protection-row__ladder-step--${protectionStopLadderStepClass(step)}`;
    const label = document.createElement("b");
    label.textContent = protectionStopLadderShortLabel(step);
    const detail = document.createElement("span");
    detail.textContent = protectionStopLadderStepDetail(step, proposal.stop_risk || {});
    item.title = protectionStopLadderStepTitle(step, proposal.stop_risk || {});
    item.append(label, detail);
    wrap.append(item);
  }
  return wrap;
}

function protectionStopLadderDisplaySteps(ladder = []) {
  const preferred = ["fixed_5pct", "fixed_10pct", "policy_chosen", "atr_candidate", "matched_open_stop"];
  const byKind = new Map();
  for (const step of ladder || []) {
    const kind = String(step.kind || step.label || "").toLowerCase();
    if (kind && !byKind.has(kind)) byKind.set(kind, step);
  }
  const steps = [];
  for (const kind of preferred) {
    if (byKind.has(kind)) steps.push(byKind.get(kind));
  }
  for (const step of ladder || []) {
    if (steps.includes(step)) continue;
    steps.push(step);
    if (steps.length >= 5) break;
  }
  return steps.slice(0, 5);
}

function protectionStopLadderStepClass(step = {}) {
  const kind = String(step.kind || step.label || "").toLowerCase();
  if (kind.includes("policy_chosen")) return "selected";
  if (kind.includes("matched_open")) return "open";
  if (kind.includes("max") || kind.includes("min")) return "bound";
  return "plain";
}

function protectionStopLadderShortLabel(step = {}) {
  const kind = String(step.kind || "").toLowerCase();
  const label = String(step.label || "").trim();
  if (kind === "fixed_5pct") return "5%";
  if (kind === "fixed_10pct") return "10%";
  if (kind === "policy_chosen") return "Policy";
  if (kind === "atr_candidate") return "ATR";
  if (kind === "policy_min") return "Min";
  if (kind === "policy_max") return "Max";
  if (kind === "matched_open_stop") return "Open";
  return label || "Stop";
}

function protectionStopLadderStepDetail(step = {}, risk = {}) {
  const parts = [];
  if (hasNumericValue(step.stop_price)) parts.push(numberRead(step.stop_price));
  const loss = firstNumber(step.estimated_loss_base, step.estimated_loss_ccy);
  if (hasNumericValue(loss)) {
    // Ladder steps carry no per-step currency field; the parent risk block
    // labels base and contract-currency losses.
    const currency = protectionLossCurrency(step.estimated_loss_base !== undefined, risk);
    parts.push(compactWholeMoney(Math.abs(loss), currency));
  }
  return parts.join(" / ") || "--";
}

function protectionStopLadderStepTitle(step = {}, risk = {}) {
  const detail = protectionStopLadderStepDetail(step, risk);
  const pctText = hasNumericValue(step.percent) ? `${pct(step.percent)} offset. ` : "";
  return `${step.label || protectionStopLadderShortLabel(step)}: ${pctText}stop/loss ${detail}.`;
}

function protectionStopLadderLabel(ladder = [], risk = {}) {
  return (ladder || []).slice(0, 5).map((step) => {
    const loss = firstNumber(step.estimated_loss_base, step.estimated_loss_ccy);
    const currency = protectionLossCurrency(step.estimated_loss_base !== undefined, risk);
    const amount = hasNumericValue(loss) ? compactWholeMoney(Math.abs(loss), currency) : "";
    const stop = hasNumericValue(step.stop_price) ? `stop ${numberRead(step.stop_price)}` : "";
    return [step.label, stop, amount].filter(Boolean).join(" ");
  }).filter(Boolean).join("; ");
}

function protectionProposalDTE(proposal = {}) {
  for (const value of proposal.details || []) {
    const match = String(value || "").match(/^dte=(\d+)$/);
    if (match) return Number(match[1]);
  }
  return null;
}


// protectionDecisionFlags keeps only the flag chips that bear on acting
// on this row: hard blockers (halt/LULD) and execution friction (borrow
// tight, fee extreme, Reg SHO). Context-only and stale/unknown-source
// chips stay on the hero rail and detail surfaces — repeated per row they
// are noise, not risk disclosure.
function protectionDecisionFlags(proposal = {}, events = {}) {
  return protectionEffectiveMarketFlags(proposal, events).filter((flag) => {
    const tone = marketEventTone(flag);
    return tone === "hard" || tone === "friction";
  });
}


// protectionQuoteFor resolves the live quote a row's action would execute
// against: stocks/ETFs read the shared market-quote poller (~15s tick);
// option rows read the position leg's own premium bid/ask from the greeks
// cache (~60s tick). Null when unavailable — nil means unavailable, the
// row simply shows no quote line.
function protectionQuoteFor(proposal = {}) {
  const action = String(proposal.action || "").toUpperCase();
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  if (secType === "OPT" || secType === "OPTION") {
    const leg = protectionOptionLeg(proposal);
    if (!leg) return null;
    const price = action === "BUY" ? leg.option_ask : leg.option_bid;
    if (!hasNumericValue(price) || price <= 0) return null;
    return {
      label: action === "BUY" ? "ask premium" : "bid premium",
      price,
      at: leg.quote_price_at || "",
      stale: Boolean(leg.stale),
      info: leg.stale ? `stale option quote${leg.stale_reason ? `: ${leg.stale_reason}` : ""}` : "",
    };
  }
  const symbol = proposal.symbol || proposal.contract?.symbol || "";
  const quote = quoteBySymbol(state.snapshot?.market_quotes?.quotes || {}, symbol);
  if (!quote) return null;
  const price = action === "BUY" ? firstNumber(quote.ask, quote.ask_price) : firstNumber(quote.bid, quote.bid_price);
  if (!hasNumericValue(price) || price <= 0) return null;
  return {
    label: action === "BUY" ? "ask" : "bid",
    price,
    at: quote.quote_price_at || quote.price_at || quote.as_of || "",
    stale: protectionQuoteFrozen(quote),
    info: protectionQuoteStatusLabel(quote),
  };
}

function protectionOptionLeg(proposal = {}) {
  const legs = state.snapshot?.positions?.options || [];
  const contract = proposal.contract || {};
  const conID = Number(contract.con_id || 0);
  if (conID > 0) {
    const byID = legs.find((leg) => Number(leg.con_id || 0) === conID);
    if (byID) return byID;
  }
  const symbol = normalizeSymbol(proposal.symbol || contract.symbol || "");
  return legs.find((leg) =>
    normalizeSymbol(leg.symbol) === symbol &&
    String(leg.expiry || "") === String(contract.expiry || "") &&
    Number(leg.strike || 0) === Number(contract.strike || 0) &&
    String(leg.right || "").toUpperCase() === String(contract.right || "").toUpperCase()) || null;
}


// protectionQuoteFrozen mirrors the muted-gray rule for stale/unknown
// data: anything not firm+live renders muted and never tick-colored, so
// a frozen close can't masquerade as a live tick.
function protectionQuoteFrozen(quote = {}) {
  const quality = String(quote.quote_quality || "").trim().toLowerCase();
  const dataType = String(quote.data_type || "").trim().toLowerCase();
  return (quality !== "" && quality !== "firm") || (dataType !== "" && dataType !== "live");
}


// protectionPositionLine renders holding-level decision context: the
// exposure being acted on (position market value and its share of NLV) and
// today's P&L move, colored green/red by sign with an explicit +/- sign so
// direction reads without relying on color. Distinct from the metric line,
// which is the per-bucket decision number. Risk reduction acts on a whole
// single-name group, so it omits a leg share count and leads with the dollar
// exposure — the dollar value is the size there. Parts vanish individually
// when their typed field is missing; never fabricated.
function protectionPositionLine(proposal = {}) {
  const currency = proposal.contract?.currency || "";
  const parts = [];
  if (proposal.bucket !== "risk_reduction") {
    const qty = Math.abs(Number(proposal.position_quantity || 0));
    if (qty > 0) {
      parts.push(`Held: ${qty} ${protectionPositionUnitLabel(proposal)}`);
    }
  }
  if (hasNumericValue(proposal.position_market_value) && proposal.position_market_value !== 0) {
    let value = `Position value: ${money(proposal.position_market_value, currency)}`;
    if (typeof proposal.market_value_pct_nlv === "number") {
      value += ` (${pct(Math.abs(proposal.market_value_pct_nlv))} NLV)`;
    }
    parts.push(value);
  }
  const dayMoney = firstNumber(proposal.position_day_change_money);
  const hasDay = hasNumericValue(dayMoney);
  if (parts.length === 0 && !hasDay) return null;
  const line = document.createElement("small");
  line.className = "protection-row__position";
  if (parts.length > 0) line.append(document.createTextNode(parts.join(" · ")));
  if (hasDay) {
    line.append(document.createTextNode(`${parts.length > 0 ? " · " : ""}Today: `));
    const move = document.createElement("span");
    const dir = dayMoney > 0 ? "up" : dayMoney < 0 ? "down" : "";
    move.className = "protection-quote" + (dir ? ` protection-quote--${dir}` : "");
    // position_day_change_money may be a base-currency group aggregate, so
    // never fall back to the contract currency for its label.
    let text = signedMoneyRead(dayMoney, proposal.position_day_change_currency || "");
    if (hasNumericValue(proposal.position_day_change_pct)) {
      const p = proposal.position_day_change_pct;
      text += ` (${p > 0 ? "+" : ""}${p.toFixed(1)}%)`;
    }
    move.textContent = text;
    line.append(move);
  }
  return line;
}

function protectionPositionUnitLabel(proposal = {}) {
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  if (secType === "OPT" || secType === "OPTION") {
    const multiplier = Number(proposal.contract?.multiplier || 0);
    return multiplier > 0 ? `contracts (x${multiplier})` : "contracts";
  }
  return "shares";
}

function protectionQuoteLine(proposal = {}) {
  const quote = protectionQuoteFor(proposal);
  if (!quote) return null;
  const line = document.createElement("small");
  line.className = "protection-row__quote";
  const label = document.createElement("span");
  label.textContent = `${quote.label} `;
  const price = document.createElement("span");
  price.className = "protection-quote";
  if (quote.stale) {
    price.classList.add("protection-quote--stale");
  } else {
    const dir = protectionQuoteTickDir(proposal.key, quote.price, quote.at);
    if (dir) price.classList.add(`protection-quote--${dir}`);
  }
  price.textContent = numberRead(quote.price);
  line.append(label, price);
  if (quote.info) line.title = quote.info;
  return line;
}


// protectionQuoteTickDir colors the latest observed move: green up, red
// down, neutral (inherited gray) on first observation or when fresh data
// repeats the price. "Settled" means a NEWER data timestamp served the
// same price — re-renders without fresh data keep the current color, so
// expanding an unrelated panel doesn't wipe a flash; option legs without
// timestamps keep their direction until the premium actually changes.
function protectionQuoteTickDir(key, price, at = "") {
  const ticks = state.protectionQuoteTicks;
  const prev = ticks[key];
  if (!prev || !hasNumericValue(prev.price)) {
    ticks[key] = { price, at, dir: "" };
    return "";
  }
  if (price === prev.price) {
    if (at && prev.at && at !== prev.at) {
      ticks[key] = { price, at, dir: "" };
    }
    return ticks[key].dir;
  }
  const dir = price > prev.price ? "up" : "down";
  ticks[key] = { price, at: at || prev.at, dir };
  return dir;
}


// protectionQuantityStepper lets the trader trim more or less than the
// daemon proposed on risk-reduction rows. The choice is presentation
// state only: the daemon re-clamps to [1, max_quantity] at preview and
// submit, and the override dies with the proposal revision so a newly
// generated proposal always starts from its own quantity.
function protectionQuantityStepper(proposal = {}) {
  if (proposal.bucket !== "risk_reduction") return null;
  const max = Number(proposal.max_quantity || 0);
  if (max <= 1) return null;
  const proposed = Number(proposal.quantity || 0);
  const current = protectionEffectiveQuantity(proposal);
  const wrap = document.createElement("div");
  wrap.className = "protection-qty";
  const dec = document.createElement("button");
  dec.type = "button";
  dec.className = "protection-qty__step";
  dec.textContent = "−";
  dec.disabled = current <= 1;
  dec.setAttribute("aria-label", "Decrease sell size");
  dec.addEventListener("click", () => nudgeProtectionQuantity(proposal, -1));
  const value = document.createElement("span");
  value.className = "protection-qty__value";
  value.textContent = `${current} of ${max}`;
  const inc = document.createElement("button");
  inc.type = "button";
  inc.className = "protection-qty__step";
  inc.textContent = "+";
  inc.disabled = current >= max;
  inc.setAttribute("aria-label", "Increase sell size");
  inc.addEventListener("click", () => nudgeProtectionQuantity(proposal, 1));
  wrap.append(dec, value, inc);
  const unit = proposed > 0 && hasNumericValue(proposal.notional) && proposal.notional > 0
    ? proposal.notional / proposed
    : null;
  if (unit) {
    const approx = document.createElement("span");
    approx.className = "protection-qty__approx";
    approx.textContent = `≈ ${money(current * unit, proposal.contract?.currency || "")}`;
    wrap.append(approx);
  }
  if (current !== proposed) {
    const reset = document.createElement("button");
    reset.type = "button";
    reset.className = "protection-qty__reset";
    reset.textContent = `proposed ${proposed} ↺`;
    reset.title = "Reset to the proposed quantity";
    reset.addEventListener("click", () => setProtectionQuantity(proposal, proposed));
    wrap.append(reset);
  }
  return wrap;
}

const protectionQuantityAcceleratedStep = 10;

function protectionQuantityStepDelta(current = 0, direction = 1) {
  const dir = direction < 0 ? -1 : 1;
  const qty = Math.max(1, Math.trunc(Number(current || 0)));
  if (qty < protectionQuantityAcceleratedStep) return dir;
  if (qty <= protectionQuantityAcceleratedStep && dir < 0) return -1;
  if (qty % protectionQuantityAcceleratedStep !== 0) return dir;
  return dir * protectionQuantityAcceleratedStep;
}

function nudgeProtectionQuantity(proposal = {}, direction = 1) {
  const current = protectionEffectiveQuantity(proposal);
  setProtectionQuantity(proposal, current + protectionQuantityStepDelta(current, direction));
}

function protectionEffectiveQuantity(proposal = {}) {
  const override = state.protectionQtyOverrides[proposal.key];
  if (!override || override.revision !== proposal.revision) {
    return Number(proposal.quantity || 0);
  }
  return override.qty;
}

function setProtectionQuantity(proposal, qty) {
  const max = Number(proposal.max_quantity || 0) || 1;
  const clamped = Math.min(Math.max(Math.round(qty), 1), max);
  state.protectionQtyOverrides = {
    ...state.protectionQtyOverrides,
    [proposal.key]: { revision: proposal.revision, qty: clamped },
  };
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
}

function protectionTrailOffsetLabel(trail = {}) {
  if (hasNumericValue(trail.trailing_percent)) return `offset ${pct(trail.trailing_percent)}`;
  if (hasNumericValue(trail.trailing_amount)) return `offset ${numberRead(trail.trailing_amount)}`;
  return "";
}

function protectionTrailSizingFallback(proposal = {}) {
  return Boolean(proposal.trail_sizing?.fallback);
}

function protectionTrailSizingLabel(sizing = {}) {
  const chosen = Number(sizing.chosen_pct || 0);
  if (!hasNumericValue(chosen) || chosen <= 0) return "";
  const range = protectionTrailSizingRangeLabel(sizing);
  const prefix = range ? `${range}, ` : "";
  if (sizing.fallback) {
    return `${prefix}${pct(chosen)} fallback trail used (dynamic stop unavailable)`;
  }
  const source = protectionTrailSizingSourceLabel(sizing.selected_by);
  const max = Number(sizing.policy_max_pct || 0);
  const capped = sizing.capped && hasNumericValue(max) && max > 0
    ? `, capped at ${pct(max)}`
    : "";
  return `${prefix}chosen ${pct(chosen)} by ${source}${capped}`;
}

function protectionTrailSizingRangeLabel(sizing = {}) {
  const min = Number(sizing.policy_min_pct || 0);
  const max = Number(sizing.policy_max_pct || 0);
  if (!hasNumericValue(min) || !hasNumericValue(max) || min <= 0 || max <= 0) {
    return "";
  }
  return `range ${pct(min)}-${pct(max)}`;
}

function protectionTrailSizingSourceLabel(value = "") {
  switch (String(value || "").trim().toLowerCase()) {
    case "atr":
      return "ATR";
    case "spread_floor":
      return "spread";
    case "policy_default":
      return "policy";
    case "policy_min":
      return "policy min";
    default:
      return String(value || "policy").replaceAll("_", " ");
  }
}

function protectionLiveTrailStop(proposal = {}, trail = {}) {
  const symbol = proposal.symbol || proposal.contract?.symbol;
  const action = String(proposal.action || "").toUpperCase();
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  const quote = secType === "OPT" ? null : quoteBySymbol(state.snapshot?.market_quotes?.quotes || {}, symbol);
  const quoteLabel = protectionReferenceLabel(proposal);
  const inferredReference = protectionInferredReference(proposal, trail, action);
  const reference = quote
    ? action === "BUY"
      ? firstNumber(quote.ask, quote.ask_price, inferredReference)
      : firstNumber(quote.bid, quote.bid_price, inferredReference)
    : inferredReference;
  if (!hasNumericValue(reference) || reference <= 0) return null;
  const offset = hasNumericValue(trail.trailing_percent)
    ? reference * trail.trailing_percent / 100
    : trail.trailing_amount;
  if (!hasNumericValue(offset) || offset <= 0) return null;
  const stop = quote
    ? action === "BUY" ? reference + offset : Math.max(reference - offset, 0.0001)
    : hasNumericValue(trail.initial_stop_price) ? trail.initial_stop_price : action === "BUY" ? reference + offset : Math.max(reference - offset, 0.0001);
  return { reference, quoteLabel, stop, quoteInfo: protectionQuoteStatusLabel(quote) };
}

function protectionReferenceLabel(proposal = {}) {
  const action = String(proposal.action || "").toUpperCase();
  const secType = String(proposal.sec_type || proposal.contract?.sec_type || "").toUpperCase();
  if (secType === "OPT") return action === "BUY" ? "ask premium" : "bid premium";
  return action === "BUY" ? "ask" : "bid";
}

function protectionInferredReference(proposal = {}, trail = {}, action = "") {
  const amount = hasNumericValue(trail.trailing_amount) ? trail.trailing_amount : null;
  const percent = hasNumericValue(trail.trailing_percent) ? trail.trailing_percent : null;
  const stop = hasNumericValue(trail.initial_stop_price) ? trail.initial_stop_price : null;
  if (amount && stop) {
    return action === "BUY" ? Math.max(stop - amount, 0.0001) : stop + amount;
  }
  if (percent && stop) {
    const ratio = percent / 100;
    if (action === "BUY") return Math.max(stop / (1 + ratio), 0.0001);
    if (ratio < 1) return Math.max(stop / (1 - ratio), 0.0001);
  }
  return null;
}

function protectionQuoteStatusLabel(quote = null) {
  if (!quote) return "";
  const parts = [];
  const dataType = String(quote.data_type || "").toLowerCase();
  if (quote.stale || quote.stale_reason) parts.push("stale");
  else if (dataType.includes("delayed")) parts.push("delayed");
  else if (dataType.includes("frozen")) parts.push("frozen");
  if (quote.price_as_of) parts.push(quote.price_as_of);
  else if (quote.price_at) parts.push(shortTimeWithZone(quote.price_at));
  return parts.join(" ");
}

function protectionStopChanged(snapshotStop, liveStop) {
  if (!hasNumericValue(snapshotStop) || !hasNumericValue(liveStop)) return false;
  return Math.abs(snapshotStop - liveStop) >= Math.max(0.01, Math.abs(snapshotStop) * 0.0025);
}

function proposalIsBuyToCover(proposal = {}) {
  const action = String(proposal.action || "").toUpperCase();
  const effect = String(proposal.position_effect || "").toLowerCase();
  return action === "BUY" &&
    Number(proposal.position_quantity || 0) < 0 &&
    (effect === "close" || effect === "reduce");
}

function protectionHeroMarketFlags(rows = [], marketEvents = {}) {
  const counts = new Map();
  for (const proposal of rows) {
    for (const flag of proposal.market_flags || []) {
      if (!marketEventFlagVisible(flag)) continue;
      const key = flag.id;
      const existing = counts.get(key) || { flag, count: 0 };
      existing.count += 1;
      counts.set(key, existing);
    }
  }
  const items = [...counts.values()].map(({ flag, count }) => ({
    flag,
    options: { label: `${flag.label || marketEventIDLabel(flag.id)} ${count}` },
  }));
  if (items.length > 0) return items;
  return marketEventHealthItems(marketEvents);
}

function protectionContractLabel(contract = {}) {
  if (String(contract.sec_type || "").toUpperCase() !== "OPT") {
    const currency = String(contract.currency || "").trim().toUpperCase();
    const market = proposalMarketLabel(proposalMarketKey({ contract }));
    const primary = String(contract.primary_exchange || contract.primary_exch || contract.exchange || "").trim().toUpperCase();
    if (currency || primary) return [currency, market === "US market" ? "" : market, primary && primary !== "SMART" ? primary : ""].filter(Boolean).join(" ");
    return "";
  }
  const right = String(contract.right || "").trim().toUpperCase();
  const strike = typeof contract.strike === "number" && contract.strike > 0 ? formatStrike(contract.strike) : "";
  const expiry = formatExpiry(contract.expiry || "");
  const optionSide = strike && right ? `${strike}${right}` : right || strike;
  const currency = String(contract.currency || "").trim().toUpperCase();
  return [expiry, optionSide, currency].filter(Boolean).join(" ");
}

function protectionPreviewGate(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  const blocker = protectionEffectiveBlockers(proposal, state.snapshot?.market_events || {})[0];
  if (blocker) return { ready: false, reason: blockerText(blocker) };
  if (!trading.can_preview) return { ready: false, reason: "Broker preview is not enabled by trading.status" };
  return { ready: true, reason: "Preview this protection proposal with broker WhatIf; no order is placed" };
}

function protectionSubmitGate(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  const blocker = protectionEffectiveBlockers(proposal, state.snapshot?.market_events || {})[0];
  if (blocker) return { ready: false, reason: blockerText(blocker) };
  if (!trading.can_write) return { ready: false, reason: protectionWriteUnavailableReason(trading) };
  const calendar = protectionMarketCalendar(proposal);
  const session = calendar?.session;
  if (!session) {
    return { ready: true, reason: protectionMarketStateHint(proposal) };
  }
  if (session.is_open) {
    return { ready: true, reason: protectionMarketStateHint(proposal) };
  }
  return { ready: true, reason: protectionMarketStateHint(proposal) };
}

function protectionPreviewSubmitGate(proposal = {}, previewResult = null) {
  if (!previewResult) return { ready: false, reason: "Run preview first" };
  if (previewResult.pending) return { ready: false, reason: "Broker WhatIf preview is still running" };
  const blocker = (previewResult.blockers || [])[0];
  if (blocker) return { ready: false, reason: blockerText(blocker) };
  if (!protectionPreviewSubmitEligible(previewResult)) {
    return { ready: false, reason: protectionPreviewSubmitBlockedReason(previewResult) };
  }
  if (protectionPreviewStale(previewResult, proposal)) {
    return { ready: false, reason: "Live suggestion changed; preview again before submitting" };
  }
  const writeGate = protectionSubmitGate(proposal);
  if (!writeGate.ready) return writeGate;
  return { ready: true, reason: "Submit the stop after confirmation; the daemon runs a fresh broker WhatIf before placing it" };
}

function protectionPreviewStateKey(proposal = {}) {
  return `${proposal.key || ""}@${proposal.revision || ""}`;
}

function protectionPreviewText(result = null, proposal = {}) {
  if (!result) return "";
  if (result.local && result.pending) {
    const draft = proposal.bucket === "trailing_stop" ? protectionStopDraftSummary(proposal) : protectionProposalTitle(proposal);
    return `Order draft ready; broker WhatIf running · ${draft}`;
  }
  if (result.pending) return "Previewing broker WhatIf; no order is placed";
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Preview blocked; no order placed · ${blockerText(blocker)}`;
  const preview = result.preview || {};
  const whatIfStatus = String(preview.what_if?.status || "").trim();
  const submitEligible = result.submit_eligible || preview.submit_eligible;
  const whatIfAccepted = whatIfStatus.toLowerCase() === "accepted";
  const parts = [
    protectionPreviewOutcomeLabel({ submitEligible, whatIfAccepted, whatIfStatus, accepted: result.accepted }),
    submitEligible ? "submit eligible" : "not submit eligible",
  ];
  const tokenID = result.preview_token_id || preview.preview_token_id || "";
  if (tokenID) parts.push(`token ${shortPreviewTokenID(tokenID)}`);
  const expiresAt = result.preview_token_expires_at || preview.preview_token_expires_at || "";
  if (expiresAt) parts.push(`expires ${shortTimeWithZone(expiresAt)}`);
  const whatIfDetails = protectionWhatIfDetails(preview.what_if || {});
  if (whatIfDetails) parts.push(whatIfDetails);
  if (!submitEligible && whatIfStatus && whatIfAccepted) parts.push("WhatIf accepted");
  if (!submitEligible && preview.what_if?.message) parts.push(shortPreviewMessage(preview.what_if.message));
  if (protectionPreviewStale(result, proposal)) parts.push("live suggestion changed");
  return parts.filter(Boolean).join(" · ");
}

function protectionPreviewOutcomeLabel({ submitEligible = false, whatIfAccepted = false, whatIfStatus = "", accepted = false } = {}) {
  if (submitEligible) return "Broker WhatIf accepted; no order placed";
  if (whatIfStatus) return `Broker WhatIf ${labelize(whatIfStatus)}; no order placed`;
  if (accepted) return "Draft previewed; no order placed";
  return "Preview returned; no order placed";
}

function protectionPreviewSubmitEligible(result = {}) {
  return Boolean(result.submit_eligible || result.preview?.submit_eligible);
}

function protectionPreviewSubmitBlockedReason(result = {}) {
  const preview = result.preview || {};
  const whatIf = preview.what_if || {};
  if (whatIf.message) return shortPreviewMessage(whatIf.message);
  if (whatIf.status) return `Broker WhatIf ${labelize(whatIf.status)}`;
  if (preview.token_minted === false) return "Broker preview did not mint a submit token";
  return "Broker WhatIf is not submit eligible";
}

function protectionWhatIfDetails(whatIf = {}) {
  const margin = whatIf.margin || {};
  const currency = margin.currency || margin.commission_currency || "";
  const parts = [];
  if (hasNumericValue(margin.commission)) {
    parts.push(`commission ${compactMoney(margin.commission, margin.commission_currency || currency)}`);
  }
  if (hasNumericValue(margin.initial_margin_after)) {
    parts.push(`init margin ${compactMoney(margin.initial_margin_after, currency)}`);
  }
  if (margin.warning_text) parts.push(shortPreviewMessage(margin.warning_text));
  return parts.join(" · ");
}

function protectionSubmitStateText({ result = null, gate = {}, busy = false, previewResult = null } = {}) {
  if (busy) return "Submitting order; fresh broker WhatIf running";
  if (result) return protectionSubmitResultText(result);
  if (!previewResult) return "";
  if (previewResult.pending) return "";
  if (!gate.ready) return `Submit blocked · ${gate.reason}`;
  if (!protectionPreviewSubmitEligible(previewResult)) return `Submit unavailable · ${protectionPreviewSubmitBlockedReason(previewResult)}`;
  return `Ready; Submit order sends the broker write to ${protectionWriteConfirmationLabel()}`;
}

function protectionSubmitStateClass({ result = null, gate = {}, busy = false } = {}) {
  const classes = ["protection-row__submit-state"];
  if (busy) {
    classes.push("protection-row__submit-state--pending");
  } else if (result?.accepted || result?.place?.accepted) {
    classes.push("protection-row__submit-state--ready");
  } else if (result?.blockers?.length || (gate && gate.ready === false)) {
    classes.push("protection-row__submit-state--blocked");
  } else {
    classes.push("protection-row__submit-state--ready");
  }
  return classes.join(" ");
}

function protectionSubmitResultText(result = {}) {
  if (result.local && result.pending) return "Submitting order; fresh broker WhatIf running";
  const blocker = (result.blockers || [])[0];
  if (blocker) return `Submit blocked · ${blockerText(blocker)}`;
  const orderRef = result.order_ref || result.place?.order_ref || "";
  const placeStatus = result.place?.lifecycle_status || result.place?.status || result.place?.send_state || "";
  if (result.accepted || result.place?.accepted) {
    return ["Submitted to broker", orderRef ? `order ${orderRef}` : "", placeStatus].filter(Boolean).join(" · ");
  }
  const message = result.message || result.place?.message || "";
  return message ? `Submit returned · ${message}` : "Submit returned without an accepted broker order";
}

function protectionSubmitButtonTitle({ blocked = false, previewBusy = false, submitBusy = false, gate = {} } = {}) {
  if (blocked) return "Proposal is blocked";
  if (previewBusy) return "Broker WhatIf preview is still running";
  if (submitBusy) return "Submitting stop order";
  if (!gate.ready) return gate.reason || "Submit unavailable";
  return gate.reason || `Submit the previewed stop to ${protectionWriteConfirmationLabel()}; the daemon runs a fresh broker WhatIf first`;
}

function protectionPreviewStale(result = {}, proposal = {}) {
  const boundTrail = result.preview?.draft?.trail || result.proposal?.trail || null;
  const liveTrail = proposal.trail || null;
  if (!boundTrail || !liveTrail) return false;
  const live = protectionLiveTrailStop(proposal, liveTrail);
  if (!live) return false;
  return protectionStopChanged(boundTrail.initial_stop_price, live.stop);
}

function protectionStopDraftSummary(proposal = {}) {
  const parts = [
    protectionProposalTitle(proposal),
    protectionMetricText(proposal),
  ].filter(Boolean);
  return parts.join(" · ");
}

function protectionBlockerText(proposal = {}) {
  const blockers = proposal.blockers || [];
  if (blockers.length === 0) return "Proposal is blocked";
  return blockers.map(blockerText).join("; ");
}


// Risk-over-target prefers the daemon's base-currency aggregate so the
// panel's money metrics share one currency; the per-contract-currency
// aggregate is the compat fallback for older snapshots. An absent or MIX
// currency is unrenderable — never coerce to USD.
function protectionRiskExcessSummary(counts = {}) {
  const baseCurrency = normalizeCurrency(counts.base_currency || "");
  if (hasNumericValue(counts.risk_reduction_excess_notional_base) && baseCurrency) {
    const value = counts.risk_reduction_excess_notional_base;
    return {
      text: compactWholeMoney(value, baseCurrency),
      title: value > 0
        ? "Risk-reduction proposal exposure above policy target, in the account base currency."
        : "No risk-reduction proposal exposure above target.",
      risk: value > 0,
    };
  }
  const riskExcessCurrency = protectionRiskExcessCurrency(counts);
  if (typeof counts.risk_reduction_excess_notional === "number" && riskExcessCurrency) {
    const value = counts.risk_reduction_excess_notional;
    return {
      text: compactWholeMoney(counts.risk_reduction_excess_notional, riskExcessCurrency),
      title: value > 0 ? "Risk-reduction proposal exposure above policy target." : "No risk-reduction proposal exposure above target.",
      risk: value > 0,
    };
  }
  if (Number(counts.risk_reduction || 0) > 0) {
    return {
      text: "Review",
      title: "Risk-reduction proposals exist but their excess is not summable to one currency (mixed currencies or FX conversion unavailable).",
      risk: true,
    };
  }
  return { text: "--", title: "No risk-reduction proposal exposure above target.", risk: false };
}

function protectionRiskExcessCurrency(counts = {}) {
  const currency = String(counts.risk_reduction_excess_currency || "").trim().toUpperCase();
  // "MIX" marked a raw sum across currencies in pre-2026-06-12 persisted
  // snapshots — not a number in any currency. An absent currency on a
  // present notional is equally unrenderable; never coerce either to USD.
  if (currency === "MIX") return "";
  return currency;
}

function formatStrike(value) {
  if (typeof value !== "number") return "";
  return Number.isInteger(value) ? String(value) : value.toFixed(2).replace(/\.?0+$/, "");
}

function formatExpiry(value) {
  const raw = String(value || "").trim();
  if (/^\d{8}$/.test(raw)) {
    return `${raw.slice(0, 4)}-${raw.slice(4, 6)}-${raw.slice(6, 8)}`;
  }
  return raw;
}

async function submitProtectionProposal(proposal) {
  const previewKey = protectionPreviewStateKey(proposal);
  const previewResult = state.protectionPreviews[previewKey] || null;
  const gate = protectionUsesPreviewFlow(proposal) ? protectionPreviewSubmitGate(proposal, previewResult) : protectionSubmitGate(proposal);
  if (!gate.ready) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: { blockers: [{ code: "submit_gate_blocked", message: gate.reason }], as_of: new Date().toISOString() },
    };
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    return;
  }
  const confirmation = protectionWriteConfirmation(proposal);
  if (!confirmation) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: { blockers: [{ code: "confirmation_cancelled", message: "broker submit confirmation was cancelled" }], as_of: new Date().toISOString() },
    };
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    return;
  }
  // The trader-adjusted quantity rides into the submit body and the
  // pending record so every surface (title, busy badge, confirmation)
  // shows the size actually being sent. The daemon re-clamps to
  // [1, max_quantity] regardless.
  const submitQuantity = protectionEffectiveQuantity(proposal);
  state.protectionSubmitBusy = previewKey;
  state.protectionSubmits = {
    ...state.protectionSubmits,
    [previewKey]: { local: true, pending: true, proposal: { ...proposal, quantity: submitQuantity }, as_of: new Date().toISOString() },
  };
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/proposals/submit", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        key: proposal.key,
        revision: proposal.revision,
        quantity: submitQuantity,
        fast_path: true,
        timeout_ms: protectionPreviewTimeoutMs(proposal),
        confirm_account: confirmation.account,
        confirm_mode: confirmation.mode,
      }),
    });
    const body = await readJSONOrText(res);
    if (!res.ok) throw new Error(body.error || body.message || String(body));
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: body,
    };
    await refreshOpenOrders();
  } catch (err) {
    state.protectionSubmits = {
      ...state.protectionSubmits,
      [previewKey]: {
        blockers: [{ code: "submit_failed", message: err.message }],
        as_of: new Date().toISOString(),
      },
    };
  } finally {
    if (state.protectionSubmitBusy === previewKey) state.protectionSubmitBusy = "";
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  }
}

async function previewProtectionProposal(proposal) {
  const previewKey = protectionPreviewStateKey(proposal);
  const previewQuantity = protectionEffectiveQuantity(proposal);
  state.protectionPreviewBusy = previewKey;
  state.protectionPreviews = {
    ...state.protectionPreviews,
    [previewKey]: {
      local: true,
      pending: true,
      proposal: { ...proposal, quantity: previewQuantity },
      as_of: new Date().toISOString(),
    },
  };
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  try {
    const res = await fetch("/api/proposals/preview", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      credentials: "include",
      body: JSON.stringify({
        key: proposal.key,
        revision: proposal.revision,
        quantity: previewQuantity,
        timeout_ms: protectionPreviewTimeoutMs(proposal),
        fast_path: proposal.bucket === "trailing_stop",
      }),
    });
    if (!res.ok) throw new Error(await res.text());
    const result = await res.json();
    state.protectionPreviews = {
      ...state.protectionPreviews,
      [previewKey]: result,
    };
  } catch (err) {
    state.protectionPreviews = {
      ...state.protectionPreviews,
      [previewKey]: {
        blockers: [{ code: "preview_failed", message: err.message }],
        as_of: new Date().toISOString(),
      },
    };
  } finally {
    if (state.protectionPreviewBusy === previewKey) state.protectionPreviewBusy = "";
    renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
  }
}

function protectionPreviewTimeoutMs(proposal = {}) {
  return proposal.bucket === "trailing_stop" ? 5000 : 10000;
}

async function ignoreProtectionProposal(proposal) {
  const res = await fetch("/api/proposals/ignore", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    credentials: "include",
    body: JSON.stringify({ key: proposal.key, revision: proposal.revision }),
  });
  if (!res.ok) throw new Error(await res.text());
  await refreshProtectionProposals();
}

async function refreshProtectionProposals() {
  const res = await fetch("/api/proposals/refresh", { method: "POST", credentials: "include" });
  if (res.ok) {
    const proposals = await res.json();
    applyProtectionSnapshot(proposals);
    renderAll();
  }
}

function protectionMarketCalendar(proposal = {}) {
  const market = proposalMarketKey(proposal);
  const current = currentMarketCalendar(state.snapshot || {});
  if (marketCalendarMatches(current, market)) return current;
  if (Object.hasOwn(state.proposalMarketCalendars, market)) return state.proposalMarketCalendars[market];
  queueProposalMarketCalendarSync(market);
  return null;
}

function proposalMarketKey(proposal = {}) {
  const contract = proposal.contract || {};
  const explicit = String(contract.market || "").trim().toLowerCase();
  if (explicit) return explicit;
  const secType = String(contract.sec_type || proposal.sec_type || "").trim().toUpperCase();
  if (secType === "OPT") return "us-options";
  const primary = String(contract.primary_exchange || contract.primary_exch || contract.exchange || "").trim().toUpperCase();
  if (primary === "IBIS" || primary === "XETRA") return "de";
  return "us";
}

function proposalMarketLabel(market = "") {
  switch (String(market || "").toLowerCase()) {
    case "de":
      return "Xetra";
    case "us-options":
      return "US options";
    default:
      return "US market";
  }
}

function marketCalendarMatches(calendar, market = "") {
  if (!calendar) return false;
  const got = String(calendar.market || calendar.session?.market || "").toLowerCase();
  const want = String(market || "us").toLowerCase();
  const aliases = {
    us: ["us", "us_equity", "us-equity"],
    "us-options": ["us-options", "us_options", "us_option", "us-options"],
    de: ["de", "xetra", "de_xetra", "de-xetra"],
  };
  return (aliases[want] || [want]).includes(got);
}

function queueProposalMarketCalendarSync(market = "") {
  const key = String(market || "us").toLowerCase();
  if (Object.hasOwn(state.proposalMarketCalendars, key) || state.proposalMarketCalendarBusy[key]) return;
  state.proposalMarketCalendarBusy = { ...state.proposalMarketCalendarBusy, [key]: true };
  fetch(`/api/market-calendar?market=${encodeURIComponent(key)}`, { credentials: "include" })
    .then((res) => {
      if (!res.ok) throw new Error("market calendar unavailable");
      return res.json();
    })
    .then((calendar) => {
      state.proposalMarketCalendars = { ...state.proposalMarketCalendars, [key]: calendar };
    })
    .catch(() => {
      state.proposalMarketCalendars = { ...state.proposalMarketCalendars, [key]: null };
    })
    .finally(() => {
      const busy = { ...state.proposalMarketCalendarBusy };
      delete busy[key];
      state.proposalMarketCalendarBusy = busy;
      renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {}, state.snapshot?.market_events || {});
    });
}

export { DERISK_PREVIEW_VALID_MS, cancelProtectionDerisk, deriskBasketLine, deriskLegRow, deriskPreviewExpired, deriskPreviewRemainingMs, deriskRequestRef, deriskValidityTicker, formatExpiry, formatStrike, goDurationMinutes, ignoreProtectionProposal, marketCalendarMatches, nudgeProtectionQuantity, previewProtectionDerisk, previewProtectionProposal, proposalIsBuyToCover, proposalMarketKey, proposalMarketLabel, protectionActionLabel, protectionActionTitle, protectionBlockerText, protectionBucketLabel, protectionButtonTitle, protectionContractLabel, protectionDecisionFlags, protectionDeriskStateText, protectionEffectiveQuantity, protectionExecutionTriggerLabel, protectionExecutionWarningLabel, protectionFinalSubmitLabel, protectionHeroMarketFlags, protectionInferredReference, protectionLiveTrailStop, protectionLossCurrency, protectionMarketCalendar, protectionMarketStateHint, protectionMetricText, protectionNeedsSnapshotSync, protectionOptionLeg, protectionPositionLine, protectionPositionUnitLabel, protectionPreviewGate, protectionPreviewOutcomeLabel, protectionPreviewStale, protectionPreviewStateKey, protectionPreviewSubmitBlockedReason, protectionPreviewSubmitEligible, protectionPreviewSubmitGate, protectionPreviewText, protectionPreviewTimeoutMs, protectionProposalDTE, protectionProposalTitle, protectionQuantityAcceleratedStep, protectionQuantityStepDelta, protectionQuantityStepper, protectionQuoteFor, protectionQuoteFrozen, protectionQuoteLine, protectionQuoteStatusLabel, protectionQuoteTickDir, protectionReason, protectionReasonText, protectionReferenceLabel, protectionRiskExcessCurrency, protectionRiskExcessSummary, protectionRiskTicket, protectionRiskTicketParts, protectionRiskTicketTitle, protectionRow, protectionSideLabel, protectionSnapshotRefreshReason, protectionStopChanged, protectionStopDraftSummary, protectionStopLadder, protectionStopLadderDisplaySteps, protectionStopLadderLabel, protectionStopLadderShortLabel, protectionStopLadderStepClass, protectionStopLadderStepDetail, protectionStopLadderStepTitle, protectionStopRiskGapLabel, protectionStopRiskGapName, protectionStopRiskLossLabel, protectionSubmitButtonTitle, protectionSubmitGate, protectionSubmitLabel, protectionSubmitResultText, protectionSubmitStateClass, protectionSubmitStateText, protectionThetaSummary, protectionTrailOffsetLabel, protectionTrailSizingFallback, protectionTrailSizingLabel, protectionTrailSizingRangeLabel, protectionTrailSizingSourceLabel, protectionTransientSnapshotBlocker, protectionUsesPreviewFlow, protectionWhatIfDetails, queueProposalMarketCalendarSync, queueProtectionSnapshotSync, reduceEligibleHoldings, reduceIsOption, refreshProtectionProposals, renderProtectionDerisk, renderProtectionDeriskBasket, renderProtectionExposure, renderProtectionPanel, renderProtectionTimestamp, setProtectionQuantity, submitProtectionDerisk, submitProtectionProposal, syncDeriskValidityTicker, syncProtectionSnapshot };
