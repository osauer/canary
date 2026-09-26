package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A config.toml part the daemon cannot read never stops it (owner decision
// 2026-09-26): the daemon serves, and status, the brief and the data-health
// alert name the part. A bad maintenance-window schedule, which used to stop
// startup, is one such part.
func TestDaemonStartsOnAnUnreadableConfigPartAndReportsIt(t *testing.T) {
	dir := shortTempDir(t)
	t.Setenv("XDG_STATE_HOME", dir)
	cfg := &config.Resolved{Gateway: config.Gateway{Host: "127.0.0.1", Port: new(4002), ClientID: new(99),
		MaintenanceWindows: []string{"Someday 25:00-26:00 Mars/Olympus"}}}
	cfg.Daemon.SetIdleTimeout(0)
	srv := New(Options{
		Config: cfg, SocketPath: filepath.Join(dir, "ibkrd.sock"), Version: "test",
		Logger: NewLogger(&bytes.Buffer{}, "error"), StateDatabasePath: filepath.Join(dir, "daemon.db"),
		ConfigIssues: []config.Issue{{Key: "flex.enabled", Problem: "cannot be read; Canary's default is in force"}},
	})
	srv.orderJournal = newOrderJournalStore(filepath.Join(dir, "order-journal.jsonl"))
	serving := make(chan struct{}, 1)
	srv.initialAcceptLoopStartedForTest = func() { serving <- struct{}{} }
	srv.attempterFactory = func(_ discover.Endpoint) connectAttempter { return &fakeAttempter{blockUntilCtxDone: true} }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() { started <- srv.Start(ctx) }()
	select {
	case <-serving:
	case err := <-started:
		t.Fatalf("an unreadable config part stopped the daemon: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("daemon socket was not served")
	}

	var sub rpc.SubsystemHealth
	for _, s := range srv.subsystemHealth(false, nil) {
		if s.Name == "config" {
			sub = s
		}
	}
	if sub.Status != "degraded" || !strings.Contains(sub.Message, "flex.enabled") || !strings.Contains(sub.Message, "gateway.maintenance_windows") {
		t.Fatalf("config subsystem = %+v, want degraded naming both parts", sub)
	}
	if verdict := authoritativeHealthVerdict(&rpc.HealthResult{Connected: true, GatewayPhase: rpc.GatewayPhaseReady, Subsystems: []rpc.SubsystemHealth{sub}}); verdict.State != "ATTENTION" {
		t.Fatalf("verdict = %+v, want ATTENTION", verdict)
	}
	row := srv.briefConfigRow()
	if row == nil || row.Status != rpc.BriefStatusAttention || len(row.Issues) != 2 || row.AutomationPaused {
		t.Fatalf("brief config row = %+v, want attention with two issues and no automation pause", row)
	}
	ready := composeBriefReady(rpc.BriefMarketSection{}, rpc.BriefCalendarSection{}, rpc.BriefRiskSection{Config: row}, rpc.BriefPortfolioSection{}, rpc.BriefProcessSection{}, rpc.BriefReadyProposalsRow{})
	if !slices.Contains(ready.Ranked, rpc.BriefReadyRowConfig) || !slices.Contains(briefAttentionOrder(ready, nil), rpc.BriefAttentionReadyPrefix+rpc.BriefReadyRowConfig) {
		t.Fatalf("ranked %v / attention %v, want the config row", ready.Ranked, briefAttentionOrder(ready, nil))
	}

	now := time.Now().UTC()
	facts, _, covered, _ := alertShadowDataHealthFacts(alertShadowDataHealthInput{AsOf: now, GatewayPhase: alertShadowGatewayReady,
		Health: rpc.HealthResult{Connected: true, Subsystems: []rpc.SubsystemHealth{{Name: "storage", Status: "ready"}, sub,
			{Name: "quote", Status: "ready"}, {Name: "history", Status: "ready"}, {Name: "chain", Status: "ready"}}}})
	if !covered || len(facts) != 1 || facts[0].Root != "subsystem:config" || alertDataHealthPresentationCode(facts[0].Root) != rpc.AlertPresentationDataHealthConfig {
		t.Fatalf("data-health facts = %+v (covered %v), want one config fact", facts, covered)
	}
	cancel()
	select {
	case err := <-started:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after cancellation")
	}
	srv.Stop()
}

// Unreadable [trading] order limits run on Canary's defaults, and
// pre-authorised submission pauses while they do; manual reduce-only work
// continues. A part that does not shape automation pauses nothing.
func TestUnreadableOrderLimitsRunOnDefaultsAndPauseAutomation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[gateway]\naccount = \"DU111\"\nclient_id = 21\n[trading]\nmode = \"paper\"\nmax_notional = \"2.5k\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, issues, err := config.LoadForDaemon(path)
	if err != nil {
		t.Fatalf("unreadable order limit stopped the daemon: %v", err)
	}
	resolved, err := cfg.Resolve()
	if err != nil || resolved.Trading.MaxNotional != 10000 || resolved.Trading.Mode != config.TradingModePaper {
		t.Fatalf("resolved trading = %+v (%v), want the default max_notional and the paper pin", resolved.Trading, err)
	}

	rig := newAutomaticTestRig(t, `pre_authorised = ["trailing_stop"]`)
	if _, ok := rig.engine.automaticPolicy(); !ok {
		t.Fatal("pre-authorised submission should run before any config issue")
	}
	rig.server.addConfigIssue(config.Issue{Key: "flex.enabled", Problem: "cannot be read"})
	if _, ok := rig.engine.automaticPolicy(); !ok {
		t.Fatal("a [flex] issue must not pause pre-authorised submission")
	}
	for _, issue := range issues {
		rig.server.addConfigIssue(issue)
	}
	if _, ok := rig.engine.automaticPolicy(); ok {
		t.Fatal("pre-authorised submission must pause while [trading] runs on defaults")
	}
	if paused, sections := rig.server.configPausesAutomation(); !paused || !slices.Equal(sections, []string{"trading"}) {
		t.Fatalf("pause = %v %v, want [trading]", paused, sections)
	}
	prop := rig.stopProposal()
	revision := rig.install(prop)
	rig.cycle()
	rig.noRecord(prop.Key, revision)
	if _, blockers, handled := rig.engine.fastPathPreviewProposal(prop.Key, revision); handled {
		for _, b := range blockers {
			if b.Code == "config_automation_paused" {
				t.Fatalf("the pause blocked a manual reduce-only preview: %+v", blockers)
			}
		}
	}
	if row := rig.server.briefConfigRow(); row == nil || !row.AutomationPaused || !strings.Contains(row.Detail, "pre-authorised submission is paused") {
		t.Fatalf("brief config row = %+v, want the pause named", row)
	}
}
