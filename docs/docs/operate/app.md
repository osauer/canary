# Mobile App

Updated: 2026-07-21 01:15 CEST

The mobile app layer is served by `ibkr app`. It is a HyperServe process that
serves the PWA, owns pairing, streams `/api/events`, and sends opt-in canary
push notifications.

## Local Run

```sh
ibkr app
```

In another shell:

```sh
ibkr app pair
```

Scan the QR code or open the printed pairing URL. The URL contains a short-lived
pairing id plus nonce, not a durable secret. After the first successful pair,
the browser keeps a device credential and the app stores a matching device grant
under the app state directory. Restarting `ibkr app` clears only the in-memory
session cookie; the paired device should silently mint a fresh session from that
device grant.

The default app bind is LAN-capable (`0.0.0.0:8765`), so the same app process
can serve a local browser preview at `http://127.0.0.1:8765` and a phone at the
Mac's LAN URL. For a local preview, override only the pairing URL:

```sh
ibkr app pair --public-url http://127.0.0.1:8765 --json
```

Use plain `ibkr app pair` for the phone so the running app host's configured
public URL is used. Do not run the shared app host with `--addr 127.0.0.1:8765`
when a phone needs to pair too.

## Remote Relay

Remote mode keeps the normal app host local, then opens an outbound connector to
`https://remote.osauer.dev`:

```sh
ibkr app --remote
```

In another shell:

```sh
ibkr app pair
```

The printed pairing URL uses the relay origin and includes a `remote=` route id.
The route id and connector token are persisted locally and resumed across app
restarts, new builds, and relay-side route expiry: a token-matched resume
revives the route at the relay, so the route id (and with it every paired
phone) survives arbitrary Mac downtime as long as the app state directory
keeps `state.json`. Registration happens inside the connector loop with
backoff, so a relay or DNS outage at startup degrades the relay instead of
killing `ibkr app --remote`. A new route id is minted only when the relay
definitively rejects the held credentials, and the app logs that paired
devices must re-pair.

Phone-side addressing is double-tracked: the relay sets the
`ibkr_remote_route` cookie with a 400-day Max-Age and refreshes it on every
visit, and the SPA mirrors the route id in `localStorage`. A navigation that
arrives without the cookie gets a recovery page that rebuilds it from the
mirror; a navigation while the Mac connector is down gets an auto-retrying
wait page instead of raw JSON. The paired device credential itself re-mints
sessions silently after every app restart, retries transient failures at
page load, and only shows the "scan a fresh QR code" instruction when the
app definitively rejected the device.

Session continuity does not depend on script-visible storage: pairing also
sets a long-lived HttpOnly `ibkr_app_device` cookie whose hash is stored on
the device grant, and the app mints fresh sessions from it. This is what
keeps an iOS Home Screen install alive; its container inherits Safari's
cookies but not Safari's localStorage/IndexedDB, so a key-based re-login can
never run there. Key/secret logins re-provision the cookie, and grants keep
a capped list of valid cookie hashes so Safari and the installed app (twin
copies of the same cookie jar) never invalidate each other. Pair in Safari
first, then Add to Home Screen, so the install snapshot carries the cookie.
Every auth outcome is logged as `ibkr app auth:` lines in
`~/Library/Logs/ibkr/app.err.log`.

For an unsupervised app host, an app-only restart can switch remote mode on:

```sh
ibkr restart --app --remote
```

Do not pass that override to a loaded LaunchAgent: supervised restart rejects
runtime flag overrides because the loaded plist is authoritative. Install a
new supervised host in remote mode with:

```sh
ibkr setup app --remote
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.osauer.ibkr-app.plist
```

To change an already-loaded LaunchAgent, run `ibkr setup app --remote`, unload
the existing `gui/$(id -u)/com.osauer.ibkr-app` job, and bootstrap the rewritten
plist again. Ordinary later restarts use `ibkr restart --app` with no `--remote`
override; launchd preserves the plist configuration.

Remote mode needs the Mac, TWS/Gateway, `ibkr app --remote`, and the Cloudflare
Worker route to stay up. The app exposes relay connection state in
`/api/bootstrap` under `relay`.

## Restart

```sh
ibkr restart
```

On the default daemon socket, this restarts a running app first and only then
restarts the shared daemon. When the
`com.osauer.ibkr-app` LaunchAgent is loaded, the app is restarted through
`launchctl kickstart -k`: any unsupervised (orphaned) app process is stopped
first so launchd can own the app again, and app flag overrides are rejected
with a pointer to rewrite and reload `ibkr setup app` because the plist
arguments win. The loaded job's executable must resolve to the current
installed `ibkr` binary. launchd may throttle the respawn for about ten
seconds; the command waits for a stable supervised PID. Without a loaded LaunchAgent, the unsupervised behavior
applies: SIGTERM the running app, preserve its flags such as `--addr`,
`--public-url`, `--remote`, and `--state-dir`, and start a detached
replacement. If no app is running, plain `ibkr restart` leaves the app
stopped.

The ordered app and daemon stages are intentionally non-atomic. If the app
cannot be restarted and verified, the daemon is not touched. If the later
daemon restart fails, the successful app restart is not rolled back; correct
the reported daemon failure and rerun `ibkr restart`.

Use `ibkr restart --app` for app-only restart/start workflows, including cases
where no app is running yet.

`ibkr app restart` is an alias for `ibkr restart --app` for the app-focused
workflow.

To switch an old local-only app host back to the shared local-preview plus
phone mode:

```sh
ibkr restart --app --addr 0.0.0.0:8765
```

When `--addr` is overridden without `--public-url`, restart clears any preserved
app `--public-url` flag so the app can derive the Mac's LAN URL again.

For SPA development, use the refresh target so embedded assets and the running
app host stay in sync:

```sh
make app-refresh
```

## Browser Smoke

To avoid QR-driven manual testing for every frontend issue, run a browser smoke
against an already-running app:

```sh
make app-smoke
```

Override the target when the app is on a different address:

```sh
make app-smoke APP_SMOKE_URL=http://127.0.0.1:18765
```

To test app restart recovery without touching the default app state:

```sh
make app-lifecycle-smoke
```

The smoke script:

- creates a pairing session through `/api/pairing/sessions`;
- opens the pairing URL in Playwright;
- optionally removes `globalThis.Notification` before page load;
- requires a real `/api/events` SSE snapshot;
- waits for the dashboard;
- clicks the `Snapshot` debug tool;
- fails on browser page errors or console errors.

The lifecycle smoke starts an isolated app on `127.0.0.1:18765`, pairs without
QR, disables WebCrypto to exercise the local HTTP fallback credential, runs
`ibkr restart --app`, and verifies that the same browser context reauthenticates
and returns to `Live` with an SSE subscriber. A stale or consumed pairing URL is
also recoverable after pairing: the SPA strips the one-time pairing query and
falls through to the saved device credential before asking for a new QR code.

The script first tries the local Node `playwright` package, then the Codex
bundled runtime when available. If Playwright's managed Chromium browser is not
installed, it falls back to installed Chrome. Outside Codex, install Playwright
for the local Node environment before running the smoke.

The Go test gate also includes static compatibility tests for browser globals in
`web/app/app_compat_test.go`, so direct unguarded `Notification` references fail
under `go test ./...`.

For the full SPA development and debugging playbook, including P/L semantics and
Browser plugin caveats, see [Canary SPA Development](../../guides/canary-spa-dev.md).
