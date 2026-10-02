# Build capacity on a live Desk host

Canary's Make build, check and test entrypoints inspect the repository, Go build
cache and temporary filesystems before compilation. They refuse below **8 GiB
available**, including when a separate cache volume is low. This is an admission
floor, not a guarantee that a long build fits. Keep **15–20 GiB available** before
parallel full gates, and avoid running duplicate full gates in multiple worktrees.
Direct `go` commands do not run this repository preflight.

Go's build cache trims entries unused for five days, normally scanning once a
day. It has no size ceiling: recent worktree and race-build artifacts can fill
the same volume that holds Canary's durable audit state. `make clean` removes
`bin/` and `dist/`; it does not clear this cache.

When space is low:

1. Coordinate all builders and let active Go builds stop. Leave Desk, Canary,
   TWS, audit files, policy and account state alone.
2. Inspect `go env GOCACHE` and `df -h`. If the disposable Go build cache is the
   cause, run **`go clean -cache`** deliberately with no competing builds. Do not
   clear the module cache: it contains downloaded dependencies and the toolchain.
3. Recheck capacity with `make build-space-check`, then resume one full gate on
   the exact frozen candidate. A cleared cache makes the next compile slower.

The preflight never deletes files, schedules cleanup or alters services. A
capacity refusal is not an audit-state corruption report; any runtime integrity
alarm still requires its own evidence-preserving recovery procedure.
