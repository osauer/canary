import { sign } from "./auth.js";
import { state } from "./state.js";

const $ = (id) => document.getElementById(id);

function currentSettings() {
  return state.settings || state.snapshot?.settings || {};
}

function purgeRestoreSettingEnabled() {
  // This setting controls the workflow/read surface only. It is never broker-write
  // authority; the daemon keeps purge/restore preview and submission disabled.
  const setting = currentSettings().features?.purge_restore?.enabled;
  return setting?.value !== false;
}

function stockProtectionSettingEnabled() {
  const setting = currentSettings().features?.stock_protection?.enabled;
  return setting?.value !== false;
}

function quoteTimestamp(quote) {
  return quote?.quote_price_at || quote?.price_at || quote?.as_of || "";
}

function normalizeSymbol(value) {
  return String(value || "").trim().toUpperCase();
}

function normalizeCurrency(value) {
  return String(value || "").trim().toUpperCase();
}

function mergeCurrency(left, right) {
  const a = normalizeCurrency(left);
  const b = normalizeCurrency(right);
  if (!a) return b;
  if (!b || a === b) return a;
  return "MIX";
}

// AccountResult keeps legacy scalar keys for wire compatibility, so a bare 0
// cannot say whether the broker reported zero or never sent the field. The
// daemon-authored authority envelope is the only presence signal the app uses.
function accountAuthority(account = {}) {
  const authority = account?.authority;
  return authority && typeof authority === "object" ? authority : null;
}

function accountFieldAvailable(account = {}, field = "") {
  const authority = accountAuthority(account);
  if (!authority || authority.availability !== "available" || !authority.fields) return false;
  return authority.fields[field] === true;
}

function accountFieldValue(account = {}, field = "") {
  const value = account?.[field];
  return accountFieldAvailable(account, field) && hasNumericValue(value) ? value : null;
}

function accountBaseCurrency(account = {}) {
  if (!accountFieldAvailable(account, "base_currency")) return "";
  return normalizeCurrency(account.base_currency);
}

function displayMoney(value, currency) {
  return money(value, currency);
}

function signedDisplayMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const sign = value > 0 ? "+" : value < 0 ? "-" : "";
  return sign + displayMoney(Math.abs(value), currency);
}

function protectionWriteUnavailableReason(trading = {}) {
  const blocker = (trading.write_blockers || trading.blockers || [])[0];
  if (blocker?.code || blocker?.message) {
    return `${blocker.code || "write_blocked"}: ${blocker.message || "broker writes are not enabled"}`;
  }
  if (trading.mode === "paper") return "Paper preview is enabled, but broker writes are not enabled for this build/session";
  return "Broker writes are not enabled by trading.status";
}

function shortPreviewTokenID(tokenID = "") {
  const value = String(tokenID || "");
  return value.length > 18 ? `${value.slice(0, 18)}...` : value;
}

function shortPreviewMessage(message = "") {
  const value = String(message || "").replace(/\s+/g, " ").trim();
  return value.length > 80 ? `${value.slice(0, 77)}...` : value;
}

function protectionWriteConfirmation(proposal = {}) {
  const trading = state.snapshot?.trading || {};
  if (!trading.mode || !trading.account) return null;
  return { account: trading.account, mode: trading.mode };
}

function protectionWriteConfirmationLabel() {
  const trading = state.snapshot?.trading || {};
  return [trading.mode, trading.account].filter(Boolean).join("/") || "broker account";
}


// Blockers arrive as machine {code, message}. Users read the sentence; the
// code stays as a greppable suffix instead of leading the line.
function blockerText(blocker) {
  if (!blocker) return "";
  const msg = String(blocker.message || "").trim() || labelize(String(blocker.code || ""));
  const human = msg.charAt(0).toUpperCase() + msg.slice(1);
  return blocker.code ? `${human} (${blocker.code})` : human;
}

function readJSONOrText(res) {
  return res.text().then((text) => {
    if (!text) return {};
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  });
}


// money never invents a currency: an unknown/mixed currency renders the
// bare amount (optionally suffixed with a non-ISO label such as MIX) so a
// EUR-base account can never see a $ label on an unlabeled number.
function money(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const ccy = normalizeCurrency(currency);
  if (/^[A-Z]{3}$/.test(ccy) && ccy !== "MIX") {
    return new Intl.NumberFormat(undefined, { style: "currency", currency: ccy }).format(value);
  }
  const amount = new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
  return ccy ? `${amount} ${ccy}` : amount;
}


// signedMoneyRead formats a P&L amount with an explicit leading +/- so the
// sign is legible without relying on color (NO_COLOR, color-blindness),
// mirroring the CLI proposal renderer.
function signedMoneyRead(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const formatted = money(Math.abs(value), currency);
  if (value > 0) return "+" + formatted;
  if (value < 0) return "-" + formatted;
  return formatted;
}

function compactMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const ccy = normalizeCurrency(currency);
  const prefix = ccy ? `${ccy} ` : "";
  const abs = Math.abs(value);
  if (abs >= 1000000) {
    return `${prefix}${(value / 1000000).toFixed(abs >= 10000000 ? 1 : 2)}m`;
  }
  if (abs >= 100000) {
    return `${prefix}${(value / 1000).toFixed(0)}k`;
  }
  if (abs >= 10000) {
    return `${prefix}${(value / 1000).toFixed(1)}k`;
  }
  return money(value, ccy);
}

function compactWholeMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const ccy = normalizeCurrency(currency);
  const compact = Math.abs(value) >= 1000;
  const amountOptions = { minimumFractionDigits: 0, maximumFractionDigits: 0 };
  if (compact) amountOptions.notation = "compact";
  if (/^[A-Z]{3}$/.test(ccy) && ccy !== "MIX") {
    return new Intl.NumberFormat(undefined, {
      ...amountOptions,
      style: "currency",
      currency: ccy,
    }).format(value);
  }
  const amount = new Intl.NumberFormat(undefined, amountOptions).format(value);
  return ccy ? `${amount} ${ccy}` : amount;
}


// "risk" (red) is reserved for a breached threshold; "alert" (amber) marks
// actionable-but-not-breached metrics so red keeps its scarcity value.
function setMetricTone(el, tone = "neutral") {
  if (!el) return;
  el.classList.remove("metric-risk", "metric-alert", "metric-neutral");
  el.classList.add(tone === "risk" ? "metric-risk" : tone === "alert" ? "metric-alert" : "metric-neutral");
}

function renderSensitiveSignedMoney(id, value, currency) {
  const el = $(id);
  if (!hasNumericValue(value)) {
    el.className = "signed";
    el.textContent = "--";
    return;
  }
  if (sensitiveMoneyHidden(value)) {
    el.className = "signed is-private";
    el.textContent = privacyMask();
    return;
  }
  el.className = signedClass(value);
  el.textContent = money(value, currency);
}

function renderSensitiveText(id, value, hasValue) {
  const el = $(id);
  if (!hasValue) {
    el.classList.remove("is-private");
    el.textContent = "--";
    return;
  }
  if (!state.accountValueVisible) {
    el.classList.add("is-private");
    el.textContent = privacyMask();
    return;
  }
  el.classList.remove("is-private");
  el.textContent = value;
}

// The account id is the one sensitive string the eye toggle also masks. Unlike
// money values (fully hidden as "******"), an id keeps its first and last two
// characters — the operator still recognizes which account is shown, without
// the full identifier sitting on screen or in a screenshot.
function maskAccountId(id) {
  const value = String(id || "").trim();
  if (!value) return "";
  if (value.length <= 3) return `${value[0]}•••••`;
  return `${value[0]}•••••${value.slice(-2)}`;
}

// renderSensitiveAccountId mirrors renderSensitiveText, but hides the account id
// with the id-preserving mask instead of the full money mask. A placeholder
// (e.g. "Account unresolved") is not a sensitive id and renders as-is.
function renderSensitiveAccountId(elementId, accountId, placeholder = "--") {
  const el = $(elementId);
  if (!el) return;
  const id = String(accountId || "").trim();
  if (!id) {
    el.classList.remove("is-private");
    el.textContent = placeholder || "--";
    return;
  }
  if (!state.accountValueVisible) {
    el.classList.add("is-private");
    el.textContent = maskAccountId(id);
    return;
  }
  el.classList.remove("is-private");
  el.textContent = id;
}

function sensitiveMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? money(value, currency) : privacyMask();
}

function sensitiveDisplayMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  return state.accountValueVisible ? displayMoney(value, currency) : privacyMask();
}

function sensitiveMoneyHidden(value) {
  return hasNumericValue(value) && !state.accountValueVisible;
}

function privacyMask() {
  return "******";
}

function hasNumericValue(value) {
  return Number.isFinite(value);
}

function firstNumber(...values) {
  return values.find((value) => typeof value === "number");
}

function signedClass(value) {
  if (!hasNumericValue(value) || value === 0) return "signed";
  return "signed " + (value > 0 ? "ok" : "risk");
}

function signedTone(value, inverse = false) {
  if (!hasNumericValue(value) || value === 0) return "neutral";
  const good = inverse ? value > 0 : value >= 0;
  return good ? "ok" : "risk";
}

function numberRead(value) {
  if (!hasNumericValue(value)) return "--";
  return new Intl.NumberFormat(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 }).format(value);
}

function riskMoney(value, currency) {
  if (!hasNumericValue(value)) return "--";
  const amount = new Intl.NumberFormat(undefined, {
    maximumFractionDigits: 0,
    minimumFractionDigits: 0,
  }).format(value);
  const ccy = normalizeCurrency(currency);
  return ccy ? `${amount} ${ccy}` : amount;
}

function pct(value) {
  if (!hasNumericValue(value)) return "--";
  return value.toFixed(1) + "%";
}

function wholePct(value) {
  if (!hasNumericValue(value)) return "--";
  return Math.round(value) + "%";
}

function signedPct(value) {
  if (!hasNumericValue(value)) return "--";
  const sign = value > 0 ? "+" : "";
  return sign + value.toFixed(2) + "%";
}

function cleanDetail(value) {
  if (!value) return "--";
  return String(value).replaceAll("_", " ");
}

function labelize(value) {
  const words = cleanDetail(value).split(/\s+/).filter(Boolean);
  if (words.length === 0) return "--";
  return words.map((word) => word.charAt(0).toUpperCase() + word.slice(1)).join(" ");
}

function ageLabel(minutes) {
  if (minutes < 1) return "now";
  if (minutes < 60) return `${minutes}m old`;
  const hours = Math.floor(minutes / 60);
  const rest = minutes % 60;
  return rest === 0 ? `${hours}h old` : `${hours}h ${rest}m old`;
}

function renderFreshnessTimestamp(target, value, options = {}) {
  const el = typeof target === "string" ? $(target) : target;
  if (!el) return;
  // Markup may pin a static explanatory title (e.g. "Market regime
  // freshness"); keep it as a prefix instead of clobbering it.
  if (el.dataset.freshnessLabel === undefined) {
    el.dataset.freshnessLabel = el.title || "";
  }
  const label = el.dataset.freshnessLabel;
  const at = value instanceof Date ? value : parseDate(value);
  if (!at) {
    // A missing timestamp is a degraded state, so it stays visible even for
    // quiet-when-fresh callers.
    el.hidden = false;
    el.textContent = options.fallback || "no timestamp";
    el.title = label;
    el.classList.add("stale");
    return;
  }
  const ageMS = Date.now() - at.getTime();
  const ageMinutes = Math.max(0, Math.floor(ageMS / 60000));
  const staleMinutes = typeof options.staleMinutes === "number" ? options.staleMinutes : 15;
  const stale = ageMinutes >= staleMinutes;
  const absolute = shortTime(at.toISOString());
  // Monitor panel heads run quiet: freshness is the expected state, so only
  // staleness earns ink. The footer sync strip stays the one always-on clock.
  if (options.quietWhenFresh) {
    el.hidden = !stale;
    if (!stale) {
      el.textContent = "";
      el.title = label ? `${label} · ${absolute}` : absolute;
      el.classList.remove("stale");
      return;
    }
  }
  if (ageMinutes < 1) {
    // Fresh: "HH:MM · now" restates itself; keep the clock time in the title.
    el.textContent = options.compact ? "now" : "updated now";
  } else {
    el.textContent = options.compact
      ? `${absolute} · ${ageLabel(ageMinutes)}`
      : `${stale ? "stale" : "updated"} ${absolute} · ${ageLabel(ageMinutes)}`;
  }
  el.title = label ? `${label} · ${absolute}` : absolute;
  el.classList.toggle("stale", stale);
}

function parseDate(value) {
  if (!value) return null;
  const at = new Date(value);
  return Number.isNaN(at.getTime()) ? null : at;
}

function shortTime(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" });
}

function shortTimeWithZone(value) {
  if (!value) return "--";
  return new Date(value).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
    timeZoneName: "short",
  });
}

export { $, accountAuthority, accountBaseCurrency, accountFieldAvailable, accountFieldValue, ageLabel, blockerText, cleanDetail, compactMoney, compactWholeMoney, currentSettings, displayMoney, firstNumber, hasNumericValue, labelize, maskAccountId, mergeCurrency, money, normalizeCurrency, normalizeSymbol, numberRead, parseDate, pct, privacyMask, protectionWriteConfirmation, protectionWriteConfirmationLabel, protectionWriteUnavailableReason, purgeRestoreSettingEnabled, quoteTimestamp, readJSONOrText, renderFreshnessTimestamp, renderSensitiveAccountId, renderSensitiveSignedMoney, renderSensitiveText, riskMoney, sensitiveDisplayMoney, sensitiveMoney, sensitiveMoneyHidden, setMetricTone, shortPreviewMessage, shortPreviewTokenID, shortTime, shortTimeWithZone, signedClass, signedDisplayMoney, signedMoneyRead, signedPct, signedTone, stockProtectionSettingEnabled, wholePct };
