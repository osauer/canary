package ibkr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

type prewarmReplyWriter struct{ reply func() }

func (w prewarmReplyWriter) Write(p []byte) (int, error) { w.reply(); return len(p), nil }

func prewarmTestContract() Contract {
	return Contract{Symbol: "SYNTH", SecType: "OPT", Expiry: "20991016", Exchange: "CBOE", Currency: "USD", TradingClass: "SYNTH", Multiplier: 100}
}
func prewarmTestReply(c *Connection) (int, []string) {
	c.reqIDMu.Lock()
	id := c.reqIDSeq - 1
	c.reqIDMu.Unlock()
	f := make([]string, 29)
	f[0] = strconv.Itoa(msgContractData)
	f[1] = strconv.Itoa(id)
	f[2] = "SYNTH"
	f[3] = "OPT"
	f[4] = "20991016"
	f[6] = "100"
	f[7] = "C"
	f[8] = "CBOE"
	f[9] = "USD"
	f[12] = "SYNTH"
	f[13] = "654321"
	f[15] = "100"
	f[21] = "CBOE"
	return id, f
}
func prewarmTestEnd(c *Connection, id int) {
	c.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(id)}, c.BrokerSessionEpoch())
}

func TestPrewarmEarlyCompletionAndWarmCache(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	sends := 0
	c.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		sends++
		id, f := prewarmTestReply(c)
		// Replies (including duplicate data/end) all arrive before send returns.
		c.dispatchHandlers(msgContractData, f, c.BrokerSessionEpoch())
		c.dispatchHandlers(msgContractData, f, c.BrokerSessionEpoch())
		prewarmTestEnd(c, id)
		prewarmTestEnd(c, id)
	}})
	for i := range 2 {
		result := c.PrewarmOptionChain(t.Context(), "SYNTH", []string{"20991016"}, "SYNTH", time.Second)[0]
		if result.Err != nil || result.Listed != 1 || result.Cached != 1-i || result.Dropped != 0 {
			t.Fatalf("pass %d: %+v", i, result)
		}
	}
	if sends != 2 {
		t.Fatalf("warm cache caused extra route attempts: %d", sends)
	}
}

func TestPrewarmRejectedRouteCompletesWithoutTimeout(t *testing.T) {
	for _, modern := range []bool{false, true} {
		t.Run(fmt.Sprint(modern), func(t *testing.T) {
			c, _ := newReadyWireTestConnection(t)
			c.writer = bufio.NewWriter(prewarmReplyWriter{func() {
				id, _ := prewarmTestReply(c)
				epoch := c.BrokerSessionEpoch()
				// Other request IDs, stale epochs and farm confirmations are not terminal.
				c.dispatchHandlers(msgErrMsg, []string{"4", "2", strconv.Itoa(id + 1), "200", "unrelated"}, epoch)
				c.dispatchHandlers(msgErrMsg, []string{"4", "2", strconv.Itoa(id), "200", "stale"}, epoch+1)
				c.dispatchHandlers(msgErrMsg, []string{"4", "2", strconv.Itoa(id), "2106", "informational"}, epoch)
				if modern {
					c.dispatchHandlers(msgSystemNotification, syntheticSystemNotice(id, 200), epoch)
				} else {
					c.dispatchHandlers(msgErrMsg, []string{"4", "2", strconv.Itoa(id), "200", "synthetic invalid route"}, epoch)
				}
			}})
			counts, err := c.prewarmOneExpiryAttempt(t.Context(), prewarmTestContract(), time.Second)
			if counts.listed != 0 || err == nil || !strings.Contains(err.Error(), "IBKR 200") {
				t.Fatalf("terminal rejection: %+v %v", counts, err)
			}
			c.handlersMu.RLock()
			defer c.handlersMu.RUnlock()
			for _, kind := range []int{msgContractData, msgContractDataEnd, msgErrMsg, msgSystemNotification} {
				if len(c.msgHandlers[kind]) != 0 {
					t.Fatalf("handler leak for %d", kind)
				}
			}
		})
	}
}

func TestPrewarmWrongScopeAndPartialResultsNeverClaimCompletion(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	c.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		id, f := prewarmTestReply(c)
		wrong := append([]string(nil), f...)
		wrong[12] = "OTHER"
		c.dispatchHandlers(msgContractData, wrong, c.BrokerSessionEpoch())
		wrong = append([]string(nil), f...)
		wrong[4] = "20991116"
		c.dispatchHandlers(msgContractData, wrong, c.BrokerSessionEpoch())
		c.dispatchHandlers(msgContractData, f, c.BrokerSessionEpoch()+1)
		c.dispatchHandlers(msgContractDataEnd, []string{strconv.Itoa(msgContractDataEnd), "1", strconv.Itoa(id + 1)}, c.BrokerSessionEpoch())
		c.dispatchHandlers(msgContractData, f, c.BrokerSessionEpoch())
	}})
	counts, err := c.prewarmOneExpiryAttempt(t.Context(), prewarmTestContract(), 10*time.Millisecond)
	if counts.listed != 1 || counts.cached != 1 || err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("partial result: %+v %v", counts, err)
	}
	if len(c.optionContractCache) != 1 {
		t.Fatal("wrong-scope detail cached")
	}
}

func TestPrewarmTruncationAndCancellationRemainFailures(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	c.writer = bufio.NewWriter(prewarmReplyWriter{func() {
		id, f := prewarmTestReply(c)
		for range 16_385 {
			c.dispatchHandlers(msgContractData, f, c.BrokerSessionEpoch())
		}
		prewarmTestEnd(c, id)
	}})
	counts, err := c.prewarmOneExpiryAttempt(t.Context(), prewarmTestContract(), time.Second)
	if err == nil || counts.dropped != 1 || counts.listed != 1 {
		t.Fatalf("truncation: %+v %v", counts, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, r := range c.PrewarmOptionChain(ctx, "SYNTH", []string{"20991016", "20991116", "20991216", "21000116", "21000216"}, "SYNTH", time.Second) {
		if !errors.Is(r.Err, context.Canceled) {
			t.Fatalf("cancelled fanout: %+v", r)
		}
	}
}

func TestRetainedOptionsSurviveResetAndPartialRestore(t *testing.T) {
	c, _ := newReadyWireTestConnection(t)
	connector := NewConnector(&ConnectorConfig{})
	t.Cleanup(connector.conn.rateLimiter.Stop)
	connector.conn = c
	connector.SeedContractDetails("SYNTH", ContractDetailsLite{ConID: 123456, Symbol: "SYNTH", SecType: "STK"})
	key := optionContractKey("SYNTH", "SYNTH", "20991016", 100, "C")
	detail := ContractDetailsLite{ConID: 654321, Symbol: "SYNTH", SecType: "OPT", TradingClass: "SYNTH", Expiry: "20991016", Strike: 100, Right: "C"}
	options := map[string]ContractDetailsLite{key: detail}
	connector.SeedOptionContracts(options)
	store := NewContractStore(t.TempDir())
	if err := store.Save(connector.SnapshotContracts(), options, "synthetic"); err != nil {
		t.Fatal(err)
	}
	c.invalidateUnstampedObservationAuthority()
	if err := store.SaveRetainingOptions(connector.SnapshotContracts(), connector.SnapshotOptionContracts(), "synthetic"); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.LoadOptions()
	if err != nil || len(loaded) != 1 {
		t.Fatalf("reset erased durable options: %d %v", len(loaded), err)
	}
	other := detail
	other.Right = "P"
	other.ConID++
	otherKey := optionContractKey("SYNTH", "SYNTH", "20991016", 100, "P")
	if err := store.SaveRetainingOptions(connector.SnapshotContracts(), map[string]ContractDetailsLite{otherKey: other}, "synthetic"); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadOptions()
	if err != nil || len(loaded) != 2 {
		t.Fatalf("partial restore erased options: %d %v", len(loaded), err)
	}
	detail.ConID++
	if err := store.SaveRetainingOptions(connector.SnapshotContracts(), map[string]ContractDetailsLite{key: detail}, "synthetic"); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadOptions()
	if err != nil || loaded[key].ConID != detail.ConID {
		t.Fatal("fresh tuple did not replace retained hint")
	}
	bad := detail
	bad.TradingClass = "OTHER"
	if err := store.SaveRetainingOptions(nil, map[string]ContractDetailsLite{key: bad}, "synthetic"); err == nil {
		t.Fatal("bad tuple published")
	}
	loaded, err = store.LoadOptions()
	if err != nil || len(loaded) != 2 {
		t.Fatal("failed save erased last good")
	}
}
