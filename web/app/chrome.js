import { renderAll } from "./app.js";
import { handleAttentionContextChange } from "./alert-inbox.js";
import { renderRulesCard, renderStressDetail, renderRegimePanel } from "./stress.js";
import { renderOpportunitiesPanel } from "./opportunities.js";
import { setPortfolioExpansion } from "./portfolio.js";
import { renderProtectionPanel } from "./protection.js";
import { $ } from "./shared.js";
import { normalizedTab, state } from "./state.js";
import { renderAccountPanel, setUnderlyingExpansion } from "./underlyings.js";

function setupBottomTabs() {
  const tabs = $("bottomTabs");
  if (!tabs) return;
  if (tabs.dataset.bound !== "true") {
    const activate = (event) => {
      const button = event.target.closest("[data-tab]");
      if (!button || !tabs.contains(button)) return;
      if (event.type === "pointerup" && event.pointerType === "mouse") return;
      if (event.type === "click" && Date.now() - Number(tabs.dataset.lastPointerActivation || 0) < 600) return;
      if (event.type === "pointerup") {
        tabs.dataset.lastPointerActivation = String(Date.now());
        event.preventDefault();
      }
      if (button.disabled || button.getAttribute("aria-disabled") === "true") {
        setActiveTab("monitor");
        return;
      }
      setActiveTab(button.dataset.tab || "monitor");
    };
    tabs.addEventListener("click", activate);
    tabs.addEventListener("pointerup", activate);
    tabs.dataset.bound = "true";
  }
  setActiveTab(state.activeTab, { persist: false });
}

function setActiveTab(tab, options = {}) {
  state.activeTab = normalizedTab(tab);
  if (options.persist !== false) {
    localStorage.setItem("canaryActiveTab", state.activeTab);
  }
  renderTabs();
  handleAttentionContextChange();
}

function renderTabs() {
  const active = normalizedTab(state.activeTab);
  if (active !== state.activeTab) {
    state.activeTab = active;
    localStorage.setItem("canaryActiveTab", active);
  }
  for (const panel of document.querySelectorAll("[data-tab-panel]")) {
    panel.hidden = panel.dataset.tabPanel !== active;
  }
  const accountPanel = $("accountPanel");
  if (accountPanel) accountPanel.hidden = active === "settings";
  for (const button of document.querySelectorAll("[data-tab]")) {
    const selected = button.dataset.tab === active;
    button.classList.toggle("active", selected);
    button.setAttribute("aria-selected", String(selected));
  }
}

function setAccountValueVisible(visible) {
  state.accountValueVisible = Boolean(visible);
  localStorage.setItem("canaryAccountValueVisible", String(state.accountValueVisible));
  renderAll();
}

function syncAccountPrivacyState() {
  document.body.dataset.accountValues = state.accountValueVisible ? "visible" : "hidden";
}

function resetViewportScroll() {
  // The Panel Dark shell scrolls inside #appScroll, not the window or the
  // .shell wrapper — resetting only the latter two leaves the real container
  // where it was.
  for (const el of [document.getElementById("appScroll"), document.querySelector(".shell")]) {
    if (el && (el.scrollTop !== 0 || el.scrollLeft !== 0)) {
      el.scrollTo(0, 0);
    }
  }
  if (window.scrollX !== 0 || window.scrollY !== 0) {
    window.scrollTo(0, 0);
  }
}

function ensureRegimeStressExpansion(stress = {}) {
  if (state.detailPreferenceSet || state.regimeStressExpansionInitialized) return;
  state.stressDetailOpen = false;
  state.regimeDetailOpen = false;
  state.regimeStressExpansionInitialized = true;
}


// Regime and stress detail can now open independently (or together) — both
// live inside one shared deck below the split, so opening one no longer
// changes the other's position on the page. See docs/design note in the
// merged-panel spec: the mutual-exclusion this used to enforce existed to
// stop two independently-tall sibling panels from fighting for vertical
// rhythm, and that premise no longer holds once they share one deck.
function setRegimeStressExpansion(which, open) {
  state.detailPreferenceSet = true;
  if (which === "regime") {
    state.regimeDetailOpen = open;
  } else {
    state.stressDetailOpen = open;
  }
  renderRegimePanel(state.snapshot || {});
  renderStressDetail(state.snapshot?.stress || {});
}

function panelTapIgnored(target) {
  return Boolean(target?.closest?.([
    "button",
    "a",
    "input",
    "select",
    "textarea",
    "label",
    "summary",
    ".detail-panel",
    ".regime-detail-panel",
    ".underlying-book__list-panel",
    ".underlying-bulk-actions",
    ".underlying-action-result",
    ".account-overview-detail",
    ".portfolio-detail-panel",
    ".alert-focus",
  ].join(",")));
}

function handleExpandablePanelTap(event, which) {
  if (panelTapIgnored(event.target)) return;
  const open = which === "regime" ? !state.regimeDetailOpen : !state.stressDetailOpen;
  setRegimeStressExpansion(which, open);
}

function handleUnderlyingPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setUnderlyingExpansion(!state.underlyingDetailOpen);
}

function handlePortfolioPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setPortfolioExpansion(!state.portfolioDetailOpen);
}

function handleProtectionPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setProtectionExpansion(!state.protectionOpen);
}

function handleOpportunitiesPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setOpportunitiesExpansion(!state.opportunitiesOpen);
}

function handleAccountPanelTap(event) {
  if (panelTapIgnored(event.target)) return;
  setAccountOverviewExpansion(!state.accountOverviewOpen);
}

function setAccountOverviewExpansion(open) {
  state.accountOverviewOpen = Boolean(open);
  renderAccountPanel(state.snapshot?.account || {}, state.snapshot?.positions || {}, state.snapshot?.stress || {});
}

function setProtectionExpansion(open) {
  state.protectionOpen = Boolean(open);
  renderProtectionPanel(state.snapshot?.proposals || {}, state.snapshot?.auto_trade || {});
}

function setOpportunitiesExpansion(open) {
  state.opportunitiesOpen = Boolean(open);
  renderOpportunitiesPanel(state.snapshot?.opportunities || {});
}


// One sheet primitive for the whole panel: a full-height <dialog> in the
// Panel Dark register. The native modal is the right underlying element —
// it brings focus containment, Escape, the backdrop, and a top layer that
// no tab panel can hide underneath — so a sheet is a styled .app-dialog,
// not a second overlay mechanism.
//
// Opening a sheet also opens the depth surface it seats: the panels below
// already gate their rendering (and, for proposals, their snapshot refresh)
// on those expansion flags, so the sheet's own open state drives them
// rather than a parallel flag that could drift.
function sheetElement(id) {
  const sheet = $(id);
  return sheet && typeof sheet.showModal === "function" ? sheet : null;
}

function setSheetOpen(id, open) {
  const sheet = sheetElement(id);
  if (!sheet) return;
  if (open && !sheet.open) sheet.showModal();
  else if (!open && sheet.open) sheet.close();
}

function setProtectionSheetOpen(open) {
  setProtectionExpansion(open);
  setSheetOpen("protectionSheet", open);
}

function setRulesSheetOpen(open) {
  state.rulesDetailOpen = Boolean(open);
  renderRulesCard(state.snapshot?.rules);
  setSheetOpen("rulesSheet", open);
}

function setUnderlyingsSheetOpen(open) {
  setUnderlyingExpansion(open);
  setSheetOpen("underlyingsSheet", open);
}

export { ensureRegimeStressExpansion, handleAccountPanelTap, handleExpandablePanelTap, handleOpportunitiesPanelTap, handlePortfolioPanelTap, handleProtectionPanelTap, handleUnderlyingPanelTap, panelTapIgnored, renderTabs, resetViewportScroll, setAccountOverviewExpansion, setAccountValueVisible, setActiveTab, setOpportunitiesExpansion, setProtectionExpansion, setProtectionSheetOpen, setRegimeStressExpansion, setRulesSheetOpen, setSheetOpen, setUnderlyingsSheetOpen, setupBottomTabs, sheetElement, syncAccountPrivacyState };
