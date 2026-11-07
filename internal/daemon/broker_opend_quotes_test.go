package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/broker"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// bookOfNames builds a book holding count names on the simulated account, plus the
// names the regime streams.
func bookOfNames(t *testing.T, count, quota int) *futusim.Book {
	t.Helper()
	var instruments, positions strings.Builder
	for i := range count {
		code := fmt.Sprintf("HK.%05d", i+1)
		fmt.Fprintf(&instruments, `{"code":%q,"name":"SYNTH","lot_size":100,"currency":"HKD","start":10,"tick":0.01},`, code)
		if i > 0 {
			positions.WriteString(",")
		}
		fmt.Fprintf(&positions, `{"acc_id":1000001,"code":%q,"quantity":100,"cost":9.5}`, code)
	}
	raw := fmt.Sprintf(`{"clock":"2026-11-04T01:30:00Z","subscription_quota":%d,"fx":{"HKD":1},
		"accounts":[{"acc_id":1000001,"env":"simulate","markets":["HK","US"],"cash":{"HKD":54321.09}}],
		"instruments":[%s{"code":"US.SPY","name":"SYNTH","lot_size":1,"currency":"USD","start":500,"tick":0.01},
		{"code":"US.QQQ","name":"SYNTH","lot_size":1,"currency":"USD","start":400,"tick":0.01},
		{"code":"US.HYG","name":"SYNTH","lot_size":1,"currency":"USD","start":80,"tick":0.01}],
		"positions":[%s]}`, quota, instruments.String(), positions.String())
	book, err := futusim.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return book
}

// A book past the quota streams the held names first, then the regime's, and reads
// the rest by snapshot, and each mark says which it was.
func TestLinesGoToTheBookThenTheRegimeAndTheRestAreSnapshots(t *testing.T) {
	book := bookOfNames(t, 3, 5)
	sim := futusim.Start(t, futusim.Options{Book: book})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.refreshOpenDLines(ctx, []string{"HK.00001", "HK.00002", "HK.00003"})
	lines := srv.opend.Connector().Lines()
	for _, code := range []string{"HK.00001", "HK.00002", "HK.00003", "US.SPY", "US.QQQ"} {
		if _, ok := lines[code]; !ok {
			t.Errorf("no line for %s: %v", code, lines)
		}
	}
	if _, ok := lines["US.HYG"]; ok {
		t.Fatalf("a line past the quota: %v", lines)
	}
	// The daemon's own session loop records into the budget too.
	srv.opendBudget.mu.Lock()
	reason := srv.opendBudget.reason
	srv.opendBudget.mu.Unlock()
	if reason == "" {
		t.Fatal("the budget does not say why a wanted name has no line")
	}

	// A new holding takes a line: a regime name yields after its minimum hold.
	book.Clock.Advance(futu.SubscriptionMinimumHold)
	srv.refreshOpenDLines(ctx, []string{"HK.00001", "HK.00002", "HK.00003", "US.SPY", "US.HYG"})
	lines = srv.opend.Connector().Lines()
	if _, ok := lines["US.HYG"]; !ok {
		t.Fatalf("the new holding has no line: %v", lines)
	}
	if _, ok := lines["US.QQQ"]; ok {
		t.Fatalf("the regime name kept its line over a holding: %v", lines)
	}
}

// A release inside the minimum hold leaves the line; the next refresh past the hold
// releases it.
func TestARegimeLineInsideItsHoldWaitsForTheNextRefresh(t *testing.T) {
	book := bookOfNames(t, 1, 2)
	sim := futusim.Start(t, futusim.Options{Book: book})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.refreshOpenDLines(ctx, []string{"HK.00001"})
	if _, ok := srv.opend.Connector().Lines()["US.SPY"]; !ok {
		t.Fatalf("lines %v", srv.opend.Connector().Lines())
	}
	srv.refreshOpenDLines(ctx, []string{"HK.00001", "US.QQQ"})
	if _, ok := srv.opend.Connector().Lines()["US.SPY"]; !ok {
		t.Fatal("a line was released inside its hold")
	}
	book.Clock.Advance(futu.SubscriptionMinimumHold)
	srv.refreshOpenDLines(ctx, []string{"HK.00001", "US.QQQ"})
	lines := srv.opend.Connector().Lines()
	if _, ok := lines["US.QQQ"]; !ok {
		t.Fatalf("the holding has no line after the hold: %v", lines)
	}
	if _, ok := lines["US.SPY"]; ok {
		t.Fatalf("the regime line outlived its hold: %v", lines)
	}
}

// A quota another client spent turns the next stream acquire into a snapshot with
// the typed reason, and the seam serves the name anyway.
func TestQuotaSpentElsewhereServesTheNameBySnapshot(t *testing.T) {
	book := bookOfNames(t, 2, 100)
	// Spent before the session starts: the daemon reads the book as soon as OpenD
	// is ready, and a line taken then would serve the name.
	book.SetSubscriptionQuota(0)
	sim := futusim.Start(t, futusim.Options{Book: book})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	srv.opendBudget.record([]string{"HK.00001"}, "")
	_, _, err := srv.opend.Subscribe(ctx, broker.Instrument{ID: "HK.00001"}, nil)
	if !errors.Is(err, broker.ErrSnapshotOnly) || !errors.Is(err, futu.ErrQuotaExhausted) {
		t.Fatalf("subscribe past the quota: %v", err)
	}
	srv.opendBudget.record(nil, "")
	if _, _, err := srv.opend.Subscribe(ctx, broker.Instrument{ID: "HK.00002"}, nil); !errors.Is(err, broker.ErrSnapshotOnly) {
		t.Fatalf("a name outside the budget: %v", err)
	}
	_, release, err := srv.subs.HoldContract(ctx, ibkrlib.Contract{Symbol: "00002", SecType: "STK", Exchange: "SEHK", Currency: "HKD"})
	if !errors.Is(err, broker.ErrSnapshotOnly) {
		t.Fatalf("the manager's acquire: %v", err)
	}
	release(ctx)

	quote, err := srv.opendQuote(ctx, rpc.QuoteSnapshotParams{Contract: rpc.ContractParams{Symbol: "00002"}})
	if err != nil || quote.Delivery != "snapshot" || quote.Last == nil {
		t.Fatalf("quote %+v %v", quote, err)
	}
}

// Delivery and PriceSource say which marks came from a batch, and the brief's
// overview names the split only under OpenD.
func TestSnapshotMarksAreNamedAndTheBriefCountsThem(t *testing.T) {
	book := bookOfNames(t, 130, 100)
	sim := futusim.Start(t, futusim.Options{Book: book})
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	positions, err := srv.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if len(positions.Stocks) != 130 {
		t.Fatalf("%d rows", len(positions.Stocks))
	}
	if got := len(srv.opend.Connector().Lines()); got != 100 {
		t.Fatalf("%d lines, want the quota's 100", got)
	}
	streamed, batched := 0, 0
	for _, row := range positions.Stocks {
		switch row.PriceSource {
		case "snapshot":
			batched++
		case "":
			streamed++
		default:
			t.Fatalf("price source %q", row.PriceSource)
		}
		if row.Mark <= 0 {
			t.Fatalf("row without a mark: %+v", row)
		}
	}
	// A line has no quote until OpenD pushes one, so the first read batches them all.
	if batched != 130 || streamed != 0 {
		t.Fatalf("first read: %d batched, %d streamed", batched, streamed)
	}
	sim.Book().Clock.Advance(5 * time.Second)
	sim.PushQuotes()
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.opend.Snapshot()) < 100 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	positions, err = srv.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	streamed, batched = 0, 0
	for _, row := range positions.Stocks {
		if row.PriceSource == "snapshot" {
			batched++
		} else {
			streamed++
		}
	}
	if batched != 30 || streamed != 100 {
		t.Fatalf("second read: %d batched, %d streamed", batched, streamed)
	}
	if _, err := srv.opend.Connector().RefreshLoad(ctx); err != nil {
		t.Fatal(err)
	}
	if got := srv.opendQuoteSentence(); got != "30 of 130 quotes are snapshots; 100 of 100 subscriptions in use" {
		t.Fatalf("sentence %q", got)
	}
	res, err := srv.handleBriefSnapshot(ctx, &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !coverageNames(res, "quotes are snapshots") {
		t.Fatalf("coverage %+v", res.Narrative.Overview.Coverage)
	}
	// IBKR's quote names no delivery.
	if strings.Contains(string(mustJSON(rpc.Quote{Symbol: "SPY"})), `"delivery"`) {
		t.Fatal("a quote with no delivery serialises one")
	}
}

// fakeHeldQuotes refuses the first release as held, then accepts it.
type fakeHeldQuotes struct {
	fakeConnector
	releases chan struct{}
	held     bool
}

func (f *fakeHeldQuotes) UnsubscribeForSession(context.Context, broker.SessionBinding, string) error {
	if !f.held {
		f.held = true
		return &broker.ReleaseHeldError{Key: "HK.00001", RetryAfter: 10 * time.Millisecond}
	}
	f.releases <- struct{}{}
	return nil
}

// A release inside the broker's minimum hold is deferred, not lost.
func TestManagerDefersAReleaseInsideTheHold(t *testing.T) {
	quotes := &fakeHeldQuotes{releases: make(chan struct{}, 1)}
	m := newSubManager(func() marketQuotes { return quotes })
	entry := &subEntry{sym: "HK.00001", owner: quotes, binding: fakeSession, taps: map[*frameTap]struct{}{}, stop: make(chan struct{})}
	m.teardown(entry, "HK.00001")
	select {
	case <-quotes.releases:
	case <-time.After(2 * time.Second):
		t.Fatal("the deferred release never ran")
	}
}

// A market this login has no right for keeps its own holdings priced by OpenD's
// position and does not cost the other markets their quotes; a read of it says
// which right is missing, not that the connection is down.
func TestAMarketWithoutAQuoteRightIsNamedAndLeavesTheOthers(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	sim.SetQuoteRight("SG", 5) // QotRight_No
	srv, stop := startOpenDServer(t, sim, opendTestAccount)
	defer stop()
	waitOpenDReady(t, srv)
	deadline := time.Now().Add(5 * time.Second)
	for srv.opend.Connector().Health().QuoteRights["SG"] != "No" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	positions, err := srv.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range positions.Stocks {
		seen[row.Currency] = true
		if row.Mark <= 0 {
			t.Errorf("row without a price: %+v", row)
		}
		if row.Currency == "SGD" && row.PriceSource != "position" {
			t.Errorf("SG row source %q", row.PriceSource)
		}
		if row.Currency != "SGD" && row.PriceSource == "position" {
			t.Errorf("%s lost its quote", row.Symbol)
		}
	}
	if !seen["SGD"] || !seen["HKD"] {
		t.Fatalf("the book lost its mix: %v", seen)
	}
	if got := srv.opendQuoteSentence(); !strings.Contains(got, "no SG quote right") {
		t.Errorf("brief sentence %q", got)
	}
	res := dispatchRaw(t, srv, rpc.MethodQuoteSnapshot, `{"contract":{"instrument":{"broker":"opend","id":"SG.D05"}},"timeout_ms":2000}`)
	if res.Ok || res.Error.Code != rpc.CodeSymbolInactive || !strings.Contains(res.Error.Message, "no SG quote right") {
		t.Errorf("quote: %+v", res)
	}
}
