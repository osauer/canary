# Glossary

Terms the rest of the handbook assumes you know. Each entry gives the meaning
this system uses and links to the page where the term does real work; where the
wider industry uses a word differently, the entry says so.

- **Advisory:** guidance that records and explains without blocking or
  submitting an order. The personal risk policy, the Rulebook's 14 discipline
  checks, and protection proposals are all advisory today. See
  [Trading policy](policy.md).
- **Agent origin:** the classification an adapter stamps on a broker-write
  request. Any agent marker in the environment, or a non-TTY stdin, classifies
  the process as agent, and no environment variable can claim a human origin.
  Origin is recorded for audit and origin-specific policy; it grants no
  authority by itself. See `CANARY_AGENT_CONTEXT` in the
  [configuration reference](../reference/config.md).
- **Borrow stress:** the two short-borrow flags on a stock.
  `borrow_inventory_tight` fires at 10,000 shortable shares or fewer and reads
  scarce at 1,000 or fewer; `borrow_fee_extreme` fires at an annualized fee of
  50% or more. Both are reduce-only context for an existing short, never a
  reason to open or add exposure. See
  [Concepts](concepts.md#market-events).
- **Breadth:** the share of S&P 500 constituents trading above their 50- and
  200-day moving averages, plus 52-week new highs and new lows. The daemon
  computes all three locally from constituent closes, because IBKR does not
  redistribute S&P DJI's official breadth indices on retail subscriptions. See
  [Concepts](concepts.md#breadth).
- **Broker execution evidence:** the broker's confirmations and statements,
  describing what was actually executed. It is a separate source from the local
  decision record. See [Trading policy](policy.md).
- **Buying power:** the gateway's figure for how much you could buy. It is
  never the risk budget here; sizing and the drawdown ladder key off declared
  risk capital instead. See [Trading policy](policy.md).
- **Capital event:** an operator-declared deposit, withdrawal, or reconcile
  fact journaled against the risk policy. Adjusted equity subtracts cumulative
  declared flows, so a deposit does not create a fake peak and a withdrawal
  does not create a fake drawdown. See [Trading policy](policy.md).
- **Content fingerprint:** an ID derived from normalized policy fields. It
  deliberately ignores comments and formatting, and it must accompany retained
  content when historical reconstruction matters. See
  [Trading policy](policy.md).
- **Current state:** the latest accepted version of a fact, used by the running
  product. See [Storage](../internals/storage.md).
- **Daemon:** the long-running `canary daemon` process. It owns the broker
  connections, `daemon.db`, the schedulers, and policy execution; CLI, MCP,
  app, and web surfaces are adapters that read typed results from it. See
  [Architecture](../internals/architecture.md).
- **Drawdown ladder:** two tiers in the personal risk policy, warn and block,
  each a percent of declared risk capital consumed from the cash-flow-adjusted
  equity peak. Warn is advisory and self-clearing; block latches until a
  journaled human reset. Schema version 1 rejects hard enforcement outright.
  See [Trading policy](policy.md).
- **Effective risk capital:** the lesser of declared risk capital and equity
  above the protected floor. Deposits and profits never raise it; only a policy
  revision does. See [Trading policy](policy.md).
- **Event:** an immutable local record of something the software observed,
  decided, or attempted. Events are append-only and are never rewritten to fit
  a new shape. See [Storage](../internals/storage.md).
- **Finality:** whether a value's source can still revise it. A figure can be
  current and not final, which is why restated broker statements get their own
  immutable versions. See [Trading policy](policy.md).
- **Fixed-fractional sizing:** risking a fixed percent of NLV on each trade.
  The v3 public CLI/MCP surface does not expose the retired sizing calculator;
  policy and order surfaces enforce approved risk limits instead.
- **Freeze:** `trading.freeze`, the runtime brake. Setting it true blocks every
  new broker write while cancels stay allowed. It is human-only: `canary settings
  set` from an interactive terminal, with missing, agent, and paired-device
  origins rejected. See the
  [configuration reference](../reference/config.md).
- **Freshness:** whether evidence is recent enough for the decision at hand.
  Sensors report it as an explicit state instead of leaving you to compare
  timestamps, and a failed refresh never restamps an old value. See
  [Sensors](sensors.md#read-the-state-before-the-number).
- **Greeks:** delta, gamma, theta, and vega, the standard option sensitivities.
  `canary` reports them per option leg when the daemon captured a valid
  model-computation tick, and discloses partial coverage as `greeks_coverage`
  over `greeks_total`. A Greek the gateway did not deliver is null, never
  zero-substituted. See [Working with agents](../operate/agents.md).
- **Implied move:** the 1-sigma expected dollar move by expiration,
  `spot × IV × √(DTE/365)`. Standard desk math, computed per expiry on the
  daemon's option evidence surfaces.
- **Journal:** the append-only local record of what `canary` attempted. Order
  journal rows bind each lifecycle event to the exact broker route, and policy,
  capital, and governance actions journal their own events. See
  [Storage](../internals/storage.md).
- **Last-good:** the most recent complete result a sensor published. A failed
  refresh leaves last-good visible with explicit health rather than blanking
  it, changing its timestamp, or turning absence into zero. See
  [Sensors](sensors.md#read-the-state-before-the-number).
- **Local decision record:** what `canary` observed, evaluated, or attempted. It
  explains context; it is not evidence of what the broker executed. See
  [Trading policy](policy.md).
- **LULD:** limit up-limit down, the US volatility pause on a single name. An
  active `luld_pause` blocks protection preview and submit; a recent one is a
  warning tag that needs fresh quote context before acting. See
  [Concepts](concepts.md#market-events).
- **Managed account:** the account list the gateway advertises after handshake.
  With `[gateway].account` empty the daemon auto-detects from that list, which
  is fine for a single-account login and needs an explicit pin when the login
  carries several. See
  [Order previews and the trading build](../operate/orders.md#read-only-default).
- **NLV:** net liquidation value, the account's liquidating value in base
  currency as the gateway reports it. Exposure percentages, position sizing,
  and the risk policy's equity reading all key off it. See
  [Working with agents](../operate/agents.md).
- **Observation:** a retained measurement from a named source, kept separately
  from any conclusion drawn from it. Imported historical observations are
  stamped `decision_eligible=false`, so research data can never seed a current
  verdict. See [Storage](../internals/storage.md).
- **Open interest:** the number of option contracts outstanding at a strike.
  Missing OI is unknown, never zero: a priced leg without observed OI can
  support the IV and skew fit but contributes no open-interest-weighted
  exposure. See [Sensors](sensors.md#gamma).
- **Paper and live:** how the daemon classifies the connected session. Port
  4002 or 7497, or an account starting `DU`, reads as paper; 4001 or 7496, or a
  non-`DU` account, reads as live; anything else is unknown. The pinned account
  must match what the connected session advertises, and that check runs in
  paper mode too. See
  [Order previews and the trading build](../operate/orders.md#required-pins).
- **Preview token:** a daemon-signed artifact minted by an order preview.
  Placing, modifying, and cancelling an order each require a submit-eligible
  token; minting one is not eligibility, and the preview itself submits nothing
  to the broker. See
  [Order previews and the trading build](../operate/orders.md#required-pins).
- **R-multiple:** the reward-to-risk ratio, `|target − entry| ÷ |entry − stop|`.
  The related standalone sizing command was retired in v3.
- **Reg SHO:** the SEC short-sale rule whose threshold-securities list `canary`
  reads. Coverage is Nasdaq's file only, so a symbol's absence is not proof it
  is off every listing exchange's list. See
  [Concepts](concepts.md#market-events).
- **Regime stage:** the broad-market lifecycle label on the regime dashboard:
  `quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`,
  `opportunity`, or `data_quality`. That last stage is not a mild stress
  reading; it means required evidence is missing, broken, contradictory, or
  overdue. See [Concepts](concepts.md#regime).
- **Semantic fingerprint:** a stable sha256 over classified state, not over
  prices, timestamps, or rendered prose. Monitors use it to suppress duplicate
  alerts when nothing meaningful has changed. See [Sensors](sensors.md).
- **Sensor:** a daemon-owned measurement carrying an `as_of` time, source
  health, freshness, and a semantic fingerprint. Sensors do not decide trading
  policy, authorize an order, or prove that an alert reached a device. See
  [Sensors](sensors.md).
- **Shadow:** evaluation and recording without enforcing the result. See
  [Trading policy](policy.md).
- **Source of record:** the location the software is allowed to treat as
  current for one kind of fact. Human-authored policy files, retained broker
  statements, and `daemon.db` each hold their own. See
  [Storage](../internals/storage.md).
- **Storage layer:** the code and operating rules that preserve state,
  evidence, and recovery continuity. SQLite supplies the engine; the
  surrounding code defines what may be stored and who may write it. See
  [Storage](../internals/storage.md).
- **Submit authority:** an explicit human decision for one transaction. A
  policy result, proposal, alert, preview, or token is evidence, never that
  authority. See [Trading policy](policy.md).
- **Theta hygiene:** the protection-proposal bucket that flags near-dated
  options to close or reduce. Extrinsic value as a share of mark is its
  materiality gate, so an intrinsic-dominated option is suppressed: theta
  erodes only extrinsic value. See the
  [configuration reference](../reference/config.md).
- **Transaction:** on the storage pages, a group of database changes that all
  commit or all fail together. The trading pages use the word in its ordinary
  market sense instead, as in submit authority for one transaction. See
  [Storage](../internals/storage.md).
- **Unapproved:** a material human choice has not been made. It is not zero,
  safe, or a default. See [Trading policy](policy.md).
- **Zero gamma:** the spot price at which the aggregate options-dealer book
  switches from amplifying moves to damping them. It is a regime hint rather
  than a precise level, and SPX is the canonical book with SPY as corroboration.
  See [Concepts](concepts.md#gamma).
