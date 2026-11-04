# Canary Remote Relay Worker

Cloudflare Worker + Durable Object relay for `canary app --remote`.

The deployed Worker name `ibkr-remote-relay`, `/healthz` service value,
`RELAY_SESSION` binding, `RelaySession` class and migration, route cookie
`ibkr_remote_route`, and local-storage key `ibkrRemoteRoute` are continuity
pins for the existing Durable Object namespace and paired devices. Renaming
those identifiers requires a separately authorized state-transfer deployment;
this source rename deliberately leaves them unchanged.

## Deploy

Deployment requires a separate explicit owner instruction. The repository-owned
helper uses Node.js 22 or later and its built-in HTTP, multipart and filesystem
APIs; no npm installation or Wrangler login is needed. `deploy.json` owns the
Worker identity, compatibility settings, binding and migration history.

Supply `CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` through the environment
from your secret manager. Scope the token to the intended account with Workers
Scripts Write permission (Workers Scripts Read suffices for `--check`). Do not
put credentials in source files or command-line arguments.

Check the existing deployment, then upload only after owner authorization:

```sh
cd cloudflare/remote-relay
node deploy.js --check
node deploy.js --deploy
```

The helper requires the existing Worker to have migration tag `v1`, the local
`RelaySession` namespace binding, and the declared compatibility date and flags.
It refuses drift instead of migrating or repairing it implicitly. Updates use
Cloudflare's [Replace Worker Script Content API](https://developers.cloudflare.com/api/resources/workers/subresources/scripts/subresources/content/methods/update/),
which preserves configuration and metadata, including secrets, extra bindings,
limits, logging settings and the existing Durable Object namespace. A successful
update checks that the settings are unchanged. Repeated deployments replace the
same code without replaying migrations or changing stored sessions. They may
create another Worker version; they are not version-history no-ops.

The helper never writes routes, domains, DNS or workers.dev settings. Existing
domain and route attachments continue to target `ibkr-remote-relay`. It neither
deletes nor recreates an existing Worker. Coordinate deployments with other
operators: the settings check can detect concurrent changes, but is not a lock.
HTTP errors, unexpected responses and redirects fail closed. Diagnostics show
HTTP status and numeric API error codes, never API response bodies or credentials.
Requests time out after 30 seconds and are never retried automatically; after a
failed upload or verification, inspect Cloudflare before deciding to retry.

This helper updates an existing relay only. A missing Worker is an error, never
permission to create or reset it. The retained `v1` migration history declares
`new_sqlite_classes: ["RelaySession"]`; deployments never resend that migration.
Initial provisioning and future migrations require a separate owner-authorized
procedure. Cloudflare's generic upload API is an upsert, so an absence check
followed by an upload cannot safely guarantee creation without replacement.

If the existing relay needs a first domain attachment, the dashboard steps are:

1. Open **Workers & Pages**.
2. Select `ibkr-remote-relay`.
3. Open **Settings** → **Domains & Routes**.
4. Add custom domain `remote.osauer.dev` in zone `osauer.dev`.
5. Wait for the certificate to become active.
6. Verify:

```sh
curl https://remote.osauer.dev/healthz
```

## Local Tests

```sh
node --test test/*.test.js
```

From the repository root, `make remote-relay-check` runs the same tests without
npm. Deployment tests use only a synthetic loopback HTTP server, including
multipart uploads, repeat deployment, migration drift, preservation of settings,
failed writes and credential redaction. They make no Cloudflare requests; live
deployment and continuity proof require a later authorized deployment.
