# The paired app

Updated: 2026-07-25 20:23 CEST

Run `canary app` on the Mac to get Canary on your phone. That single process serves
the progressive web app, handles pairing, streams live updates to paired devices
over `/api/events`, and sends opt-in push notifications.

## Pair a phone

```sh
canary app
```

In another shell:

```sh
canary app pair
```

Scan the QR code or open the printed pairing URL. The URL contains a short-lived
pairing id plus nonce, not a durable secret. After the first successful pair,
the browser keeps a device credential and the app stores a matching device grant
under the app state directory.

On iOS, pair in Safari first and then Add to Home Screen, so the install
snapshot carries the pairing cookie. The installed app's container inherits
Safari's cookies but not Safari's `localStorage` or `IndexedDB`, so a key-based
re-login can never run there.

The default app bind is LAN-capable (`0.0.0.0:8765`), so the same app process
can serve a local browser preview at `http://127.0.0.1:8765` and a phone at the
Mac's LAN URL. Use plain `canary app pair` for the phone, so the running app
host's configured public URL is used. For a local preview, override only the
pairing URL:

```sh
canary app pair --public-url http://127.0.0.1:8765 --json
```

Do not run the shared app host with `--addr 127.0.0.1:8765` when a phone needs
to pair too.

## Staying paired

Restarting `canary app` clears only the in-memory session cookie. The paired
device mints a fresh session from its device grant silently, retries transient
failures at page load, and shows the "scan a fresh QR code" instruction only
when the app definitively rejected the device. A stale or already-consumed
pairing URL is recoverable too: the app strips the one-time pairing query and
falls through to the saved device credential before asking for a new QR code.

Session continuity does not depend on script-visible storage. Pairing sets a
long-lived HttpOnly `ibkr_app_device` cookie whose hash is stored on the device
grant, and the app mints fresh sessions from it. Key/secret logins re-provision
the cookie, and grants keep a capped list of valid cookie hashes so Safari and
the installed app (twin copies of the same cookie jar) never invalidate each
other.

Every auth outcome is logged as `canary app auth:` lines in
`~/Library/Logs/ibkr/app.err.log`.

## Remote access

Remote mode keeps the normal app host local, then opens an outbound connector to
`https://remote.osauer.dev`:

```sh
canary app --remote
```

In another shell:

```sh
canary app pair
```

The printed pairing URL uses the relay origin and includes a `remote=` route id.

Remote mode needs the Mac, TWS/Gateway, `canary app --remote`, and the Cloudflare
Worker route to stay up. The app exposes relay connection state in
`/api/bootstrap` under `relay`.

### What survives a restart

The route id and connector token are persisted locally and resumed across app
restarts, new builds, and relay-side route expiry. A token-matched resume
revives the route at the relay, so the route id, and with it every paired phone,
survives arbitrary Mac downtime as long as the app state directory keeps
`state.json`. Registration happens inside the connector loop with backoff, so a
relay or DNS outage at startup degrades the relay instead of killing `canary app
--remote`.

A new route id is minted only when the relay definitively rejects the held
credentials, and the app logs that paired devices must re-pair.

### What the phone sees when something is wrong

Phone-side addressing is double-tracked. The relay sets the `ibkr_remote_route`
cookie with a 400-day Max-Age and refreshes it on every visit, and the app
mirrors the route id in `localStorage`. A navigation that arrives without the
cookie gets a recovery page that rebuilds it from the mirror. A navigation while
the Mac connector is down gets an auto-retrying wait page instead of raw JSON.

### Running remote mode under launchd

For an unsupervised app host, an app-only restart can switch remote mode on:

```sh
canary restart --app --remote
```

Do not pass that override to a loaded LaunchAgent: supervised restart rejects
runtime flag overrides because the loaded plist is authoritative. Install a
new supervised host in remote mode with:

```sh
canary setup app --remote
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.osauer.ibkr-app.plist
```

To change an already-loaded LaunchAgent, run `canary setup app --remote`, unload
the existing `gui/$(id -u)/com.osauer.ibkr-app` job, and bootstrap the rewritten
plist again. Ordinary later restarts use `canary restart --app` with no `--remote`
override; launchd preserves the plist configuration.

## Restart

```sh
canary restart
```

On the default daemon socket, this restarts a running app first and only then
restarts the shared daemon. If no app is running, plain `canary restart` leaves
the app stopped.

The two stages are ordered but not atomic. An app-stage failure leaves the
daemon untouched; a daemon-stage failure does not roll the already-restarted app
back. Fix the reported stage and rerun `canary restart`.
[Updating](../start/updating.md#restarting-local-processes-canary-restart) has the
flags, JSON output, and socket-scope rules.

When the `com.osauer.ibkr-app` LaunchAgent is loaded, the app is restarted
through `launchctl kickstart -k`. Any unsupervised (orphaned) app process is
stopped first so launchd can own the app again, and app flag overrides are
rejected with a pointer to rewrite and reload `canary setup app` because the plist
arguments win. The loaded job's executable must resolve to the current installed
`canary` binary. launchd may throttle the respawn for about ten seconds; the
command waits for a stable supervised PID.

Without a loaded LaunchAgent, the unsupervised behavior applies: SIGTERM the
running app, preserve its flags such as `--addr`, `--public-url`, `--remote`,
and `--state-dir`, and start a detached replacement.

Use `canary restart --app` for app-only restart/start workflows, including cases
where no app is running yet. `canary app restart` is an alias for the same thing.

To switch an old local-only app host back to the shared local-preview plus
phone mode:

```sh
canary restart --app --addr 0.0.0.0:8765
```

When `--addr` is overridden without `--public-url`, restart clears any preserved
app `--public-url` flag so the app can derive the Mac's LAN URL again.

## Developing the app

Building or debugging the app itself is contributor work. [Canary SPA
Development](../../../internal-docs/guides/canary-spa-dev.md) has the
development and debugging playbook, including P/L semantics and Browser plugin
caveats, and `make help` lists the app refresh and browser-smoke targets.
