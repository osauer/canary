package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/loglevel"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// Logger is a tiny slog-backed front for the daemon. It also configures the
// pkg/ibkr internal logger so library output funnels through the same handler.
type Logger struct {
	l *slog.Logger
	// blockedIncident reports whether the daemon's authority-storage incident
	// is open. A warning that carries corestore.ErrBlocked then joins that
	// incident at debug instead of repeating a latch the store owner already
	// announced. Nil, the zero value, keeps every warning.
	blockedIncident atomic.Pointer[func() bool]
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
func (l *Logger) Debugf(f string, args ...any) {
	if l.debugEnabled() {
		l.l.Debug(fmt.Sprintf(f, args...))
	}
}

// Infof logs a formatted message at info level.
func (l *Logger) Infof(f string, args ...any) { l.l.Info(fmt.Sprintf(f, args...)) }

// SetBlockedIncidentHook installs the membership check Warnf consults when an
// argument wraps corestore.ErrBlocked. The hook may only join an incident its
// owner already opened (logepisode.State.Join); a dependency can never open
// one, so with no incident the warning stays a warning. A nil hook restores
// plain warnings. Safe on a nil Logger.
func (l *Logger) SetBlockedIncidentHook(join func() bool) {
	if l == nil {
		return
	}
	if join == nil {
		l.blockedIncident.Store(nil)
		return
	}
	l.blockedIncident.Store(&join)
}

// Warnf logs a formatted message at warning level. A warning whose arguments
// carry corestore.ErrBlocked during an open authority-storage incident is a
// dependent symptom of the latch already announced and lands at debug.
func (l *Logger) Warnf(f string, args ...any) {
	if l.joinsBlockedIncident(args) {
		l.Debugf(f, args...)
		return
	}
	l.l.Warn(fmt.Sprintf(f, args...))
}

func (l *Logger) joinsBlockedIncident(args []any) bool {
	join := l.blockedIncident.Load()
	if join == nil {
		return false
	}
	for _, arg := range args {
		if err, ok := arg.(error); ok && errors.Is(err, corestore.ErrBlocked) {
			return (*join)()
		}
	}
	return false
}

// Errorf logs a formatted message at error level.
func (l *Logger) Errorf(f string, args ...any) { l.l.Error(fmt.Sprintf(f, args...)) }

func (l *Logger) debugEnabled() bool {
	return l.l.Enabled(context.Background(), slog.LevelDebug)
}
