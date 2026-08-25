import { $, calendarDate, calendarDateTime, hasNumericValue, labelize, money, privacyMask, readJSONOrText, signedMoneyRead } from "./shared.js";
import { state } from "./state.js";

const EDGE_WINDOWS = new Set(["90d", "365d"]);
const EDGE_HORIZONS = new Set([1, 5, 20]);
const EDGE_STATES = new Set(["action_required", "backfilling", "current", "degraded", "insufficient_evidence", "unavailable"]);
const EDGE_ACTIONS = ["open", "add", "trim", "exit"];

async function refreshEdge(changeID = "") {
  if (!state.authenticated) return false;
  const requestedChange = String(changeID || "").trim();
  if (requestedChange && (requestedChange.length > 128 || !requestedChange.startsWith("change_"))) {
    state.edgeError = "Canary Edge received an invalid finding reference";
    renderEdge();
    return false;
  }
  const requestID = (state.edgeRequestID || 0) + 1;
  state.edgeRequestID = requestID;
  state.edgeBusy = true;
  state.edgeError = "";
  renderEdge();
  try {
    const path = requestedChange ? `/api/edge?${new URLSearchParams({ change: requestedChange })}` : "/api/edge";
    const response = await fetch(path, { credentials: "include" });
    const body = await readJSONOrText(response);
    if (!response.ok) throw new Error(typeof body === "string" ? body : body.error || "Canary Edge unavailable");
    if (!validEdgeResult(body)) throw new Error("Canary Edge returned an invalid typed result");
    if (requestedChange && body.change?.id !== requestedChange) throw new Error("Canary Edge returned the wrong finding explanation");
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
  if (!EDGE_STATES.has(result.state) || !EDGE_WINDOWS.has(result.window)) return false;
  if (!EDGE_HORIZONS.has(Number(result.horizon_sessions)) || result.not_execution !== true) return false;
  if (!Array.isArray(result.action_rollups) || !Array.isArray(result.findings) || result.findings.length > 3) return false;
  if (!Array.isArray(result.options) || !result.coverage || typeof result.coverage !== "object" || !result.method) return false;
  if (result.fingerprint && (result.method.metric !== "Decision price impact" || !String(result.method.headline_selection || "").trim() || !String(result.method.finding_ranking || "").trim())) return false;
  if (result.change != null && !validEdgeChange(result.change)) return false;
  return result.findings.every((finding) => String(finding?.change_id || "").startsWith("change_") && hasNumericValue(finding?.decision_notional_base) && Number(finding.decision_notional_base) > 0 && hasNumericValue(finding?.decision_impact_base) && hasNumericValue(finding?.decision_impact_pct))
    && result.options.every((option) => String(option?.id || "").startsWith("option_") && option?.actual_only === true);
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
    if (hasImpact && (score.reason || !hasNumericValue(score.decision_notional_base) || score.decision_notional_base <= 0 || !hasNumericValue(score.decision_impact_pct))) return false;
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
  const setup = result.state === "action_required";
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
    insufficient_evidence: "Account evidence may be usable, but Canary is still waiting for broker-confirmed trade history. It will retry automatically.",
    unavailable: "No sound Edge result is currently available.",
  }[result.state] || labelize(result.state);
  const reason = result.reason ? ` ${labelize(result.reason)}.` : "";
  target.textContent = copy + reason;
  target.classList.add(result.state === "degraded" || result.state === "insufficient_evidence" || result.state === "unavailable" ? "edge-status--risk" : "edge-status--watch");
}

function edgeHasResults(result) {
  return Boolean(result.account || result.action_rollups?.length || result.findings?.length || result.options?.length || result.change || result.fingerprint);
}

function renderEdgeSetup(result) {
  const setup = result.setup || {};
  $("edgeSetupReason").textContent = result.reason
    ? `Canary cannot calculate broker-truth results: ${labelize(result.reason)}.`
    : "Canary needs the canonical IBKR Flex profile before it can calculate this review.";
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
  $("edgeImpactLens").textContent = `${period} · ${result.horizon_sessions}-session headline`;
  $("edgeHeadline").textContent = state.accountValueVisible
    ? (result.headline || "No highlighted finding has sufficient evidence.")
    : "Reveal account values to view the monetary headline.";
  $("edgeHeadline").classList.toggle("is-private", !state.accountValueVisible && Boolean(result.headline));
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
    meta.textContent = `${calendarDate(finding.executed_at)} · ${finding.horizon_sessions} sessions · ${labelize(finding.direction)}${impactPct}`;
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
  const options = result.options || [];
  $("edgeOptionsCount").textContent = `${options.length} result${options.length === 1 ? "" : "s"}`;
  if (options.length === 0) {
    $("edgeOptionList").replaceChildren(emptyEdgeRow("No broker-actual option result is available for this window."));
    return;
  }
  $("edgeOptionList").replaceChildren(...options.map((option) => {
    const row = document.createElement("div");
    row.className = "edge-option-row";
    const identity = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = option.symbol || option.underlying || "Option position";
    const meta = document.createElement("small");
    meta.textContent = `${labelize(option.grouping)} · ${Number(option.leg_count || 0)} leg${Number(option.leg_count || 0) === 1 ? "" : "s"}`;
    identity.append(title, meta);
    const values = document.createElement("div");
    values.className = "edge-option-row__values";
    for (const [label, value] of [["Actual", option.actual_pnl_base], ["Realized", option.realized_pnl_base], ["Open", option.open_pnl_base]]) {
      if (value == null) continue;
      const span = document.createElement("span");
      span.textContent = `${label} ${edgeMoney(value, edgeCurrency(result))}`;
      span.className = moneyTone(value);
      span.classList.toggle("is-private", !state.accountValueVisible);
      values.append(span);
    }
    if (values.childElementCount === 0) values.textContent = "P/L unavailable";
    row.append(identity, values);
    return row;
  }));
}

function renderEdgeMethod(result) {
  const coverage = result.coverage || {};
  const scored = Number(coverage.scored_by_horizon?.[String(result.horizon_sessions)] ?? coverage.scored_by_horizon?.[result.horizon_sessions] ?? 0);
  $("edgeCoverageSummary").textContent = `${scored}/${Number(coverage.trade_changes || 0)} scored at ${result.horizon_sessions}`;
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

function moneyTone(value) {
  if (!hasNumericValue(value) || value === 0) return "";
  return value > 0 ? "ok" : "risk";
}

export { edgeHasResults, refreshEdge, renderEdge, validEdgeResult };
