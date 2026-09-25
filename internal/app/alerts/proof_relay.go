package alerts

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/app/state"
	"github.com/osauer/canary/v2/internal/rpc"
)

// ProofReporter delivers one push delivery proof to the daemon.
type ProofReporter interface {
	ReportPushDeliveryProof(context.Context, rpc.PushDeliveryProof) (*rpc.AlertDeliveryProofResult, error)
}

const (
	defaultProofRelayEvery   = 5 * time.Minute
	defaultProofRelayTimeout = 5 * time.Second
)

// ProofRelay reports the app's redacted push delivery proof to the daemon:
// once at start, after every journal change (a send or a device receipt),
// and on a slow heartbeat so the daemon's received_at shows whether this app
// host is still reporting. Reporting is evidence only; a failure never
// blocks delivery and is retried by the next change or heartbeat.
type ProofRelay struct {
	Store    *state.Store
	Reporter ProofReporter
	Every    time.Duration
	Timeout  time.Duration
	Now      func() time.Time

	wakeOnce sync.Once
	wake     chan struct{}
	mu       sync.Mutex
	failing  bool
}

// NewProofRelay returns a relay for store that reports through reporter.
func NewProofRelay(store *state.Store, reporter ProofReporter) *ProofRelay {
	return &ProofRelay{Store: store, Reporter: reporter}
}

func (r *ProofRelay) wakeChannel() chan struct{} {
	r.wakeOnce.Do(func() { r.wake = make(chan struct{}, 1) })
	return r.wake
}

// Notify asks the relay to report soon. It never blocks; bursts coalesce.
func (r *ProofRelay) Notify() {
	if r == nil {
		return
	}
	select {
	case r.wakeChannel() <- struct{}{}:
	default:
	}
}

// Run reports until ctx is cancelled.
func (r *ProofRelay) Run(ctx context.Context) {
	if r == nil || r.Store == nil || r.Reporter == nil {
		return
	}
	every := r.Every
	if every <= 0 {
		every = defaultProofRelayEvery
	}
	wake := r.wakeChannel()
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	r.ReportOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			r.ReportOnce(ctx)
		case <-ticker.C:
			r.ReportOnce(ctx)
		}
	}
}

// ReportOnce projects the current proof and relays it, logging only when the
// relay starts or stops failing so a daemon outage is one line, not one per
// heartbeat.
func (r *ProofRelay) ReportOnce(ctx context.Context) {
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	proof := r.Store.PushDeliveryProof(now)
	var err error
	if err = rpc.ValidatePushDeliveryProof(proof); err == nil {
		timeout := r.Timeout
		if timeout <= 0 {
			timeout = defaultProofRelayTimeout
		}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err = r.Reporter.ReportPushDeliveryProof(callCtx, proof)
		cancel()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case err != nil && !r.failing:
		r.failing = true
		slog.Warn("canary app push proof: relay to daemon failed; retrying on the next change or heartbeat", "error", err)
	case err == nil && r.failing:
		r.failing = false
		slog.Warn("canary app push proof: relay to daemon recovered")
	}
}
