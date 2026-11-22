package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/config"
	"github.com/osauer/canary/v3/internal/discover"
	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A simulated daemon (Options.Simulator) runs the real daemon against the TWS
// simulator in pkg/ibkr/twssim: cmd/canary-sim and the hermetic end-to-end
// tests set it, the installed canary binary never does. It changes only the
// inputs that have no TWS source, and refuses to start unless the config
// points at a loopback simulator in paper mode:
//
//   - the regime's public series (FRED, the Federal Reserve, Treasury and
//     Cboe's VVIX and VIX3M closes) read steady synthetic levels instead of
//     the public sites;
//   - the Rulebook's regime stage is latched calm (quiet) whenever no current
//     latch exists: the simulator's quotes and bars publish a real regime,
//     but a cold breadth needs about 74 minutes of IBKR history pacing, dealer
//     gamma needs SPX option chains the simulator does not list, and a
//     closed-date (weekend) snapshot never latches, so the real projection
//     would leave every Add held on "current regime evidence is
//     unavailable";
//   - once the first session is ready, the capital journal records one
//     reconcile event with origin "simulator", because a simulated book has
//     no Flex statements to reconcile against and the drawdown brake refuses
//     every risk-adding order while the reconcile clock has never started
//     (risk.EvaluateUnreconciledClock).
const simulatorOrigin = "simulator"

// checkSimulatorConfig refuses a simulated daemon whose broker endpoint could
// be a real TWS or Gateway: the port must be pinned, on loopback and off
// IBKR's API ports (so discovery never probes them), the account a paper
// (DU) account and the trading mode paper.
func checkSimulatorConfig(cfg *config.Resolved) error {
	if cfg == nil {
		return errors.New("simulator: no config")
	}
	g := cfg.Gateway
	var problems []string
	if cfg.Broker.Kind != "" && cfg.Broker.Kind != "ibkr" {
		problems = append(problems, fmt.Sprintf("broker kind %q is not ibkr", cfg.Broker.Kind))
	}
	if g.Port == nil {
		problems = append(problems, "the gateway port is not pinned, so discovery would probe IBKR's ports")
	} else if slices.Contains(discover.StandardPorts, *g.Port) {
		problems = append(problems, fmt.Sprintf("gateway port %d is an IBKR API port", *g.Port))
	}
	if ip := net.ParseIP(g.HostOrDefault()); ip == nil || !ip.IsLoopback() {
		problems = append(problems, fmt.Sprintf("gateway host %q is not a loopback address", g.Host))
	}
	if !strings.HasPrefix(strings.ToUpper(g.Account), "DU") {
		problems = append(problems, fmt.Sprintf("account %q is not a paper (DU) account", g.Account))
	}
	if cfg.Trading.Mode != "paper" {
		problems = append(problems, fmt.Sprintf("trading mode %q is not paper", cfg.Trading.Mode))
	}
	if len(problems) > 0 {
		return fmt.Errorf("simulator refused: %s", strings.Join(problems, "; "))
	}
	return nil
}

// seedSimulatedEvidence runs once, when the first session is ready: it starts
// the calm regime latch and the reconcile clock of the simulated book for the
// selected account, recording the reconcile in the capital journal like any
// other, with the simulator as its origin.
func (s *Server) seedSimulatedEvidence() {
	if !s.simulator || s.riskCapital == nil || s.riskPolicies == nil {
		return
	}
	s.simulatorReconcile.Do(func() {
		if s.serverCtx != nil {
			s.latchSimulatedRegimeStage(s.serverCtx)
		}
		scope := s.currentBrokerStateScope()
		if !brokerScopeConcrete(scope) {
			s.warnf("simulator: no selected account to reconcile")
			return
		}
		pol := s.riskPolicies.snapshot().policy
		if _, err := s.riskCapital.ApplyCapitalEventForPolicyScope(rpc.CapitalEventParams{Type: "reconcile", Note: "simulated book: no Flex statements exist to reconcile against"}, simulatorOrigin, pol, scope); err != nil {
			s.warnf("simulator: reconcile event not recorded: %v", err)
		}
	})
}

// The synthetic owner's numbers a simulated book runs on: the capital,
// drawdown, override and recon keys of the risk policy and the cash reserve,
// invented for the simulator's book and none the operator's. Each list is
// "section", then its key lines.
var (
	simulatorRiskKeys = [][]string{
		{"capital", `base_currency = "%s"`, "protected_floor = 20000.0", "declared_risk_capital = 50000.0", "max_equity_age_minutes = 240", "max_unreconciled_days = 30"},
		{"drawdown", "warn_consumed_pct = 40.0", "block_consumed_pct = 80.0", `block_enforcement = "hard"`},
		{"override", "max_duration_hours = 24"},
		{"recon", "amount_tolerance_pct = 0.5", "amount_tolerance_min = 5.0", "date_window_business_days = 3", "max_report_age_days = 7", "max_equity_divergence_pct = 2.0"},
	}
	simulatorCashKeys = [][]string{
		{"cash.sweep", "reserve_floor_base = 5000.0", "reserve_pct_nlv = 2.0", "no_buy_while_borrowed = true", "keep_cash = 500.0"},
	}
)

// WriteSimulatorPolicies writes the synthetic owner's reviewed policy files
// for a simulated book into dir (a stack's own policies directory, never the
// operator's): Canary's templates without the not-yet-reviewed marker, with
// the capital and cash-reserve numbers a template leaves for the owner
// written into their sections.
func WriteSimulatorPolicies(dir, base string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	risk := slices.Clone(simulatorRiskKeys)
	risk[0] = slices.Clone(risk[0])
	risk[0][1] = fmt.Sprintf(risk[0][1], base)
	files := map[string]string{
		"risk-policy.toml":       withPolicyKeys(string(ConstitutionPolicyTemplate("simulator")), risk),
		"rulebook-policy.toml":   string(RulebookPolicyTemplate("simulator")),
		"protection-policy.toml": withPolicyKeys(string(ProtectionPolicyTemplate("simulator")), simulatorCashKeys),
	}
	for name, body := range files {
		reviewed := strings.Replace(body, PolicyUnreviewedMarker+"\n", "", 1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(reviewed), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// withPolicyKeys writes each section's key lines under its header in a
// template, or appends the section when the template has no such header.
func withPolicyKeys(template string, sections [][]string) string {
	lines := strings.Split(template, "\n")
	for _, sec := range sections {
		header := "[" + sec[0] + "]"
		at := slices.IndexFunc(lines, func(l string) bool { return strings.TrimSpace(l) == header })
		if at < 0 {
			lines = append(lines, "", header)
			at = len(lines) - 1
		}
		lines = slices.Insert(lines, at+1, sec[1:]...)
	}
	return strings.Join(lines, "\n")
}

// simulatorRegimeStageEvery is how often a simulated daemon renews its calm
// regime latch; well inside the Rulebook's 240-minute default maximum age.
const simulatorRegimeStageEvery = time.Hour

// latchSimulatedRegimeStage latches the calm stage when the Rulebook has no
// current one, and keeps it current while the daemon runs. A latch the real
// projection wrote is left alone while it is current.
func (s *Server) latchSimulatedRegimeStage(ctx context.Context) {
	latch := func() {
		now := s.orderNow()
		s.rulesRegimeStageMu.Lock()
		defer s.rulesRegimeStageMu.Unlock()
		if st := s.rulesRegimeStage; st.Bucket != "" && now.Sub(st.AsOf) < simulatorRegimeStageEvery {
			return
		}
		s.rulesRegimeStage = rulesRegimeStageState{Version: rulesRegimeStageStateVer, Bucket: risk.RegimeBucketCalm, Stage: rpc.LifecycleQuiet, AsOf: now}
		s.rulesRegimeStageLoaded = true
	}
	latch()
	go func() {
		t := time.NewTicker(simulatorRegimeStageEvery / 4)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				latch()
			}
		}
	}()
}

// simulatedRegimeStageOr keeps a simulated daemon's calm latch when a
// regime publication would install an empty one; any other daemon, and any
// publication that latches a stage, passes through unchanged.
func (s *Server) simulatedRegimeStageOr(state rulesRegimeStageState) rulesRegimeStageState {
	if !s.simulator || state.Bucket != "" {
		return state
	}
	state.Version, state.Bucket, state.Stage, state.AsOf = rulesRegimeStageStateVer, risk.RegimeBucketCalm, rpc.LifecycleQuiet, s.orderNow()
	return state
}

// simulatedRegimeSources are the regime's public inputs for a simulated or
// test daemon: a steady, current daily series for every official series and
// for Cboe's VVIX and VIX3M closes, so nothing reaches the network.
func simulatedRegimeSources() regimeSources {
	series := func(level float64) []regimeSeriesPoint {
		var points []regimeSeriesPoint
		day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
		for len(points) < 400 {
			if wd := day.Weekday(); wd != time.Saturday && wd != time.Sunday {
				points = append(points, regimeSeriesPoint{Date: day, Value: level})
			}
			day = day.AddDate(0, 0, -1)
		}
		slices.Reverse(points)
		return points
	}
	levels := map[string]float64{
		fredSeriesHYOAS: 3.1, fredSeriesIGOAS: 0.95, fredSeriesCP3M: 4.32, fredSeriesTBill3M: 4.18,
	}
	return regimeSources{
		official: func(_ context.Context, id string) ([]regimeSeriesPoint, error) {
			level, ok := levels[id]
			if !ok {
				return nil, errors.New("simulated regime sources: no series " + id)
			}
			return series(level), nil
		},
		vvix:  func(context.Context) ([]regimeSeriesPoint, error) { return series(88), nil },
		vix3m: func(context.Context) ([]regimeSeriesPoint, error) { return series(19.4), nil },
	}
}
