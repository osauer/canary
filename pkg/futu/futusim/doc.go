// Package futusim is a hermetic OpenD simulator: it speaks the real wire
// protocol of pkg/futu against a synthetic book, so the client, the daemon
// and the surfaces are proven without an OpenD, an account or the network.
//
// This layer is the book ([Book]): accounts, instruments, a deterministic
// quote walk, positions, orders and fills, the subscription and history
// quotas, all driven by an injected [Clock]. Two books loaded from the same
// fixture and moved by the same clock produce the same quotes and fills, so
// a scripted session records the same frames on every run. The fixture
// testdata/book.json is the default book; every id and figure in it is
// synthetic and obviously so, and a test holds it to the account-data
// gate's patterns.
//
// The book never reads the machine it runs on: no configuration, no XDG
// roots and no wall clock unless the caller builds its clock from one. It
// never implements the trade unlock: a locked real account stays locked.
package futusim
