package daemon

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/osauer/canary/v2/internal/loglevel"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Logger is a tiny slog-backed front for the daemon. It also configures the
// pkg/ibkr internal logger so library output funnels through the same handler.
type Logger struct {
	l *slog.Logger
}

// NewLogger constructs a slog text logger writing to w at the given level
// ("debug"|"info"|"warn"|"error"). Lifecycle markers pass at any level; see
// internal/loglevel.
func NewLogger(w io.Writer, level string) *Logger {
	l := slog.New(loglevel.NewTextHandler(w, loglevel.Parse(level)))

	ibkrlib.SetLogger(l)
	// The library's own pre-filter must stay at info: it would otherwise drop
	// the INFO "Connected to IB Gateway" lifecycle marker before the handler's
	// lifecycle floor could pass it. Level filtering is the handler's job;
	// only debug stays opt-in at the source.
	wireLevel := "info"
	if loglevel.Parse(level) == slog.LevelDebug {
		wireLevel = "debug"
	}
	ibkrlib.SetLogLevel(wireLevel)

	return &Logger{l: l}
}

// Debugf logs a formatted message at debug level.
func (l *Logger) Debugf(f string, args ...any) { l.l.Debug(fmt.Sprintf(f, args...)) }

// Infof logs a formatted message at info level.
func (l *Logger) Infof(f string, args ...any) { l.l.Info(fmt.Sprintf(f, args...)) }

// Warnf logs a formatted message at warning level.
func (l *Logger) Warnf(f string, args ...any) { l.l.Warn(fmt.Sprintf(f, args...)) }

// Errorf logs a formatted message at error level.
func (l *Logger) Errorf(f string, args ...any) { l.l.Error(fmt.Sprintf(f, args...)) }
