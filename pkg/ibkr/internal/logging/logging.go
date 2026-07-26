// Package logging is a tiny slog-backed shim that preserves the call-site API
// the ibkr package was originally written against (Component(name).Debugf/Infof/...).
//
// The package writes through the standard library's log/slog so consumers can
// install any slog handler (text, JSON, custom) before the ibkr package starts
// emitting events. The default sink is a discard handler — pkg/ibkr stays
// silent until callers wire a handler via SetLogger(...). This is deliberate:
// daemonised consumers route stderr to /dev/null and a stderr-default would
// wedge the runtime on first emit (see cmd/canary/main.go::spawnDaemon).
// Pass nil to SetLogger to silence again.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
)

// Level identifies the minimum severity accepted by the logging shim.
type Level int

const (
	// LevelDebug enables debug and higher-severity messages.
	LevelDebug Level = iota
	// LevelInfo enables informational and higher-severity messages.
	LevelInfo
	// LevelWarn enables warning and error messages.
	LevelWarn
	// LevelError enables only error messages.
	LevelError
)

var (
	currentLevel atomic.Int32
	logger       atomic.Pointer[slog.Logger]
)

func init() {
	currentLevel.Store(int32(LevelInfo))
	logger.Store(discardLogger())
}

// Configure sets the active level filter ("debug"|"info"|"warn"|"error"); unknown
// values default to info. Does not touch the installed logger — callers that
// previously wired a sink via SetLogger keep it.
func Configure(level string) {
	currentLevel.Store(int32(parseLevel(level)))
}

// SetLogger installs a custom slog.Logger. Pass nil to discard all output.
func SetLogger(l *slog.Logger) {
	if l == nil {
		l = discardLogger()
	}
	logger.Store(l)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// LevelEnabled reports whether the given level would produce output.
func LevelEnabled(l Level) bool {
	return int32(l) >= currentLevel.Load()
}

func parseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

func emit(l Level, prefix, format string, args ...any) {
	if !LevelEnabled(l) {
		return
	}
	msg := fmt.Sprintf(format, args...)
	lg := logger.Load()
	if lg == nil {
		return
	}
	attrs := []any{}
	if prefix != "" {
		attrs = append(attrs, slog.String("component", prefix))
	}
	switch l {
	case LevelDebug:
		lg.LogAttrs(context.Background(), slog.LevelDebug, msg, asAttrs(attrs)...)
	case LevelInfo:
		lg.LogAttrs(context.Background(), slog.LevelInfo, msg, asAttrs(attrs)...)
	case LevelWarn:
		lg.LogAttrs(context.Background(), slog.LevelWarn, msg, asAttrs(attrs)...)
	case LevelError:
		lg.LogAttrs(context.Background(), slog.LevelError, msg, asAttrs(attrs)...)
	}
}

func asAttrs(in []any) []slog.Attr {
	out := make([]slog.Attr, 0, len(in))
	for _, v := range in {
		if a, ok := v.(slog.Attr); ok {
			out = append(out, a)
		}
	}
	return out
}

// Entry tags log lines with a component name.
type Entry struct{ prefix string }

// Component returns a logger entry tagged with the component name.
func Component(name string) Entry { return Entry{prefix: name} }

// Debugf emits a formatted debug message when debug logging is enabled.
func (e Entry) Debugf(format string, args ...any) { emit(LevelDebug, e.prefix, format, args...) }

// Infof emits a formatted informational message when info logging is enabled.
func (e Entry) Infof(format string, args ...any) { emit(LevelInfo, e.prefix, format, args...) }

// Warnf emits a formatted warning message when warning logging is enabled.
func (e Entry) Warnf(format string, args ...any) { emit(LevelWarn, e.prefix, format, args...) }

// Errorf emits a formatted error message.
func (e Entry) Errorf(format string, args ...any) { emit(LevelError, e.prefix, format, args...) }
