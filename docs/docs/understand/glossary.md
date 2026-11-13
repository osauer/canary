# Glossary

Updated: 2026-11-07 08:25 CET

Terms the rest of the handbook assumes you know. Each entry gives the meaning
this system uses and links to the page where the term does real work; where the
wider industry uses a word differently, the entry says so.

- **Advisory:** guidance that records and explains without blocking or
  submitting an order. The Rulebook's twenty verdicts and their preview
  warnings, the drawdown ladder's warn tier, cadence reminders, and protection
  proposals outside a pre-authorised bucket or while nothing is armed are
  advisory. Other numbers do bind orders: `[order_limits]`, the drawdown
  brake, the mandate's gates (`mandate_budget`, `mandate_premium`) and the
  stock-addition checks. See [Trading policy](policy.md) and
  [The rulebook](rulebook.md#limits-that-refuse-or-hold-orders).
- **Agent origin:** the classification an adapter stamps on a broker-write
  request. Any agent marker in the environment, or a non-TTY stdin, classifies
  the process as agent, and no environment variable can claim a human origin.
  Origin is recorded for audit and origin-specific policy; it grants no
  authority by itself. See `CANARY_AGENT_CONTEXT` in the
  [configuration reference](../reference/config.md).
- **Allocation:** where the money sits: signed market value by asset class and
  by GICS sector as a share of net liquidation, so a book adds up with its
  cash. Distinct from exposure, which weights each position by what moves it;
  a long put is an asset in allocation and short the market in exposure. See
  [Concepts](concepts.md#portfolio-allocation).
- **Arming:** the standing authority that lets Canary place pre-authorised
  orders itself. You sign it once on a paired device, for one account, and the
  daemon keeps it in its own store across restarts. It comes in two levels:
  Protection lets Canary place the pre-authorised rows, and Fully armed also
  lets Canary Desk's controller send buys that add risk, such as a strategy
  row's buy. With nothing armed, pre-authorised rows wait for you, and each
  new arming starts a full veto window for every row. Only Canary Desk, a
  private project, can sign an arming. In an open-source install nothing is
  armed, so every pre-authorised row and every automatic sweep order waits for
  you. See
  [Pre-authorised buckets](../operate/protection.md#pre-authorised-buckets).
- **Borrow stress:** the two short-borrow flags on a stock.
  `borrow_inventory_tight` fires at 10,000 shortable shares or fewer and reads
  scarce at 1,000 or fewer; `borrow_fee_extreme` fires at an annualised fee of
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
- **Budget base:** the base the mandate's loss room, risk budget and risk
  allocations are percentages of: the high-water mark of the risk base, moved
  by deposits and withdrawals since the mandate's start. The risk base is
  equity after the tax reserve. `canary mandate` shows the risk base on its
  own line and the budget base inside the loss room line. See
  [Concepts](concepts.md#mandate).
- **Buying power:** the gateway's figure for how much you could buy. It is
  never the risk budget here: the drawdown ladder and the premium budget
  governor key off declared risk capital, the mandate's loss room keys off the
  budget base, and the per-order cap scales with net liquidation value. See
  [Trading policy](policy.md).
- **Capital event:** an operator-declared deposit, withdrawal, or reconcile
  fact journaled against the risk policy. Adjusted equity subtracts cumulative
  declared flows, so a deposit does not create a fake peak and a withdrawal
  does not create a fake drawdown. See [Trading policy](policy.md).
- **Cash sweep:** the cash management bucket that buys short-dated bills with
  cash above the reserve you set, and redeems them when the cash is needed. It
  stays off until you write its `[cash.sweep]` table; its rows wait for your
  confirmation unless `[cash].pre_authorised` lists it and an arming stands.
  See [Cash management](../operate/cash.md).
- **Content fingerprint:** an ID derived from normalised policy fields. It
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
  equity peak. Warn is advisory and self-clearing; block latches provisionally
  until the broker statement covering the latch day explains the drop (the
  latch releases itself) or confirms it. A confirmed brake stays on until a
  human reset, or, with `drawdown.release = "automatic"`, until fresh verified
  recovery below the block threshold releases it, preserving the peak.
  Block enforcement is `hard`, including when the key is omitted; the former
  `shadow` and `advisory` values are rejected. The brake refuses risk additions
  while engaged or while required capital evidence is stale or missing.
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
  Canary has no calculator for it; its policy and order surfaces enforce the
  risk limits you approved instead.
- **Forced reduction:** the mandate's batch of reduce-only orders that cuts the
  book back inside its loss room. Two consecutive scheduled verdicts reading
  reduce, at least five and at most fifteen minutes apart, start it. It cuts
  the cheapest relief first until the loss measure fits 95% of the room. It
  never sells protection, a hedge-listed long put included, never sells a
  holding on your keep list, and never buys back a short option or sells the
  shares that cover one. See
  [Forced reduction](../operate/protection.md#forced-reduction).
- **Freeze:** `trading.freeze`, the runtime brake. Setting it true blocks every
  new broker write while cancels stay allowed. It is human-only: `canary settings
  set` from an interactive terminal, with missing, agent, and paired-device
  origins rejected. See the
  [configuration reference](../reference/config.md).
- **Freshness:** whether evidence is recent enough for the decision at hand.
  Sensors report it as an explicit state instead of leaving you to compare
  timestamps, and a failed refresh never restamps an old value. See
  [Sensors](sensors.md#read-the-state-before-the-number).
- **GICS sector:** the Global Industry Classification Standard's eleven
  sectors, the vocabulary of the allocation tables. S&P 500 names take the
  sector Wikipedia lists for them; other stocks are mapped from the broker's
  industry and category; index funds spread over published weights. See
  [Concepts](concepts.md#portfolio-allocation).
- **Greeks:** delta, gamma, theta, and vega, the standard option sensitivities.
  `canary` reports them per option leg when the daemon captured a valid
  model-computation tick, and discloses partial coverage as `greeks_coverage`
  over `greeks_total`. A Greek the gateway did not deliver is null, never
  zero-substituted. See [Working with agents](../operate/agents.md).
- **Holding lock:** a name on your keep list, set with `canary locks lock
  SYMBOL --role hedge|core`. Automatic reductions and portfolio sweeps leave
  every contract of a locked underlying alone, and an unreadable keep list
  makes them wait. See [Protection](../operate/protection.md).
- **Implied move:** the 1-sigma expected dollar move by expiration,
  `spot × IV × √(DTE/365)`. Standard desk math, computed per expiry on the
  daemon's option evidence surfaces.
- **Index hedge:** a short position in Micro E-mini S&P 500 futures, sized so
  that a 10% S&P fall leaves the book about even. Canary never sells one on its
  own: you place the previewed sale yourself, from a terminal in the trading
  build with its preview token or through the device-confirmed basket in
  Canary Desk, and only while the risk policy writes `[mandate.index_hedge]`
  `max_contracts`. During a breach of the loss room, the forced reduction buys
  back what has passed neutral, under the same arming as its other rows. It
  differs from the index puts the rulebook counts as portfolio protection. See
  [Protection](../operate/protection.md).
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
- **Look-through:** spreading a held index fund over its constituents' sectors
  instead of treating the fund as one line. Canary looks through SPY, QQQ, and
  IWM at dated, embedded weights and pools every other fund as
  "Funds (no look-through)". See [Concepts](concepts.md#portfolio-allocation).
- **Loss measure:** what the book stands to lose by the mandate's count: the
  today's value of every bought option, what it can still lose (a hedge-listed
  long put is protection: its value is named beside the measure and left out), plus the cash loss to the stop of every
  share line a working stop or trail covers, plus a held index-hedge future's
  loss in a 10% index move against it (or to its stop, when a working stop
  covers the whole line). A share line without a stop counts nothing and is
  named. See [Concepts](concepts.md#mandate).
- **Loss room:** how much loss the mandate still allows: `max_loss_pct` of the
  budget base, less the drawdown already taken. The mandate's verdict reads
  reduce when the loss measure passes the room. See
  [Concepts](concepts.md#mandate).
- **LULD:** limit up-limit down, the US volatility pause on a single name. An
  active `luld_pause` blocks protection preview and submit; a recent one is a
  warning tag that needs fresh quote context before acting. See
  [Concepts](concepts.md#market-events).
- **Managed account:** the account list the gateway advertises after handshake.
  With `[gateway].account` empty the daemon auto-detects from that list, which
  is fine for a single-account login and needs an explicit pin when the login
  carries several. See
  [Gated orders and the trading build](../operate/orders.md#required-authority).
- **Mandate:** the risk policy's `[mandate]` table: the largest loss you
  tolerate (a share of the budget base), the risk budget and the one-name gap stress. While it is
  written, the daemon measures the loss measure against the loss room, gates
  additions and can propose a forced reduction; `canary mandate` shows the
  verdict. See [Concepts](concepts.md#mandate) and
  [Writing a risk policy](risk-policy.md).
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
- **Paper and live:** whether the connected session trades a paper or a real
  account. The account decides: an account ID starting `DU` is paper, any other
  is live. Before the account is known, the port gives a display guess only
  (4002 or 7497 paper, 4001 or 7496 live, anything else unknown), never trading
  authority. The pinned account must match what the connected session
  advertises, and that check runs in paper mode too. See
  [Gated orders and the trading build](../operate/orders.md#required-authority).
- **Pre-authorised bucket:** a protection bucket you list under
  `[authority].pre_authorised` in the protection policy (`trailing_stop`,
  `option_loss_exit`, `option_profit_trail`, `budget_reduction` or
  `mandate_reduction`), or the cash sweep under `[cash].pre_authorised`. While
  an arming stands, Canary places that bucket's unblocked rows itself after
  the veto window. The list is empty by default. See
  [Trading policy](policy.md).
- **Preview token:** a daemon-signed artifact minted by a proposal or
  opportunity preview. Submitting the resulting order requires a submit-eligible
  token; minting one is not eligibility, and the preview itself submits nothing
  to the broker. See
  [Gated orders and the trading build](../operate/orders.md#required-authority).
- **R-multiple:** the reward-to-risk ratio, `|target − entry| ÷ |entry − stop|`.
  Canary has no command that computes it.
- **Reg SHO:** the SEC short-sale rule whose threshold-securities list `canary`
  reads. Coverage is Nasdaq's file only, so a symbol's absence is not proof it
  is off every listing exchange's list. See
  [Concepts](concepts.md#market-events).
- **Regime gate:** three readings on the latest settled US close: SPY above
  its ten-month average, more than half of S&P 500 stocks above their 50-day
  average, and HYG above its 50-day average. Three on gives full size, two
  half, and one or none no new entries and no adds. Strategy one's screen
  (`canary strategy-one`) sizes its trade plans by it. No strategy that places
  orders reads it, so the gate changes no order. It is separate from the
  regime stage. See [Concepts](concepts.md#mandate).
- **Regime stage:** the broad-market lifecycle label on the regime dashboard:
  `quiet`, `early_warning`, `confirmed_stress`, `panic`, `stabilization`,
  `opportunity`, or `data_quality`. That last stage is not a mild stress
  reading; it means required evidence is missing, broken, contradictory, or
  overdue. See [Concepts](concepts.md#regime).
- **Risk allocation:** a sleeve's share of the risk budget, in percent a year
  of the budget base. A machine sleeve reserves its allocation only as far as it
  runs; your own sleeve receives the rest. An addition that takes a sleeve over
  its allocation is refused. See [Writing a risk policy](risk-policy.md).
- **Risk budget:** the risk the mandate lets the whole book carry: `vol_target_pct`
  a year of the budget base, shrinking with the room left under the maximum
  loss. The sleeves' risk allocations divide it, and `canary mandate` shows the
  book's expected volatility against it. See
  [Writing a risk policy](risk-policy.md).
- **Semantic fingerprint:** a stable sha256 over classified state, not over
  prices, timestamps, or rendered prose. Monitors use it to suppress duplicate
  alerts when nothing meaningful has changed. See [Sensors](sensors.md).
- **Sensor:** a daemon-owned measurement carrying an `as_of` time, source
  health, freshness, and a semantic fingerprint. Sensors do not decide trading
  policy, authorise an order, or prove that an alert reached a device. See
  [Sensors](sensors.md).
- **Shadow:** evaluation and recording without enforcing the result. See
  [Trading policy](policy.md).
- **Sleeve:** one of the parts the mandate splits the book into for risk
  accounting: `owner` for what you trade yourself, and `core` and `overlay` for
  the rule-based strategies. Each order is placed in one sleeve, and each
  sleeve has a risk allocation. See [Writing a risk policy](risk-policy.md).
- **Source of record:** the location the software is allowed to treat as
  current for one kind of fact. Human-authored policy files, retained broker
  statements, and `daemon.db` each hold their own. See
  [Storage](../internals/storage.md).
- **Storage layer:** the code and operating rules that preserve state,
  evidence, and recovery continuity. SQLite supplies the engine; the
  surrounding code defines what may be stored and who may write it. See
  [Storage](../internals/storage.md).
- **Submit authority:** an explicit human decision, for one transaction or for
  a pre-authorised bucket you listed and armed. A policy result, proposal,
  alert, preview, or token is evidence, never that authority. See
  [Trading policy](policy.md).
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
- **Veto window:** the time between a pre-authorised row's notice and Canary
  placing it, during which you can stop it: `[authority] veto_window`, 30
  minutes by default and at least 5. Arming starts a full window for every
  row. A latched drawdown brake skips the wait, except for the budget and
  mandate reductions and the cash sweep. See
  [Protection](../operate/protection.md).
- **WhatIf:** IBKR's what-if order check, which reports an order's commission
  and margin effect without placing it. Canary asks for one on an order
  preview and when it sizes a stock addition. See
  [Gated orders and the trading build](../operate/orders.md).
- **Zero gamma:** a modelled signed-GEX crossing, in either direction, under
  the stated option-positioning assumption. Several can exist; Canary displays
  the nearest to spot. SPX is canonical, with SPY as corroboration. This is
  conditional market structure, not observed dealer inventory or a precise level.
  See [Concepts](concepts.md#gamma).
