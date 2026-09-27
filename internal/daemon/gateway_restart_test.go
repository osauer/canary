package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/rpc"
)

func restartAt(t *testing.T, raw string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		t.Fatal(err)
	}
	return at
}

func restartMonitor(t *testing.T, zone string) *gatewayRestartMonitor {
	t.Helper()
	s, err := parseGatewayRestart(config.Gateway{RestartTime: "23:45", RestartTimezone: zone}, restartAt(t, "2026-09-27T20:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	return &gatewayRestartMonitor{schedule: s}
}

func restartReady() *rpc.HealthResult {
	h := &rpc.HealthResult{Connected: true, GatewayPhase: rpc.GatewayPhaseReady, DataHealth: &rpc.DataHealthResult{}}
	h.DataHealth.Summary.State = "current"
	return h
}

func TestGatewayRestartConfiguration(t *testing.T) {
	for _, g := range []config.Gateway{
		{RestartTime: "24:00"}, {RestartTime: "9:00"}, {RestartTime: "23:45", RestartTimezone: "CET"},
		{RestartTime: "23:45", RestartTimezone: "Mars/Olympus"}, {RestartTime: "23:45", RestartGrace: "garbage"},
		{RestartTime: "23:45", RestartGrace: "0s"}, {RestartTime: "23:45", RestartGrace: "2h"}, {RestartTimezone: "UTC"},
	} {
		if _, err := parseGatewayRestart(g, time.Now()); err == nil {
			t.Fatalf("accepted invalid schedule %+v", g)
		}
	}
	for _, zone := range []string{"", "local", "Local"} {
		m := restartMonitor(t, zone)
		r := m.snapshot(restartAt(t, "2026-09-27T23:46:00Z"), &rpc.HealthResult{})
		if r.State != "unknown" || r.TimezoneSource != "unknown" || !r.ScheduledAt.IsZero() {
			t.Fatalf("inferred local zone: %+v", r)
		}
	}
}

func TestGatewayRestartSocketLossRecoveryAndOverrun(t *testing.T) {
	m := restartMonitor(t, "UTC")
	down := &rpc.HealthResult{GatewayPhase: rpc.GatewayPhasePortDown}
	at := restartAt(t, "2026-09-27T23:46:00Z")
	before := authoritativeHealthVerdict(down)
	r := m.snapshot(at, down) // no 1100 or broker notice: socket loss alone
	if r.State != "expected_restart" || r.Source != "operator" || r.Freshness != "unverified" || r.APIDiscovery != restartDiscoveryUnsupported {
		t.Fatalf("unexpected observation: %+v", r)
	}
	if authoritativeHealthVerdict(down) != before || before.State != "OFFLINE" {
		t.Fatal("schedule changed readiness")
	}
	if r = m.snapshot(at.Add(4*time.Minute), down); r.State != "overrun" {
		t.Fatalf("deadline did not overrun: %+v", r)
	}
	health := restartReady()
	health.DataHealth.Summary.State = "stale"
	if r = m.snapshot(at.Add(5*time.Minute), health); r.State != "overrun" || !r.RecoveredAt.IsZero() {
		t.Fatalf("socket-up hid stale data: %+v", r)
	}
	health.DataHealth.Summary.State = "current"
	if r = m.snapshot(at.Add(6*time.Minute), health); r.State != "recovered" || r.RecoveredAt.IsZero() {
		t.Fatalf("fresh recovery absent: %+v", r)
	}
	if r.Freshness != "unverified" {
		t.Fatal("successful recovery claimed verification of Gateway settings")
	}
}

func TestGatewayRestartDoesNotClaimUpstreamLossOrOldOutage(t *testing.T) {
	at := restartAt(t, "2026-09-27T23:44:00Z")
	m := restartMonitor(t, "UTC")
	down := &rpc.HealthResult{GatewayPhase: rpc.GatewayPhasePortDown}
	m.snapshot(at, down)
	if r := m.snapshot(at.Add(2*time.Minute), down); r.State != "unexpected_outage" || !r.ScheduledAt.IsZero() {
		t.Fatalf("preexisting outage relabeled: %+v", r)
	}
	if r := m.snapshot(at.Add(24*time.Hour+2*time.Minute), down); r.State != "unexpected_outage" {
		t.Fatal("old outage became today's restart")
	}
	m = restartMonitor(t, "UTC")
	h := restartReady()
	h.GatewayPhase = rpc.GatewayPhaseBackendLinkDown
	if r := m.snapshot(at.Add(2*time.Minute), h); r.State != "scheduled" || !r.OutageObservedAt.IsZero() {
		t.Fatalf("backend loss called local restart: %+v", r)
	}
}

func TestGatewayRestartUnknownDiscoveryDoesNotInventObservation(t *testing.T) {
	m := &gatewayRestartMonitor{}
	r := m.snapshot(restartAt(t, "2026-09-27T23:46:00Z"), restartReady())
	if r.Source != "unknown" || r.APIDiscovery != restartDiscoveryUnsupported || r.Freshness != "unverified" {
		t.Fatalf("fabricated capability: %+v", r)
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "observed_at") || strings.Contains(string(data), "config_loaded_at") {
		t.Fatalf("fabricated settings observation: %s", data)
	}
	// Unsupported discovery remains explicit even if the connection is ready.
	if r.State != "unknown" {
		t.Fatal("ready socket fabricated a schedule")
	}
}

func TestGatewayRestartTimezonesDSTAndClockRegression(t *testing.T) {
	var m *gatewayRestartMonitor
	for _, at := range []string{"2026-07-02T03:46:00Z", "2026-12-02T04:46:00Z"} {
		m = restartMonitor(t, "America/New_York")
		if r := m.snapshot(restartAt(t, at), &rpc.HealthResult{}); r.State != "expected_restart" {
			t.Fatalf("DST offset not respected: %+v", r)
		}
	}
	zone, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		clock string
		day   time.Time
	}{{"02:30", time.Date(2026, 3, 8, 0, 0, 0, 0, zone)}, {"01:30", time.Date(2026, 11, 1, 0, 0, 0, 0, zone)}} {
		s, err := parseGatewayRestart(config.Gateway{RestartTime: tc.clock, RestartTimezone: "America/New_York"}, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := s.occurrence(tc.day); ok {
			t.Fatal("guessed DST skipped or repeated wall-clock time")
		}
	}
	m = restartMonitor(t, "UTC")
	at := restartAt(t, "2026-09-27T23:46:00Z")
	m.snapshot(at, &rpc.HealthResult{})
	if r := m.snapshot(at.Add(-time.Minute), &rpc.HealthResult{}); r.State != "unknown" || !r.OutageObservedAt.IsZero() {
		t.Fatalf("clock regression kept authority: %+v", r)
	}
}

func TestGatewayRestartNormalRecoveryWaitsForHealth(t *testing.T) {
	m := restartMonitor(t, "UTC")
	at := restartAt(t, "2026-09-27T23:45:00Z")
	m.snapshot(at, &rpc.HealthResult{GatewayPhase: rpc.GatewayPhasePortRejecting})
	h := restartReady()
	h.DataHealth.Summary.State = "stale"
	if r := m.snapshot(at.Add(time.Minute), h); r.State != "recovering" {
		t.Fatalf("premature recovery: %+v", r)
	}
	h.DataHealth.Summary.State = "current"
	if r := m.snapshot(at.Add(2*time.Minute), h); r.State != "recovered" {
		t.Fatalf("fresh recovery absent: %+v", r)
	}
	// A subsequent outage establishes a new episode; old recovery cannot
	// turn an unrelated failure into a planned restart.
	if r := m.snapshot(at.Add(time.Hour), &rpc.HealthResult{}); r.State != "unexpected_outage" || !r.RecoveredAt.IsZero() {
		t.Fatalf("old recovery reused: %+v", r)
	}
}

func TestGatewayRestartStaleSnapshotPreservesDeadline(t *testing.T) {
	m := restartMonitor(t, "UTC")
	at := restartAt(t, "2026-09-27T23:46:00Z")
	down := &rpc.HealthResult{GatewayPhase: rpc.GatewayPhasePortDown}
	first := m.snapshot(at, down)
	m.snapshot(at.Add(4*time.Minute), down)
	if r := m.snapshot(at.Add(time.Minute), restartReady()); r.State != "unknown" {
		t.Fatalf("old sample reused as recovery: %+v", r)
	}
	r := m.snapshot(at.Add(5*time.Minute), down)
	if r.State != "overrun" || !r.OutageObservedAt.Equal(first.OutageObservedAt) || !r.Deadline.Equal(first.Deadline) {
		t.Fatalf("stale sample reset episode: %+v", r)
	}
}

func TestGatewayRestartMalformedScheduleReportsConfigIssue(t *testing.T) {
	at := restartAt(t, "2026-09-27T23:46:00Z")
	s := &Server{cfg: &config.Resolved{Gateway: config.Gateway{RestartTime: "invalid"}}, now: func() time.Time { return at }}
	s.configureGatewayRestart()
	issues, _ := s.configIssuesSnapshot()
	if len(issues) != 1 {
		t.Fatalf("missing config issue: %+v", issues)
	}
	if r := s.gatewayRestart.snapshot(at, restartReady()); r.State != "unknown" || !r.Deadline.IsZero() {
		t.Fatalf("invalid declaration gained a window: %+v", r)
	}
}
