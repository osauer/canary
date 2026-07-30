import { humanList } from "./stress.js";
import { $, cleanDetail, labelize, parseDate, shortTimeWithZone } from "./shared.js";
import { state } from "./state.js";

function renderSourceBanners(snap) {
  const snapshotErrors = (snap.errors || []).filter((err) => err.source !== "market_quotes");
  const summary = snapshotIssueSummary(snapshotErrors, snap);
  setBanner("snapshotErrorBanner", "snapshotErrorText", summary.text, summary.title);
  $("bannerStack").hidden = snapshotErrors.length === 0;
}

function snapshotIssueSummary(errors, snap = {}) {
  if (!errors.length) return { text: "", title: "" };
  const sources = [...new Set(errors.map((err) => snapshotSourceLabel(err.source)).filter(Boolean))];
  const sourceText = humanList(sources, 3);
  const title = errors.map((err) => `${err.source}: ${err.message}`).join(" | ");
  const missingSources = errors
    .filter((err) => !snapshotPayloadPresent(snap, err.source))
    .map((err) => snapshotSourceLabel(err.source));
  const unavailablePortfolio = errors
    .filter((err) => ["account", "positions"].includes(String(err.source || "").toLowerCase()))
    .filter((err) => !snapshotPayloadPresent(snap, err.source))
    .map((err) => snapshotSourceLabel(err.source));
  if (unavailablePortfolio.length > 0) {
    const unavailableSources = [...new Set(unavailablePortfolio)];
    if (unavailableSources.includes("account") && unavailableSources.includes("positions")) {
      return { text: "Account and positions unavailable.", title };
    }
    const unavailableText = humanList(unavailableSources, 3) || "Data";
    return { text: `${unavailableText.charAt(0).toUpperCase()}${unavailableText.slice(1)} unavailable.`, title };
  }
  const gateway = gatewayIssueText(snap);
  if (gateway) {
    return { text: gateway, title };
  }
  if (missingSources.length > 0) {
    const missingText = humanList([...new Set(missingSources)], 3) || "Data";
    return { text: `${missingText.charAt(0).toUpperCase()}${missingText.slice(1)} unavailable.`, title };
  }
  return {
    text: `${sourceText || "Data"} feed interrupted; showing last good snapshot.`,
    title,
  };
}

function snapshotPayloadPresent(snap, source) {
  const sourceKey = String(source || "").toLowerCase();
  const key = sourceKey === "calendar" ? "market_calendar" : sourceKey;
  const payload = snap?.[key];
  return Boolean(payload) && typeof payload === "object" && Object.keys(payload).length > 0;
}

function gatewayIssueText(snap = {}) {
  const direct = String(snap.status?.last_error || "").trim();
  const source = direct || (snap.errors || []).map((err) => err.message).find((msg) => /client id .*already in use/i.test(String(msg || ""))) || "";
  if (!source) return "";
  let text = String(source)
    .replace(/^gateway_unavailable:\s*/i, "")
    .replace(/^ibkr connection unavailable:\s*/i, "")
    .replace(/^ibkr:\s*client id already in use:\s*/i, "")
    .trim();
  if (!/client id .*already in use/i.test(text)) return "";
  text = text.charAt(0).toUpperCase() + text.slice(1);
  if (!/[.!?]$/.test(text)) text += ".";
  return text;
}

function snapshotSourceLabel(source) {
  switch (String(source || "").toLowerCase()) {
    case "account":
      return "account";
    case "positions":
      return "positions";
    case "status":
      return "gateway status";
    case "calendar":
      return "market calendar";
    case "trading":
      return "trading status";
    case "stress":
      return "stress";
    case "regime":
      return "regime";
    default:
      return cleanDetail(source);
  }
}

function setBanner(bannerID, textID, text, title = "") {
  const banner = $(bannerID);
  if (!banner) return;
  banner.hidden = !text;
  const target = $(textID);
  target.textContent = text || "--";
  target.title = title || text || "";
}

function renderTopbar(snap) {
  const label = marketSessionLabel(currentMarketCalendar(snap));
  const line = $("connectionLine");
  const strip = document.querySelector(".market-strip");
  line.textContent = label.side || label.text || state.connectionText;
  line.classList.remove("market-open", "market-closed", "market-warn");
  strip?.classList.remove("market-open", "market-closed", "market-warn");
  if (label.tone) {
    line.classList.add(label.tone);
    strip?.classList.add(label.tone);
  }
  const phase = $("sessionPhase");
  phase.textContent = label.phase;
  // The chip prints a countdown only; the session's own words (weekend,
  // holiday, early close, calendar coverage) stay reachable as its title.
  phase.title = label.text || "";
  const marketDot = $("marketStateDot");
  if (marketDot) {
    const dotLabel = label.dotTitle || label.text || "Market session status";
    marketDot.setAttribute("aria-label", dotLabel);
    marketDot.title = dotLabel;
  }
}

function currentMarketCalendar(snap) {
  return state.marketCalendarOverride || snap.market_calendar;
}

function setupMarketSelect() {
  const select = $("marketSelect");
  if (!select) return;
  select.value = state.selectedMarket;
  select.addEventListener("change", () => {
    state.selectedMarket = select.value || "us";
    localStorage.setItem("canarySelectedMarket", state.selectedMarket);
    if (state.selectedMarket === "us") {
      state.marketCalendarOverride = null;
      renderTopbar(state.snapshot || {});
      return;
    }
    refreshSelectedMarketCalendar();
  });
}

async function refreshSelectedMarketCalendar() {
  const select = $("marketSelect");
  const market = state.selectedMarket || "us";
  if (select) select.disabled = true;
  try {
    const res = await fetch(`/api/market-calendar?market=${encodeURIComponent(market)}`, { credentials: "include" });
    if (!res.ok) throw new Error(await res.text());
    state.marketCalendarOverride = await res.json();
  } catch {
    state.marketCalendarOverride = null;
  } finally {
    if (select) select.disabled = false;
    renderTopbar(state.snapshot || {});
  }
}

function renderSyncStrip(snap) {
  const strip = $("syncStrip");
  if (!strip) return;
  const updatedAt = parseDate(snap.updated_at);
  if (!updatedAt) {
    strip.hidden = true;
    return;
  }

  const ageMinutes = Math.max(0, Math.floor((Date.now() - updatedAt.getTime()) / 60000));
  const sourceIssues = Object.values(snap.sources || {}).filter((meta) => meta?.error).length;
  const stateLabel = !state.connectionOK
    ? "syncing"
    : sourceIssues > 0
      ? "degraded"
      : ageMinutes >= 5
        ? "stale"
        : "live";
  // Panel Dark foot plate: "Snapshot HH:MM:SS · Auto" in the engraved
  // register. The transport word moved into the plate's title so the line
  // stays a stamp rather than a status paragraph.
  $("syncStatusLabel").textContent = sourceIssues > 0 ? "Data gaps" : "Snapshot";
  $("syncStatusTime").textContent = shortTimeWithZone(snap.updated_at);
  $("syncStatusState").textContent = labelize(stateLabel);
  strip.classList.toggle("sync-strip--degraded", stateLabel !== "live");
  strip.title = state.connectionOK ? "SSE connected" : "SSE reconnecting";
  strip.hidden = false;
}

function marketSessionLabel(calendar) {
  const session = calendar?.session;
  if (!session) {
    return {
      text: state.connectionOK ? "Waiting for official market calendar" : "App connection offline",
      tone: state.connectionOK ? "market-warn" : "market-closed",
      phase: state.connectionOK ? "syncing" : "offline",
      countdownVerb: "opens",
      countdown: "--",
      side: state.connectionOK ? "Calendar pending" : "Offline",
      dotTitle: state.connectionOK ? "Market calendar is loading" : "App stream is reconnecting",
    };
  }
  const now = Date.now();
  const stateText = String(session.state || "").toLowerCase();
  const reason = session.reason ? ` (${session.reason})` : "";
  const open = parseDate(session.open);
  const close = parseDate(session.close);
  const nextOpen = parseDate(session.next_open);
  if (session.is_open) {
    const timeLeft = countdownLabel(close);
    const phase = stateText === "early_close" ? "early" : "RTH";
    return {
      text: session.reason || "Regular cash session",
      tone: "market-open",
      phase: marketStatusPhrase(phase, "closes", timeLeft),
      countdownVerb: "closes",
      countdown: timeLeft || "live",
      side: marketSessionNow(session),
      dotTitle: stateText === "early_close" ? "Selected market is open in an early-close session" : "Selected market is open",
    };
  }

  if (open && now < open.getTime()) {
    const untilOpen = countdownLabel(open);
    return {
      text: session.state === "early_close" ? session.reason || "Shortened session ahead" : "Regular cash session",
      tone: "market-warn",
      phase: marketStatusPhrase("", "opens", untilOpen),
      countdownVerb: "opens",
      countdown: untilOpen || "--",
      side: marketSessionNow(session),
      dotTitle: "Selected market is pre-open",
    };
  }

  if (close && nextOpen && now >= close.getTime()) {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Next regular cash session",
      tone: "market-closed",
      phase: marketStatusPhrase("", "opens", untilOpen),
      countdownVerb: "opens",
      countdown: untilOpen || "--",
      side: marketSessionNow(session),
      dotTitle: stateText === "early_close" ? "Selected market has closed after an early-close session" : "Selected market is closed",
    };
  }

  if (stateText === "holiday") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason || "Official market holiday",
      tone: "market-closed",
      phase: marketStatusPhrase("", "opens", untilOpen),
      countdownVerb: "opens",
      countdown: untilOpen || "--",
      side: marketSessionNow(session),
      dotTitle: "Selected market is closed for a holiday",
    };
  }

  if (stateText === "closed") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: session.reason === "weekend" ? "Weekend closure" : `Outside regular cash session${reason}`,
      tone: "market-closed",
      phase: marketStatusPhrase("", "opens", untilOpen),
      countdownVerb: "opens",
      countdown: untilOpen || "--",
      side: marketSessionNow(session),
      dotTitle: session.reason === "weekend" ? "Selected market is closed for the weekend" : "Selected market is closed",
    };
  }

  if (stateText === "unknown") {
    const untilOpen = countdownLabel(nextOpen);
    return {
      text: `Calendar coverage unavailable${reason}`,
      tone: "market-warn",
      phase: marketStatusPhrase("unknown", "opens", untilOpen),
      countdownVerb: "opens",
      countdown: untilOpen || "--",
      side: marketSessionNow(session),
      dotTitle: "Selected market calendar status is unknown",
    };
  }

  const untilOpen = countdownLabel(nextOpen);
  return {
    text: session.reason || `Official calendar${reason}`,
    tone: "market-warn",
    phase: marketStatusPhrase(cleanDetail(session.state), "opens", untilOpen),
    countdownVerb: "opens",
    countdown: untilOpen || "--",
    side: marketSessionNow(session),
    dotTitle: "Selected market calendar status needs attention",
  };
}

// Session chip register: "RTH · closes 3:59" while the market trades,
// "opens 17:12" otherwise. The chip carries the market code itself (the
// #marketSelect control), so the phrase never repeats it, and a missing
// countdown degrades to the phase word rather than printing "opens --".
function marketStatusPhrase(phase, verb, countdown) {
  const timing = countdown ? `${verb} ${countdown}` : "";
  return [phase, timing].filter(Boolean).join(" · ") || "closed";
}

function marketSessionNow(session) {
  const parts = new Intl.DateTimeFormat(undefined, {
    day: "numeric",
    month: "short",
    year: "numeric",
    hour: "2-digit",
    minute: "2-digit",
    timeZoneName: "short",
    timeZone: session?.timezone || undefined,
  }).formatToParts(new Date());
  const visiblePartTypes = new Set(["day", "month", "year", "hour", "minute", "dayPeriod", "timeZoneName", "literal"]);
  return parts
    .filter(({ type }) => visiblePartTypes.has(type))
    .map(({ value }) => value)
    .join("")
    .trim();
}

// Minutes precision: a ticking seconds digit is motion without information
// on a session countdown, and the readout must not flicker.
function countdownLabel(target) {
  if (!target) return "";
  const ms = target.getTime() - Date.now();
  if (ms <= 0) return "";
  const totalMinutes = Math.ceil(ms / 60000);
  const days = Math.floor(totalMinutes / 1440);
  const hours = Math.floor((totalMinutes % 1440) / 60);
  const minutes = totalMinutes % 60;
  const clock = `${hours}:${String(minutes).padStart(2, "0")}`;
  return days > 0 ? `${days}d ${clock}` : clock;
}

function greeksCoverage(portfolio, positions) {
  if (portfolio.greeks_total > 0) {
    return `${portfolio.greeks_coverage || 0} of ${portfolio.greeks_total}`;
  }
  if ((positions.options || []).length === 0) {
    return "No options";
  }
  return "--";
}

function greeksMeaning(portfolio, positions) {
  const total = portfolio.greeks_total || 0;
  const covered = portfolio.greeks_coverage || 0;
  if (total > 0 && covered >= total) {
    return "All option legs have model Greeks for risk totals.";
  }
  if (total > 0) {
    return "Some option legs are missing model Greeks; totals are partial.";
  }
  if ((positions.options || []).length === 0) {
    return "No option legs need model Greeks in this snapshot.";
  }
  return "Model Greeks unavailable for this option snapshot.";
}

export { countdownLabel, currentMarketCalendar, gatewayIssueText, greeksCoverage, greeksMeaning, marketSessionLabel, marketSessionNow, marketStatusPhrase, refreshSelectedMarketCalendar, renderSourceBanners, renderSyncStrip, renderTopbar, setBanner, setupMarketSelect, snapshotIssueSummary, snapshotSourceLabel };
