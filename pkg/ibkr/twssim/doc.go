// Package twssim is a simulated TWS: a broker and market on a loopback TCP
// port that speaks the TWS API at server version 203, so the real daemon, its
// real IBKR connector and its wire codec run against it without TWS, an
// account or the network.
//
// The state is a [Book]: one account with cash per currency, positions,
// a contract catalogue with deterministic quotes and daily bars, orders and
// executions. A [Server] answers the handshake, startAPI, account updates and
// summary, positions, PnL, contract details, market data, history, open
// orders, executions, WhatIf previews, placed and cancelled orders, and fills
// marketable limit orders at the touch. Every request a client sends is kept,
// decoded, by the [Recorder]; a request the simulator does not model is
// answered with an error at once, never left hanging.
//
// What it proves and what it does not. The simulator decodes the client's
// placeOrder and cancelOrder by the broker's own protocol-buffer field table
// (schema.go, from IB Gateway 10.50), so a field the broker would drop shows
// as IgnoredByBroker. Every other frame is written to the layouts pkg/ibkr's
// decoder and its existing test fakes read, so on those the simulator agrees
// with Canary by construction: it proves sequencing, gates, recovery and
// evidence handling, not that real TWS sends a frame in that shape.
//
// Behaviour taken from assumption rather than from a captured frame, an
// existing fake or IBKR's documentation:
//
//	behaviour                                              status
//	marketable BUY LMT fills whole at the ask at once       assumed
//	fill sequence: execDetails, openOrder, orderStatus,     assumed
//	  commissionReport, then account and portfolio push
//	WhatIf answer: openOrder only, status PreSubmitted,     assumed
//	  margin 25% of gross, commission min(max(1, 0.005/sh),
//	  1% notional) as commission, min and max
//	child order (parentId) waits PreSubmitted, then works   assumed
//	transmit=false order held, unanswered, until a child    assumed
//	  with transmit=true transmits the pair
//	DAY order sent while the session is closed rests        assumed
//	  PreSubmitted; Submitted and fillable at the open
//	child rejected: parent left untransmitted at the broker assumed
//	parent rejected: child cancelled with it (error 202)    assumed
//	DAY parent ending partly filled: child cancelled, or    assumed (option)
//	  resized to the filled quantity (ChildOnPartialExpiry)
//	option prices and greeks: Black-Scholes, 30% vol, 4%    assumed
//	  rate; open interest 500
//	BAG: quoted from its legs; fills whole at the net ask,  assumed
//	  one execution per leg under the BAG's order id
//	10123 for a combination leg with openClose 1 or 2       assumed (option)
//	a line taken flat stays a zero row on updatePortfolio   assumed
//	  and position callbacks for the session
//	every scripted Fault (reject 201/442, reject after ack, assumed
//	  PreSubmitted, partial, silent, drop, error 110/103/200/354/10089)
//	market data type 1 (live) at every hour                 assumed
//	daily bars on weekdays only, no exchange holidays       assumed
//	the last daily bar is the last weekday whose New York   assumed
//	  open (09:30) has passed: the session's forming bar
//	execution time "yyyyMMdd-HH:mm:ss" in UTC               assumed
//	error 10354 for attachedOrders (Options, off by default) read from the
//	                                                        10.50 bytecode
//	WSH requests answered with error 10276                  assumed
//	an SEHK listing's board lot carried as minSize,         assumed
//	  sizeIncrement and suggestedSizeIncrement (100), valid
//	  exchanges without SMART, the HKEX spread table as its
//	  market rule
//	a stock order off its board lot rejected, on WhatIf     assumed
//	  and on place, error 201 with a reason naming the lot
//	reqMatchingSymbols: stocks whose symbol or company name assumed
//	  starts with the pattern, by contract id, at most 16,
//	  no derivative sec types
//	a cash debit (a buy in a currency not held) reported    assumed
//	  as a negative CashBalance and SettledCash row
//	each currency's rate to the base listed as its IDEALPRO assumed
//	  pair (EUR.USD, USD.HKD) at that rate, spread 1 pip
//	  or 0.01%, contract id 905000 + the pair's place
//	BUY orders, parentId with transmit false, BAG orders    never sent live
//
// Live-proven by past fills on the real account, so modelled from fact: STK
// SELL TRAIL GTC with openClose C and trigger method 2, and OPT SELL LMT DAY
// with the Adaptive algorithm.
//
// The simulator never reads the machine it runs on and refuses to listen
// anywhere but loopback or on IBKR's API ports ([LiveAPIPorts]). Every id,
// ticker and figure in [Default] is invented, except the public contract ids
// of the regime's inputs (SPY, HYG, VIX, VIX3M, SPX, USD.JPY), which the
// daemon seeds its contract cache with.
package twssim
