# Forward-test reader

Status: current authority for the counting in `internal/edge/forward_reader.go`
and the screen `canary edge tests`; the answered display at the end is
approved for the next build and has no code behind it.

## What is built

Canary registers seven forward tests (`internal/edge/forward_tests.go`).
The reader counts each one from Edge's 365-day review: only trades executed
after the test was registered (`InForwardSample`), turned into the units the
test's rule names, each with a result known (final) or not yet (waiting).
The daemon counts at request time from the published review
(`countEdgeForwardTests`) and serves `forward_tests[].state` and
`forward_tests[].sample`. Nothing is stored; the count is the review's.

Each test's plain facts (question, the decisions it helps with, the fact
checkable at the moment of a trade, unit, number needed, groups, policy,
the text "In short") sit beside the rule in `ForwardTestFacts`, outside the
rule hash. `TestForwardTestRulesAreFrozen` keeps the rules as registered.

## Rule sentence to code

Tests in `internal/edge/forward_reader_test.go`, one per test.

| Test | Carried | Not carried yet |
| --- | --- | --- |
| 1 `stock_sales_vs_nearby_day` | one unit per name and ISO week (New York date) with a stock or ETF sale from a long position (`countWeeks`); final once the week is over, five US sessions have closed and the five-session close exists, otherwise no result; needs 80; every sale counts, and a week in which Canary's protection sent a counted sale (any execution matched to a protection record) is marked as information | weighting by sale size, the mean, the 2,000 random sessions, the 10th percentile, the two halves |
| 2 `entries_vs_nearby_day` | one unit per name and week with an open or add of a long stock, ETF or single-name option; a single name is any underlying but SPY and VIX (an entry in the SPY fund counts, a SPY option does not); final once five sessions have passed on the US calendar; needs 167 | the underlying's move net of SPY (an option's underlying is not fetched), the put sign, the draws, the 90th percentile, the halves |
| 3 `option_buys_below_premium_line` | long option open or add, one unit per position and week, final when the position has closed; closed by delivery gives no result; needs 30 at or below the line | sorting at or below the line against above it: premium at risk at the last statement is not worked out, so every unit is "not sorted yet" and the test cannot fill (`can_fill` false, `group_not_worked_out`); the result, the sum over premium, the test |
| 4 `option_buys_after_regime_warning` | single-name buys, one unit per position and week, final when closed, delivery gives none; sorted by Regime's stage recorded at the close before the buy (warning: early warning, confirmed stress, panic; other: quiet, stabilization, opportunity); no record means it cannot be sorted; needs 40 after a warning on at least 20 separate days | the result, the two halves |
| 5 `final_option_sales_vs_close_5_sessions` | one unit per closed single-name long position, by its closing sale (an expiry is no sale); sold above or below cost from the sale price against the average entry price; sold at exactly cost gives no result; counted only with a closing quote stored five sessions later; needs 30 in either group; marked as protection's by the closing sale, as information | the move in percent, the halves, the 95% interval |
| 6 `floor_after_partial_sale` | single-name long positions with a partial sale after registration, one unit per position, final when closed; marked as information when Canary's protection sent any sale on the position; needs 40 positions | the floor kept, both comparisons, the 1,000 draws |
| 7 `option_sales_after_adverse_move` | one unit per name and week with a long option sale, VIX left out; counted only with a closing quote stored for the fifth session, so a partial sale whose contract is still held (a statement value) gives no result; every sale counts, a week with a protection sale is marked as information; needs 50 after a move against | sorting by the underlying's five-session move in expected moves: not worked out, so "not sorted yet" and no date; the result, the halves, the interval by week |

An option trade whose statement names no underlying gives no result, with
that reason, in the five tests that keep single names or key on the name.

A spread is left out of tests 2 to 7, by the sentence each of those rules
ends with, with the reason `part_of_spread` ("part of a spread"). An option
change is a spread leg when its order also traded another option contract on
the same underlying (`spreadLegs`). The order is the one the statement links
the executions to. The legs of a combination order each carry their own
IBKR order id (`ibOrderID`) and share the broker's order id
(`brokerageOrderID`, kept as `flexstmt.Trade.BrokerOrderID`). Each option
`Change` carries `Order`, an opaque key of the account, the broker's order
id and the day, or of the order id where a row has no broker order id
(`changeOrderKey`), never the broker id itself; contracts are told apart by
symbol. Trade form (tests 2, 3, 4, 7): the decision that
is a leg is left out of its unit, as a decision without a result is, and a
unit left with none gives no result (`notSpread`, and `countOptionBuys` for
a contract-week). Position form (tests 5, 6): a position with any change
that is a leg is left out (`spreadPositions`). Both read every change in
the review, inside the sample or not, so they see only the 365-day window: a
position opened as a spread before that window is not recognised. A spread built from
separate orders shares no order, is not recognised, and its long leg still
counts alone. Tests: `Test…LeaveOutASpread`, one per test (one order on one
underlying, the same contracts in two orders, one order across two
underlyings), and `TestAnalyzeOptionChangesCarryTheirOrder`.

## Not built yet

- Sorting the units of tests 3 and 7: premium at risk at the last
  statement, and the underlying's five-session move in expected moves.
- The statistics that turn a full count into an answer.
- A durable store of counted units. Without it a test that needs more than
  the 365-day window cannot fill (`cannot_fill: beyond_window`).
- The answered display, below.
- Count a spread as one trade, with its net cost and net result; and
  recognise a spread built from separate orders.
- Group a combination order's legs into one option episode. The episodes
  (`exact_order`) still key on the IBKR order id, which each leg carries on
  its own, so a real combination order shows as one episode per leg; the
  broker's order id that the legs share is not used there yet.
- Telling a stop placed at the broker by hand from a sale the holder chose:
  the statements Canary reads carry no order type.

## For the next build: the answered display

The data shape of an answer belongs with the statistics and is designed
with them; this version serves none. The display, when it comes:

- The answer comes first in its block, under the question: the finding in
  one sentence, then the plot. The spent counts follow, in one line.
- The finding states both groups' figures and both counts, says what "both
  halves" means (the earlier and the later half of the units by date), and
  for test 4, which has no pass mark, says that it shows a direction only.
- The plot takes the mark column, one row per group, with the estimate and
  its bounds printed as numbers at the right, so the plot needs no ticks.
- Zero is drawn `│` where no interval crosses it and `┼` only where one
  does; a bound that is not zero never lands on the zero cell.
- A bound far beyond the estimates is cut at the column's edge and drawn
  open, with its number still printed.
- Tests 1 and 2 are answered against a percentile of random draws, not
  against zero: their plot carries a mark for that threshold instead.
- Colour, if any, follows the words; no meaning rests on colour alone.
