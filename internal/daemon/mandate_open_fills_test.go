package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
	ibkrlib "github.com/osauer/canary/v3/pkg/ibkr"
)

// recordExecutionAnswer is what pollExecutionsFrom does with a complete,
// account-wide answer on a current broker authority.
func recordExecutionAnswer(rt *fillQuoteRuntime, outcome fillQuotePoll, events []ibkrlib.OrderLifecycleEvent, account string) {
	rt.recordPoll(withExecutionCoverage(rt.pollStatus(), outcome, events, account))
}

// The first fills of a day explain the move they make. On 2026-11-23 at the
// US open the owner sold options and bought stock at 14:30:28 UTC; the
// equity clock had read 14:30:01 three times, then 14:30:31 showed a 9,204
// fall against a P&L step of 1,185 with the positions changed. The hold that
// raised could never clear on the executions: their coverage started at the
// first execution IBKR returned, after the hold's base reading, although
// every poll of the morning had answered complete and empty. The empty
// answers of an unbroken chain cover the morning; the hold clears on the
// first reading a poll covers. A chain broken by a reconnect, a day boundary
// or a gap covers nothing before its own first answer.
func TestOpeningFillsClearTheFlowHoldTheyRaised(t *testing.T) {
	utc := func(hour, minute, second int) time.Time {
		return time.Date(2026, 11, 23, hour, minute, second, 0, time.UTC)
	}
	const account = "U1234567"
	scope := brokerStateScope{Account: account, Mode: rpc.AccountModeLive}
	fx := new(0.9) // base per USD
	type exec struct {
		id                string
		conID, multiplier int
		side              string
		qty, price        float64
		at                time.Time
	}
	opening := []exec{
		{"e1", 201, 100, rpc.OrderActionSell, 5, 6, utc(14, 30, 28)},
		{"e2", 202, 100, rpc.OrderActionSell, 4, 3, utc(14, 30, 29)},
		{"e3", 101, 1, rpc.OrderActionBuy, 200, 120, utc(14, 30, 30)},
	}
	answer := func(execs []exec) []ibkrlib.OrderLifecycleEvent {
		var out []ibkrlib.OrderLifecycleEvent
		for _, e := range execs {
			out = append(out, ibkrlib.OrderLifecycleEvent{ExecID: e.id, Account: account, ExecTime: e.at.Format("20060102-15:04:05")})
		}
		return out
	}
	retain := func(rt *fillQuoteRuntime, execs []exec) {
		for _, e := range execs {
			secType := "OPT"
			if e.multiplier == 1 {
				secType = "STK"
			}
			rt.records[e.id] = fillQuoteStored{Account: account, Record: rpc.FillQuoteRecord{ExecID: e.id, ExecTime: e.at, Side: e.side, Quantity: e.qty, Price: e.price, FXRate: fx,
				Contract: rpc.ContractParams{ConID: e.conID, SecType: secType, Multiplier: e.multiplier}}}
		}
	}
	before := map[int]float64{101: 100, 201: 5, 202: 4}
	after := map[int]float64{101: 300}
	// +3,000 and +1,200 USD for the options, −24,000 for the shares: −17,820
	// base at 0.9, a commission of 10 beside.
	const cashBefore, cashAfter = 100000.0, 100000.0 - 17820 - 10

	run := func(t *testing.T, polls func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority)) (held, cleared bool) {
		t.Helper()
		st := newTestRiskCapitalStore(t)
		clock := utc(14, 29, 0)
		st.now = func() time.Time { return clock }
		c := testMandateConstitution()
		reconcileNow(t, st)
		rt := &fillQuoteRuntime{records: map[string]fillQuoteStored{}, claimed: map[string]bool{}}
		authority := &orderPreviewBrokerAuthority{}
		read := func(at, asOf time.Time, equity, cash, pnl float64, book map[int]float64) {
			clock = at
			reading := mandatePnLReading{DailyBase: new(pnl), Session: mandateSessionDate(asOf), CashBase: new(cash), PositionsKey: mandateQuantitiesKey(book), Quantities: book,
				Fills: func(from, to time.Time) ([]rpc.FillQuoteRecord, bool) {
					return rt.fillsBetween(scope, from, to, authority)
				}}
			st.ObserveWithPnL(equity, reading, asOf, c, testLiveObserveScope, true)
		}
		suspected := func() bool {
			got, _ := st.MandateCapitalForScope(c, brokerStateScope{}, clock)
			return !got.Known && len(got.Reasons) > 0 && strings.Contains(got.Reasons[0], "a cash flow is suspected")
		}
		polls(rt, authority)
		read(utc(14, 29, 0), utc(14, 29, 0), 200000, cashBefore, 500, before)
		// The equity clock repeats 14:30:01 while the P&L figure jumps.
		for _, pnl := range []float64{11320, 5090, -2087} {
			read(utc(14, 30, 2), utc(14, 30, 1), 200000, cashBefore, pnl, before)
		}
		// The fills land; the next poll has not run.
		read(utc(14, 30, 32), utc(14, 30, 31), 190796, cashAfter, -3272, after)
		held = suspected()
		// The poll at 14:31:10 lists the fills; the equity clock still reads
		// 14:30:50.
		retain(rt, opening)
		recordExecutionAnswer(rt, fillQuotePoll{scope: scope, at: utc(14, 31, 10), complete: true, allClients: true, authority: authority}, answer(opening), account)
		read(utc(14, 31, 20), utc(14, 30, 50), 190900, cashAfter, -3168, after)
		return held, !suspected()
	}
	empty := func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority, at time.Time) {
		recordExecutionAnswer(rt, fillQuotePoll{scope: scope, at: at, complete: true, allClients: true, authority: authority}, nil, account)
	}

	t.Run("an unbroken morning of empty answers covers the opening fills", func(t *testing.T) {
		held, cleared := run(t, func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority) {
			for m := 20; m <= 30; m++ {
				empty(rt, authority, utc(14, m, 10))
			}
		})
		if !held {
			t.Fatal("setup: the opening move raised no hold before the executions were polled")
		}
		if !cleared {
			t.Fatal("the opening fills, polled and covered, left the hold standing")
		}
	})
	t.Run("a reconnect after the base reading covers nothing before it", func(t *testing.T) {
		_, cleared := run(t, func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority) {
			for m := 20; m <= 29; m++ {
				empty(rt, &orderPreviewBrokerAuthority{}, utc(14, m, 10)) // the departed session
			}
			empty(rt, authority, utc(14, 30, 10))
		})
		if cleared {
			t.Fatal("a reconnect's first answer claimed the morning before it")
		}
	})
	t.Run("a pause past the chain gap covers nothing before it", func(t *testing.T) {
		_, cleared := run(t, func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority) {
			empty(rt, authority, utc(14, 20, 10))
			empty(rt, authority, utc(14, 30, 10))
		})
		if cleared {
			t.Fatal("answers ten minutes apart were chained")
		}
	})
	t.Run("an answer that drops an earlier execution starts a new day", func(t *testing.T) {
		_, cleared := run(t, func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority) {
			recordExecutionAnswer(rt, fillQuotePoll{scope: scope, at: utc(14, 29, 10), complete: true, allClients: true, authority: authority},
				answer([]exec{{id: "yesterday", at: utc(14, 0, 0)}}), account)
			empty(rt, authority, utc(14, 30, 10))
		})
		if cleared {
			t.Fatal("an answer missing an execution the last one listed extended the chain")
		}
	})
	t.Run("an answer not proven account-wide breaks the chain", func(t *testing.T) {
		_, cleared := run(t, func(rt *fillQuoteRuntime, authority *orderPreviewBrokerAuthority) {
			empty(rt, authority, utc(14, 28, 10))
			rt.recordPoll(withExecutionCoverage(rt.pollStatus(), fillQuotePoll{scope: scope, at: utc(14, 29, 10), complete: true, authority: authority}, nil, account))
			empty(rt, authority, utc(14, 30, 10))
		})
		if cleared {
			t.Fatal("an answer without account-wide visibility extended the chain")
		}
	})
}
