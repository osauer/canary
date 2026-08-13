// Package loglevel owns the shared log-verbosity contract for the daemon and
// the app host: one four-value level string, and a text handler that keeps
// process-lifecycle markers visible at any level. With the default now warn,
// the INFO start/stop markers would otherwise vanish — and they are the only
// evidence the nightly log check has for clustering starts into a restart
// loop.
package loglevel

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// Parse maps the shared level contract ("debug", "info", "warn", "error") to
// a slog level. Unknown or empty values fall back to warn, the product
// default.
func Parse(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "error":
		return slog.LevelError
	default:
		// "warn", its legacy "warning" alias, empty, and unknown values.
		return slog.LevelWarn
	}
}

// lifecyclePrefixes are the process-lifecycle markers that must reach the log
// at any configured level. scripts/log-monitor keys restart-loop detection on
// these exact strings; a level change must never blind it.
var lifecyclePrefixes = []string{
	"Connected to IB Gateway", // daemon: broker session established
	"canary app serving",      // app host: process start
	"Shutting down server.",   // app host (HyperServe): clean stop
}

func lifecycleMessage(msg string) bool {
	for _, prefix := range lifecyclePrefixes {
		if strings.HasPrefix(msg, prefix) {
			return true
		}
	}
	return false
}

// NewTextHandler returns a slog text handler on w honoring level, except that
// lifecycle markers at INFO and above always pass.
func NewTextHandler(w io.Writer, level slog.Level) slog.Handler {
	return &handler{
		inner: slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}),
		floor: level,
	}
}

type handler struct {
	inner slog.Handler
	floor slog.Level
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	// INFO records must reach Handle so lifecycle messages can pass a
	// warn/error floor; everything below max(floor→INFO cap) stays cheap to
	// discard here.
	return level >= min(h.floor, slog.LevelInfo)
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < h.floor && !lifecycleMessage(r.Message) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{inner: h.inner.WithAttrs(attrs), floor: h.floor}
}

func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{inner: h.inner.WithGroup(name), floor: h.floor}
}
