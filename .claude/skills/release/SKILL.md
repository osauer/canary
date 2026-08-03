---
name: release
description: Cut a Canary release end-to-end with exactly one human stop. Autonomously preflights auth (verify-first Actions-OIDC), shared-tree state, version stamps, changelog, and gates; presents a findings-first GO/NO-GO; then fires and supervises `make release` and runs the full post-release verification (assets, tags, registry, fresh-clone build, live site stamps). Use when asked to cut, ship, prepare, or verify a release. Never tags, pushes, or creates GitHub releases directly; never force-pushes; never implements feature code in-release.
---

The procedure of record is `.agents/docs/release-procedure.md`. Read it in
full and follow it stage by stage; do not infer policy from summaries or
from this wrapper.
