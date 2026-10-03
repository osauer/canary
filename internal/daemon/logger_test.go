package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
)

func TestWarnfJoinsOpenAuthorityIncidentAtDebug(t *testing.T) {
	blocked := fmt.Errorf("persist alert episode registry: %w", corestore.ErrBlocked)
	open := func() bool { return true }
	closed := func() bool { return false }
	cases := []struct {
		name      string
		hook      func() bool
		level     string
		arg       any
		wantLevel string // "WARN", "DEBUG", or "" for no line
		wantJoins int
	}{
		{name: "open incident lands at debug", hook: open, level: "debug", arg: blocked, wantLevel: "DEBUG", wantJoins: 1},
		{name: "open incident writes nothing below debug", hook: open, level: "warn", arg: blocked, wantLevel: "", wantJoins: 1},
		{name: "closed incident keeps the warning", hook: closed, level: "warn", arg: blocked, wantLevel: "WARN", wantJoins: 1},
		{name: "no hook keeps the warning", hook: nil, level: "warn", arg: blocked, wantLevel: "WARN"},
		{name: "unrelated error never consults the hook", hook: open, level: "warn", arg: errors.New("disk on fire"), wantLevel: "WARN"},
		{name: "non-error argument never consults the hook", hook: open, level: "warn", arg: corestore.ErrBlocked.Error(), wantLevel: "WARN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, tc.level)
			joins := 0
			if tc.hook != nil {
				hook := tc.hook
				logger.SetBlockedIncidentHook(func() bool { joins++; return hook() })
			}
			logger.Warnf("contract cache: save: %v", tc.arg)
			out := buf.String()
			switch tc.wantLevel {
			case "":
				if out != "" {
					t.Fatalf("expected no output, got %q", out)
				}
			default:
				if strings.Count(out, "level="+tc.wantLevel) != 1 || !strings.Contains(out, "contract cache: save:") {
					t.Fatalf("want one %s line, got %q", tc.wantLevel, out)
				}
			}
			if joins != tc.wantJoins {
				t.Fatalf("hook joins=%d want %d", joins, tc.wantJoins)
			}
		})
	}
}

type blockedCountedString struct{ calls *int }

func (s blockedCountedString) String() string { *s.calls++; return "formatted" }

func TestWarnfSuppressedByIncidentDoesNotFormat(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "warn")
	logger.SetBlockedIncidentHook(func() bool { return true })
	calls := 0
	logger.Warnf("%s: %v", blockedCountedString{&calls}, fmt.Errorf("persist: %w", corestore.ErrBlocked))
	if calls != 0 || buf.Len() != 0 {
		t.Fatalf("suppressed dependent warning formatted: calls=%d out=%q", calls, buf.String())
	}
}

func TestBlockedIncidentHookIsNilSafe(t *testing.T) {
	var nilLogger *Logger
	nilLogger.SetBlockedIncidentHook(func() bool { return true })
	var buf bytes.Buffer
	logger := NewLogger(&buf, "warn")
	logger.SetBlockedIncidentHook(func() bool { return true })
	logger.SetBlockedIncidentHook(nil)
	logger.Warnf("x: %v", corestore.ErrBlocked)
	if strings.Count(buf.String(), "level=WARN") != 1 {
		t.Fatalf("cleared hook must restore warnings, got %q", buf.String())
	}
}
