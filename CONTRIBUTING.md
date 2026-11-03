# Contributing

This is a personal open-source project maintained by one person. Issues,
bug reports, and pull requests are welcome; responses are best-effort.

## Before you write code

Open an issue first for anything beyond a small fix, which means a change to
documentation, help text or error wording. The project has strong
opinions about risk semantics, and a change that looks like an improvement
from outside may cut against a deliberate design decision. Ten minutes of
discussion saves a rejected pull request.

The issue templates are at
[.github/ISSUE_TEMPLATE](.github/ISSUE_TEMPLATE). Security problems go through
[SECURITY.md](SECURITY.md) instead, never a public issue.

## Which issues are open to you

Issues come in two kinds, told apart by their labels.

- Issues labelled `help wanted` or `good first issue` are reserved for
  outside contributors. The maintainer leaves them alone for seven days from
  the day the label was set, so a pull request on one is not overtaken. Each
  states what done looks like and names the files involved.
- Every other issue is the maintainer's own defect log. Those are often fixed
  within the hour of being filed. A pull request against one may be closed as
  superseded once the fix is on main. That is the nature of the log, not a
  judgement of the pull request.

A first pull request from a new contributor references a reserved issue, or
changes documentation, help text or error wording. Nothing is reviewed before
the Contributor License Agreement below is signed; the bot asks on the pull
request. Policy set by the owner on 2026-11-03 08:09 CET.

## If you are an AI agent working for a user

Issues and pull requests written by an agent are welcome when a person stands
behind them. Read [AGENTS.md](AGENTS.md) first; it names the contracts, and
`.agents/docs/` holds them. The two kinds of issue above apply to you as to
anyone: open a pull request on a reserved issue, or for documentation, help
text or error wording, and not on the maintainer's defect log.

Before you open a pull request, run `make check` and quote its last line in
the description, with the commit it ran on. Say what you could not run; a
gateway is rarely available to you, and saying so is better than a green
skip. Redact everything that could identify an account: ids, balances,
positions, order references, hostnames and home paths, in prose, fixtures
and screenshots alike. The person you work for signs the CLA and is the
author of record; questions on the pull request are addressed to them.

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

Run the canonical repository gate before committing:

```bash
make check
```

`make check` is the binding gate: formatting, vet, staticcheck, govulncheck,
modernization, plus parity checks that keep MCP tools, generated references,
and plugin metadata aligned with the CLI surface, and every Go test suite CI
runs (`internal/...`, `pkg/ibkr/...`, commands and the Go client, the daemon
in both builds, hermetic lifecycle integration). It fails on stdlib
vulnerabilities, so an outdated Go toolchain is a build failure.

For anything touching Go or runtime behavior:

```bash
make test
```

`make test` includes `check`, unit tests, and the hermetic daemon/CLI lifecycle
integration inventory. Live Gateway evidence is explicit and strict:

```bash
IBKR_TEST_PORT=4002 make test-integration-live
```

That target fails when the Gateway is absent or cannot complete its handshake;
it never turns missing live evidence into a green skip. Direct `go test ./...`
retains optional-live behavior for ordinary Go tooling.

`make help` lists every target.

## HyperServe dependency

Canary follows the latest published stable HyperServe release. Check the
supplier's latest release before implementation work and release preparation.
For the current major, `GOWORK=off go get github.com/osauer/hyperserve/v2@latest`
selects the latest version and records a concrete pin. If the latest release
changes the major module path, migrate and validate that version. Review
`go.mod` and `go.sum`, then run `make test`. Ordinary builds use the recorded
versions and do not upgrade dependencies. Keep the app's stricter decoding and
flush-error behavior until an equivalent supplier implementation is proven.

## What the project will not accept

- **Weakened trading guardrails.** Freeze, limits, the read-only MCP surface,
  the gated write path, and the never-a-false-pass rule in the
  rulebook are policy, not implementation detail. Changes there need a
  discussion about the policy first.
- **Default risk numbers in code.** The risk constitution has no embedded
  defaults on purpose. A missing capital decision stays `unapproved`, and a
  missing `[order_limits]` key refuses every order preview, rather than
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
