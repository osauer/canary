package logging

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
)

// recordingHandler keeps every record it is asked to handle.
type recordingHandler struct {
	records []slog.Record
	attrs   [][]slog.Attr
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	var attrs []slog.Attr
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	h.records = append(h.records, r)
	h.attrs = append(h.attrs, attrs)
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func install(t *testing.T) *recordingHandler {
	t.Helper()
	h := &recordingHandler{}
	SetLogger(slog.New(h))
	Configure("info")
	t.Cleanup(func() {
		SetLogger(nil)
		Configure("info")
	})
	return h
}

// Each severity reaches the installed handler at the matching slog level,
// tagged with the component, and the level filter drops what is below it.
func TestEmitMapsSeveritiesAndFiltersByLevel(t *testing.T) {
	h := install(t)
	entry := Component("IBKR Wire")
	Configure("debug")
	entry.Debugf("frame %d", 1)
	entry.Infof("frame %d", 2)
	entry.Warnf("frame %d", 3)
	entry.Errorf("frame %d", 4)
	want := []slog.Level{slog.LevelDebug, slog.LevelInfo, slog.LevelWarn, slog.LevelError}
	if len(h.records) != len(want) {
		t.Fatalf("records = %d, want %d", len(h.records), len(want))
	}
	for i, r := range h.records {
		if r.Level != want[i] {
			t.Errorf("record %d level %v, want %v", i, r.Level, want[i])
		}
		if len(h.attrs[i]) != 1 || h.attrs[i][0].Key != "component" || h.attrs[i][0].Value.String() != "IBKR Wire" {
			t.Errorf("record %d attrs %v, want component=IBKR Wire", i, h.attrs[i])
		}
	}
	if h.records[1].Message != "frame 2" {
		t.Errorf("message %q", h.records[1].Message)
	}

	h.records = nil
	Configure("warn")
	entry.Debugf("dropped")
	entry.Infof("dropped")
	entry.Warnf("kept")
	entry.Errorf("kept")
	if len(h.records) != 2 || h.records[0].Level != slog.LevelWarn || h.records[1].Level != slog.LevelError {
		t.Fatalf("warn filter kept %v", h.records)
	}
	h.records = nil
	Configure("error")
	entry.Warnf("dropped")
	entry.Errorf("kept")
	if len(h.records) != 1 || h.records[0].Level != slog.LevelError {
		t.Fatalf("error filter kept %v", h.records)
	}
	// An entry without a component carries no component attribute.
	h.records, h.attrs = nil, nil
	Configure("debug")
	Component("").Infof("bare")
	if len(h.records) != 1 || len(h.attrs[0]) != 0 {
		t.Fatalf("bare entry attrs = %v", h.attrs)
	}
}

func TestConfigureParsesLevelsAndDefaultsToInfo(t *testing.T) {
	install(t)
	cases := map[string]Level{
		"debug": LevelDebug, " DEBUG ": LevelDebug,
		"info": LevelInfo, "": LevelInfo, "verbose": LevelInfo, "trace": LevelInfo,
		"warn": LevelWarn, "warning": LevelWarn, "Warning": LevelWarn,
		"error": LevelError, "ERROR": LevelError,
	}
	for raw, want := range cases {
		Configure(raw)
		if !LevelEnabled(want) {
			t.Errorf("Configure(%q): level %d not enabled", raw, want)
		}
		if want > LevelDebug && LevelEnabled(want-1) {
			t.Errorf("Configure(%q): level %d enabled below the filter", raw, want-1)
		}
	}
}

// Nothing is written until a sink is installed, and SetLogger(nil) silences
// the package again without touching the level filter.
func TestSetLoggerNilDiscardsOutput(t *testing.T) {
	h := install(t)
	Configure("debug")
	SetLogger(nil)
	Component("x").Errorf("silent")
	if len(h.records) != 0 {
		t.Fatalf("records after SetLogger(nil): %d", len(h.records))
	}
	if !LevelEnabled(LevelDebug) {
		t.Fatal("SetLogger(nil) changed the level filter")
	}
	var buf bytes.Buffer
	SetLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	Component("x").Warnf("audible %d", 7)
	if !bytes.Contains(buf.Bytes(), []byte("level=WARN")) || !bytes.Contains(buf.Bytes(), []byte("component=\"x\"")) && !bytes.Contains(buf.Bytes(), []byte("component=x")) {
		t.Fatalf("text sink got %q", buf.String())
	}
}

func TestAsAttrsKeepsOnlyAttrs(t *testing.T) {
	out := asAttrs([]any{slog.String("a", "1"), 7, "x", slog.Int("b", 2)})
	if len(out) != 2 || out[0].Key != "a" || out[1].Key != "b" {
		t.Fatalf("asAttrs = %v", out)
	}
}
