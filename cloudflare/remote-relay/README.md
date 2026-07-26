# Canary Remote Relay Worker

Cloudflare Worker + Durable Object relay for `canary app --remote`.

The deployed Worker name `ibkr-remote-relay`, `/healthz` service value,
`RELAY_SESSION` binding, `RelaySession` class and migration, route cookie
`ibkr_remote_route`, and local-storage key `ibkrRemoteRoute` are continuity
pins for the existing Durable Object namespace and paired devices. Renaming
those identifiers requires a separately authorized state-transfer deployment;
this source rename deliberately leaves them unchanged.

## Deploy

```sh
cd cloudflare/remote-relay
npm install
npx wrangler login
npx wrangler deploy
```

Then in the Cloudflare dashboard:

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
npm test
```
