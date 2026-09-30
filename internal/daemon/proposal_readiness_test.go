package daemon

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/marketcal"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// fixtureCEST1519 is Monday 28 September 2026 15:19 CEST: 09:19 in New York,
// eleven minutes before the regular US open at 13:30Z.
var fixtureCEST1519 = time.Date(2026, 9, 28, 13, 19, 0, 0, time.UTC)

func officialCalendar(m marketcal.Market, at time.Time) (marketcal.Session, error) {
	return marketcal.New().SessionAt(m, at)
}

// readinessPreviewServer is a paper preview server at a fixed instant on the
// official calendar. Its fake broker counts the quotes it is asked for.
func readinessPreviewServer(t *testing.T, dir string, at time.Time) (*Server, *atomic.Int32) {
	t.Helper()
	srv := newOrderPreviewTestServerIn(t, config.Trading{Mode: config.TradingModePaper}, dir)
	srv.now = func() time.Time { return at }
	srv.previewSessionAt = officialCalendar
	// No regime stage observed (the calm reading), never a stage from disk.
	srv.rulesRegimeStageLoaded = true
	quotes := new(atomic.Int32)
	bid, ask := 2.05, 2.15
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		quotes.Add(1)
		return rpc.OrderQuoteSnapshot{Symbol: "AAA", Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive}, nil
	}
	srv.orderPreviewPositionImpact = fixedPreviewPosition(4, 2, rpc.OrderPositionEffectReduce)
	return srv, quotes
}

func readinessOption() rpc.ContractParams {
	return rpc.ContractParams{ConID: 700001, Symbol: "AAA", SecType: "OPT", Currency: "USD", Exchange: "SMART", Expiry: "20261120", Right: "C", Strike: 50, Multiplier: 100, TradingClass: "AAA"}
}

func readinessRow(bucket string) rpc.TradeProposal {
	return rpc.TradeProposal{Key: bucket + ":synthetic", Revision: "sha256:synthetic", State: rpc.TradeProposalStateGenerated, Bucket: bucket,
		Symbol: "AAA", SecType: "OPT", Action: rpc.OrderActionSell, Quantity: 2, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay,
		PositionEffect: rpc.OrderPositionEffectReduce, Contract: readinessOption()}
}

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

func TestPreviewSessionGateRefusesClosedMarketBeforeQuote(t *testing.T) {
	srv, quotes := readinessPreviewServer(t, t.TempDir(), fixtureCEST1519)
	_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Contract: readinessOption(), Quantity: 2})
	blockers := previewFailureBlockers(err)
	if err == nil || len(blockers) != 1 || blockers[0].Code != previewMarketClosedCode || !strings.Contains(blockers[0].Message, "opens 2026-09-28T13:30:00Z") {
		t.Fatalf("patient limit before the open: err %v, blockers %+v; want one market_closed blocker dated 13:30Z", err, blockers)
	}
	if code, _ := classifyError(err); code != rpc.CodeBadRequest {
		t.Fatalf("typed refusal changed the RPC error class to %q", code)
	}
	if quotes.Load() != 0 {
		t.Fatalf("a closed session still requested %d quotes", quotes.Load())
	}
	// An explicit limit needs no live mid, so it passes the gate to its quote.
	limit := 2.10
	_, err = srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Contract: readinessOption(), Quantity: 2, LimitPrice: &limit})
	if quotes.Load() != 1 {
		t.Fatalf("explicit limit asked for %d quotes (err %v), want 1", quotes.Load(), err)
	}
}

// authorityFingerprint identifies every durable write of the authority: its
// head (event sequence and generation) and the bytes of the database and WAL.
func authorityFingerprint(t *testing.T, core *corestore.Store, dbPath string) string {
	t.Helper()
	head, err := core.AuthorityHead(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%+v", head)
	for _, suffix := range []string{"", "-wal"} {
		raw, err := os.ReadFile(dbPath + suffix)
		if errors.Is(err, fs.ErrNotExist) {
			b.WriteString(" absent")
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&b, " %x", sha256.Sum256(raw))
	}
	return b.String()
}

func readDecisionLines(t *testing.T, path string) []decisionEvent {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []decisionEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ev decisionEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			t.Fatalf("decision line %q: %v", scanner.Text(), err)
		}
		out = append(out, ev)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// The phase-1 witness: at 15:19 CEST a governor prepare is refused as
// market_closed opening at 13:30Z, before any quote request; the refusal
// writes exactly one decision line at log level warn, and daemon.db, the
// record of authority, is unchanged.
func TestPrepareAtClosedMarketRecordsOneDecisionOutsideAuthority(t *testing.T) {
	dir := t.TempDir()
	srv, quotes := readinessPreviewServer(t, dir, fixtureCEST1519)
	var daemonLog bytes.Buffer
	srv.logger = NewLogger(&daemonLog, "warn")
	dbPath := testOrderAuthorityPath(filepath.Join(dir, "order-journal.jsonl"))
	srv.decisions = newDecisionLog(dbPath)
	t.Cleanup(func() { _ = srv.decisions.Close() })
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), srv.coreStore); err != nil {
		t.Fatal(err)
	}
	prop := readinessRow(rpc.TradeProposalBucketBudgetReduction)
	engine := &proposalEngine{server: srv, now: srv.now, store: &proposalStore{},
		resolve: func(context.Context, string, string) (rpc.TradeProposal, []rpc.TradingBlocker, error) {
			return prop, nil, nil
		}}
	if _, _, err := engine.store.bindCore(t.Context(), srv.coreStore); err != nil {
		t.Fatal(err)
	}
	before := authorityFingerprint(t, srv.coreStore, dbPath)

	out, err := engine.Prepare(t.Context(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: prop.Revision})
	if err != nil || out.Accepted || out.PreparedRef != "" || out.Preparation != nil {
		t.Fatalf("closed-market prepare: accepted %v, preparation %v, err %v", out.Accepted, out.Preparation, err)
	}
	if len(out.Blockers) != 1 || out.Blockers[0].Code != previewMarketClosedCode {
		t.Fatalf("blockers = %+v, want market_closed", out.Blockers)
	}
	r := out.Readiness
	opens, send := utc(2026, 9, 28, 13, 30), utc(2026, 9, 28, 13, 45)
	if r == nil || r.Code != rpc.ReadinessMarketClosed || r.Market != string(marketcal.MarketUSOptions) || r.SessionState != rpc.ReadinessSessionPreOpen ||
		r.OpensAt == nil || !r.OpensAt.Equal(opens) || r.DefaultSendAt == nil || !r.DefaultSendAt.Equal(send) || !r.Queueable ||
		!slices.Equal(r.CanaryCodes, []string{previewMarketClosedCode}) {
		t.Fatalf("readiness = %+v, want market_closed opening 13:30Z, default send 13:45Z, queueable", r)
	}
	if quotes.Load() != 0 {
		t.Fatalf("closed-market prepare requested %d quotes", quotes.Load())
	}
	if after := authorityFingerprint(t, srv.coreStore, dbPath); after != before {
		t.Fatalf("daemon.db changed:\nbefore %s\nafter  %s", before, after)
	}
	lines := readDecisionLines(t, filepath.Join(filepath.Dir(dbPath), decisionLogFile))
	if len(lines) != 1 {
		t.Fatalf("decision lines = %d, want 1: %+v", len(lines), lines)
	}
	ev := lines[0]
	if ev.Svc != "canary" || ev.Event != "proposal.prepare" || ev.Outcome != decisionBlocked || ev.Code != rpc.ReadinessMarketClosed ||
		!slices.Equal(ev.Codes, []string{previewMarketClosedCode}) || ev.IDs.Key != prop.Key || ev.IDs.Rev != prop.Revision ||
		ev.Bucket != prop.Bucket || ev.Market != "us_options" || ev.Session != rpc.ReadinessSessionPreOpen || ev.OpensAt == nil || !ev.OpensAt.Equal(opens) ||
		!strings.Contains(ev.Reason, "requires an open market session") {
		t.Fatalf("decision line = %+v", ev)
	}
}

// A prepared submission refused before anything is sent is recorded too, and
// its line never carries the private reference.
func TestPreparedSubmitRefusalRecordsLineWithoutReference(t *testing.T) {
	dir := t.TempDir()
	srv, _ := readinessPreviewServer(t, dir, utc(2026, 9, 28, 14, 0))
	dbPath := testOrderAuthorityPath(filepath.Join(dir, "order-journal.jsonl"))
	srv.decisions = newDecisionLog(dbPath)
	t.Cleanup(func() { _ = srv.decisions.Close() })
	engine := &proposalEngine{server: srv, now: srv.now}
	reference := preparedProposalPrefix + ".AAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAA"
	out, err := engine.Submit(t.Context(), rpc.TradeProposalSubmitParams{Key: "budget_reduction:synthetic", Revision: "sha256:synthetic", PreparedRef: reference, FastPath: true, Origin: rpc.OrderOriginHumanTTY})
	if err != nil || out.Accepted || len(out.Blockers) != 1 || out.Blockers[0].Code != "prepared_reference_unavailable" ||
		out.Readiness == nil || out.Readiness.Code != rpc.ReadinessNotExecutable {
		t.Fatalf("unknown reference: %+v, err %v", out, err)
	}
	path := filepath.Join(filepath.Dir(dbPath), decisionLogFile)
	lines := readDecisionLines(t, path)
	if len(lines) != 1 || lines[0].Event != "proposal.submit_prepared" || lines[0].Outcome != decisionBlocked ||
		!slices.Equal(lines[0].Codes, []string{"prepared_reference_unavailable"}) || lines[0].IDs.Key != "budget_reduction:synthetic" {
		t.Fatalf("decision lines = %+v", lines)
	}
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte(reference)) || bytes.Contains(raw, []byte(preparedProposalPrefix)) {
		t.Fatalf("decision log carries the private reference (err %v)", err)
	}
}

func TestProposalReadinessClassification(t *testing.T) {
	trail := readinessRow(rpc.TradeProposalBucketTrailingStop)
	trail.OrderType, trail.TIF, trail.SecType = rpc.OrderTypeTRAIL, rpc.OrderTIFGTC, "STK"
	trail.Contract = rpc.ContractParams{ConID: 700002, Symbol: "BBB", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	trail.Trail = &rpc.OrderTrailSpec{OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: new(8.0), InitialStopPrice: 11.5}
	stock := readinessRow(rpc.TradeProposalBucketRiskReduction)
	stock.SecType, stock.Contract = "STK", rpc.ContractParams{ConID: 700003, Symbol: "BBB", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	shadow := readinessRow(rpc.TradeProposalBucketBudgetReduction)
	shadow.Shadow = true
	lossExit := readinessRow(rpc.TradeProposalBucketOptionLossExit)
	blocker := func(code string) rpc.TradingBlocker {
		return rpc.TradingBlocker{Code: code, Message: code + " message"}
	}
	governor := readinessRow(rpc.TradeProposalBucketBudgetReduction)

	cases := []struct {
		name      string
		at        time.Time
		prop      rpc.TradeProposal
		blockers  []rpc.TradingBlocker
		live      bool
		code      string
		session   string
		opens     time.Time
		send      time.Time
		queueable bool
		message   string
	}{
		{name: "15:19 CEST before the open", at: fixtureCEST1519, prop: governor, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionPreOpen,
			opens: utc(2026, 9, 28, 13, 30), send: utc(2026, 9, 28, 13, 45), queueable: true},
		{name: "opening window", at: utc(2026, 9, 28, 13, 34), prop: governor, code: rpc.ReadinessOpeningWindow, session: rpc.ReadinessSessionOpen,
			opens: utc(2026, 9, 28, 13, 30), send: utc(2026, 9, 28, 13, 45), queueable: true},
		{name: "open after the window", at: utc(2026, 9, 28, 14, 0), prop: governor, code: rpc.ReadinessReady, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30)},
		{name: "loss exit waits for a live bid", at: fixtureCEST1519, prop: lossExit, blockers: []rpc.TradingBlocker{blocker("option_rth_closed"), blocker("live_option_quote_required")},
			code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionPreOpen, opens: utc(2026, 9, 28, 13, 30), send: utc(2026, 9, 28, 13, 45), message: "option_rth_closed message"},
		{name: "hard blocker outranks a closed market", at: fixtureCEST1519, prop: lossExit, blockers: []rpc.TradingBlocker{blocker("option_rth_closed"), blocker("directional_intent_required")},
			code: rpc.ReadinessNotExecutable, session: rpc.ReadinessSessionPreOpen, opens: utc(2026, 9, 28, 13, 30), message: "directional_intent_required message"},
		{name: "wide spread", at: utc(2026, 9, 28, 14, 0), prop: lossExit, blockers: []rpc.TradingBlocker{blocker("option_spread_too_wide")},
			code: rpc.ReadinessSpreadTooWide, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30), message: "option_spread_too_wide message"},
		{name: "stale quote", at: utc(2026, 9, 28, 14, 0), prop: lossExit, blockers: []rpc.TradingBlocker{blocker(previewQuoteStaleCode)},
			code: rpc.ReadinessQuoteUnusable, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30), message: previewQuoteStaleCode + " message"},
		{name: "halt", at: utc(2026, 9, 28, 14, 0), prop: governor, blockers: []rpc.TradingBlocker{blocker("market_event_halt_regulatory_or_news")},
			code: rpc.ReadinessHalted, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30), message: "market_event_halt_regulatory_or_news message"},
		{name: "freeze", at: utc(2026, 9, 28, 14, 0), prop: governor, blockers: []rpc.TradingBlocker{blocker(tradingFrozenBlockerCode)},
			code: rpc.ReadinessTradingFrozen, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30), message: tradingFrozenBlockerCode + " message"},
		{name: "broker link down on a row", at: utc(2026, 9, 28, 14, 0), prop: governor, live: true,
			code: rpc.ReadinessBrokerUnavailable, session: rpc.ReadinessSessionOpen, opens: utc(2026, 9, 28, 13, 30), message: "the broker link to IBKR Gateway/TWS is not ready"},
		{name: "seeded trail needs no session", at: fixtureCEST1519, prop: trail, code: rpc.ReadinessReady, session: rpc.ReadinessSessionPreOpen, opens: utc(2026, 9, 28, 13, 30)},
		{name: "stock after the close", at: utc(2026, 9, 28, 20, 30), prop: stock, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionAfterClose,
			opens: utc(2026, 9, 29, 13, 30), send: utc(2026, 9, 29, 13, 35), queueable: true},
		{name: "weekend", at: utc(2026, 10, 3, 12, 0), prop: governor, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionClosed,
			opens: utc(2026, 10, 5, 13, 30), send: utc(2026, 10, 5, 13, 45), queueable: true},
		{name: "Thanksgiving", at: utc(2026, 11, 26, 15, 0), prop: governor, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionHoliday,
			opens: utc(2026, 11, 27, 14, 30), send: utc(2026, 11, 27, 14, 45), queueable: true},
		{name: "Berlin on CET, New York on EDT", at: utc(2026, 10, 26, 13, 19), prop: governor, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionPreOpen,
			opens: utc(2026, 10, 26, 13, 30), send: utc(2026, 10, 26, 13, 45), queueable: true},
		{name: "shadow rows never queue", at: fixtureCEST1519, prop: shadow, code: rpc.ReadinessMarketClosed, session: rpc.ReadinessSessionPreOpen,
			opens: utc(2026, 9, 28, 13, 30), send: utc(2026, 9, 28, 13, 45)},
		{name: "refusal without a proposal", at: utc(2026, 9, 28, 14, 0), blockers: []rpc.TradingBlocker{blocker("stale_revision")},
			code: rpc.ReadinessNotExecutable, session: rpc.ReadinessSessionUnknown, message: "stale_revision message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readinessPreviewServer(t, t.TempDir(), tc.at)
			if tc.live {
				srv.gatewayReadyForTrading = func() bool { return false }
			}
			engine := &proposalEngine{server: srv, now: srv.now}
			r := engine.classifyReadiness(tc.prop, tc.blockers, tc.live, readinessSessions{})
			if r.Code != tc.code || r.SessionState != tc.session || r.Queueable != tc.queueable || r.Message != tc.message && tc.message != "" {
				t.Fatalf("readiness = %+v, want code %s session %s queueable %v message %q", r, tc.code, tc.session, tc.queueable, tc.message)
			}
			if tc.opens.IsZero() != (r.OpensAt == nil) || r.OpensAt != nil && !r.OpensAt.Equal(tc.opens) {
				t.Fatalf("opens_at = %v, want %v", r.OpensAt, tc.opens)
			}
			if tc.send.IsZero() != (r.DefaultSendAt == nil) || r.DefaultSendAt != nil && !r.DefaultSendAt.Equal(tc.send) {
				t.Fatalf("default_send_at = %v, want %v", r.DefaultSendAt, tc.send)
			}
		})
	}
}

func TestTypedPreviewFailures(t *testing.T) {
	open := utc(2026, 9, 28, 14, 0)
	bid, ask := 2.05, 2.15
	cases := []struct {
		name  string
		quote rpc.OrderQuoteSnapshot
		code  string
		class string
	}{
		{name: "stale", quote: rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, Stale: true, StaleReason: "bid/ask source timestamp is unavailable"}, code: previewQuoteStaleCode, class: rpc.ReadinessQuoteUnusable},
		{name: "delayed", quote: rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataDelayed}, code: previewQuoteNotLiveCode, class: rpc.ReadinessQuoteUnusable},
		{name: "one-sided", quote: rpc.OrderQuoteSnapshot{Bid: &bid, DataType: rpc.MarketDataLive}, code: previewQuoteNotTwoSidedCode, class: rpc.ReadinessQuoteUnusable},
		{name: "quote session context closed", quote: rpc.OrderQuoteSnapshot{Bid: &bid, Ask: &ask, DataType: rpc.MarketDataLive, SessionContext: &rpc.MarketSession{IsOpen: false, State: "closed"}}, code: previewMarketClosedCode, class: rpc.ReadinessMarketClosed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := readinessPreviewServer(t, t.TempDir(), open)
			srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
				return tc.quote, nil
			}
			_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Contract: readinessOption(), Quantity: 2})
			blockers := previewFailureBlockers(err)
			if err == nil || len(blockers) != 1 || blockers[0].Code != tc.code {
				t.Fatalf("err %v, blockers %+v, want %s", err, blockers, tc.code)
			}
			if code, _ := classifyError(err); code != rpc.CodeBadRequest {
				t.Fatalf("typed refusal changed the RPC error class to %q", code)
			}
			engine := &proposalEngine{server: srv, now: srv.now}
			if r := engine.refusalReadiness(readinessRow(rpc.TradeProposalBucketBudgetReduction), blockers, err); r.Code != tc.class {
				t.Fatalf("readiness %+v, want %s", r, tc.class)
			}
		})
	}
	t.Run("broker link down", func(t *testing.T) {
		srv, quotes := readinessPreviewServer(t, t.TempDir(), open)
		srv.gatewayReadyForTrading = func() bool { return false }
		_, err := srv.previewOrder(t.Context(), rpc.OrderPreviewParams{Action: "sell", Contract: readinessOption(), Quantity: 2})
		blockers := previewFailureBlockers(err)
		if !errors.Is(err, ErrTradingDisabled) || !slices.ContainsFunc(blockers, func(b rpc.TradingBlocker) bool { return b.Code == "gateway_unavailable" }) || quotes.Load() != 0 {
			t.Fatalf("err %v, blockers %+v, quotes %d", err, blockers, quotes.Load())
		}
		engine := &proposalEngine{server: srv, now: srv.now}
		if r := engine.refusalReadiness(readinessRow(rpc.TradeProposalBucketBudgetReduction), blockers, err); r.Code != rpc.ReadinessBrokerUnavailable {
			t.Fatalf("readiness %+v, want broker_unavailable", r)
		}
	})
}

// Rows are classified when served; the engine's own snapshot, which is what
// daemon.db stores, never carries readiness, so it cannot move a revision.
func TestServedRowsCarryReadinessNeverStored(t *testing.T) {
	srv, _ := readinessPreviewServer(t, t.TempDir(), fixtureCEST1519)
	scope := brokerStateScope{Account: "DU1234567", Mode: "paper"}
	prop := readinessRow(rpc.TradeProposalBucketBudgetReduction)
	engine := &proposalEngine{server: srv, now: srv.now, scope: func() brokerStateScope { return scope }}
	engine.snapshot = rpc.TradeProposalSnapshot{Kind: rpc.TradeProposalSnapshotKind, SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion, AsOf: fixtureCEST1519,
		Revision: prop.Revision, AccountID: scope.Account, AccountMode: scope.Mode, Proposals: []rpc.TradeProposal{prop}}
	served := engine.Snapshot(false)
	if len(served.Proposals) != 1 || served.Proposals[0].Readiness == nil || served.Proposals[0].Readiness.Code != rpc.ReadinessMarketClosed {
		t.Fatalf("served row readiness = %+v", served.Proposals)
	}
	if engine.snapshot.Proposals[0].Readiness != nil {
		t.Fatal("readiness reached the stored snapshot")
	}
	sources := rpc.TradeProposalSourceFingerprints{}
	if proposalRevision(rpc.Fingerprint{}, sources, scope, served.Proposals) != proposalRevision(rpc.Fingerprint{}, sources, scope, engine.snapshot.Proposals) {
		t.Fatal("readiness changed the proposal revision")
	}
}

func TestDecisionReasonMasksAccountsAndStaysBounded(t *testing.T) {
	got := boundedDecisionReason(`proposal snapshot was generated for account "DU1234567" mode "paper" but the connected session is account "U1234567"`)
	if strings.Contains(got, "1234567") || strings.Count(got, "[account]") != 2 {
		t.Fatalf("reason kept an account code: %q", got)
	}
	if n := len([]rune(boundedDecisionReason(strings.Repeat("word ", 400)))); n != decisionReasonRunes {
		t.Fatalf("bounded reason has %d runes, want %d", n, decisionReasonRunes)
	}
}

// latchRegimeStage sets the regime stage the Rulebook reads; a zero stage
// is one never observed.
func latchRegimeStage(srv *Server, stage, bucket string, asOf time.Time) {
	srv.rulesRegimeStageMu.Lock()
	defer srv.rulesRegimeStageMu.Unlock()
	srv.rulesRegimeStageLoaded = true
	srv.rulesRegimeStage = rulesRegimeStageState{}
	if bucket != "" {
		srv.rulesRegimeStage = rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: bucket, Stage: stage, AsOf: asOf}
	}
}

// Owner decision 2026-09-30 12:35 CEST: at a stress open, while the latched
// regime stage in force reads confirmed stress, the options opening offset is
// 30 minutes instead of 15. A carried (stale) confirmed stage counts as its
// own stage, never as calm; calm, early warning and a stage never observed
// keep 15; stocks keep 5. Readiness, the queued window and the
// pre-authorisation scheduler all read the same offset.
func TestStressOpenDoublesTheOptionsOpeningOffset(t *testing.T) {
	opens := utc(2026, 9, 28, 13, 30)
	stock := readinessRow(rpc.TradeProposalBucketRiskReduction)
	stock.SecType, stock.Contract = "STK", rpc.ContractParams{ConID: 700003, Symbol: "BBB", SecType: "STK", Currency: "USD", Exchange: "SMART"}
	for _, c := range []struct {
		name          string
		stage, bucket string
		age           time.Duration
		stress        bool
	}{
		{name: "never observed"},
		{name: "calm", stage: rpc.LifecycleQuiet, bucket: risk.RegimeBucketCalm, age: time.Minute},
		{name: "early warning", stage: rpc.LifecycleEarlyWarning, bucket: risk.RegimeBucketEarlyWarning, age: time.Minute},
		{name: "confirmed", stage: rpc.LifecycleConfirmedStress, bucket: risk.RegimeBucketConfirmed, age: time.Minute, stress: true},
		{name: "carried confirmed", stage: rpc.LifecyclePanic, bucket: risk.RegimeBucketConfirmed, age: 6 * time.Hour, stress: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			offset := 15 * time.Minute
			if c.stress {
				offset = 30 * time.Minute
			}
			send := opens.Add(offset)
			srv, _ := readinessPreviewServer(t, t.TempDir(), fixtureCEST1519)
			latchRegimeStage(srv, c.stage, c.bucket, fixtureCEST1519.Add(-c.age))
			if _, carried := srv.rulebookRegimeStage(risk.DefaultRulebookPolicy(), fixtureCEST1519); carried != (c.age > 4*time.Hour) {
				t.Fatalf("carried = %v", carried)
			}
			engine := &proposalEngine{server: srv, now: func() time.Time { return srv.now() }}
			governor := readinessRow(rpc.TradeProposalBucketBudgetReduction)

			// Before the open: the default send is the open plus the offset.
			r := engine.classifyReadiness(governor, nil, false, readinessSessions{})
			if r.Code != rpc.ReadinessMarketClosed || r.DefaultSendAt == nil || !r.DefaultSendAt.Equal(send) || r.StressOpen != c.stress {
				t.Fatalf("pre-open readiness = %+v, want default send %s stress %v", r, send, c.stress)
			}
			phrase := "stress open: the regime reads confirmed stress, so options send from " + send.Format(time.RFC3339) + ", 30 minutes after the open"
			if strings.Contains(r.Message, "stress open") != c.stress || c.stress && !strings.HasSuffix(r.Message, "; "+phrase) {
				t.Fatalf("pre-open message = %q", r.Message)
			}
			// Stocks keep five minutes whatever the regime.
			if s := engine.classifyReadiness(stock, nil, false, readinessSessions{}); s.DefaultSendAt == nil || !s.DefaultSendAt.Equal(opens.Add(5*time.Minute)) || s.StressOpen {
				t.Fatalf("stock readiness = %+v", s)
			}

			// Twenty minutes after the open: ready in calm, still the opening
			// window at a stress open.
			srv.now = func() time.Time { return opens.Add(20 * time.Minute) }
			r = engine.classifyReadiness(governor, nil, false, readinessSessions{})
			switch {
			case c.stress && (r.Code != rpc.ReadinessOpeningWindow || r.DefaultSendAt == nil || !r.DefaultSendAt.Equal(send) || r.Message != phrase || !r.StressOpen || !r.Queueable):
				t.Fatalf("stress open window = %+v", r)
			case !c.stress && (r.Code != rpc.ReadinessReady || r.DefaultSendAt != nil || r.Message != ""):
				t.Fatalf("calm open = %+v", r)
			}

			// The queued window and the pre-authorised due time start at the
			// same offset.
			srv.now = func() time.Time { return fixtureCEST1519 }
			governor.LimitPrice = new(2.10)
			terms, blockers := engine.queuedTerms(governor, protectionPolicy{}, rpc.ProtectionPolicyStatus{}, 0, brokerStateScope{Account: "DU1234567", Mode: "paper"}, fixtureCEST1519)
			if len(blockers) != 0 || !terms.NotBefore.Equal(send) || !terms.NotAfter.Equal(send.Add(queuedSendWindow)) || engine.queuedStressOpen(terms) != c.stress {
				t.Fatalf("queued terms %s–%s blockers %+v", terms.NotBefore, terms.NotAfter, blockers)
			}
			if due := engine.automaticSessionDue(governor, opens.Add(10*time.Minute)); !due.Equal(send) {
				t.Fatalf("pre-authorised due = %s, want %s", due, send)
			}
		})
	}
}
