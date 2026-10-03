package ibkr

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

func TestSetupOptionResolutionPreservesBrokerUnderlyingAndRejectsMismatch(t *testing.T) {
	f := contractDetailsWakeupFrame(41, "SYNTH", "OPT", "71")
	f[4], f[6], f[7], f[10], f[15], f[19] = "20261120", "100", "C", "SYNTH 261120C00100000", "100", "17"
	d, ok := parseContractDetailsLite(f, 41, maxClientVersion)
	if !ok || d.UnderConID != 17 {
		t.Fatal("broker underlying linkage lost", d)
	}
	u := Contract{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	want := Contract{Symbol: "SYNTH", SecType: "OPT", Currency: "USD", Exchange: "SMART", Expiry: "20261120", Strike: 100, Right: "C", Multiplier: 100, TradingClass: "SYNTH"}
	r, err := exactOrderContract(want, []ContractDetailsLite{*d})
	if err != nil || ValidateSetupCallResolution(r, u, "20261120", 100) != nil {
		t.Fatal("exact standard call refused", r, err)
	}
	for _, change := range []func(*ResolvedOrderContract){
		func(r *ResolvedOrderContract) { r.UnderConID = 18 },
		func(r *ResolvedOrderContract) { r.UnderConID = 0 },
		func(r *ResolvedOrderContract) { r.Contract.Multiplier = 10 },
		func(r *ResolvedOrderContract) { r.Contract.TradingClass = "SYNTH1" },
		func(r *ResolvedOrderContract) { r.Contract.Strike = 101 },
		func(r *ResolvedOrderContract) { r.Contract.Right = "P" },
		func(r *ResolvedOrderContract) { r.Contract.LocalSymbol = "" },
	} {
		bad := r
		change(&bad)
		if ValidateSetupCallResolution(bad, u, "20261120", 100) == nil {
			t.Fatal("mismatched call admitted", bad)
		}
	}
	other := *d
	other.UnderConID = 18
	if _, err := exactOrderContract(want, []ContractDetailsLite{*d, other}); err == nil {
		t.Fatal("contradictory broker linkage was not ambiguous")
	}
}

func TestSetupOptionsWirePinsUnderlyingAndRequiresCompleteResponse(t *testing.T) {
	for _, mode := range []string{"complete", "cancel", "overflow"} {
		t.Run(mode, func(t *testing.T) {
			c, conn, out, _ := newMissTestConnector(t)
			c.mu.Lock()
			c.ready = true
			c.mu.Unlock()
			binding, ok := c.CaptureHistoricalSession()
			if !ok {
				t.Fatal("test session unavailable")
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			type result struct {
				rows map[string][]ExpiryClassedStrikes
				err  error
			}
			done := make(chan result, 1)
			go func() {
				rows, err := c.FetchSetupOptionStrikes(ctx, binding, Contract{ConID: 17, Symbol: "SYNTH", SecType: "STK", Currency: "USD", Exchange: "SMART"}, time.Second)
				done <- result{rows, err}
			}()
			var frame []string
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && frame == nil; {
				for _, f := range decodeOutboundFrames(t, conn, out.Bytes()) {
					if f[0] == strconv.Itoa(reqSecDefOptParams) {
						frame = f
					}
				}
				if frame == nil {
					time.Sleep(time.Millisecond)
				}
			}
			if len(frame) < 6 || frame[2] != "SYNTH" || frame[4] != "STK" || frame[5] != "17" {
				t.Fatal("underlying changed on wire", frame)
			}
			for _, identity := range [][3]string{{"18", "SYNTH", "100"}, {"17", "SYNTH1", "100"}, {"17", "SYNTH", "10"}, {"17", "SYNTH", "100"}} {
				fields := []string{strconv.Itoa(msgSecurityDefinitionOptionalParameter), frame[1], "SMART", identity[0], identity[1], identity[2], "1", "20261120", "1", "100", ""}
				conn.dispatchHandlers(msgSecurityDefinitionOptionalParameter, fields, conn.BrokerSessionEpoch())
			}
			if mode == "overflow" {
				for range 32 {
					conn.dispatchHandlers(msgSecurityDefinitionOptionalParameter, []string{strconv.Itoa(msgSecurityDefinitionOptionalParameter), frame[1], "SMART", "17", "SYNTH", "100", "1", "20261120", "1", "100", ""}, conn.BrokerSessionEpoch())
				}
			}
			if mode != "cancel" {
				conn.dispatchHandlers(msgSecurityDefinitionOptionalParameterEnd, []string{strconv.Itoa(msgSecurityDefinitionOptionalParameterEnd), frame[1], ""}, conn.BrokerSessionEpoch())
			} else {
				cancel()
			}
			got := <-done
			if mode == "cancel" {
				if !errors.Is(got.err, context.Canceled) || got.rows != nil {
					t.Fatal("partial cancellation became listing", got)
				}
				return
			}
			if mode == "overflow" {
				if got.err == nil || got.rows != nil {
					t.Fatal("aggregate overflow became partial listing", got)
				}
				return
			}
			if got.err != nil || len(got.rows) != 1 || len(got.rows["2026-11-20"]) != 1 || got.rows["2026-11-20"][0].TradingClass != "SYNTH" {
				t.Fatal("wrong-class or wrong-underlying listing admitted", got)
			}
		})
	}
}

func TestSetupOptionParameterBoundsBeforeCrossProduct(t *testing.T) {
	frame := func(expiries, strikes int) []string {
		out := []string{"75", "41", "SMART", "17", "SYNTH", "100", strconv.Itoa(expiries)}
		for range expiries {
			out = append(out, "20261120")
		}
		out = append(out, strconv.Itoa(strikes))
		for range strikes {
			out = append(out, "100")
		}
		return append(out, "")
	}
	if pairs, err := setupOptionParameterSize(frame(128, 512)); err != nil || pairs != 65536 {
		t.Fatal(pairs, err)
	}
	for _, fields := range [][]string{frame(129, 1), frame(1, 2049), frame(128, 513), {"75", "41", "SMART", "17", "SYNTH", "100", "128"}, {"75", "41", "SMART", "17", "SYNTH", "100", "1", "20261120", "1", "NaN", ""}} {
		if _, err := setupOptionParameterSize(fields); err == nil {
			t.Fatal("unbounded or malformed collector work admitted")
		}
	}
}
