package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/stress"
)

// The Asia simulation is a synthetic daemon socket for a Hong Kong dollar
// book held on SEHK, SGX and TSEJ: invented positions, an invented paper
// account, no broker and no order path. It serves the read methods the CLI,
// the MCP server, the stress read and Desk's display feed use, composing the
// brief, the Rulebook and the stress verdict with the daemon's own pure
// functions, so the renderers can be exercised at an Asian morning without
// a live account.
//
// Plain `go test` asserts the session words and the HKD figures. With
// CANARY_ASIA_SIM_SOCKET=<path> the test serves on that path until
// <path>.stop exists (a manual witness: point `canary`, `canary mcp` or Desk
// at it with CANARY_SOCKET). CANARY_ASIA_SIM_CLOCK=now stamps the book with
// the wall clock instead of the fixed Asian morning, for readers such as
// Desk that reject evidence dated in the future.

// asiaSimNow is 10:00 in Hong Kong on a Wednesday: SEHK, SGX and TSE are
// open, the US is closed.
var asiaSimNow = time.Date(2026, 11, 4, 10, 0, 0, 0, time.FixedZone("HKT", 8*60*60))

const asiaSimAccount = "DU0000000"

func asiaSimPositions(now time.Time) *rpc.PositionsResult {
	hk := rpc.PositionView{Symbol: "700", SecType: rpc.SecTypeStock, ConID: 900700, Exchange: "SEHK", PrimaryExch: "SEHK", Currency: "HKD", LocalSymbol: "700", Quantity: 500, Multiplier: 1,
		AvgCost: 352.40, Mark: 388.20, MarketValue: 194100, UnrealizedPnL: 17900, DayChangeMoney: new(1100.0), DayChangePct: new(0.57), MarketValueBase: new(194100.0), FXRate: new(1.0), DataType: rpc.MarketDataLive}
	sg := rpc.PositionView{Symbol: "D05", SecType: rpc.SecTypeStock, ConID: 900005, Exchange: "SGX", PrimaryExch: "SGX", Currency: "SGD", LocalSymbol: "D05", Quantity: 1000, Multiplier: 1,
		AvgCost: 41.10, Mark: 43.75, MarketValue: 43750, UnrealizedPnL: 2650, DayChangeMoney: new(-250.0), DayChangePct: new(-0.57), MarketValueBase: new(252262.5), FXRate: new(5.766), DataType: rpc.MarketDataLive}
	jp := rpc.PositionView{Symbol: "7203", SecType: rpc.SecTypeStock, ConID: 907203, Exchange: "TSEJ", PrimaryExch: "TSEJ", Currency: "JPY", LocalSymbol: "7203", Quantity: 300, Multiplier: 1,
		AvgCost: 2810, Mark: 2955, MarketValue: 886500, UnrealizedPnL: 43500, DayChangeMoney: new(-9000.0), DayChangePct: new(-1.01), MarketValueBase: new(45363.0), FXRate: new(0.05117), DataType: rpc.MarketDataLive}
	jpPut := rpc.PositionView{Symbol: "7203", SecType: rpc.SecTypeOption, ConID: 917203, Exchange: "OSE.JPN", Currency: "JPY", Quantity: 1, Multiplier: 100, Right: "P", Strike: 2800, Expiry: "20261211",
		AvgCost: 6200, Mark: 48, MarketValue: 4800, UnrealizedPnL: -1400, Delta: new(-0.22), MarketValueBase: new(246.0), FXRate: new(0.05117), DataType: rpc.MarketDataLive}
	return &rpc.PositionsResult{AsOf: now, DataType: rpc.MarketDataLive, AccountID: asiaSimAccount,
		Stocks: []rpc.PositionView{hk, sg, jp}, Options: []rpc.PositionView{jpPut},
		ByUnderlying: []rpc.PositionGroup{
			{Underlying: "700", Stock: &hk, GroupMarketValue: 194100, GroupUnrealizedPnL: 17900, GroupMarketValueBase: new(194100.0), GroupDailyPnLBase: new(1100.0), GroupDollarDeltaBase: new(194100.0), GroupEffectiveDelta: new(500.0)},
			{Underlying: "D05", Stock: &sg, GroupMarketValue: 43750, GroupUnrealizedPnL: 2650, GroupMarketValueBase: new(252262.5), GroupDailyPnLBase: new(-1441.5), GroupDollarDeltaBase: new(252262.5), GroupEffectiveDelta: new(1000.0)},
			{Underlying: "7203", Stock: &jp, Options: []rpc.PositionView{jpPut}, GroupMarketValue: 891300, GroupUnrealizedPnL: 42100, GroupMarketValueBase: new(45609.0), GroupDailyPnLBase: new(-460.5), GroupDollarDeltaBase: new(42036.0), GroupEffectiveDelta: new(278.0)},
		},
		Authority: asiaSimAuthority(rpc.AccountDataSourcePortfolioStream, now),
	}
}

func asiaSimAuthority(source rpc.AccountDataSource, now time.Time) *rpc.AccountDataAuthority {
	return &rpc.AccountDataAuthority{
		Scope:             rpc.AccountDataScope{AccountID: asiaSimAccount, AccountMode: "paper"},
		Source:            source,
		Availability:      rpc.AccountDataAvailable,
		Freshness:         rpc.AccountDataFreshnessCurrent,
		AsOf:              now,
		PortfolioComplete: true,
	}
}

func asiaSimAccountSummary(now time.Time) *rpc.AccountResult {
	return &rpc.AccountResult{
		AccountID: asiaSimAccount, AccountType: "INDIVIDUAL", BaseCurrency: "HKD",
		NetLiquidation: 1250000, BuyingPower: 2480000, AvailableFunds: 620000, ExcessLiquidity: 640000,
		TotalCash: 758000, GrossPositionValue: 492000, UnrealizedPnL: 68800, RealizedPnL: 0, Cushion: 0.51,
		DailyPnL: new(-8250.0), DataType: rpc.MarketDataLive, AsOf: now,
		CurrencyExposure: []rpc.CurrencyExposure{
			{Currency: "HKD", NetLiquidationCcy: 1250000, CashCcy: 758000, StockMarketValueCcy: 194100, ExchangeRate: 1, NetLiquidationBase: 1250000, CashObserved: true},
			{Currency: "SGD", NetLiquidationCcy: 43750, StockMarketValueCcy: 43750, ExchangeRate: 5.766, NetLiquidationBase: 252262.5, CashObserved: true},
			{Currency: "JPY", NetLiquidationCcy: 891300, StockMarketValueCcy: 886500, OptionMarketValueCcy: 4800, ExchangeRate: 0.05117, NetLiquidationBase: 45609, CashObserved: true},
		},
		Authority: asiaSimAuthority(rpc.AccountDataSourceAccountSummaryRequest, now),
	}
}

func asiaSimHealth(now time.Time) *rpc.HealthResult {
	return &rpc.HealthResult{
		Verdict: rpc.HealthVerdict{State: "ok", Reason: "simulation"}, DaemonVersion: "asia-simulation", DaemonStarted: now.Add(-time.Hour), UptimeSeconds: 3600,
		Account: asiaSimAccount, ConnectedAccount: asiaSimAccount, AccountMode: "paper",
		GatewayHost: "127.0.0.1", GatewayPort: 4002, ClientID: 15, PortOrigin: "pinned", Connected: true, GatewayPhase: rpc.GatewayPhaseReady, GatewayPhaseAt: now.Add(-time.Hour),
	}
}

// asiaSimRules evaluates the real Rulebook over the synthetic book with the
// default policy; the daemon's own input mapping names the positions.
func asiaSimRules(acct *rpc.AccountResult, pos *rpc.PositionsResult, now time.Time) *rpc.RulesResult {
	pol := risk.DefaultRulebookPolicy()
	in := risk.RuleInputs{
		AsOf: now, BaseCurrency: acct.BaseCurrency,
		Positions: risk.SourceState{Healthy: true}, Account: risk.SourceState{Healthy: true},
		NLVBase: new(acct.NetLiquidation), CashBase: new(acct.TotalCash), AvailableFundsBase: new(acct.AvailableFunds),
		ExcessLiquidityBase: new(acct.ExcessLiquidity), DailyPnLBase: acct.DailyPnL,
		SessionOpen: true,
	}
	in.Names = mapRuleNames(pos, pol, acct.BaseCurrency)
	ev := risk.EvaluateRulebook(in, pol)
	fp := rpc.Fingerprint{Version: rpc.RulebookPolicyFingerprintVersion, Key: pol.FingerprintKey()}
	counts := map[string]int{}
	for _, r := range ev.Rows {
		counts[r.Status]++
	}
	return &rpc.RulesResult{
		AsOf: now, Enabled: true, Status: "ok", PolicyID: pol.ID, PolicyVersion: pol.Version, PolicyFingerprint: &fp, Policy: &pol,
		BaseCurrency: acct.BaseCurrency, Rules: ev.Rows, Ranked: ev.Ranked, SellOnly: ev.SellOnly, BreachCounts: counts,
		InputHealth: []rpc.SourceHealth{{Source: "account", Status: rpc.SourceStatusOK, AsOf: now}, {Source: "positions", Status: rpc.SourceStatusOK, AsOf: now}},
	}
}

// asiaSimBrief composes the brief from the daemon's pure composition steps.
// The rows that need daemon state (journal, proposals, policy, recon) are
// marked unavailable rather than invented.
func asiaSimBrief(srv *Server, acct *rpc.AccountResult, pos *rpc.PositionsResult, rules *rpc.RulesResult, now time.Time) *rpc.BriefResult {
	res := &rpc.BriefResult{AsOf: now}
	cal, calErr := (&Server{}).handleMarketCalendar(&rpc.Request{Params: briefJSON(rpc.MarketCalendarParams{Market: string(briefSessionMarket(pos)), At: now, Days: 1})})
	sessionOpen := calErr != nil || cal == nil || cal.Session.IsOpen
	notSimulated := errors.New("not part of the simulation")
	market, _ := composeBriefMarket(now, acct, pos, nil, nil, nil, nil, rpc.StressConcentrationFromRules(rules), rpc.StressNetExposureFromRules(rules),
		rpc.StressMarginHeadroomFromRules(rules), nil, nil, notSimulated, notSimulated, notSimulated, sessionOpen)
	calendar := composeBriefCalendar(cal, nil, rules, calErr, notSimulated, sessionOpen)
	portfolio := srv.composeBriefPortfolio(acct, pos, nil, nil, sessionOpen)
	unavailable := briefUnavailable(notSimulated.Error())
	var riskLimits rpc.BriefRiskSection
	riskLimits.Capital.BriefRowState, riskLimits.Latch.BriefRowState = unavailable, unavailable
	var process rpc.BriefProcessSection
	process.Rules = briefRulesStatus(rules)
	review := rpc.BriefReviewSection{SessionPnL: portfolio.Account, Attribution: portfolio.Movers, Rules: process.Rules, WorkingOrders: portfolio.WorkingOrders}
	review.LastSession.BriefRowState, review.Edge.BriefRowState, review.Proposals.BriefRowState, review.Overrides.BriefRowState = unavailable, unavailable, unavailable, unavailable
	review.CapitalEvents.BriefRowState, review.Reconcile.BriefRowState, review.AutoExtend.BriefRowState = unavailable, unavailable, unavailable
	res.Review = review
	var proposals rpc.BriefReadyProposalsRow
	proposals.BriefRowState = unavailable
	res.Ready = composeBriefReady(market, calendar, riskLimits, portfolio, process, proposals)
	res.AttentionOrder = briefAttentionOrder(res.Ready, rules)
	res.BriefFingerprint = briefContentFingerprint(res)
	res.Narrative = composeBriefNarrative(res)
	return res
}

// asiaSimDisplay is the display frame Desk's feed validates: version 1, a
// generation, a sequence from 1, available, a paper scope.
func asiaSimDisplay(acct *rpc.AccountResult, pos *rpc.PositionsResult, seq uint64, at time.Time) rpc.DisplaySnapshot {
	snap := rpc.DisplaySnapshot{
		Version: 1, Generation: "asia-simulation", Sequence: seq, EmittedAt: at, Available: true,
		Scope:       rpc.AccountDataScope{AccountID: asiaSimAccount, AccountMode: "paper"},
		Account:     rpc.DisplayAccount{Currency: acct.BaseCurrency, NetLiquidation: new(acct.NetLiquidation), NetLiquidationAt: at, DailyPnL: acct.DailyPnL, UnrealizedPnL: new(acct.UnrealizedPnL), RealizedPnL: new(acct.RealizedPnL), PnLAt: at},
		PositionsAt: at, Quotes: []rpc.DisplayQuote{},
	}
	for _, p := range append(append([]rpc.PositionView{}, pos.Stocks...), pos.Options...) {
		snap.Positions = append(snap.Positions, rpc.DisplayPosition{
			UnrealizedAt: at, ValuationAt: at,
			Contract: rpc.ContractParams{ConID: p.ConID, Symbol: p.Symbol, SecType: p.SecType, Exchange: p.Exchange, PrimaryExch: p.PrimaryExch, Currency: p.Currency, LocalSymbol: p.LocalSymbol, Expiry: p.Expiry, Strike: p.Strike, Right: p.Right, Multiplier: p.Multiplier},
			Quantity: p.Quantity, AverageCost: new(p.AvgCost), Mark: new(p.Mark), MarketValue: new(p.MarketValue), UnrealizedPnL: new(p.UnrealizedPnL), DailyPnL: p.DayChangeMoney, PnLAt: at,
		})
	}
	return snap
}

type asiaSim struct {
	srv   *Server
	now   time.Time
	acct  *rpc.AccountResult
	pos   *rpc.PositionsResult
	rules *rpc.RulesResult
}

func newAsiaSim(srv *Server, now time.Time) *asiaSim {
	acct, pos := asiaSimAccountSummary(now), asiaSimPositions(now)
	return &asiaSim{srv: srv, now: now, acct: acct, pos: pos, rules: asiaSimRules(acct, pos, now)}
}

// Call answers one read in process; stress.Compose reads through it too.
func (s *asiaSim) Call(_ context.Context, method string, params, out any) error {
	raw, _ := json.Marshal(params)
	res, err := s.handle(&rpc.Request{Method: method, Params: raw})
	if err != nil {
		return err
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *asiaSim) handle(req *rpc.Request) (any, error) {
	switch req.Method {
	case rpc.MethodStatusHealth:
		return asiaSimHealth(s.now), nil
	case rpc.MethodSettingsGet:
		return rpc.PlatformSettings{}, nil
	case rpc.MethodAccountSummary:
		return s.acct, nil
	case rpc.MethodPositionsList:
		return s.pos, nil
	case rpc.MethodPositionsRisk:
		return rpc.CompactPositionsRisk(s.pos, 5), nil
	case rpc.MethodMarketCalendar:
		return (&Server{}).handleMarketCalendar(req)
	case rpc.MethodRulesSnapshot:
		return s.rules, nil
	case rpc.MethodRegimeSnapshot:
		return rpc.RegimeSnapshotResult{AsOf: s.now}, nil
	case rpc.MethodBriefSnapshot:
		return asiaSimBrief(s.srv, s.acct, s.pos, s.rules, s.now), nil
	case rpc.MethodStressSnapshot:
		res, regime, err := stress.Compose(context.Background(), s)
		if err != nil {
			return nil, err
		}
		monitor := rpc.CompactRegimeMonitor(&regime)
		return &rpc.StressSnapshotResult{Stress: res, RegimeMonitor: &monitor}, nil
	}
	return nil, &rpc.Error{Code: rpc.CodeUnknownMethod, Message: "not part of the Asia simulation: " + req.Method}
}

// serveConn speaks the daemon's newline-delimited JSON framing: one response
// per request, and a display subscription that streams until the peer
// closes.
func (s *asiaSim) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	enc := json.NewEncoder(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req rpc.Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(rpc.Response{Ok: false, Error: &rpc.Error{Code: rpc.CodeBadRequest, Message: err.Error()}})
			continue
		}
		if req.Method == rpc.MethodDisplaySubscribe {
			s.streamDisplay(ctx, req.ID, conn, enc)
			return
		}
		res, err := s.handle(&req)
		if err != nil {
			var rerr *rpc.Error
			if !errors.As(err, &rerr) {
				rerr = &rpc.Error{Code: rpc.CodeInternal, Message: err.Error()}
			}
			_ = enc.Encode(rpc.Response{ID: req.ID, Ok: false, Error: rerr})
			continue
		}
		raw, _ := json.Marshal(res)
		_ = enc.Encode(rpc.Response{ID: req.ID, Ok: true, Result: raw})
	}
}

func (s *asiaSim) streamDisplay(ctx context.Context, id string, conn net.Conn, enc *json.Encoder) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for seq := uint64(1); ; seq++ {
		raw, _ := json.Marshal(asiaSimDisplay(s.acct, s.pos, seq, time.Now().UTC()))
		if err := enc.Encode(rpc.Response{ID: id, Ok: true, Stream: true, Frame: raw}); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *asiaSim) serve(ctx context.Context, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		go s.serveConn(ctx, conn)
	}
}

func TestAsiaSimulationServesAnAsianMorning(t *testing.T) {
	sim := newAsiaSim(newTestServer(t), asiaSimNow)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A manual witness holds the socket at a caller-chosen path until
	// <path>.stop exists.
	if hold := os.Getenv("CANARY_ASIA_SIM_SOCKET"); hold != "" {
		if os.Getenv("CANARY_ASIA_SIM_CLOCK") == "now" {
			sim = newAsiaSim(newTestServer(t), time.Now().UTC().Truncate(time.Second))
		}
		_ = os.Remove(hold)
		l, err := net.Listen("unix", hold)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		go sim.serve(ctx, l)
		t.Logf("Asia simulation serving on %s until %s.stop exists", hold, hold)
		for {
			if _, err := os.Stat(hold + ".stop"); err == nil {
				_ = os.Remove(hold + ".stop")
				return
			}
			time.Sleep(time.Second)
		}
	}

	dir, err := os.MkdirTemp("/tmp", "asia-sim-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go sim.serve(ctx, l)

	conn, err := dial.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var brief rpc.BriefResult
	if err := conn.Call(ctx, rpc.MethodBriefSnapshot, nil, &brief); err != nil {
		t.Fatal(err)
	}
	// The session follows the venue holding most of the book's base value:
	// the Singapore line outweighs the Hong Kong line once converted to HKD.
	if brief.Ready.Session.Market != "sg_sgx" || brief.Ready.Session.State != "regular" || !brief.Ready.Session.IsOpen {
		t.Fatalf("session row at an Asian morning: %+v, want the open Singapore regular session", brief.Ready.Session)
	}
	if brief.Review.SessionPnL.BaseCurrency != "HKD" || brief.Review.SessionPnL.EquityBase == nil {
		t.Fatalf("session P&L row: %+v, want Hong Kong dollar figures", brief.Review.SessionPnL)
	}
	narrative, _ := json.Marshal(brief.Narrative)
	if !strings.Contains(string(narrative), "HKD 1,250,000") {
		t.Fatalf("narrative carries no house-format HKD figure: %s", narrative)
	}

	var st rpc.StressSnapshotResult
	if err := conn.Call(ctx, rpc.MethodStressSnapshot, nil, &st); err != nil {
		t.Fatal(err)
	}
	if st.Stress.AsOf.IsZero() || st.Stress.Fingerprint.Key == "" {
		t.Fatalf("stress read composed nothing: %+v", st.Stress)
	}

	var cal rpc.MarketCalendarResult
	if err := conn.Call(ctx, rpc.MethodMarketCalendar, rpc.MarketCalendarParams{Market: "sg", At: asiaSimNow, Days: 1}, &cal); err != nil {
		t.Fatal(err)
	}
	if cal.Market != "sg_sgx" || !cal.Session.IsOpen {
		t.Fatalf("Singapore at 10:00 Hong Kong time: %+v", cal.Session)
	}

	// Desk's feed contract: version 1, generation, sequence from 1, paper scope.
	frames := 0
	streamCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	err = conn.Stream(streamCtx, rpc.MethodDisplaySubscribe, nil, func(raw json.RawMessage) error {
		var snap rpc.DisplaySnapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			return err
		}
		if snap.Version != 1 || snap.Generation == "" || snap.Sequence == 0 || !snap.Available || snap.Scope.AccountMode != "paper" || len(snap.Positions) != 4 {
			return fmt.Errorf("display frame fails Desk's envelope check: %+v", snap)
		}
		frames++
		if frames >= 2 {
			return io.EOF
		}
		return nil
	})
	if frames < 2 && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("display stream: frames=%d err=%v", frames, err)
	}
	if frames < 2 {
		t.Fatalf("display stream delivered %d frames in 3s", frames)
	}
}
