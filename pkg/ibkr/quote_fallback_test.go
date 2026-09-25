package ibkr

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func newQuoteFallbackFixture(t *testing.T) (*Connector, *Connection, *safeBuffer, Contract) {
	t.Helper()
	conn, c, out, _, _ := newQueuedInstructionReconnectFixture(t)
	conn.brokerSessionEpoch.Store(1)
	if err := c.SetMarketDataType(2); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.invalidateUnstampedConnectorObservations(conn) })
	return c, conn, out, Contract{ConID: 90123, Symbol: "SYNTH", SecType: "IND", Exchange: "NASDAQ", Currency: "USD"}
}
func rejectQuote(t *testing.T, c *Connector, key string, code int) {
	t.Helper()
	c.subMu.RLock()
	id := c.subscriptions[key].ReqID
	c.subMu.RUnlock()
	origin, ok := c.CaptureSession()
	if !ok {
		t.Fatal("session missing")
	}
	if work := c.recoverFromSystemNotice(origin, reqAliasEntry{secType: "IND"}, &systemNotification{tickerID: int64(id), code: code, message: "synthetic refusal"}); work != nil {
		work()
	}
}
func quoteTick(c *Connector, key string, tick int, price float64) {
	c.subMu.RLock()
	id := c.subscriptions[key].ReqID
	c.subMu.RUnlock()
	c.handleTickPrice([]string{"1", "1", fmt.Sprint(id), fmt.Sprint(tick), fmt.Sprint(price)})
}
func quoteType(c *Connector, key string, kind int) {
	c.subMu.RLock()
	id := c.subscriptions[key].ReqID
	c.subMu.RUnlock()
	c.conn.processMarketDataTypeAtEpoch([]string{"58", "1", fmt.Sprint(id), fmt.Sprint(kind)}, c.conn.BrokerSessionEpoch())
}
func TestSharedQuote354RecoversExactRouteAndRetainsDelayedEvidence(t *testing.T) {
	c, conn, out, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldID := c.subscriptions[key].ReqID
	rejectQuote(t, c, key, 354)
	if abs := c.MarketDataAbsences(); len(abs) != 1 || abs[0].Code != 354 || abs[0].FallbackDataType != 0 {
		t.Fatalf("recorded refusal hidden before any delayed price: %+v", abs)
	}
	sub := c.subscriptions[key]
	if sub.ReqID == oldID {
		t.Fatal("rejected request was not replaced")
	}
	select {
	case <-sub.RejectCh:
		t.Fatal("poller aborted before fallback")
	default:
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	n := len(frames)
	if n < 3 || strings.TrimRight(strings.Join(frames[n-3], ","), ",") != "59,1,4" || strings.TrimRight(strings.Join(frames[n-1], ","), ",") != "59,1,2" {
		t.Fatalf("missing bounded mode override: %v", frames)
	}
	request := frames[n-2]
	if request[3] != "90123" || request[4] != "SYNTH" || request[5] != "IND" || request[10] != "NASDAQ" || request[16] != delayedQuoteGenericTicks {
		t.Fatalf("route or ticks changed: %v", request)
	}
	quoteTick(c, key, 75, 100)
	if got := c.MarketDataSnapshot()[key].FeedType; got != 3 {
		t.Fatalf("delayed tick before callback classified %d", got)
	}
	quoteType(c, key, 4)
	md := c.MarketDataSnapshot()[key]
	if md.Close != 100 || md.FeedType != 4 || md.CloseAt.IsZero() {
		t.Fatalf("missing delayed close evidence: %+v", md)
	}
	abs := c.MarketDataAbsences()
	if len(abs) != 1 || abs[0].FallbackDataType != 4 || abs[0].FallbackReceivedAt.IsZero() {
		t.Fatalf("bad recovery status: %+v", abs)
	}
	if err := c.UnsubscribeMarketData(key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil); err != nil {
		t.Fatalf("live backoff suppressed usable delayed feed: %v", err)
	}
	if !c.subscriptions[key].delayedFallback {
		t.Fatal("did not reuse successful delayed mode inside live retry window")
	}
}

// TestShared354DuringFarmImpairmentDisclosesDelayedService witnesses the
// entitlement refusal that vanished from status and data health while the
// line kept serving delayed data, because an unrelated impaired farm vetoed
// the absence record. The veto itself must still keep live requests open, and
// the refusal must stay undisclosed until a delayed price is served: data
// health turns an empty quote shell that carries a 354 into a not_entitled
// ibkr:quotes problem, which a possibly transient farm-outage 354 is not.
func TestShared354DuringFarmImpairmentDisclosesDelayedService(t *testing.T) {
	c, _, _, contract := newQuoteFallbackFixture(t)
	c.dataFarmMu.Lock()
	c.dataFarms = map[string]DataFarmStatus{dataFarmKey("market", "usfuture"): {Name: "usfuture", Type: "market", Status: "broken"}}
	c.dataFarmMu.Unlock()
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	if !c.subscriptions[key].delayedFallback {
		t.Fatal("impaired-farm 354 did not fall back to delayed data")
	}
	if absent := c.marketDataAbsenceFor(key); absent != nil {
		t.Fatalf("impaired-farm 354 became a suppressing absence record: %v", absent)
	}
	if mode, err := c.sharedQuoteMode(key); mode != 0 || err != nil {
		t.Fatalf("disclosure gated a new live request: mode=%d err=%v", mode, err)
	}
	if abs := c.MarketDataAbsences(); len(abs) != 0 {
		t.Fatalf("vetoed 354 disclosed before any delayed price was served: %+v", abs)
	}
	quoteType(c, key, 4)
	if abs := c.MarketDataAbsences(); len(abs) != 0 {
		t.Fatalf("delayed mode without a price disclosed the vetoed 354: %+v", abs)
	}
	quoteTick(c, key, 75, 100)
	abs := c.MarketDataAbsences()
	if len(abs) != 1 || abs[0].Key != key || abs[0].Code != 354 || abs[0].FallbackDataType != 4 || abs[0].ObservedAt.IsZero() || !abs[0].RetryAt.Equal(abs[0].ObservedAt.Add(marketDataAbsenceRetry)) {
		t.Fatalf("delayed service lost its refusal evidence: %+v", abs)
	}
	c.absenceNow = func() time.Time { return abs[0].RetryAt }
	if len(c.MarketDataAbsences()) != 0 {
		t.Fatal("refusal evidence outlived its live re-probe window")
	}
	c.absenceNow = nil

	sub := c.subscriptions[key]
	origin, _ := c.CaptureSession()
	if err := c.replaceSharedQuote(t.Context(), origin, key, sub, sub.ReqID, 2); err != nil {
		t.Fatal(err)
	}
	if len(c.MarketDataAbsences()) != 1 {
		t.Fatal("pending live probe hid the delayed service it still shows")
	}
	quoteType(c, key, 1)
	quoteTick(c, key, 1, 102)
	if abs := c.MarketDataAbsences(); len(abs) != 0 {
		t.Fatalf("confirmed live recovery kept disclosing the refusal: %+v", abs)
	}
}
func TestSharedQuoteFailedDelayedAttemptBacksOff(t *testing.T) {
	c, conn, out, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	n := len(decodeOutboundFrames(t, conn, out.Bytes()))
	rejectQuote(t, c, key, 354)
	if len(decodeOutboundFrames(t, conn, out.Bytes())) != n {
		t.Fatal("recursive delayed retry")
	}
	if err := c.UnsubscribeMarketData(key); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil); err == nil {
		t.Fatal("failed delayed request did not back off")
	}
	if len(decodeOutboundFrames(t, conn, out.Bytes())) != n {
		t.Fatal("backoff leaked wire requests")
	}
}
func TestQuoteLiveProbeKeepsOldClockAndCannotMixDelayedPricesIntoLive(t *testing.T) {
	c, _, _, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	quoteType(c, key, 3)
	quoteTick(c, key, 68, 100)
	quoteTick(c, key, 67, 101)
	sub := c.subscriptions[key]
	timestamp := time.Now().Add(-20 * time.Minute).Truncate(time.Second)
	c.handleTickString([]string{"46", "1", fmt.Sprint(sub.ReqID), "88", fmt.Sprint(timestamp.Unix())})
	old := c.MarketDataSnapshot()[key]
	if !old.LastTradeTime.Equal(timestamp) {
		t.Fatal("delayed timestamp ignored")
	}
	origin, _ := c.CaptureSession()
	if err := c.replaceSharedQuote(t.Context(), origin, key, sub, sub.ReqID, 2); err != nil {
		t.Fatal(err)
	}
	quoteType(c, key, 1)
	pending := c.MarketDataSnapshot()[key]
	if pending.FeedType != 3 || pending.Last != 100 || !pending.LastAt.Equal(old.LastAt) || !pending.LastTradeTime.Equal(timestamp) {
		t.Fatal("live probe relabeled or refreshed retained delayed evidence")
	}
	for _, tick := range []int{6, 20, 225} {
		quoteTick(c, key, tick, 103)
		pending = c.MarketDataSnapshot()[key]
		if pending.FeedType != 3 || pending.Last != 100 || !pending.LastAt.Equal(old.LastAt) || !sub.LastPriceTickAt.IsZero() || sub.previousQuote == nil {
			t.Fatal("statistics-only live probe discarded the delayed quote or stopped recovery")
		}
		if len(c.MarketDataAbsences()) == 0 {
			t.Fatal("statistics-only live probe cleared the access restriction")
		}
	}
	quoteTick(c, key, 1, 102)
	live := c.MarketDataSnapshot()[key]
	if live.FeedType != 1 || live.Bid != 102 || live.Ask != 0 || live.Last != 0 {
		t.Fatalf("mixed clocks: %+v", live)
	}
	if len(c.MarketDataAbsences()) != 0 {
		t.Fatal("confirmed live recovery did not clear refusal")
	}
}
func TestQuoteFallbackDoesNotReplayExactOrRetiredSubscriptions(t *testing.T) {
	c, conn, out, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	sub := c.subscriptions[key]
	sub.SessionEpoch = conn.BrokerSessionEpoch()
	n := len(decodeOutboundFrames(t, conn, out.Bytes()))
	rejectQuote(t, c, key, 354)
	if sub.ReqID != sub.rejectedReqID || len(decodeOutboundFrames(t, conn, out.Bytes())) != n {
		t.Fatal("exact quote auto-replayed")
	}
	sub.SessionEpoch = 0
	origin, _ := c.CaptureSession()
	conn.brokerSessionEpoch.Add(1)
	if err := c.replaceSharedQuote(t.Context(), origin, key, sub, sub.ReqID, 4); err == nil {
		t.Fatal("retired socket replayed")
	}
	if len(decodeOutboundFrames(t, conn, out.Bytes())) != n {
		t.Fatal("retired replay wrote")
	}
}
func TestQuoteModeOverrideCannotLeakToConcurrentRequest(t *testing.T) {
	c, conn, out, contract := newQuoteFallbackFixture(t)
	origin, _ := c.CaptureSession()
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			mode := 0
			if i%2 == 0 {
				mode = 4
			}
			_, err := conn.requestMarketDataWithContractForEpochMode(context.Background(), contract, "", false, false, origin.epoch, false, mode, nil)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())
	overrides := 0
	for i := 1; i < len(frames); i++ {
		if strings.TrimRight(strings.Join(frames[i], ","), ",") == "59,1,4" {
			if i+2 >= len(frames) || frames[i+1][0] != "1" || strings.TrimRight(strings.Join(frames[i+2], ","), ",") != "59,1,2" {
				t.Fatal("another request entered temporary delayed mode")
			}
			overrides++
			i += 2
		} else if frames[i][0] != "1" {
			t.Fatalf("unexpected frame %v", frames[i])
		}
	}
	if overrides != 8 {
		t.Fatalf("overrides=%d", overrides)
	}
}

func TestQuoteBackendRecoveryRetainsDelayedMode(t *testing.T) {
	c, conn, out, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	quoteType(c, key, 4)
	quoteTick(c, key, 75, 100)
	old := c.MarketDataSnapshot()[key]
	origin, _ := c.CaptureSession()
	n := len(decodeOutboundFrames(t, conn, out.Bytes()))
	replayed, dropped := c.replayMarketDataSubscriptions(origin)
	if replayed != 1 || dropped != 0 {
		t.Fatalf("replayed=%d dropped=%d", replayed, dropped)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())[n:]
	if len(frames) != 3 || frames[0][0] != "59" || frames[0][2] != "4" {
		t.Fatalf("lost delayed request mode: %v", frames)
	}
	md := c.MarketDataSnapshot()[key]
	if md.Close != 100 || md.FeedType != 4 || !md.CloseAt.Equal(old.CloseAt) {
		t.Fatal("backend recovery lost delayed cached value or clock")
	}
}

type failedQuoteModeWrite struct {
	output *safeBuffer
	prefix int
}

func (w *failedQuoteModeWrite) Write(data []byte) (int, error) {
	n, _ := w.output.Write(data[:min(w.prefix, len(data))])
	return n, errors.New("synthetic failure after mode frame")
}

func TestQuoteFailedModeRestoreIsRepairedBeforeNextRequest(t *testing.T) {
	_, conn, out, contract := newQuoteFallbackFixture(t)
	epoch := conn.BrokerSessionEpoch()
	// Accept only the complete mode-4 frame, then fail before the quote and
	// restore frames reach the wire. The next request must repair that mode.
	conn.writer = bufio.NewWriter(&failedQuoteModeWrite{output: out, prefix: 4 + len(conn.encodeMsg(reqMarketDataType, 1, 4))})
	if _, err := conn.requestMarketDataWithContractForEpochMode(t.Context(), contract, "", false, false, epoch, false, 4, nil); err == nil {
		t.Fatal("injected wire failure not reported")
	}
	conn.writer = bufio.NewWriter(out)
	n := len(decodeOutboundFrames(t, conn, out.Bytes()))
	if _, err := conn.RequestMarketDataWithContract(t.Context(), contract, "", false, false); err != nil {
		t.Fatal(err)
	}
	frames := decodeOutboundFrames(t, conn, out.Bytes())[n:]
	if len(frames) != 3 || frames[0][0] != "59" || frames[0][2] != "2" || frames[1][0] != "1" {
		t.Fatalf("mode repair missing: %v", frames)
	}
}

// TestDelayedLineFromRecordedRefusalReprobesWhenRefusalLifts witnesses the
// empty status.market_data_access beside an index still served delayed. A
// line opened on the delayed feed because of a recorded 354 armed its live
// probe a full window after it was opened, while status names the refusal
// only for the window after the 354: for up to half an hour the line stayed
// delayed with no refusal named. The probe now runs when the refusal lifts.
func TestDelayedLineFromRecordedRefusalReprobesWhenRefusalLifts(t *testing.T) {
	c, _, _, contract := newQuoteFallbackFixture(t)
	key, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil)
	if err != nil {
		t.Fatal(err)
	}
	rejectQuote(t, c, key, 354)
	quoteType(c, key, 4)
	quoteTick(c, key, 75, 100)
	if err := c.UnsubscribeMarketData(key); err != nil {
		t.Fatal(err)
	}
	// The line is reopened twenty minutes into the refusal's window.
	c.absenceMu.Lock()
	refusal := c.mktDataAbsent[key]
	refusal.at = time.Now().Add(-20 * time.Minute)
	c.mktDataAbsent[key] = refusal
	c.absenceMu.Unlock()
	if _, err := c.SubscribeMarketDataWithContract(t.Context(), contract, nil); err != nil {
		t.Fatal(err)
	}
	c.subMu.RLock()
	sub := c.subscriptions[key]
	delayed, probeAt := sub.delayedFallback, sub.liveRetryAt
	c.subMu.RUnlock()
	lifts := refusal.at.Add(marketDataAbsenceRetry)
	if !delayed || !probeAt.Equal(lifts) {
		t.Fatalf("delayed line re-probes live at %s; its refusal stops being named at %s", probeAt.Format(time.TimeOnly), lifts.Format(time.TimeOnly))
	}
	if abs := c.MarketDataAbsences(); len(abs) != 1 || !abs[0].RetryAt.Equal(probeAt) {
		t.Fatalf("status does not name the refusal until the probe: %+v", abs)
	}
}
