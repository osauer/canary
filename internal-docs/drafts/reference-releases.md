# Releases and support

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** Someone deciding whether to trust a download, or working out
which version they are on and whether it still gets fixes.

**Questions the page has to answer**

- What a release contains: binaries per platform, the Desktop bundle, the
  signed checksums file, the registry entry.
- How to verify a download, with the exact commands.
- The version scheme, and what a minor and a patch release each imply.
- Which versions are supported, and what support means for a single-maintainer
  project. Honest scope, no invented service levels.
- How to report a security issue, linking to the repository policy.
- Where the changelog lives.

**Draw from**

- `SECURITY.md` and `CHANGELOG.md`.
- `scripts/build-release-artifacts.sh` and `scripts/release-verify.sh` for what
  is actually produced and checked.
- `docs/docs/start/updating.md`, which covers the update mechanics. This page covers
  trust and lifecycle instead, and links there.

**Boundaries to keep**

- Do not describe the release process itself. That is maintainer material and
  stays in the repository.
