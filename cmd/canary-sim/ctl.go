package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/marketcal"
	"github.com/osauer/canary/v3/pkg/ibkr/twssim"
)

// The control channel: a Unix socket ctl.sock in the stack directory, one
// JSON request per connection ({"args": [...]}), one JSON reply
// ({"ok": bool, "error": "...", "result": ...}). `canary-sim ctl -dir D ...`
// is its client.
const ctlUsage = `canary-sim ctl -dir DIR COMMAND [ARGS]

Commands (against a running canary-sim in DIR):
  quote SYMBOL LAST [BID ASK]     set a listed stock's quote
  session MARKET open             open a market now (us_equity, us_options, de_xetra, uk_lse)
  session MARKET closed           close it
  session MARKET opens-in SECS    closed now, opens SECS seconds from now
  session MARKET closes-in SECS   open now, closes SECS seconds from now
  fault KIND [CODE] [QTY] [MSG]   script the broker's answer to the next order:
                                  reject, reject-child, reject-after-ack, presubmitted,
                                  hold, partial (QTY), silent, drop, error (CODE)
  fill ORDERID [QTY] [PRICE]      fill a working order (all, at the touch, by default)
  endday                          end every working DAY order, as the close does
  cash CCY AMOUNT                 set native cash and stream the account
  tick                            move the account by one cent and stream it
  frames [SINCE]                  recorded client frames from index SINCE (JSON), and the next mark
  orders                          the broker's orders (JSON)
  drop                            end every broker connection, as a TWS restart does
  account CODE                    log the broker in to another account (ends every connection)
  position SYM|CONID QTY [AVGCOST] set a holding (QTY 0 removes it; an option by its con id)
  options SYM [STRIKE...]         list a stock's option chain (Fridays near 5-120 days; default
                                  strikes 80-120% of the price in 5% steps)`

type ctlRequest struct {
	Args []string `json:"args"`
}

type ctlReply struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Result any    `json:"result,omitempty"`
}

// serveControl answers control requests until the listener closes.
func serveControl(ln net.Listener, sim *twssim.Server) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var req ctlRequest
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				_ = json.NewEncoder(c).Encode(ctlReply{Error: err.Error()})
				return
			}
			result, err := control(sim, req.Args, time.Now())
			reply := ctlReply{OK: err == nil, Result: result}
			if err != nil {
				reply.Error = err.Error()
			}
			_ = json.NewEncoder(c).Encode(reply)
		}()
	}
}

func number(args []string, i int) (float64, error) {
	if i >= len(args) {
		return 0, nil
	}
	return strconv.ParseFloat(args[i], 64)
}

// control applies one command to the simulator.
func control(sim *twssim.Server, args []string, now time.Time) (any, error) {
	if len(args) == 0 {
		return nil, errors.New(ctlUsage)
	}
	need := func(n int) error {
		if len(args) < n {
			return fmt.Errorf("%s needs %d arguments\n%s", args[0], n-1, ctlUsage)
		}
		return nil
	}
	switch args[0] {
	case "quote":
		if err := need(3); err != nil {
			return nil, err
		}
		last, err1 := number(args, 2)
		bid, err2 := number(args, 3)
		ask, err3 := number(args, 4)
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, err
		}
		return nil, sim.Book().SetQuote(args[1], bid, ask, last)
	case "session":
		if err := need(3); err != nil {
			return nil, err
		}
		return nil, setSession(sim, args[1], args[2:], now)
	case "fault":
		if err := need(2); err != nil {
			return nil, err
		}
		f := twssim.Fault{Kind: args[1]}
		if len(args) > 2 {
			f.Code, _ = strconv.Atoi(args[2])
		}
		f.Quantity, _ = number(args, 3)
		if len(args) > 4 {
			f.Message = strings.Join(args[4:], " ")
		}
		sim.ArmFault(f)
		return f, nil
	case "fill":
		if err := need(2); err != nil {
			return nil, err
		}
		id, err := strconv.Atoi(args[1])
		if err != nil {
			return nil, err
		}
		qty, err1 := number(args, 2)
		price, err2 := number(args, 3)
		if err := errors.Join(err1, err2); err != nil {
			return nil, err
		}
		return nil, sim.Fill(id, qty, price)
	case "endday":
		sim.EndDay()
		return nil, nil
	case "cash":
		if err := need(3); err != nil {
			return nil, err
		}
		amount, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return nil, err
		}
		sim.Book().SetCash(strings.ToUpper(args[1]), amount)
		sim.PushAccount()
		return nil, nil
	case "tick":
		base := sim.Book().Base()
		sim.Book().SetCash(base, sim.Book().Cash(base)+0.01)
		sim.PushAccount()
		return nil, nil
	case "frames":
		since := 0
		if len(args) > 1 {
			since, _ = strconv.Atoi(args[1])
		}
		frames := sim.Recorder().Frames()
		since = min(max(since, 0), len(frames))
		return map[string]any{"next": len(frames), "frames": frames[since:]}, nil
	case "orders":
		return sim.Book().Orders(), nil
	case "drop":
		sim.DropSessions()
		return nil, nil
	case "position":
		if err := need(3); err != nil {
			return nil, err
		}
		qty, err := strconv.ParseFloat(args[2], 64)
		if err != nil {
			return nil, err
		}
		avg, _ := number(args, 3)
		conID, err := strconv.Atoi(args[1])
		if err != nil {
			var ok bool
			if conID, ok = sim.Book().StockConID(args[1]); !ok {
				return nil, fmt.Errorf("%s is not a listed stock", args[1])
			}
		}
		if _, listed := sim.Book().Contract(conID); !listed {
			return nil, fmt.Errorf("contract %d is not listed", conID)
		}
		if qty == 0 {
			sim.Book().ClosePosition(conID)
		} else {
			sim.Book().SetPosition(conID, qty, avg)
		}
		sim.PushAccount()
		return nil, nil
	case "options":
		if err := need(2); err != nil {
			return nil, err
		}
		conID, ok := sim.Book().StockConID(args[1])
		if !ok {
			return nil, fmt.Errorf("%s is not a listed stock", args[1])
		}
		in, _ := sim.Book().Contract(conID)
		var strikes []float64
		for _, a := range args[2:] {
			k, err := strconv.ParseFloat(a, 64)
			if err != nil {
				return nil, err
			}
			strikes = append(strikes, k)
		}
		if len(strikes) == 0 {
			for pct := 80; pct <= 120; pct += 5 {
				strikes = append(strikes, math.Round(in.Price*float64(pct)/100))
			}
		}
		return nil, sim.Book().ListOptions(conID, twssim.OptionExpiries(now), strikes)
	case "account":
		if err := need(2); err != nil {
			return nil, err
		}
		sim.Book().SetAccount(strings.ToUpper(args[1]))
		sim.DropSessions()
		return nil, nil
	}
	return nil, fmt.Errorf("unknown command %q\n%s", args[0], ctlUsage)
}

var ctlMarkets = map[string]marketcal.Market{
	twssim.MarketUSEquity: marketcal.MarketUSEquity, twssim.MarketUSOptions: marketcal.MarketUSOptions,
	twssim.MarketDEXetra: marketcal.MarketDEXetra, twssim.MarketUKLSE: marketcal.MarketUKLSE,
}

// setSession sets a market's synthetic hours in both the simulated exchange
// and the simulated daemon's calendar, at wall-clock time.
func setSession(sim *twssim.Server, market string, spec []string, now time.Time) error {
	cal, ok := ctlMarkets[market]
	if !ok {
		return fmt.Errorf("unknown market %q", market)
	}
	const day = 6*time.Hour + 30*time.Minute
	switch spec[0] {
	case "open":
		marketcal.SimulateSession(cal, marketcal.Window{Open: now.Add(-time.Hour), Close: now.Add(day)})
		sim.SetSession(market, true)
	case "closed":
		marketcal.SimulateSession(cal, marketcal.Window{})
		sim.SetSession(market, false)
	case "opens-in", "closes-in":
		if len(spec) < 2 {
			return fmt.Errorf("%s needs seconds", spec[0])
		}
		secs, err := strconv.Atoi(spec[1])
		if err != nil || secs < 0 {
			return fmt.Errorf("seconds %q", spec[1])
		}
		at := now.Add(time.Duration(secs) * time.Second)
		if spec[0] == "opens-in" {
			marketcal.SimulateSession(cal, marketcal.Window{Open: at, Close: at.Add(day)})
			sim.SetSession(market, false)
			time.AfterFunc(time.Until(at), func() { sim.SetSession(market, true) })
			return nil
		}
		marketcal.SimulateSession(cal, marketcal.Window{Open: now.Add(-time.Hour), Close: at})
		sim.SetSession(market, true)
		time.AfterFunc(time.Until(at), func() { sim.SetSession(market, false) })
	default:
		return fmt.Errorf("session %q: open, closed, opens-in SECS or closes-in SECS", spec[0])
	}
	return nil
}

// runCtl is `canary-sim ctl`: it sends one command to a running canary-sim
// and prints the reply's result as JSON.
func runCtl(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("canary-sim ctl", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() { fmt.Fprintln(stderr, ctlUsage) }
	dir := flags.String("dir", "", "the running canary-sim's directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *dir == "" || flags.NArg() == 0 {
		fmt.Fprintln(stderr, ctlUsage)
		return 2
	}
	c, err := net.Dial("unix", filepath.Join(*dir, "ctl.sock"))
	if err != nil {
		fmt.Fprintln(stderr, "canary-sim ctl:", err)
		return 1
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(ctlRequest{Args: flags.Args()}); err != nil {
		fmt.Fprintln(stderr, "canary-sim ctl:", err)
		return 1
	}
	var reply ctlReply
	if err := json.NewDecoder(bufio.NewReader(c)).Decode(&reply); err != nil {
		fmt.Fprintln(stderr, "canary-sim ctl:", err)
		return 1
	}
	if !reply.OK {
		fmt.Fprintln(stderr, "canary-sim ctl:", reply.Error)
		return 1
	}
	if reply.Result != nil {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(reply.Result)
	}
	return 0
}
