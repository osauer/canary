# Contributing

This is a personal open-source project maintained by one person. Issues,
bug reports, and pull requests are welcome; responses are best-effort.

## Before you write code

Open an issue first for anything beyond a small fix. The project has strong
opinions about risk semantics, and a change that looks like an improvement
from outside may cut against a deliberate design decision. Ten minutes of
discussion saves a rejected pull request.

The issue templates are at
[.github/ISSUE_TEMPLATE](.github/ISSUE_TEMPLATE). Security problems go through
[SECURITY.md](SECURITY.md) instead, never a public issue.

## Never include account data

Wire captures, daemon logs, `--json` output, and screenshots routinely contain
account IDs, balances, positions, and order references. Redact before pasting
anything into an issue, a pull request, or a test fixture. See
[SECURITY.md](SECURITY.md#diagnostic-data-sensitivity).

Real account data has reached this public repository before. Assume every file
you attach is permanent.

## The Contributor License Agreement

First-time contributors sign [CLA.md](CLA.md). A bot comments on your pull
request with instructions; you reply once and later pull requests are
recognized automatically.

**Read section 2 before you sign.** The project is MIT and stays MIT, but the
CLA grants the right to sublicense, which means your contribution may end up
in a paid closed-source product built around this code. That is the reason the
agreement exists, and it is stated in plain terms rather than buried in the
legal text. If you are not comfortable with it, say so on the issue — some
changes can be described rather than contributed.

Signatures are recorded as your GitHub username and a timestamp on the
`cla-signatures` branch. Nothing else is collected.

## Running the checks

```bash
make check
```

`make check` is the binding gate: formatting, vet, staticcheck, govulncheck,
modernization, plus parity checks that keep MCP tools, generated references,
and plugin metadata aligned with the CLI surface. It fails on stdlib
vulnerabilities, so an outdated Go toolchain is a build failure.

For anything touching Go or runtime behavior:

```bash
make test
```

`make test` includes `check` and adds unit and integration tests. Integration
tests under `test/integration/` connect to a live IB Gateway on
`127.0.0.1:4001` and skip cleanly when none is reachable, so they will not
hang on a machine with no gateway. Override the port with `IBKR_TEST_PORT`.

`make help` lists every target.

## What the project will not accept

- **Weakened trading guardrails.** Freeze, limits, the preview-only MCP order
  surface, the gated write path, and the never-a-false-pass rule in the
  rulebook are policy, not implementation detail. Changes there need a
  discussion about the policy first.
- **Default risk numbers in code.** The risk constitution has no embedded
  defaults on purpose. A missing decision stays `unapproved` rather than
  silently becoming a number somebody did not choose.
- **Stale data presented as fresh.** Sensors degrade to `unknown` and fail
  closed. A change that makes a degraded reading look clean will be rejected
  even if it is more convenient.
- **Windows support.** The daemon uses `setsid`, `flock`, and AF_UNIX sockets.
  WSL works; native Windows is out of scope.

## Style

Match the surrounding code. Go idioms are expected to be current, not the ones
from five years ago; `make check` enforces this through `go fix -diff` and
`go tool modernize`, and any output is a failure.

Commit messages follow conventional commits, matching the existing history:

```
fix(daemon): reconnect after a gateway handshake timeout
docs(site): correct the install prerequisites
```

User-visible changes need a `CHANGELOG.md` entry under the right Keep a
Changelog heading. Internal refactors do not.

## Pull requests

Keep them focused. One concern per pull request, with the tests that prove it.
A pull request that fixes a bug and reformats four files is two pull requests.

Say what you verified and what you did not. "Unit tests pass, no gateway
available so integration was skipped" is a useful sentence and an honest one.
