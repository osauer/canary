package daemon

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Logger is a tiny slog-backed front for the daemon. It also configures the
// pkg/ibkr internal logger so library output funnels through the same handler.
type Logger struct {
	l *slog.Logger
}

// NewLogger constructs a slog text logger writing to w at the given level
// ("debug"|"info"|"warn"|"error").
func NewLogger(w io.Writer, level string) *Logger {
	lv := parseLevel(level)
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: lv})
	l := slog.New(h)

	ibkrlib.SetLogger(l)
	ibkrlib.SetLogLevel(level)

	return &Logger{l: l}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Debugf logs a formatted message at debug level.
func (l *Logger) Debugf(f string, args ...any) { l.l.Debug(fmt.Sprintf(f, args...)) }

// Infof logs a formatted message at info level.
func (l *Logger) Infof(f string, args ...any) { l.l.Info(fmt.Sprintf(f, args...)) }

// Warnf logs a formatted message at warning level.
func (l *Logger) Warnf(f string, args ...any) { l.l.Warn(fmt.Sprintf(f, args...)) }

// Errorf logs a formatted message at error level.
func (l *Logger) Errorf(f string, args ...any) { l.l.Error(fmt.Sprintf(f, args...)) }
