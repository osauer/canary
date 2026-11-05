# MCP Bundle release tool

This private npm project locks `@anthropic-ai/mcpb` at 2.1.2 and records its
transitive dependencies and integrity hashes in `package-lock.json`. It is a
release build tool, separate from the product and browser test dependencies.

`scripts/build-mcpb.sh` resolves this directory relative to itself, runs
`npm ci --include=dev --ignore-scripts --no-audit --no-fund --dry-run=false`, and invokes only
this installation's `node_modules/.bin/mcpb`. The release tag checkout therefore
owns both the script and its tooling lock, even when a newer checkout controls
release recovery. A failed install or missing binary stops assembly; there is no
global, npx or environment-selected package fallback. Ambient npm dry-run
configuration cannot leave a previous installed binary in use.

The packager retains validation, packing, optional signing and signature
verification, bundle information, unpacking, wrapper-version checks, and full
LICENSE preservation. Lifecycle scripts are disabled: unsigned packaging and
the sign/verify commands run without install scripts. Optional signing still uses
`MCPB_SIGN_CERT`, `MCPB_SIGN_KEY` and `MCPB_SIGN_INTERMEDIATE`.

The normal `make check` runs the offline fixture test through
`release-packaging-check`. To exercise a fresh locked install, unsigned packaging
and rejection of an untrusted signature with synthetic native binaries and a
temporary signing key:

```sh
bash scripts/build-mcpb_test.sh --real
```

That smoke downloads locked npm packages into a disposable fixture checkout;
it never builds or installs Canary, uses release signing credentials, publishes
artifacts, or contacts the broker. It does not prove trusted release signing.
Updating the tool is a deliberate edit to
this manifest followed by npm lockfile generation and both test modes.

The `tmp` override patches GHSA-ph9p-34f9-6g65 and GHSA-52f5-9888-hmc6. MCPB
2.1.2 reaches `tmp` only through `external-editor`, which asks for `^0.0.33` and
has no patched release in that range. Drop the override when an MCPB update stops
depending on `external-editor`.
