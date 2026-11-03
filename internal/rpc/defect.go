package rpc

import "errors"

// DefectHint ends the message of an error that is Canary's own fault: a
// refused invariant, an internal inconsistency, a recovered handler panic.
// It tells the reader, human or agent, where the next step is; a
// configuration, gateway, subscription or request error never carries it.
const DefectHint = "; run `canary feedback`"

// DefectError marks an error as a Canary defect. The daemon classifies it
// as CodeInternal and its message ends with DefectHint.
type DefectError struct{ Err error }

// Error is the cause's message with the hint appended.
func (e *DefectError) Error() string { return e.Err.Error() + DefectHint }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *DefectError) Unwrap() error { return e.Err }

// Defect wraps err as a Canary defect; nil stays nil.
func Defect(err error) error {
	if err == nil {
		return nil
	}
	return &DefectError{Err: err}
}

// IsDefect reports whether err is, or wraps, a Canary defect.
func IsDefect(err error) bool {
	var d *DefectError
	return errors.As(err, &d)
}
