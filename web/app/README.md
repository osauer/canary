# ibkr Mobile App

This is the small PWA served by `ibkr app`. It is meant for the thing you want
on a phone: is the local IBKR setup alive, what does the account look like, and
is the portfolio canary asking for attention?

The Protection panel shows proposal-bound market-event chips when halt, LULD,
borrow, fee, or Reg SHO flags affect current protection proposals. Active halt
and active LULD chips are hard blockers; recent halt/LULD and borrow/Reg SHO/fee
chips are evidence tags. A reducing short `BUY` proposal is labeled `Buy to
cover`.

The Underlyings panel shows held-name market-event tags in the hero and row
tags for affected symbols, including long holdings where borrow pressure is
observational squeeze context. Stale or unknown sources stay visible with
source/as-of detail instead of disappearing.

Start the app host on the Mac that runs TWS or IB Gateway:

```sh
ibkr app
```

Pair a phone from another terminal:

```sh
ibkr app pair
```

Scan the QR code. The QR opens a short-lived pairing URL; it is not a permanent
secret. After pairing, the browser keeps its own device key and connects back to
the app for bootstrap data and live SSE updates. Restarting the app should not
require pairing again: the old session cookie is in-memory, but the browser can
mint a new session from its saved device key/secret.

The app host keeps one push-alert setting, `alert_settings.mode` — `none`,
`act_only`, or `watch_and_act` — changed from the PWA's Alerts tab (`PUT
/api/alerts/settings`) and stored with the paired-device state under
`IBKR_APP_STATE_DIR`.

Useful while developing or testing:

```sh
make app-check
make app-refresh
make app-refresh-smoke APP_SMOKE_BROWSER=webkit
ibkr restart --app --timeout 15s
ibkr app restart --timeout 15s
make app-smoke APP_SMOKE_BROWSER=webkit
make app-lifecycle-smoke APP_SMOKE_BROWSER=webkit
```

For source edits, prefer `make app-refresh` before Browser verification because
the SPA is embedded in the installed `ibkr` binary. The detailed development
playbook lives in
[`internal-docs/guides/canary-spa-dev.md`](../../internal-docs/guides/canary-spa-dev.md).

App icons are generated PNGs. The checked-in `icon-512.png` is the canonical
512×512 asset; regenerate it and the smaller PWA/favicon sizes with:

```sh
web/app/generate-icons.sh
```

If the original canary source sheet is available locally, recrop the canonical
512px asset and regenerate all derived sizes with:

```sh
IBKR_CANARY_ICON_SOURCE_SHEET=/path/to/source.png \
IBKR_CANARY_ICON_CROP=y,x,height,width \
web/app/generate-icons.sh
```

Trading workflows, HTTP MCP, debug diagnostics, and production relay hosting are
future work.

Design and architecture notes live in
[`internal-docs/design/mobile-app-mvp.md`](../../internal-docs/design/mobile-app-mvp.md).
