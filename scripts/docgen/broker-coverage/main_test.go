package main

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon"
)

// The page lists every input and every capability with its methods.
func TestRenderListsEveryInputAndCapability(t *testing.T) {
	body := render()
	for _, in := range daemon.BrokerInputs() {
		if !strings.Contains(body, "| "+in.Name+" |") {
			t.Errorf("input %q missing", in.Name)
		}
	}
	methods := daemon.CapabilityMethods()
	_, capabilityTables, _ := strings.Cut(body, "## How each capability runs")
	for _, c := range daemon.Capabilities() {
		if strings.Count(capabilityTables, "\n| "+c.Name+" |") != 2 {
			t.Errorf("capability %q is not in both tables", c.Name)
		}
		for _, m := range methods[c.Name] {
			if !strings.Contains(body, "`"+m+"`") {
				t.Errorf("method %s of %q missing", m, c.Name)
			}
		}
	}
}
