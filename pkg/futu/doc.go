// Package futu implements a clean-room Go client for the Futu OpenD TCP
// protocol, the gateway process through which Futu and moomoo accounts are
// reached. It connects to a local OpenD and does not depend on Futu's client
// libraries.
//
// The package is built in the shape of the TWS client in pkg/ibkr: a
// low-level connection owns one socket, the handshake, serial numbers,
// framing and message dispatch, and a connector on top owns lifecycle,
// subscriptions, request helpers and in-memory observations. This first
// layer holds what every later layer builds on: the 44-byte frame
// ([EncodeFrame], [ReadFrame]), the typed errors ([Error], [ErrFrame],
// [ErrPaced], [ErrServerVersion], [ErrTradingDisabled]) and the pacing
// table ([Limits], [Pacer]) that keeps the client inside OpenD's documented
// per-interface frequency limits before OpenD would refuse a request.
//
// Canary speaks protobuf only; a frame whose body format byte names JSON is
// refused. Encryption is not implemented: the client asks OpenD for no
// packet encryption, and an OpenD configured with an RSA key refuses the
// session. The loopback socket to a local OpenD is the trust boundary.
//
// The package's statement coverage report is published at
// https://osauer.dev/canary/coverage/pkg-futu.html.
//
// # Order writes
//
// This package transports broker requests; it does not grant trading
// authority. In the default build every order-writing method returns
// [ErrTradingDisabled] before a frame is written. There is no method for
// OpenD's trade unlock (protocol 2005) in any build: the daemon that uses
// this client never unlocks an account, and a test fails on any encoder for
// that protocol.
package futu
