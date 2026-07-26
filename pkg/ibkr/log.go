package ibkr

import (
	"log/slog"

	"github.com/osauer/canary/v2/pkg/ibkr/internal/logging"
)

// SetLogger installs a custom slog.Logger as the sink for all messages
// produced by the IBKR library. Pass nil to discard output entirely.
//
// Daemons and command-line tools should call this once at startup so library
// output funnels through the same handler the rest of the application uses.
func SetLogger(l *slog.Logger) { logging.SetLogger(l) }

// SetLogLevel adjusts the active level by string ("debug"|"info"|"warn"|"error").
// Unknown values default to "info".
func SetLogLevel(level string) { logging.Configure(level) }
