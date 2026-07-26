// Package ibkr implements a clean-room Go client for the Interactive Brokers
// TWS wire protocol. It connects directly to TWS or IB Gateway and does not
// depend on Interactive Brokers client libraries.
//
// The low-level [Connection] type owns one protocol session, including the
// socket, handshake, request identifiers, framing, and message handlers. Most
// callers should use [Connector], which builds on one Connection and provides
// lifecycle management, subscriptions, request/response helpers, and
// in-memory observations. Construct a Connector with [NewConnector] and start
// it with [Connector.Start]. Protocol coverage is purpose-driven rather than a
// complete implementation of every TWS API operation.
//
// # Order writes
//
// This package transports broker requests; it does not grant trading
// authority. In the default build, the unrestricted [Connector.SubmitOrder],
// [Connector.CancelOrder], [Connection.PlaceOrder], and
// [Connection.CancelOrder] methods, plus [Connector.ExerciseOptions] and
// [Connection.ExerciseOptions], return [ErrTradingDisabled] before sending a
// position-changing frame. Building with the "trading" tag enables those raw
// methods.
//
// The narrower paper-order methods are present in both build modes. They
// validate a caller-supplied [PaperOrderGate] for a concrete paper account and
// matching connection coordinates before writing. That validation is not a
// substitute for application-level authorization, preview, policy, freeze,
// journaling, or reconciliation controls. The Canary daemon invokes broker-write
// paths only in trading-capable builds and owns those additional controls;
// direct library users must provide an equivalent authority boundary.
//
// Broker WhatIf previews are present in both build modes. They send a broker
// evaluation request that does not create a working order and report accepted,
// rejected, or unavailable status; a preview result never grants submit
// authority.
// Open-order snapshots are likewise reads, but cover API-created orders and do
// not by themselves bind manual TWS orders.
//
// # Logging
//
// The package is silent by default. Use [SetLogger] to install a log/slog sink
// and [SetLogLevel] to select the minimum emitted level.
//
// # Trademark
//
// "Interactive Brokers", "IBKR", "TWS", and "IB Gateway" are trademarks of
// Interactive Brokers Group, Inc. or its affiliates. They are used here only
// to identify the protocol. This package is not built, endorsed, or supported
// by Interactive Brokers.
package ibkr
