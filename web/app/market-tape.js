import { $, calendarDate, calendarDateTime, readJSONOrText, weekdayName } from "./shared.js";
import { state } from "./state.js";

const metricNames = { volume: "ETF volume", relative_volume_20: "20-session relative volume", pct_above_20dma: "20-day participation", pct_above_200dma: "200-day participation", highs_lows: "Closing highs/lows", advance_decline: "Daily participation", constituent_volume: "Directional share volume" };
const sourceNames = { spx: "SPX price", qqq: "QQQ price and volume", breadth: "S&P 500 breadth" };
const finite = (v) => typeof v === "number" && Number.isFinite(v);
const optional = (v) => v === null || finite(v);
const count = (v) => Number.isSafeInteger(v) && v >= 0;
const day = (v) => typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v) && Number.isFinite(Date.parse(`${v}T12:00:00Z`)) && new Date(`${v}T12:00:00Z`).toISOString().slice(0, 10) === v;
const number = (v, suffix = "", digits = 2) => finite(v) ? `${v.toFixed(digits)}${suffix}` : "—";
const signed = (v, suffix = "%") => finite(v) ? `${v > 0 ? "+" : ""}${v.toFixed(2)}${suffix}` : "—";

function validTapeReading(r) {
  if (r === undefined || r === null) return true;
  const text = (v) => typeof v === "string" && v.length <= 2000;
  const list = (v) => Array.isArray(v) && v.length <= 20 && v.every(text);
  return text(r.headline) && text(r.summary) && list(r.watch_for) && list(r.limits) && Array.isArray(r.evidence) && r.evidence.length <= 20
    && r.evidence.every((e) => e && [e.key, e.label, e.value, e.meaning].every(text));
}

function validParticipation(p, members) {
  if (p === undefined || p === null) return true;
  if (p.method !== "constituent-participation-v1" || !Number.isFinite(Date.parse(p.recorded_at)) || !/^[0-9a-f]{64}$/.test(p.membership_id)) return false;
  if (![p.coverage_20, p.coverage_ad, p.coverage_volume, p.advancing, p.declining, p.unchanged].every((n) => count(n) && n <= members)) return false;
  if (p.advancing + p.declining + p.unchanged !== p.coverage_ad || p.coverage_volume > p.coverage_ad) return false;
  if (![p.pct_above_20dma, p.advance_pct, p.up_volume_pct].every((v) => v === null || finite(v) && v >= 0 && v <= 100)) return false;
  if (![p.advancing_volume, p.declining_volume, p.unchanged_volume].every((v) => finite(v) && v >= 0)) return false;
  return (p.coverage_20 > 0) === (p.pct_above_20dma !== null) && (p.advancing + p.declining > 0) === (p.advance_pct !== null)
    && (p.advancing_volume + p.declining_volume > 0) === (p.up_volume_pct !== null) && (p.coverage_ad === 0 || Number.isFinite(Date.parse(p.input_observed_at)));
}

function validMarketTape(result) {
  if (!result || result.schema_version !== "market-tape-v1" || result.not_predictive !== true || result.historical_availability !== "unknown" || result.timezone !== "America/New_York") return false;
  if (!["available", "partial", "unavailable"].includes(result.coverage_status) || !Number.isFinite(Date.parse(result.as_of)) || !day(result.latest_session)) return false;
  if (!Array.isArray(result.sessions) || result.sessions.length < 5 || result.sessions.length > 60 || result.sessions.at(-1)?.date !== result.latest_session) return false;
  const price = (p, index) => p === null || Boolean(p && finite(p.close) && p.close > 0 && optional(p.change_pct) && optional(p.window_change_pct)
    && (p.volume === null || count(p.volume)) && (p.relative_volume_20 === null || finite(p.relative_volume_20) && p.relative_volume_20 >= 0)
    && (!index || p.volume === null && p.relative_volume_20 === null));
  const breadth = (b) => {
    if (b === null) return true;
    if (!b || !count(b.member_count) || b.member_count === 0 || !optional(b.change_50_pp) || !validParticipation(b.participation, b.member_count)) return false;
    for (const [value, coverage] of [[b.pct_above_50dma, b.coverage_50], [b.pct_above_200dma, b.coverage_200]]) {
      if (!count(coverage) || coverage > b.member_count || !(value === null ? coverage === 0 : finite(value) && value >= 0 && value <= 100 && coverage > 0)) return false;
    }
    if (!count(b.coverage_highs_lows) || b.coverage_highs_lows > b.member_count) return false;
    return b.new_highs === null && b.new_lows === null ? b.coverage_highs_lows === 0
      : count(b.new_highs) && count(b.new_lows) && b.coverage_highs_lows > 0 && b.new_highs + b.new_lows <= b.coverage_highs_lows;
  };
  if (!result.sessions.every((s, i) => s && day(s.date) && (i === 0 || s.date > result.sessions[i - 1].date) && price(s.spx, true) && price(s.qqq, false) && breadth(s.breadth) && validTapeReading(s.reading))) return false;
  if (!Array.isArray(result.sources) || result.sources.length !== 3 || new Set(result.sources.map((s) => s?.key)).size !== 3) return false;
  if (!result.sources.every((s) => s && Object.hasOwn(sourceNames, s.key) && ["available", "partial", "unavailable"].includes(s.status) && count(s.missing_sessions) && s.missing_sessions <= result.sessions.length
    && typeof s.source === "string" && typeof s.detail === "string" && (!s.as_of || Number.isFinite(Date.parse(s.as_of)))
    && (!s.covered_through || day(s.covered_through)) && Object.entries(s.missing_metrics || {}).every(([key, value]) => Object.hasOwn(metricNames, key) && count(value) && value <= result.sessions.length))) return false;
  return Array.isArray(result.notes) && result.notes.every((s) => typeof s === "string");
}

async function refreshMarketTape() {
  if (!state.authenticated) return false;
  const sessions = Number($("marketTapeSessions")?.value || 20);
  if (![10, 20, 60].includes(sessions)) return false;
  const requestID = ++state.marketTapeRequestID;
  state.marketTapeBusy = true;
  state.marketTapeError = "";
  renderMarketTape();
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), 40000);
  try {
    const response = await fetch(`/api/market-tape?sessions=${sessions}`, { credentials: "include", signal: controller.signal, cache: "no-store" });
    const result = await readJSONOrText(response);
    if (!response.ok || !validMarketTape(result) || result.sessions.length !== sessions) throw new Error("Unavailable tape");
    if (requestID !== state.marketTapeRequestID) return false;
    const prior = state.marketTapeResult;
    const selectedDate = prior?.sessions[state.marketTapeIndex]?.date;
    const followingLatest = !prior || selectedDate === prior.latest_session;
    state.marketTapeResult = result;
    const retainedIndex = followingLatest ? -1 : result.sessions.findIndex((row) => row.date === selectedDate);
    state.marketTapeIndex = retainedIndex >= 0 ? retainedIndex : result.sessions.length - 1;
    return true;
  } catch {
    if (requestID !== state.marketTapeRequestID) return false;
    state.marketTapeError = "Market tape unavailable. Refresh to try again.";
    return false;
  } finally {
    clearTimeout(timer);
    if (requestID === state.marketTapeRequestID) {
      state.marketTapeBusy = false;
      renderMarketTape();
    }
  }
}

function element(tag, text = "", className = "") {
  const el = document.createElement(tag);
  el.textContent = text;
  if (className) el.className = className;
  return el;
}

function svgElement(tag, attributes) {
  const el = document.createElementNS("http://www.w3.org/2000/svg", tag);
  Object.entries(attributes).forEach(([name, value]) => el.setAttribute(name, value));
  return el;
}

// Each missing observation ends a path. Axes and value formatting are view
// concerns; returns and evidence coverage come only from the daemon result.
function tapePath(values, x, y) {
  let previous = false;
  return values.map((v, i) => {
    if (!finite(v)) { previous = false; return ""; }
    const point = `${previous ? "L" : "M"}${x(i).toFixed(2)},${y(v).toFixed(2)}`;
    previous = true;
    return point;
  }).filter(Boolean).join(" ");
}

function plot(title, series, selected, { bounds, suffix = "", bars = false } = {}) {
  const figure = element("figure", "", "market-tape__plot");
  const caption = element("figcaption", title);
  const legend = element("span", "", "market-tape__legend");
  series.forEach((s, i) => legend.append(element("span", s.name, `market-tape__series-${i}`)));
  caption.append(legend);
  const values = series.flatMap((s) => s.values).filter(finite);
  const lo = bounds?.[0] ?? Math.min(0, ...values);
  const hi = bounds?.[1] ?? Math.max(1, ...values);
  const x = (i) => 4 + 592 * i / (series[0].values.length - 1);
  const y = (v) => 116 - 112 * (v - lo) / (hi - lo);
  const frame = element("div", "", "market-tape__plot-frame");
  const scale = element("div", "", "market-tape__scale");
  scale.append(element("span", number(hi, suffix, 1)), element("span", number(lo, suffix, 1)));
  const svg = svgElement("svg", { viewBox: "0 0 600 120", preserveAspectRatio: "none", role: "img", "aria-label": `${title}. Select a session below for exact values.`, class: "market-tape__svg" });
  svg.append(svgElement("line", { x1: 0, x2: 600, y1: y(0), y2: y(0), class: "market-tape__baseline" }));
  svg.append(svgElement("line", { x1: x(selected), x2: x(selected), y1: 0, y2: 120, class: "market-tape__cursor" }));
  series.forEach((s, index) => {
    if (bars) {
      s.values.forEach((v, i) => {
        if (!finite(v)) return;
        const width = Math.min(18, 400 / s.values.length);
        svg.append(svgElement("rect", { x: x(i) - width / 2, y: y(v), width, height: Math.max(1, y(0) - y(v)), class: `market-tape__bar market-tape__series-${index}` }));
      });
    } else {
      svg.append(svgElement("path", { d: tapePath(s.values, x, y), class: `market-tape__line market-tape__series-${index}` }));
      s.values.forEach((v, i) => {
        if (finite(v)) svg.append(svgElement("circle", { cx: x(i), cy: y(v), r: 2, class: `market-tape__dot market-tape__series-${index}` }));
      });
    }
  });
  frame.append(scale, svg);
  figure.append(caption, frame);
  if (values.length === 0) figure.append(element("p", "No observations available for this chart.", "market-tape__muted"));
  return figure;
}

function renderMarketTape() {
  const result = state.marketTapeResult;
  $("marketTapeRefresh").disabled = state.marketTapeBusy;
  $("marketTapeSessions").disabled = state.marketTapeBusy;
  $("marketTapeStatus").textContent = state.marketTapeError || (state.marketTapeBusy ? "Reading daily observations…" : result ? `${result.coverage_status === "available" ? "Available" : result.coverage_status === "partial" ? "Partial coverage" : "Unavailable"} · through ${calendarDate(result.latest_session)}` : "Open to read completed US sessions.");
  $("marketTapeContent").hidden = !result;
  if (!result) return;
  $("marketTapeReadAt").textContent = `Snapshot read ${calendarDateTime(result.as_of, { timeZoneName: "short" })}${state.marketTapeBusy || state.marketTapeError ? " · showing the previous read" : ""}. Gaps are missing observations.`;
  const rows = result.sessions;
  const index = Math.max(0, Math.min(rows.length - 1, state.marketTapeIndex));
  const slider = $("marketTapeSession");
  slider.min = "0"; slider.max = String(rows.length - 1); slider.value = String(index);
  const row = rows[index];
  const dateLabel = `${weekdayName(row.date)} · ${calendarDate(row.date)}`;
  slider.setAttribute("aria-valuetext", dateLabel);
  $("marketTapeSelectedDate").textContent = dateLabel;
  const reading = row.reading;
  $("marketTapeHeadline").textContent = reading?.headline || "Interpretation unavailable for this read";
  $("marketTapeSummary").textContent = reading?.summary || "The recorded measurements remain visible below.";
  const giveback = reading?.evidence.find((e) => e.key === "giveback");
  if (giveback) $("marketTapeSummary").textContent += ` ${giveback.label}: ${giveback.value}.`;
  $("marketTapeMeaning").replaceChildren(...(reading?.evidence || []).flatMap((e) => [element("dt", `${e.label} · ${e.value}`), element("dd", e.meaning)]));
  $("marketTapeWatch").textContent = reading ? `Next-session checks: ${reading.watch_for.join(" ")}` : "";
  $("marketTapeLimits").replaceChildren(...(reading?.limits || []).map((v) => element("li", v)));
  $("marketTapeStart").textContent = calendarDate(rows[0].date);
  $("marketTapeEnd").textContent = calendarDate(rows.at(-1).date);
  $("marketTapePlots").replaceChildren(
    plot("Price change from first displayed close", [{ name: "SPX", values: rows.map((r) => r.spx?.window_change_pct) }, { name: "QQQ", values: rows.map((r) => r.qqq?.window_change_pct) }], index, { suffix: "%" }),
    plot("S&P 500 participation", [{ name: "Above 50-day", values: rows.map((r) => r.breadth?.pct_above_50dma) }, { name: "Above 200-day", values: rows.map((r) => r.breadth?.pct_above_200dma) }], index, { bounds: [0, 100], suffix: "%" }),
    plot("QQQ reported volume", [{ name: "ETF shares · millions", values: rows.map((r) => finite(r.qqq?.volume) ? r.qqq.volume / 1e6 : null) }], index, { bars: true, suffix: "M" }),
    plot("Daily constituent participation", [{ name: "Advancer share", values: rows.map((r) => r.breadth?.participation?.advance_pct) }, { name: "Up-volume share", values: rows.map((r) => r.breadth?.participation?.up_volume_pct) }], index, { bounds: [0, 100], suffix: "%" }),
  );
  const b = row.breadth;
  const p = b?.participation;
  const metrics = [
    ["SPX daily change", signed(row.spx?.change_pct), `Close ${number(row.spx?.close)}`],
    ["QQQ daily change", signed(row.qqq?.change_pct), `Close ${number(row.qqq?.close)}`],
    ["Above 50-day", number(b?.pct_above_50dma, "%", 1), `${signed(b?.change_50_pp, " pp")} daily · ${b?.coverage_50 || 0}/${b?.member_count || "—"} covered`],
    ["QQQ relative volume", number(row.qqq?.relative_volume_20, "×"), `${finite(row.qqq?.volume) ? row.qqq.volume.toLocaleString("en-US") : "—"} reported shares`],
    ["Above 200-day", number(b?.pct_above_200dma, "%", 1), `${b?.coverage_200 || 0}/${b?.member_count || "—"} covered`],
    ["Closing highs / lows", `${number(b?.new_highs, "", 0)} / ${number(b?.new_lows, "", 0)}`, `Prior 252 sessions · ${b?.coverage_highs_lows || 0}/${b?.member_count || "—"} covered`],
    ["Above 20-day", number(p?.pct_above_20dma, "%", 1), `${p?.coverage_20 || 0}/${b?.member_count || "—"} covered`],
    ["Daily advancer share", number(p?.advance_pct, "%", 1), `${p?.coverage_ad || 0}/${b?.member_count || "—"} covered · unchanged excluded`],
    ["Up-volume share", number(p?.up_volume_pct, "%", 1), `${p?.coverage_volume || 0}/${b?.member_count || "—"} covered · unchanged excluded`],
  ];
  $("marketTapeValues").replaceChildren(...metrics.map(([label, value, detail]) => {
    const item = element("div", "", "market-tape__value");
    item.append(element("span", label), element("strong", value), element("small", detail));
    return item;
  }));
  $("marketTapeSources").replaceChildren(...result.sources.map((source) => {
    const item = element("li");
    item.append(element("strong", `${sourceNames[source.key]} · ${source.status}`));
    item.append(element("p", `${source.missing_sessions} missing ${source.key === "breadth" ? "50-day breadth" : "price"} sessions. Acquired ${source.as_of ? calendarDateTime(source.as_of, { timeZoneName: "short" }) : "unknown"}. ${source.detail}`));
    Object.entries(source.missing_metrics || {}).forEach(([key, n]) => item.append(element("p", `${metricNames[key]}: unavailable for ${n} sessions.`)));
    return item;
  }));
  $("marketTapeNotes").replaceChildren(...result.notes.map((note) => element("li", note)));
  if (p) $("marketTapeNotes").append(element("li", `Selected participation revision computed ${calendarDateTime(p.recorded_at, { timeZoneName: "short" })}; latest paired input acquired ${p.input_observed_at ? calendarDateTime(p.input_observed_at, { timeZoneName: "short" }) : "unknown"}. These are collection clocks, not historical publication times.`));
}

function setupMarketTape() {
  $("marketTapePanel").addEventListener("toggle", () => { if ($("marketTapePanel").open) void refreshMarketTape(); });
  $("marketTapeRefresh").addEventListener("click", refreshMarketTape);
  $("marketTapeSessions").addEventListener("change", refreshMarketTape);
  $("marketTapeSession").addEventListener("input", (event) => {
    state.marketTapeIndex = Number(event.target.value);
    renderMarketTape();
  });
}

export { refreshMarketTape, renderMarketTape, setupMarketTape, tapePath, validMarketTape };
