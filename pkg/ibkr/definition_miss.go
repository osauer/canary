package ibkr

import "context"

type definitionMissClassifiedKey struct{}

// WithDefinitionMissClassified marks the requests sent under ctx as ones whose
// caller records IBKR's "no security definition" answer itself. The wire echo
// of that answer is then INFO even for a request that carried a conID; without
// the mark a miss on a known conID stays a warning, because it is a stale
// identity on a position or chart. The lending-market worker needs it: it
// re-reads delisted names that IBKR's short-stock file still lists and parks
// each on that answer, so the first sighting of every name was a WARN (87 lines
// on 2026-10-06).
func WithDefinitionMissClassified(ctx context.Context) context.Context {
	return context.WithValue(ctx, definitionMissClassifiedKey{}, true)
}

// DefinitionMissClassified reports whether ctx carries that mark.
func DefinitionMissClassified(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	marked, _ := ctx.Value(definitionMissClassifiedKey{}).(bool)
	return marked
}
