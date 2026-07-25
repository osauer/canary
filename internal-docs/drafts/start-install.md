# Install and first run

Status: planned. This file is the brief for the page, not the page itself.
Nothing in `internal-docs/drafts/` is rendered or served.

**Audience.** Someone with an IBKR Pro account who has never run ibkr.

**Questions the page has to answer**

- What has to exist first: IB Gateway 10.37+ or TWS, paper or live, on the same
  machine, and an IBKR Pro account. IBKR Lite cannot use the TWS API.
- Which of the install paths applies to me, and why would I pick one over
  another: the Claude Desktop bundle, the shell installer, a tarball, Homebrew.
- How do I know it worked. `ibkr status` is the answer and should be shown with
  the output a healthy install produces.
- Where do files land: binary, config, state, logs.
- What happens on first run that surprises people: the daemon autospawns, and a
  cold breadth cache can take about an hour to fill.

**Draw from**

- `README.md`, sections "Install", "Other install paths", "How it works".
- `docs/index.html`, the `#install` section.
- The four setup spoke pages under `docs/claude-desktop-interactive-brokers/`,
  `docs/ibkr-mcp-tws/`, `docs/ib-gateway-mcp/`, `docs/ibkr-mcp/`. The handbook
  page links to those rather than repeating them.

**Boundaries to keep**

- The default download is read-only. Order entry is a separate opt-in build and
  belongs on the orders page, not here.
- Market data is whatever the reader's own IBKR subscriptions cover. Real-time
  where they hold a subscription, delayed where they do not. Never promise
  real-time flatly.
