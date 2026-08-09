package ibkr

import (
	"errors"
)

// SendDisposition classifies what an error proves about one broker
// instruction at the physical transport boundary. It says nothing about
// broker acceptance: even a nil local return still requires broker lifecycle
// evidence.
type SendDisposition string

const (
	// SendDispositionDefinitelyUnsent proves that no byte from the instruction
	// reached the socket writer. Callers may safely discard provisional local
	// correlation created only for that attempt.
	SendDispositionDefinitelyUnsent SendDisposition = "definitely_unsent"
	// SendDispositionMayHaveWritten means at least one frame byte may have
	// reached the socket writer. The instruction must not be replayed blindly.
	SendDispositionMayHaveWritten SendDisposition = "may_have_written"
	// SendDispositionUnknown is the conservative fallback when an error source
	// cannot prove either of the stronger transport facts.
	SendDispositionUnknown SendDisposition = "unknown"
)

// SendDispositionError preserves the original error while attaching a
// machine-readable broker-send disposition. Construct it with
// [WithSendDisposition].
type SendDispositionError struct {
	err         error
	disposition SendDisposition
}

// Error returns the underlying broker-send error text.
func (e *SendDispositionError) Error() string { return e.err.Error() }

// Unwrap returns the underlying broker-send error.
func (e *SendDispositionError) Unwrap() error { return e.err }

// SendDisposition returns the attached transport fact.
func (e *SendDispositionError) SendDisposition() SendDisposition {
	if e == nil || !validSendDisposition(e.disposition) {
		return SendDispositionUnknown
	}
	return e.disposition
}

// WithSendDisposition attaches disposition to err without hiding its error
// chain. An existing typed disposition is retained because the innermost
// transport boundary has the most precise knowledge. Nil stays nil.
func WithSendDisposition(err error, disposition SendDisposition) error {
	if err == nil {
		return nil
	}
	if _, ok := errors.AsType[*SendDispositionError](err); ok {
		return err
	}
	if !validSendDisposition(disposition) {
		disposition = SendDispositionUnknown
	}
	return &SendDispositionError{err: err, disposition: disposition}
}

// SendDispositionOf extracts the strongest transport fact attached to err.
// Untyped errors, including errors returned by custom broker hooks, are
// conservatively unknown. A nil error also has no send disposition and returns
// unknown; callers should classify only non-nil results.
func SendDispositionOf(err error) SendDisposition {
	disposition, ok := errors.AsType[*SendDispositionError](err)
	if !ok {
		return SendDispositionUnknown
	}
	return disposition.SendDisposition()
}

func validSendDisposition(disposition SendDisposition) bool {
	switch disposition {
	case SendDispositionDefinitelyUnsent, SendDispositionMayHaveWritten, SendDispositionUnknown:
		return true
	default:
		return false
	}
}

func definitelyUnsent(err error) error {
	return WithSendDisposition(err, SendDispositionDefinitelyUnsent)
}
