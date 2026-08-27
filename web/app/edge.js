import { $, calendarDate, calendarDateTime, hasNumericValue, labelize, money, privacyMask, readJSONOrText, signedMoneyRead } from "./shared.js";
import { state } from "./state.js";

const EDGE_WINDOWS = new Set(["90d", "365d"]);
const EDGE_HORIZONS = new Set([1, 5, 20]);
const EDGE_STATES = new Set(["action_required", "backfilling", "current", "degraded", "insufficient_evidence", "unavailable"]);
const EDGE_ACTIONS = ["open", "add", "trim", "exit"];
const EDGE_OPTION_GROUPINGS = new Set(["exact_order", "unlinked_execution", "option_event"]);
const EDGE_OPTION_LIFECYCLES = new Set(["opening", "closing", "mixed", "event", "unknown"]);
const EDGE_OPTION_EVENTS = new Set(["exercise", "assignment", "expiration", "other"]);
const EDGE_OPTION_PNL_STATES = new Set(["complete", "partial", "unavailable"]);
const EDGE_OPTION_MISSING = new Set(["realized_pnl", "open_pnl", "fx_conversion", "instrument_metadata"]);
const EDGE_MARKET_LABELS = new Map([
  ["spy", "S&P 500 proxy (SPY)"], ["qqq", "Nasdaq-100 proxy (QQQ)"], ["dia", "Dow proxy (DIA)"], ["vix", "CBOE VIX"],
]);

async function refreshEdge(changeID = "", optionID = "") {
  if (!state.authenticated) return false;
  const requestedChange = String(changeID || "").trim();
  const requestedOption = String(optionID || "").trim();
  if (requestedChange && (requestedChange.length > 128 || !requestedChange.startsWith("change_"))) {
    state.edgeError = "Canary Edge received an invalid finding reference";
    renderEdge();
    return false;
  }
  if (requestedOption && (requestedOption.length > 128 || !requestedOption.startsWith("option_"))) {
    state.edgeError = "Canary Edge received an invalid option reference";
    renderEdge();
    return false;
  }
  if (requestedChange && requestedOption) {
    state.edgeError = "Canary Edge can explain one result at a time";
    renderEdge();
    return false;
  }
  const requestID = (state.edgeRequestID || 0) + 1;
  state.edgeRequestID = requestID;
  state.edgeBusy = true;
  state.edgeError = "";
  renderEdge();
  try {
    const query = requestedChange ? { change: requestedChange } : requestedOption ? { option: requestedOption } : null;
    const path = query ? `/api/edge?${new URLSearchParams(query)}` : "/api/edge";
    const response = await fetch(path, { credentials: "include" });
    const body = await readJSONOrText(response);
    if (!response.ok) throw new Error(typeof body === "string" ? body : body.error || "Canary Edge unavailable");
    if (!validEdgeResult(body)) throw new Error("Canary Edge returned an invalid typed result");
    if (requestedChange && body.change?.id !== requestedChange) throw new Error("Canary Edge returned the wrong finding explanation");
    if (requestedOption && body.option?.id !== requestedOption) throw new Error("Canary Edge returned the wrong option explanation");
    if (state.edgeRequestID !== requestID) return false;
    state.edgeResult = body;
    return true;
  } catch (error) {
    if (state.edgeRequestID !== requestID) return false;
    state.edgeError = String(error?.message || "Canary Edge unavailable");
    return false;
  } finally {
    if (state.edgeRequestID === requestID) {
      state.edgeBusy = false;
      renderEdge();
    }
  }
}

function validEdgeResult(result) {
  if (!result || typeof result !== "object") return false;
  if (result.schema_version !== "canary-edge-v3") return false;
  if (!EDGE_STATES.has(result.state) || !EDGE_WINDOWS.has(result.window)) return false;
  if (!EDGE_HORIZONS.has(Number(result.horizon_sessions)) || result.not_execution !== true) return false;
  const selection = result.horizon_selection;
  if (!selection || !["automatic", "explicit"].includes(selection.mode) || Boolean(result.automatic_horizon) !== (selection.mode === "automatic")) return false;
  if (![selection.eligible_changes, selection.scored_changes, selection.coverage_pct, selection.largest_action_sample].every(hasNumericValue)) return false;
  if (!Array.isArray(result.action_rollups) || !Array.isArray(result.findings) || result.findings.length > 3) return false;
  if (!Array.isArray(result.market_context) || !result.market_context.every(validEdgeMarketContextRollup) || !Array.isArray(result.market_context_missing)) return false;
  const presentContext = new Set(result.market_context.map((row) => row.key));
  const missingContext = new Set(result.market_context_missing);
  if (missingContext.size !== result.market_context_missing.length || result.market_context_missing.some((key) => !EDGE_MARKET_LABELS.has(key) || presentContext.has(key))) return false;
  if (!validEdgeOptionReview(result.options) || !result.coverage || typeof result.coverage !== "object" || !result.method) return false;
  if (result.fingerprint && (result.method.metric !== "Decision price impact" || !String(result.method.headline_selection || "").trim() || !String(result.method.finding_ranking || "").trim() || !String(result.method.materiality_gate || "").trim() || !String(result.method.automatic_horizon || "").trim() || !String(result.method.market_context || "").trim() || result.method.no_causal_claim !== true || result.method.no_predictive_claim !== true || result.method.not_investment_advice !== true)) return false;
  if (result.change != null && !validEdgeChange(result.change)) return false;
  if (result.option != null && !validEdgeOptionDetail(result.option)) return false;
  if (result.change != null && result.option != null) return false;
  return result.findings.every((finding) => String(finding?.change_id || "").startsWith("change_") && hasNumericValue(finding?.decision_notional_base) && Number(finding.decision_notional_base) > 0 && hasNumericValue(finding?.decision_impact_base) && hasNumericValue(finding?.decision_impact_pct) && Array.isArray(finding.market_context || []) && (finding.market_context || []).every(validEdgeMarketContext));
}

function validEdgeOptionReview(review) {
  if (!review || typeof review !== "object" || !review.coverage || !review.realized || !review.open) return false;
  const coverage = review.coverage;
  const coverageCounts = [coverage.execution_episodes, coverage.opening_episodes, coverage.opening_only_zero_episodes, coverage.closing_episodes, coverage.mixed_episodes, coverage.unknown_episodes, coverage.event_episodes];
  if (!coverageCounts.every(validEdgeCount)) return false;
  if (coverage.execution_episodes !== coverage.opening_episodes + coverage.closing_episodes + coverage.mixed_episodes + coverage.unknown_episodes || coverage.opening_only_zero_episodes > coverage.opening_episodes) return false;

  const realized = review.realized;
  const realizedCounts = [realized.positive_count, realized.negative_count, realized.flat_count, realized.complete_count, realized.partial_count, realized.unavailable_count, realized.total_count];
  if (!realizedCounts.every(validEdgeCount) || !Array.isArray(realized.episodes) || realized.episodes.length > 20 || realized.total_count < realized.episodes.length) return false;
  if (realized.total_count !== realized.complete_count + realized.partial_count + realized.unavailable_count || realized.positive_count + realized.negative_count + realized.flat_count !== realized.complete_count + realized.partial_count) return false;
  if (Boolean(realized.truncated) !== (realized.total_count > realized.episodes.length)) return false;
  if ((realized.complete_count + realized.partial_count > 0) !== hasNumericValue(realized.known_pnl_base)) return false;
  if (new Set(realized.episodes.map((episode) => episode?.id)).size !== realized.episodes.length || !realized.episodes.every(validEdgeOptionEpisode)) return false;

  const open = review.open;
  const openCounts = [open.positive_count, open.negative_count, open.flat_count, open.complete_count, open.unavailable_count, open.total_count];
  if (!openCounts.every(validEdgeCount) || !Array.isArray(open.positions) || open.positions.length > 20 || open.total_count < open.positions.length) return false;
  if (open.total_count !== open.complete_count + open.unavailable_count || open.positive_count + open.negative_count + open.flat_count !== open.complete_count) return false;
  if (Boolean(open.truncated) !== (open.total_count > open.positions.length)) return false;
  if ((open.complete_count > 0) !== hasNumericValue(open.known_pnl_base)) return false;
  if ((open.total_count > 0) !== Boolean(String(open.snapshot_date || "").trim())) return false;
  return new Set(open.positions.map((position) => position?.id)).size === open.positions.length && open.positions.every(validEdgeOptionOpenPosition);
}

function validEdgeOptionEpisode(episode) {
  if (!episode || !String(episode.id || "").startsWith("option_") || !EDGE_OPTION_GROUPINGS.has(episode.grouping) || !EDGE_OPTION_LIFECYCLES.has(episode.lifecycle)) return false;
  if (!String(episode.activity_from || "").trim() || !String(episode.activity_to || "").trim() || !Array.isArray(episode.legs) || episode.legs.length === 0) return false;
  if (episode.grouping === "option_event") {
    if (episode.lifecycle !== "event" || !EDGE_OPTION_EVENTS.has(episode.event_type)) return false;
  } else if (episode.lifecycle === "event" || episode.event_type) return false;
  if (!validEdgeOptionPNL(episode.pnl_status, episode.realized_pnl_base, episode.missing_evidence)) return false;
  return episode.legs.every((leg) => String(leg?.symbol || "").trim() && validEdgeOptionIdentity(leg, episode.missing_evidence));
}

function validEdgeOptionOpenPosition(position) {
  return Boolean(position && String(position.id || "").startsWith("option_") && String(position.symbol || "").trim() && String(position.snapshot_date || "").trim()
    && validEdgeOptionIdentity(position, position.missing_evidence) && validEdgeOptionPNL(position.pnl_status, position.open_pnl_base, position.missing_evidence));
}

function validEdgeOptionDetail(detail) {
  if (!detail || !String(detail.id || "").startsWith("option_")) return false;
  if (detail.kind === "realized_episode") {
    const episode = detail.episode;
    if (!episode || detail.open_position != null || episode.id !== detail.id || !Array.isArray(episode.legs)) return false;
    if (!validEdgeOptionEpisode({ ...episode, legs: episode.legs.map((leg) => ({ symbol: leg?.symbol, underlying: leg?.underlying, expiry: leg?.expiry, strike: leg?.strike, put_call: leg?.put_call })) })) return false;
    return episode.legs.every((leg) => String(leg?.id || "").startsWith("option-leg_")
      && ["buy", "sell", "unknown"].includes(leg.side) && ["opening", "closing", "unknown"].includes(leg.open_close)
      && [leg.strike, leg.multiplier, leg.quantity, leg.execution_price, leg.realized_pnl_base, leg.direct_costs_base].every((value) => value == null || hasNumericValue(value))
      && validEdgeOptionMissing(leg.missing_evidence));
  }
  if (detail.kind === "open_position") {
    const position = detail.open_position;
    return Boolean(position && detail.episode == null && position.id === detail.id && validEdgeOptionOpenPosition(position)
      && ["long", "short", "unknown"].includes(position.side)
      && [position.strike, position.multiplier, position.quantity, position.mark_price, position.cost_basis_money, position.open_pnl_base].every((value) => value == null || hasNumericValue(value)));
  }
  return false;
}

function validEdgeOptionIdentity(row, missing) {
  const missingInstrument = Array.isArray(missing) && missing.includes("instrument_metadata");
  const expiry = String(row?.expiry || "");
  const putCall = String(row?.put_call || "");
  if (expiry && !/^\d{4}-\d{2}-\d{2}$/.test(expiry)) return false;
  if (row?.strike != null && !hasNumericValue(row.strike)) return false;
  if (putCall && !["call", "put"].includes(putCall)) return false;
  return missingInstrument || Boolean(expiry && row?.strike != null && putCall);
}

function validEdgeOptionPNL(status, amount, missing) {
  if (!EDGE_OPTION_PNL_STATES.has(status) || !validEdgeOptionMissing(missing)) return false;
  if (status === "complete") return hasNumericValue(amount);
  if (status === "partial") return hasNumericValue(amount) && (missing || []).length > 0;
  return amount == null && (missing || []).length > 0;
}

function validEdgeOptionMissing(missing) {
  return missing == null || (Array.isArray(missing) && new Set(missing).size === missing.length && missing.every((reason) => EDGE_OPTION_MISSING.has(reason)));
}

function validEdgeCount(value) {
  return Number.isInteger(value) && value >= 0;
}

function validEdgeMarketContextIdentity(row) {
  return row && ["spy", "qqq", "dia", "vix"].includes(row.key) && ["market_proxy", "volatility_index"].includes(row.kind) && String(row.label || "").trim();
}

function validEdgeMarketContextRollup(row) {
  return validEdgeMarketContextIdentity(row) && hasNumericValue(row.sample_count) && row.sample_count > 0 && hasNumericValue(row.median_change_pct)
    && (row.kind === "volatility_index" ? hasNumericValue(row.median_change_points) : row.median_change_points == null);
}

function validEdgeMarketContext(row) {
  return validEdgeMarketContextIdentity(row) && String(row.start_day || "").trim() && String(row.end_day || "").trim()
    && hasNumericValue(row.start_close) && row.start_close > 0 && hasNumericValue(row.end_close) && row.end_close > 0 && hasNumericValue(row.change_pct)
    && (row.kind === "volatility_index" ? hasNumericValue(row.change_points) : row.change_points == null);
}

function validEdgeChange(change) {
  if (!change || typeof change !== "object" || !String(change.id || "").startsWith("change_")) return false;
  if (!EDGE_ACTIONS.includes(change.action) || !["long", "short"].includes(change.direction) || !String(change.executed_at || "").trim()) return false;
  if (![change.delta_quantity, change.position_before, change.position_after].every(hasNumericValue)) return false;
  if (![change.execution_vwap, change.multiplier, change.direct_costs_base].every((value) => value == null || hasNumericValue(value))) return false;
  if (!Array.isArray(change.scores)) return false;
  return change.scores.every((score) => {
    if (!EDGE_HORIZONS.has(Number(score?.sessions))) return false;
    const values = [score?.horizon_close, score?.horizon_fx, score?.decision_notional_base, score?.decision_impact_base, score?.decision_impact_pct];
    if (!values.every((value) => value == null || hasNumericValue(value))) return false;
    const hasImpact = score.decision_impact_base != null;
    if (hasImpact && (score.reason || !hasNumericValue(score.decision_notional_base) || score.decision_notional_base <= 0 || !hasNumericValue(score.decision_impact_pct) || !Array.isArray(score.market_context || []) || !(score.market_context || []).every(validEdgeMarketContext))) return false;
    return !score.reason || !hasImpact;
  });
}

function renderEdge() {
  const result = state.edgeResult;
  renderEdgeStatus(result);
  if (!result) {
    $("edgeSetup").hidden = true;
    $("edgeResults").hidden = true;
    $("edgeAsOf").textContent = state.edgeBusy ? "loading" : "not loaded";
    return;
  }

  $("edgeAsOf").textContent = result.as_of ? calendarDateTime(result.as_of) : labelize(result.state);
  const setup = result.state === "action_required"
    || (result.state === "insufficient_evidence" && result.reason === "trade_history_unproved" && result.setup);
  $("edgeSetup").hidden = !setup;
  if (setup) renderEdgeSetup(result);
  const hasResults = result.state !== "action_required" && edgeHasResults(result);
  $("edgeResults").hidden = !hasResults;
  if (!hasResults) return;

  renderEdgeMatrix(result);
  renderEdgeFindings(result);
  renderEdgeChange(result);
  renderEdgeAccount(result);
  renderEdgeOptions(result);
  renderEdgeOptionDetail(result);
  renderEdgeMethod(result);
}

function renderEdgeStatus(result) {
  const target = $("edgeStatus");
  target.className = "edge-status";
  if (state.edgeBusy) {
    target.textContent = "Reviewing your published broker-confirmed history…";
    target.classList.add("edge-status--busy");
    return;
  }
  if (state.edgeError) {
    target.textContent = state.edgeError;
    target.classList.add("edge-status--risk");
    return;
  }
  if (!result || result.state === "current") {
    target.textContent = "";
    return;
  }
  const copy = {
    action_required: "Flex evidence setup is required.",
    backfilling: "Backfill is running. Available results remain explicitly partial.",
    degraded: "The prior snapshot is visible, but newer evidence is still rebuilding.",
    insufficient_evidence: result.reason === "trade_history_unproved"
      ? "The completed one-year report returned no Trades section. This is not a backfill: verify the saved Flex query if the account traded during the period."
      : "The broker evidence is insufficient for a sound decision review.",
    unavailable: "No sound Edge result is currently available.",
  }[result.state] || labelize(result.state);
  const terminalTradeGap = result.state === "insufficient_evidence" && result.reason === "trade_history_unproved";
  const reason = result.reason && !terminalTradeGap ? ` ${labelize(result.reason)}.` : "";
  target.textContent = copy + reason;
  target.classList.add(result.state === "degraded" || result.state === "insufficient_evidence" || result.state === "unavailable" ? "edge-status--risk" : "edge-status--watch");
}

function edgeHasResults(result) {
  const options = result.options || {};
  return Boolean(result.account || result.action_rollups?.length || result.findings?.length || options.realized?.total_count || options.open?.total_count || options.coverage?.execution_episodes || options.coverage?.event_episodes || result.change || result.option || result.fingerprint);
}

function renderEdgeSetup(result) {
  const setup = result.setup || {};
  const unprovedTrades = result.state === "insufficient_evidence" && result.reason === "trade_history_unproved";
  $("edgeSetupTitle").textContent = unprovedTrades ? "Trade history was not returned" : "Connect broker evidence";
  $("edgeSetupReason").textContent = unprovedTrades
    ? "Canary finished the one-year report and is not waiting for more history. If this account traded during the period, verify that Trades is selected at execution detail in the saved Activity Flex Query; if it did not, there are no decisions to score."
    : (result.reason
      ? `Canary cannot calculate broker-truth results: ${labelize(result.reason)}.`
      : "Canary needs the canonical IBKR Flex profile before it can calculate this review.");
  const steps = Array.isArray(setup.steps) ? setup.steps.slice(0, 3) : [];
  $("edgeSetupSteps").replaceChildren(...steps.map((text) => {
    const item = document.createElement("li");
    item.textContent = String(text || "");
    return item;
  }));
  const sections = Array.isArray(setup.sections) ? setup.sections.length : 0;
  $("edgeSetupManifest").textContent = [setup.manifest_version, sections ? `${sections} required sections` : ""].filter(Boolean).join(" · ");
  const missing = Array.isArray(setup.missing_requirements) ? setup.missing_requirements.map(String) : [];
  const missingNode = $("edgeSetupMissing");
  const shown = missing.slice(0, 8);
  missingNode.textContent = missing.length
    ? `Missing (${missing.length}): ${shown.join(", ")}${missing.length > shown.length ? ` (+${missing.length - shown.length} more in JSON)` : ""}`
    : "";
  missingNode.hidden = missing.length === 0;
}

function renderEdgeAccount(result) {
  const account = result.account;
  if (!account) {
    $("edgeAccountValue").textContent = "Unavailable";
    $("edgeAccountValue").className = "edge-account__value";
    $("edgeAccountPeriod").textContent = "No exact equity boundaries";
    $("edgeAccountDefinition").textContent = result.method?.account_definition || "Ending equity − starting equity − statement-confirmed external flows.";
    return;
  }
  const value = $("edgeAccountValue");
  value.textContent = edgeMoney(account.profit_loss_base, account.base_currency);
  value.className = `edge-account__value ${moneyTone(account.profit_loss_base)}`.trim();
  value.classList.toggle("is-private", !state.accountValueVisible);
  $("edgeAccountPeriod").textContent = `${calendarDate(account.actual_from)} → ${calendarDate(account.actual_to)}`;
  const requested = account.requested_from && account.actual_from && String(account.requested_from).slice(0, 10) !== String(account.actual_from).slice(0, 10)
    ? ` Requested from ${calendarDate(account.requested_from)}; first available boundary shown.`
    : "";
  $("edgeAccountDefinition").textContent = `${account.definition || result.method?.account_definition || "Ending equity − starting equity − statement-confirmed external flows."}${requested}`;
}

function renderEdgeMatrix(result) {
  const period = result.window === "365d" ? "One year" : "90 days";
  $("edgeImpactLens").textContent = `${period} · ${result.automatic_horizon ? "automatic · " : ""}${result.horizon_sessions}-session headline`;
  $("edgeHeadline").textContent = state.accountValueVisible
    ? (result.headline || "No highlighted finding has sufficient evidence.")
    : "Reveal account values to view the monetary headline.";
  $("edgeHeadline").classList.toggle("is-private", !state.accountValueVisible && Boolean(result.headline));
  const contextChips = (result.market_context || []).map((context) => {
    const chip = document.createElement("span");
    const move = context.kind === "volatility_index" && hasNumericValue(context.median_change_points)
      ? `${Number(context.median_change_points) > 0 ? "+" : ""}${Number(context.median_change_points).toFixed(2)} pts`
      : edgeMarketPercent(context.median_change_pct);
    chip.textContent = `${context.label} ${move} · n=${Number(context.sample_count || 0)}`;
    return chip;
  });
  for (const key of result.market_context_missing || []) {
    const chip = document.createElement("span");
    chip.textContent = `${EDGE_MARKET_LABELS.get(key) || String(key).toUpperCase()} unavailable`;
    contextChips.push(chip);
  }
  $("edgeMarketContext").replaceChildren(...contextChips);
  const rows = new Map((result.action_rollups || []).map((row) => [String(row.action || ""), row]));
  const header = document.createElement("div");
  header.className = "edge-matrix__row edge-matrix__row--head";
  header.setAttribute("role", "row");
  header.append(matrixCell("Action", "columnheader"), ...[1, 5, 20].map((sessions) => matrixCell(`${sessions} session${sessions === 1 ? "" : "s"}`, "columnheader", sessions === result.horizon_sessions)));
  const body = EDGE_ACTIONS.map((action) => {
    const row = document.createElement("div");
    row.className = "edge-matrix__row";
    row.setAttribute("role", "row");
    row.append(matrixCell(labelize(action), "rowheader"));
    const values = new Map((rows.get(action)?.horizons || []).map((value) => [Number(value.sessions), value]));
    for (const sessions of [1, 5, 20]) {
      const rollup = values.get(sessions);
      const cell = matrixCell("", "cell", sessions === result.horizon_sessions);
      const total = document.createElement("b");
      total.textContent = rollup?.total_base == null ? "—" : edgeMoney(rollup.total_base, edgeCurrency(result));
      total.className = moneyTone(rollup?.total_base);
      total.classList.toggle("is-private", !state.accountValueVisible && rollup?.total_base != null);
      const meta = document.createElement("small");
      meta.textContent = rollup?.median_base == null
        ? `n=${Number(rollup?.sample_count || 0)}`
        : `median ${edgeMoney(rollup.median_base, edgeCurrency(result))} · n=${Number(rollup.sample_count || 0)}`;
      meta.classList.toggle("is-private", !state.accountValueVisible && rollup?.median_base != null);
      cell.append(total, meta);
      row.append(cell);
    }
    return row;
  });
  $("edgeMatrix").replaceChildren(header, ...body);
}

function matrixCell(text, role, highlighted = false) {
  const cell = document.createElement("div");
  cell.className = "edge-matrix__cell";
  cell.classList.toggle("edge-matrix__cell--highlight", highlighted);
  cell.setAttribute("role", role);
  cell.textContent = text;
  return cell;
}

function renderEdgeFindings(result) {
  const findings = (result.findings || []).slice(0, 3);
  if (findings.length === 0) {
    $("edgeFindings").replaceChildren(emptyEdgeRow("No finding has complete, non-overlapping horizon evidence."));
    return;
  }
  $("edgeFindings").replaceChildren(...findings.map((finding) => {
    const row = document.createElement("button");
    row.type = "button";
    row.className = "edge-finding";
    const expanded = result.change?.id === finding.change_id;
    row.setAttribute("aria-expanded", String(expanded));
    row.setAttribute("aria-controls", "edgeChangePanel");
    row.setAttribute("aria-label", `${finding.symbol || "Finding"} ${labelize(finding.action)}: explain the broker-backed calculation`);
    row.addEventListener("click", () => {
      if (state.edgeResult?.change?.id === finding.change_id) {
        const next = { ...state.edgeResult };
        delete next.change;
        state.edgeResult = next;
        state.edgeError = "";
        renderEdge();
        return;
      }
      void refreshEdge(finding.change_id);
    });
    const identity = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = `${finding.symbol || "—"} · ${labelize(finding.action)}`;
    const meta = document.createElement("small");
    const impactPct = hasNumericValue(finding.decision_impact_pct) ? ` · ${Number(finding.decision_impact_pct).toFixed(2)}% of decision notional` : "";
    const market = (finding.market_context || []).map(edgeMarketContextText).join(" · ");
    meta.textContent = `${calendarDate(finding.executed_at)} · ${finding.horizon_sessions} sessions · ${labelize(finding.direction)}${impactPct}${market ? ` · ${market}` : ""}`;
    identity.append(title, meta);
    const amount = document.createElement("strong");
    amount.textContent = edgeMoney(finding.decision_impact_base, edgeCurrency(result));
    amount.className = moneyTone(finding.decision_impact_base);
    amount.classList.toggle("is-private", !state.accountValueVisible);
    row.append(identity, amount);
    return row;
  }));
}

function renderEdgeChange(result) {
  const panel = $("edgeChangePanel");
  const change = result.change;
  panel.hidden = !change;
  if (!change) {
    $("edgeChangeMeta").textContent = "";
    $("edgeChangeSummary").replaceChildren();
    $("edgeChangeScores").replaceChildren();
    return;
  }

  $("edgeChangeTitle").textContent = `${change.symbol || "Position"} · ${labelize(change.action)} decision`;
  $("edgeChangeMeta").textContent = [calendarDate(change.executed_at), labelize(change.asset_class), labelize(change.direction)].filter(Boolean).join(" · ");
  const priceCurrency = String(change.currency || "").toUpperCase();
  const baseCurrency = edgeCurrency(result) || priceCurrency;
  const facts = [
    ["Position", `${edgeQuantity(change.position_before)} → ${edgeQuantity(change.position_after)}`, false],
    ["Quantity changed", edgeSignedQuantity(change.delta_quantity), false],
    ["Execution VWAP", edgePrice(change.execution_vwap, priceCurrency), change.execution_vwap != null],
    ["Contract multiplier", change.multiplier == null ? "—" : edgeQuantity(change.multiplier), false],
    ["Direct costs", edgePrice(change.direct_costs_base, baseCurrency), change.direct_costs_base != null],
    ["Counterfactual", `Leave the pre-trade position at ${edgeQuantity(change.position_before)}`, false],
  ];
  $("edgeChangeSummary").replaceChildren(...facts.flatMap(([term, description, sensitive]) => {
    const dt = document.createElement("dt");
    dt.textContent = term;
    const dd = document.createElement("dd");
    dd.textContent = description;
    dd.classList.toggle("is-private", sensitive && !state.accountValueVisible);
    return [dt, dd];
  }));

  const scores = [...change.scores].sort((left, right) => Number(left.sessions) - Number(right.sessions));
  $("edgeChangeScores").replaceChildren(...scores.map((score) => {
    const row = document.createElement("div");
    row.className = "edge-change-score";
    const identity = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = `${Number(score.sessions)} session${Number(score.sessions) === 1 ? "" : "s"}`;
    const evidence = document.createElement("small");
    if (score.reason) {
      evidence.textContent = labelize(score.reason);
    } else {
      const parts = [];
      if (score.horizon_day) parts.push(calendarDate(score.horizon_day));
      if (score.horizon_close != null) parts.push(`close ${edgePrice(score.horizon_close, priceCurrency)}`);
      if (score.horizon_fx != null) parts.push(`FX ${edgeQuantity(score.horizon_fx)}`);
      evidence.textContent = parts.join(" · ") || "Broker horizon evidence";
      evidence.classList.toggle("is-private", !state.accountValueVisible && score.horizon_close != null);
    }
    if (!score.reason && score.market_context?.length) {
      evidence.textContent += ` · ${score.market_context.map(edgeMarketContextText).join(" · ")}`;
    }
    identity.append(title, evidence);
    const amount = document.createElement("strong");
    if (score.reason || score.decision_impact_base == null) {
      amount.textContent = "Not scored";
    } else {
      amount.textContent = `${edgeMoney(score.decision_impact_base, baseCurrency)} · ${edgeSignedPercent(score.decision_impact_pct)}`;
      amount.className = moneyTone(score.decision_impact_base);
      amount.classList.toggle("is-private", !state.accountValueVisible);
    }
    row.append(identity, amount);
    return row;
  }));
}

function renderEdgeOptions(result) {
  const review = result.options || {};
  const coverage = review.coverage || {};
  const realized = review.realized || { episodes: [] };
  const open = review.open || { positions: [] };
  const currency = edgeCurrency(result);
  $("edgeOptionsCount").textContent = `${Number(realized.total_count || 0)} realized · ${Number(open.total_count || 0)} open`;
  $("edgeOptionCoverage").textContent = edgeOptionCoverageText(coverage);
  $("edgeOptionRealizedSummary").textContent = edgeOptionRealizedSummary(realized, currency);
  $("edgeOptionOpenSummary").textContent = edgeOptionOpenSummary(open, currency);
  $("edgeOptionOpenAsOf").textContent = open.snapshot_date ? calendarDate(open.snapshot_date) : "No dated snapshot";

  const realizedRows = (realized.episodes || []).map((episode) => edgeOptionResultRow({
    id: episode.id,
    title: edgeOptionEpisodeTitle(episode),
    meta: [edgeOptionDateRange(episode.activity_from, episode.activity_to), labelize(episode.lifecycle), episode.event_type ? labelize(episode.event_type) : labelize(episode.grouping), edgeOptionEvidenceText(episode.pnl_status, episode.missing_evidence)].filter(Boolean).join(" · "),
    amount: episode.realized_pnl_base,
    amountLabel: "realized",
    currency,
  }));
  $("edgeOptionRealizedList").replaceChildren(...(realizedRows.length ? realizedRows : [emptyEdgeRow(realized.total_count
    ? "No numeric realized P/L row can be magnitude-ranked; incomplete episodes remain counted above."
    : "No broker-reported realized option episode is available for this window.")]));

  const openRows = (open.positions || []).map((position) => edgeOptionResultRow({
    id: position.id,
    title: edgeOptionContractLabel(position),
    meta: [calendarDate(position.snapshot_date), "Open position", edgeOptionEvidenceText(position.pnl_status, position.missing_evidence)].filter(Boolean).join(" · "),
    amount: position.open_pnl_base,
    amountLabel: "open",
    currency,
  }));
  $("edgeOptionOpenList").replaceChildren(...(openRows.length ? openRows : [emptyEdgeRow(open.total_count
    ? "No numeric open P/L row can be magnitude-ranked; unavailable positions remain counted above."
    : "No open option position is present in the latest dated Flex snapshot.")]));
}

function edgeOptionResultRow({ id, title, meta, amount, amountLabel, currency }) {
  const row = document.createElement("button");
  row.type = "button";
  row.className = "edge-option-row edge-explanation";
  const expanded = state.edgeResult?.option?.id === id;
  row.setAttribute("aria-expanded", String(expanded));
  row.setAttribute("aria-controls", "edgeOptionPanel");
  row.setAttribute("aria-label", `${title}: explain the broker option evidence`);
  row.addEventListener("click", () => {
    if (state.edgeResult?.option?.id === id) {
      const next = { ...state.edgeResult };
      delete next.option;
      state.edgeResult = next;
      state.edgeError = "";
      renderEdge();
      return;
    }
    void refreshEdge("", id);
  });
  const identity = document.createElement("div");
  const heading = document.createElement("b");
  heading.textContent = title;
  const evidence = document.createElement("small");
  evidence.textContent = meta;
  identity.append(heading, evidence);
  const value = document.createElement("strong");
  value.className = `edge-option-row__value ${moneyTone(amount)}`.trim();
  value.textContent = amount == null ? "P/L unavailable" : edgeMoney(amount, currency);
  value.classList.toggle("is-private", amount != null && !state.accountValueVisible);
  value.title = amount == null ? "Broker P/L was not reported or could not be converted" : `${amountLabel} broker P/L`;
  row.append(identity, value);
  return row;
}

function renderEdgeOptionDetail(result) {
  const panel = $("edgeOptionPanel");
  const detail = result.option;
  panel.hidden = !detail;
  if (!detail) {
    $("edgeOptionDetailMeta").textContent = "";
    $("edgeOptionDetailSummary").replaceChildren();
    $("edgeOptionDetailLegs").replaceChildren();
    return;
  }
  const currency = edgeCurrency(result);
  if (detail.episode) {
    const episode = detail.episode;
    $("edgeOptionDetailTitle").textContent = `${episode.underlying || "Option"} · ${episode.event_type ? labelize(episode.event_type) : labelize(episode.lifecycle)} episode`;
    $("edgeOptionDetailMeta").textContent = [edgeOptionDateRange(episode.activity_from, episode.activity_to), labelize(episode.grouping), "Broker-reported execution evidence"].join(" · ");
    renderEdgeOptionFacts([
      ["Broker realized P/L", episode.realized_pnl_base == null ? "Unavailable" : edgeMoney(episode.realized_pnl_base, currency), episode.realized_pnl_base != null],
      ["P/L evidence", labelize(episode.pnl_status), false],
      ["Exact legs", String((episode.legs || []).length), false],
      ["Missing evidence", edgeOptionMissingText(episode.missing_evidence), false],
    ]);
    const legs = (episode.legs || []).map((leg) => edgeOptionLegRow(leg, currency));
    $("edgeOptionDetailLegs").replaceChildren(...legs);
    $("edgeOptionDetailLegs").hidden = legs.length === 0;
    return;
  }
  const position = detail.open_position;
  $("edgeOptionDetailTitle").textContent = `${edgeOptionContractLabel(position)} · open snapshot`;
  $("edgeOptionDetailMeta").textContent = `${calendarDate(position.snapshot_date)} · Broker-reported open-position evidence`;
  const contractCurrency = String(position.currency || "").toUpperCase();
  renderEdgeOptionFacts([
    ["Side", labelize(position.side), false],
    ["Quantity", edgeQuantity(position.quantity), false],
    ["Contract multiplier", edgeQuantity(position.multiplier), false],
    ["Broker mark", edgePrice(position.mark_price, contractCurrency), position.mark_price != null],
    ["Cost basis", edgePrice(position.cost_basis_money, contractCurrency), position.cost_basis_money != null],
    ["Broker open P/L", position.open_pnl_base == null ? "Unavailable" : edgeMoney(position.open_pnl_base, currency), position.open_pnl_base != null],
    ["P/L evidence", labelize(position.pnl_status), false],
    ["Missing evidence", edgeOptionMissingText(position.missing_evidence), false],
  ]);
  $("edgeOptionDetailLegs").replaceChildren();
  $("edgeOptionDetailLegs").hidden = true;
}

function renderEdgeOptionFacts(facts) {
  $("edgeOptionDetailSummary").replaceChildren(...facts.flatMap(([term, description, sensitive]) => {
    const dt = document.createElement("dt");
    dt.textContent = term;
    const dd = document.createElement("dd");
    dd.textContent = description;
    dd.classList.toggle("is-private", sensitive && !state.accountValueVisible);
    return [dt, dd];
  }));
}

function edgeOptionLegRow(leg, currency) {
  const row = document.createElement("div");
  row.className = "edge-option-leg";
  const identity = document.createElement("div");
  const title = document.createElement("b");
  title.textContent = edgeOptionContractLabel(leg);
  const meta = document.createElement("small");
  const contractCurrency = String(leg.currency || "").toUpperCase();
  meta.textContent = [labelize(leg.side), labelize(leg.open_close), leg.quantity == null ? "quantity unavailable" : `qty ${edgeQuantity(leg.quantity)}`, leg.execution_price == null ? "price unavailable" : `at ${edgePrice(leg.execution_price, contractCurrency)}`, leg.multiplier == null ? "" : `multiplier ${edgeQuantity(leg.multiplier)}`, (leg.missing_evidence || []).length ? `missing ${edgeOptionMissingText(leg.missing_evidence)}` : ""].filter(Boolean).join(" · ");
  identity.append(title, meta);
  const values = document.createElement("div");
  values.className = "edge-option-leg__values";
  const realized = document.createElement("strong");
  realized.textContent = leg.realized_pnl_base == null ? "Realized P/L unavailable" : edgeMoney(leg.realized_pnl_base, currency);
  realized.className = moneyTone(leg.realized_pnl_base);
  realized.classList.toggle("is-private", leg.realized_pnl_base != null && !state.accountValueVisible);
  values.append(realized);
  if (leg.direct_costs_base != null) {
    const costs = document.createElement("small");
    costs.textContent = `Costs ${edgeMoney(leg.direct_costs_base, currency)}`;
    costs.classList.toggle("is-private", !state.accountValueVisible);
    values.append(costs);
  }
  row.append(identity, values);
  return row;
}

function edgeOptionCoverageText(coverage) {
  const parts = [
    [coverage.opening_episodes, "opening"],
    [coverage.closing_episodes, "closing"],
    [coverage.mixed_episodes, "mixed exact-order"],
    [coverage.unknown_episodes, "unknown lifecycle"],
    [coverage.event_episodes, "exercise / assignment / expiration event"],
  ].filter(([count]) => Number(count || 0) > 0).map(([count, label]) => `${Number(count)} ${label}`);
  if (parts.length === 0) return "No option execution or lifecycle-event evidence is present in this window.";
  const openingZero = Number(coverage.opening_only_zero_episodes || 0);
  return `Activity coverage: ${parts.join(" · ")}.${openingZero ? ` ${openingZero} opening-only zero-P/L episode${openingZero === 1 ? " is" : "s are"} retained as activity, not ranked as a realized result.` : ""}`;
}

function edgeOptionRealizedSummary(realized, currency) {
  const total = Number(realized.total_count || 0);
  const known = realized.known_pnl_base == null ? "No numeric broker realized P/L" : `${edgeMoney(realized.known_pnl_base, currency)} known broker realized P/L`;
  const incomplete = Number(realized.partial_count || 0) + Number(realized.unavailable_count || 0);
  const shown = realized.truncated ? ` · showing ${Number(realized.episodes?.length || 0)} numeric P/L rows of ${total}; unavailable rows stay counted, not magnitude-ranked` : "";
  return `${known} · ${Number(realized.positive_count || 0)} gain · ${Number(realized.negative_count || 0)} loss · ${Number(realized.flat_count || 0)} flat${incomplete ? ` · ${incomplete} incomplete` : ""}${shown}`;
}

function edgeOptionOpenSummary(open, currency) {
  const total = Number(open.total_count || 0);
  const known = open.known_pnl_base == null ? "No numeric broker open P/L" : `${edgeMoney(open.known_pnl_base, currency)} known broker open P/L`;
  const unavailable = Number(open.unavailable_count || 0);
  const shown = open.truncated ? ` · showing ${Number(open.positions?.length || 0)} numeric P/L rows of ${total}; unavailable rows stay counted, not magnitude-ranked` : "";
  return `${known} · ${Number(open.positive_count || 0)} gain · ${Number(open.negative_count || 0)} loss · ${Number(open.flat_count || 0)} flat${unavailable ? ` · ${unavailable} unavailable` : ""}${shown}`;
}

function edgeOptionEpisodeTitle(episode) {
  const labels = (episode.legs || []).map(edgeOptionContractLabel);
  if (labels.length === 0) return episode.underlying || "Option episode";
  return `${labels.slice(0, 2).join(" + ")}${labels.length > 2 ? ` +${labels.length - 2} legs` : ""}`;
}

function edgeOptionContractLabel(contract) {
  const root = String(contract?.underlying || contract?.symbol || "Option").trim();
  const parts = [root];
  if (contract?.expiry) parts.push(calendarDate(contract.expiry));
  if (contract?.strike != null) parts.push(edgeQuantity(contract.strike));
  if (contract?.put_call) parts.push(String(contract.put_call).slice(0, 1).toUpperCase());
  return parts.join(" ");
}

function edgeOptionDateRange(from, to) {
  if (!from) return "Undated";
  if (!to || String(from).slice(0, 10) === String(to).slice(0, 10)) return calendarDate(from);
  return `${calendarDate(from)} → ${calendarDate(to)}`;
}

function edgeOptionEvidenceText(status, missing) {
  const gap = (missing || []).length ? ` · missing ${edgeOptionMissingText(missing)}` : "";
  return `${labelize(status)} P/L${gap}`;
}

function edgeOptionMissingText(missing) {
  if (!(missing || []).length) return "None reported";
  const labels = new Map([
    ["realized_pnl", "realized P/L"], ["open_pnl", "open P/L"], ["fx_conversion", "FX conversion"], ["instrument_metadata", "contract metadata"],
  ]);
  return missing.map((reason) => labels.get(reason) || labelize(reason)).join(", ");
}

function renderEdgeMethod(result) {
  const coverage = result.coverage || {};
  const scored = Number(coverage.scored_by_horizon?.[String(result.horizon_sessions)] ?? coverage.scored_by_horizon?.[result.horizon_sessions] ?? 0);
  $("edgeCoverageSummary").textContent = `${scored}/${Number(result.horizon_selection?.eligible_changes || 0)} eligible scored at ${result.horizon_sessions}`;
  const reasonEntries = Object.entries(coverage.reason_counts || {}).sort(([left], [right]) => left.localeCompare(right));
  const lines = [
    `${Number(coverage.eligible_changes || 0)} eligible of ${Number(coverage.trade_changes || 0)} reconstructed changes`,
    ...reasonEntries.map(([reason, count]) => `${labelize(reason)}: ${Number(count || 0)}`),
    ...(coverage.missing_sections?.length ? [`Missing Flex sections: ${coverage.missing_sections.map(labelize).join(", ")}`] : []),
  ];
  $("edgeCoverage").replaceChildren(...lines.map((text) => {
    const line = document.createElement("p");
    line.textContent = text;
    return line;
  }));
  const method = result.method || {};
  const definitions = [
    ["Metric", method.metric],
    ["Counterfactual", method.counterfactual],
    ["Horizons", method.horizon_definition],
    ["Headline", method.headline_selection],
    ["Finding rank", method.finding_ranking],
    ["Materiality", method.materiality_gate],
    ["Automatic horizon", method.automatic_horizon],
    ["Market context", method.market_context],
    ["Account", method.account_definition],
    ["Exclusions", method.exclusions],
    ["Options", method.options_method],
    ["Claims", "Observed results only; no causality, prediction, statistical skill, or investment advice."],
  ];
  $("edgeMethodList").replaceChildren(...definitions.flatMap(([term, description]) => {
    const dt = document.createElement("dt");
    dt.textContent = term;
    const dd = document.createElement("dd");
    dd.textContent = String(description || "—");
    return [dt, dd];
  }));
  $("edgeRevalidation").textContent = result.last_full_revalidation
    ? `Last full-year revalidation: ${calendarDateTime(result.last_full_revalidation)}.`
    : "A full-year revalidation has not completed yet.";
}

function emptyEdgeRow(text) {
  const row = document.createElement("p");
  row.className = "empty-row";
  row.textContent = text;
  return row;
}

function edgeCurrency(result) {
  return String(result.account?.base_currency || "").toUpperCase();
}

function edgeMoney(value, currency) {
  if (!hasNumericValue(value)) return "—";
  if (!state.accountValueVisible) return privacyMask();
  return currency ? signedMoneyRead(value, currency) : `${value > 0 ? "+" : ""}${money(value, "")}`;
}

function edgePrice(value, currency) {
  if (!hasNumericValue(value)) return "—";
  if (!state.accountValueVisible) return privacyMask();
  return money(value, currency);
}

function edgeQuantity(value) {
  if (!hasNumericValue(value)) return "—";
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: 6 }).format(value);
}

function edgeSignedQuantity(value) {
  if (!hasNumericValue(value)) return "—";
  return `${value > 0 ? "+" : ""}${edgeQuantity(value)}`;
}

function edgeSignedPercent(value) {
  if (!hasNumericValue(value)) return "—";
  if (!state.accountValueVisible) return privacyMask();
  return `${value > 0 ? "+" : ""}${Number(value).toFixed(2)}%`;
}

function edgeMarketPercent(value) {
  if (!hasNumericValue(value)) return "—";
  return `${value > 0 ? "+" : ""}${Number(value).toFixed(2)}%`;
}

function edgeMarketContextText(row) {
  if (row.kind === "volatility_index" && hasNumericValue(row.change_points)) {
    return `${row.label} ${Number(row.change_points) > 0 ? "+" : ""}${Number(row.change_points).toFixed(2)} pts`;
  }
  return `${row.label} ${edgeMarketPercent(row.change_pct)}`;
}

function moneyTone(value) {
  if (!hasNumericValue(value) || value === 0) return "";
  return value > 0 ? "ok" : "risk";
}

export { edgeHasResults, refreshEdge, renderEdge, validEdgeResult };
