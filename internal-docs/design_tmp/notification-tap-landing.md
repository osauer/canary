# Notification tap landing

Status: specified 2026-07-19 20:55 CEST, not implemented. Companion feature to
the shipped risk-governance notifications (`risk-governance-nudges.md`).

## Goal

A notification tap should land the operator on the exact warning it announced,
not merely on the Alerts tab. One tap from the lock screen to the specific
canary alert or governance occurrence, expanded and highlighted, with unread
semantics untouched.

## Current state (v2.2.0-13 era)

- The service worker routes clicks through a frozen destination map
  (`monitor` → `/?tab=monitor`, `brief` → `/?tab=brief`, `alerts` →
  `/?tab=alerts`); every unknown destination degrades to `monitor`. Payload URLs
  are never followed (`service-worker.test.mjs` pins hostile destinations out of
  routes).

  *Amended 2026-07-20:* the `brief` destination was added when the daily brief
  moved to its own bottom tab. Process/governance nudges that previously landed
  on `monitor` (reconcile-due-soon, confirmed-flow, monthly-pulse — the
  watch-severity governance kinds) now land on `brief`, where the reconcile
  clock, monthly pulse, and confirmed-flow context live. Act-severity governance
  occurrences still land on `alerts`, so the focus-landing contract below is
  unchanged for them. The destination remains a strict enum coerced by
  `coerceDestination`; the frozen destination map in
  [`risk-governance-nudges.md`](risk-governance-nudges.md) is amended
  accordingly.
- Push payloads already carry `display_id` (governance, `gov-<16 hex>`) or
  `alert_id` (canary) — used only as the notification tag today.
- A tap never marks anything read; reads happen solely through the SPA's
  acknowledge flow, which requires an authenticated, visible, fully rendered
  Alerts view (`alerts.js` `attentionViewReady` + `unreadRefsAppear`).

## Contract

1. **Routing stays allowlisted.** No free-form URLs from payloads, ever. The
   click route becomes `/?tab=alerts&focus=<id>` only when the notification's
   own tag matches one of two strict shapes, pinned by identical regexes in
   the worker and its tests: `gov-[0-9a-f]{16}` or the canary alert id shape
   (`alert-`/`canary-` prefixed, bounded length, `[A-Za-z0-9_-]` charset —
   confirm the exact generator in `internal/app/alerts` before pinning).
   Anything else falls back to today's plain tab routes. Rationale: focus ids
   are already-public references; URLs must never carry account data or
   free text (history and logs retain them).
2. **SPA landing behavior.** On boot or in-app navigation with a `focus`
   query parameter: read it once, strip it via `history.replaceState`
   (refresh must not re-focus), activate the Alerts tab, then after the
   histories render scroll the matching item into view, expand it (the
   tap-to-expand affordance exists), and apply a transient highlight class
   that clears on its own. Focus targeting works for both canary history
   rows and governance occurrences.
3. **Missing item fails soft.** If the focused id is no longer retained
   (cleared, compacted, superseded), land on the Alerts tab with a quiet
   one-line status ("The alert from this notification is no longer
   retained."), never an error state and never a blank pane.
4. **Read semantics unchanged.** Landing and highlighting must not add any
   read path: the acknowledge flow stays exactly as shipped (visible +
   rendered + exact watermark + the attention dwell gate added 2026-07-19:
   a short continuous dwell or an in-view interaction, so a resume flash or
   pass-through never reads). A focus landing that renders the full view
   reads it the same way a manual Alerts open does — no more, no less. The
   worker still never calls the read mutation (`alert-unread.test.mjs` pins
   `/api/attention/read` out of the worker source).
5. **Future kinds plug in flat.** Drawdown-threshold, regime-change, and
   later warning kinds reuse the same two-field scheme (destination enum +
   focus id). No per-kind URL schemes, no payload growth beyond the existing
   canonicalized allowlist.

## Touch set

- `web/app/service-worker.js` — focus-aware route construction (allowlist
  regex, fallback), tag reuse as focus id.
- `web/app/lifecycle.js` / `shell.js` — query-parameter intake and strip on
  boot; tab activation ordering with the alerts render.
- `web/app/alerts.js` — focus resolution across canary history and
  governance occurrences, scroll/expand/highlight, missing-item status.
- `web/app/styles.css` — highlight class with reduced-motion respect.
- `internal/app/alerts` (only if the canary alert id shape needs a stable
  documented generator) — no payload schema change expected.
- Tests: `service-worker.test.mjs` (routing matrix incl. hostile ids),
  `alert-unread.test.mjs` (no new read paths), `governance-ui.test.mjs`
  (focus/highlight/missing-item), `scripts/app-browser-smoke.mjs` (deep-link
  render + cursor untouched, riding the existing attention-read guard).

## Acceptance

Offline: `make app-check`, `make test`. Live: `make app-smoke` with the
deep-link assertions. Physical: on the paired iPhone, tap a real notification
and see the exact item expanded and highlighted; a second tap on a stale
notification lands on the soft-fail copy. Unread cursor moves only per the
existing acknowledge contract in both cases.
