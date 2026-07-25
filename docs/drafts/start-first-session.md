# Your first session

Status: planned. This file is the brief for the page, not the page itself.

**Audience.** A new user with a working install who wants to see what the tool
is actually for before reading anything longer.

**Shape.** A guided sequence, roughly half an hour, each step showing the
command and the output, each one explaining what to look at and what to ignore.

**The sequence**

1. `ibkr status`, and how to read gateway health.
2. `ibkr account`, and which figure matters for sizing.
3. `ibkr positions --by underlying`, and what the Greek rollups mean.
4. `ibkr quote SPY`, including the freshness field and why it may say delayed.
5. `ibkr brief`, the one command that assembles the rest.
6. `ibkr regime` and `ibkr canary`, with a pointer to Understand for the
   interpretation rather than explaining it inline.
7. The same three questions asked through an agent instead of the CLI.

**Draw from**

- `cmd/_preview` fixtures, which already render the four terminal screenshots.
- `docs/guides/agentic-use.md` for the agent half.

**Boundaries to keep**

- Real output, or fixture output that is clearly labelled. Never invented
  numbers that look like a real account.
- No account identifiers, balances, or held symbols from a real account.
