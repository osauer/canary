package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
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

// The reading is one trading session's close and the daemon keeps serving
// the last good one when a refresh cannot converge. Without the date on
// screen there was no way to tell today's breadth from a snapshot the lane
// stopped updating days ago.
func TestBreadthTextDatesTheReading(t *testing.T) {
	t.Parallel()
	computed := time.Date(2026, 7, 20, 22, 35, 0, 0, time.UTC)

	t.Run("current session", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		renderBreadthText(env, &rpc.BreadthSPXResult{
			State: rpc.BreadthStateReady, PctAbove50DMA: 61.2,
			SessionKey: "2026-07-20", AsOf: computed,
		})
		got := stdout.String()
		if !strings.Contains(got, "2026-07-20") {
			t.Errorf("session date missing:\n%s", got)
		}
		if strings.Contains(got, "stale") {
			t.Errorf("a current session must not be marked stale:\n%s", got)
		}
	})

	t.Run("stale session", func(t *testing.T) {
		var stdout bytes.Buffer
		env := &Env{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		renderBreadthText(env, &rpc.BreadthSPXResult{
			State: rpc.BreadthStateReady, PctAbove50DMA: 61.2,
			SessionKey: "2026-07-20", AsOf: computed, Stale: true,
		})
		got := stdout.String()
		if !strings.Contains(got, "stale") {
			t.Errorf("stale reading rendered as current:\n%s", got)
		}
		if !strings.Contains(got, "61.2") {
			t.Errorf("a stale close is still a real close and must still print:\n%s", got)
		}
	})
}
