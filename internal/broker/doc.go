// Package broker names the reads every broker must serve to the daemon:
// session health, account summary, positions, quotes, bars and instrument
// lookup. It is a type package: no I/O, no policy, no state, and no import of
// a broker client (pkg/ibkr, pkg/futu) or of the cross-surface contract
// (internal/rpc). The daemon reads through these interfaces; a broker adapter
// (internal/broker/ibkr, internal/broker/opend) maps its client's types onto
// them. Writes are not here: the order path keeps its concrete broker client
// behind the daemon's write transaction binding.
//
// Rules for the seam: nil means unavailable, never zero; every result carries
// its own clock and provenance; a SessionBinding is a value with nothing to
// dereference, and a binding from an earlier connector generation is never
// current.
package broker
