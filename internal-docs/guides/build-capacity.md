# Build capacity on a live Desk host

Updated: 2026-10-07 09:58 CEST

Canary's Make build, check and test entrypoints inspect the repository, Go build
cache and temporary filesystems before compilation. Below **8 GiB available**,
including when a separate cache volume is low, they trim the Go build cache
themselves and then proceed; they never refuse for space (owner instruction
2026-10-07 09:49 CEST). Keep **15–20 GiB available** before parallel full gates,
and avoid running duplicate full gates in multiple worktrees. Direct `go`
commands do not run this repository preflight.

Every Make recipe exports `GOFLAGS=-trimpath` (added to any `GOFLAGS` already
set). Builds are then path-independent, so worktrees at different paths share
build-cache entries instead of each adding its own copy. Before this, thirteen
worktrees grew the cache to 104 GB in less than a day. Release builds pass
`-trimpath` themselves.

How the trim works (`scripts/check-build-space.sh`):

- It deletes cache entries least recently used first: those untouched for more
  than 24 hours, then 12, 6 and 2 hours, and stops once every build filesystem
  is above 8 GiB.
- Go refreshes an entry's time at most once an hour while a build uses it, so
  the last step still keeps everything used in the last two hours. A build
  running in another worktree loses nothing it needs, and a trimmed entry is
  recompiled on its next use.
- Only a directory carrying Go's own cache README is touched, and only the
  entry files in its two-hex-digit subdirectories. Go's README and
  `trim.txt` stay, as does the module cache.
- If trimming cannot reach the floor, the shortfall is not the cache's. The gate
  proceeds with a warning naming the filesystem. A build that then fills the
  volume can make the daemon's state writes fail until space is freed.

When the warning appears:

1. Find what else fills the volume with `df -h` and `du`. Leave Desk, Canary,
   TWS, audit files, policy and account state alone.
2. Free that space deliberately, then rerun the gate.

An unreadable filesystem, or a cache path that is not absolute, still stops the
gate: that is a fault, not a space shortage. A trim message is not an
audit-state corruption report; any runtime integrity alarm still requires its
own evidence-preserving recovery procedure.
