package loglevel

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func TestParseSharedContract(t *testing.T) {
	cases := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"":        slog.LevelWarn,
		"bogus":   slog.LevelWarn,
		" WARN ":  slog.LevelWarn,
	}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %v, want %v", in, got, want)
		}
	}
}

// At the warn default, routine INFO must vanish while the lifecycle markers
// the nightly log check clusters restarts on must keep landing in the file.
func TestLifecycleMarkersSurviveWarnFloor(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(NewTextHandler(&buf, slog.LevelWarn))

	l.Info("Request completed", "status", 200)
	l.Debug("noise")
	l.Info("Connected to IB Gateway at 127.0.0.1:4002")
	l.Info("canary app serving", "listen", "0.0.0.0:8765")
	l.Info("Shutting down server.", "reason", "terminated")
	l.Warn("something actionable")

	out := buf.String()
	for _, absent := range []string{"Request completed", "noise"} {
		if strings.Contains(out, absent) {
			t.Errorf("sub-floor line %q reached the log:\n%s", absent, out)
		}
	}
	for _, present := range []string{"Connected to IB Gateway", "canary app serving", "Shutting down server.", "something actionable"} {
		if !strings.Contains(out, present) {
			t.Errorf("line %q missing from the log:\n%s", present, out)
		}
	}
}
