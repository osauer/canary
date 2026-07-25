package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/osauer/ibkr/v2/internal/rpc"
)

// A zero reading is the "no data yet" sentinel for every state except ready,
// so the text renderer used to print "Above 50-DMA 0.0 %" on a cold daemon.
// That reads as an extreme market signal rather than an empty cache.
func TestBreadthTextHidesUnreadyNumbers(t *testing.T) {
	t.Parallel()
	for _, state := range []rpc.BreadthState{rpc.BreadthStateCold, rpc.BreadthStateComputing} {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		renderBreadthText(env, &rpc.BreadthSPXResult{
			State:   state,
			Refresh: &rpc.BreadthRefreshProgress{Processed: 60, Total: 503},
		})
		got := stdout.String()
		if strings.Contains(got, "Above 50-DMA") {
			t.Errorf("state %q printed a reading:\n%s", state, got)
		}
		if !strings.Contains(got, "No reading yet") {
			t.Errorf("state %q did not say why it is empty:\n%s", state, got)
		}
		if !strings.Contains(got, "60 / 503") {
			t.Errorf("state %q dropped the refresh progress:\n%s", state, got)
		}
	}
}

func TestBreadthTextShowsReadyNumbers(t *testing.T) {
	t.Parallel()
	var stdout bytes.Buffer
	env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
	renderBreadthText(env, &rpc.BreadthSPXResult{State: rpc.BreadthStateReady, PctAbove50DMA: 61.2})
	if !strings.Contains(stdout.String(), "61.2") {
		t.Errorf("ready state must print its reading:\n%s", stdout.String())
	}
}
