package ibkr

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type partialOrderWriteError struct {
	calls atomic.Int32
}

type protectedTransportOperation struct {
	name string
	run  func(context.Context, *Connector, ConnectorSessionBinding, PaperOrderGate, func() error) error
}

func protectedTransportOperations() []protectedTransportOperation {
	contract := func() *Contract {
		return &Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}
	}
	order := func(id int) *RawOrder {
		return &RawOrder{OrderID: id, Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU7654321"}
	}
	exercise := func() OptionExerciseRequest {
		return OptionExerciseRequest{
			Contract: &Contract{
				ConID: 12345, Symbol: "TEST", SecType: "OPT", Expiry: "20260717", Strike: 100,
				Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "TEST",
			},
			ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: "DU7654321",
		}
	}
	return []protectedTransportOperation{
		{name: "place", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.SubmitPaperOrderForSessionGuarded(ctx, binding, gate, contract(), order(0), guard)
		}},
		{name: "modify", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.SubmitPaperOrderForSessionGuarded(ctx, binding, gate, contract(), order(150), guard)
		}},
		{name: "cancel", run: func(ctx context.Context, c *Connector, binding ConnectorSessionBinding, gate PaperOrderGate, guard func() error) error {
			return c.CancelPaperOrderForSessionGuarded(ctx, binding, gate, 99, guard)
		}},
		{name: "exercise", run: func(ctx context.Context, _ *Connector, binding ConnectorSessionBinding, _ PaperOrderGate, guard func() error) error {
			req := exercise()
			var err error
			var epoch uint64
			req.TickerID, epoch, err = binding.connection.reserveNextRequestIDForEpoch(binding.epoch)
			if err != nil {
				return err
			}
			defer binding.connection.discardRequestIDReservation(req.TickerID)
			return binding.connection.exerciseOptionsForEpochGuarded(ctx, req, &epoch, guard)
		}},
	}
}

func waitForProtectedDispatch(t *testing.T, conn *Connection, before uint64) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for conn.rateLimiter.GetMetrics().TotalRequests == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests; got != before+1 {
		t.Fatalf("protected dispatches=%d, want %d", got, before+1)
	}
}

func assertProtectedZeroWire(t *testing.T, conn *Connection, oldSocket, newSocket *safeBuffer, before uint64) {
	t.Helper()
	buffered := 0
	if conn.writer != nil {
		buffered = conn.writer.Buffered()
	}
	if oldSocket.Len() != 0 || newSocket.Len() != 0 || buffered != 0 {
		t.Fatalf("protected send wrote bytes old=%d new=%d buffered=%d", oldSocket.Len(), newSocket.Len(), buffered)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("protected dispatches=%d, want exactly one", got)
	}
}

func TestProtectedBrokerWireGuardRejectsQueuedAuthorityDrift(t *testing.T) {
	for _, blocker := range []string{"freeze", "storage"} {
		for _, operation := range protectedTransportOperations() {
			t.Run(blocker+"/"+operation.name, func(t *testing.T) {
				conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
				binding, ok := connector.CaptureSession()
				if !ok {
					t.Fatal("capture exact connector session")
				}
				conn.pauseTransport()
				t.Cleanup(conn.resumeTransport)
				before := conn.rateLimiter.GetMetrics().TotalRequests
				var blocked atomic.Bool
				var guardCalls atomic.Int32
				done := make(chan error, 1)
				go func() {
					done <- operation.run(context.Background(), connector, binding, gate, func() error {
						guardCalls.Add(1)
						if blocked.Load() {
							return fmt.Errorf("%s authority engaged", blocker)
						}
						return nil
					})
				}()
				waitForProtectedDispatch(t, conn, before)
				blocked.Store(true)
				conn.resumeTransport()
				select {
				case err := <-done:
					if err == nil || brokerSendMayHaveBeenWritten(err) {
						t.Fatalf("queued %s %s err=%v, want definite pre-wire refusal", blocker, operation.name, err)
					}
				case <-time.After(time.Second):
					t.Fatalf("queued %s %s did not return", blocker, operation.name)
				}
				if guardCalls.Load() != 1 {
					t.Fatalf("wire guard calls=%d, want exactly one", guardCalls.Load())
				}
				assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
			})
		}
	}
}

func TestOutboundSessionRevocationRejectsSendersStartingBeforeTransportLock(t *testing.T) {
	for _, lifecycle := range []string{"disconnect", "reconnect"} {
		t.Run(lifecycle, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture exact connector session")
			}
			beforeState := conn.outboundSessionState.Load()
			conn.transportMu.Lock()
			lifecycleResult := make(chan uint64, 1)
			go func() {
				if lifecycle == "reconnect" {
					lifecycleResult <- conn.beginOutboundSession()
					return
				}
				conn.invalidateOutboundSession(false)
				lifecycleResult <- 0
			}()
			deadline := time.Now().Add(time.Second)
			for conn.outboundSessionState.Load() == beforeState && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			revokedState := conn.outboundSessionState.Load()
			if revokedState == beforeState || revokedState&1 == 0 {
				conn.transportMu.Unlock()
				t.Fatalf("%s did not atomically publish revoked outbound state: before=%d after=%d", lifecycle, beforeState, revokedState)
			}

			before := conn.rateLimiter.GetMetrics().TotalRequests
			done := make(chan error, 1)
			go func() {
				done <- protectedTransportOperations()[0].run(context.Background(), connector, binding, gate, nil)
			}()
			waitForProtectedDispatch(t, conn, before)
			conn.transportMu.Unlock()
			state := <-lifecycleResult
			if lifecycle == "reconnect" {
				if !conn.activateOutboundSession(state) {
					t.Fatal("activate current reconnect generation")
				}
			}
			select {
			case err := <-done:
				if err == nil || brokerSendMayHaveBeenWritten(err) {
					t.Fatalf("post-revocation %s sender err=%v, want definite refusal", lifecycle, err)
				}
			case <-time.After(time.Second):
				t.Fatalf("post-revocation %s sender did not return", lifecycle)
			}
			assertProtectedZeroWire(t, conn, oldSocket, newSocket, before)
		})
	}
}

func (w *partialOrderWriteError) Write(p []byte) (int, error) {
	w.calls.Add(1)
	if len(p) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	return max(1, len(p)/2), io.ErrUnexpectedEOF
}

func newPartialOrderTransportConnection(t *testing.T) (*Connection, *partialOrderWriteError, PaperOrderGate) {
	t.Helper()
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)
	writer := &partialOrderWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)
	gate := PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}
	return conn, writer, gate
}

func assertOneUncertainOrderTransportAttempt(t *testing.T, conn *Connection, writer *partialOrderWriteError, send func() error) {
	t.Helper()
	before := conn.rateLimiter.GetMetrics().TotalRequests
	err := send()
	if err == nil {
		t.Fatal("partial broker-instruction write unexpectedly succeeded")
	}
	if !brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("partial broker-instruction write err=%v, want uncertain-send disposition", err)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("rate-limiter dispatches=%d, want exactly one", got)
	}
	if got := writer.calls.Load(); got != 1 {
		t.Fatalf("underlying write calls=%d, want exactly one partial attempt", got)
	}
}

func TestBrokerInstructionTransportsDoNotRetryPartialWrites(t *testing.T) {
	t.Run("place", func(t *testing.T) {
		conn, writer, gate := newPartialOrderTransportConnection(t)
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.PlacePaperOrder(gate, &IBKROrder{
				Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
				Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account,
			})
		})
	})

	t.Run("what-if", func(t *testing.T) {
		conn, writer, _ := newPartialOrderTransportConnection(t)
		before := conn.rateLimiter.GetMetrics().TotalRequests
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD",
			Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: "DU7654321",
		})
		if err != nil || result.Status != OrderWhatIfStatusUnavailable {
			t.Fatalf("partial WhatIf result=%+v err=%v, want unavailable result", result, err)
		}
		if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
			t.Fatalf("WhatIf rate-limiter dispatches=%d, want exactly one", got)
		}
		if got := writer.calls.Load(); got != 1 {
			t.Fatalf("WhatIf underlying write calls=%d, want exactly one partial attempt", got)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		conn, writer, gate := newPartialOrderTransportConnection(t)
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.CancelPaperOrder(gate, 99)
		})
	})

	t.Run("exercise", func(t *testing.T) {
		conn, writer, _ := newPartialOrderTransportConnection(t)
		req := OptionExerciseRequest{
			TickerID: 101,
			Contract: &Contract{
				ConID: 12345, Symbol: "TEST", SecType: "OPT", Expiry: "20260717", Strike: 100,
				Right: "C", Multiplier: 100, Exchange: "SMART", Currency: "USD", TradingClass: "TEST",
			},
			ExerciseAction: OptionExerciseActionExercise, ExerciseQuantity: 1, Account: "DU7654321",
		}
		if err := validateOptionExerciseRequest(req); err != nil {
			t.Fatalf("exercise fixture: %v", err)
		}
		epoch, err := conn.captureBrokerInstructionEpoch()
		if err != nil {
			t.Fatalf("capture exercise epoch: %v", err)
		}
		assertOneUncertainOrderTransportAttempt(t, conn, writer, func() error {
			return conn.sendExerciseOptionsFrame(req, epoch)
		})
	})
}

func newQueuedInstructionReconnectFixture(t *testing.T) (*Connection, *Connector, *safeBuffer, *safeBuffer, PaperOrderGate) {
	t.Helper()
	cfg := &ConnectionConfig{Host: "127.0.0.1", Port: 7497, ClientID: 41, Account: "DU7654321"}
	conn := NewConnection(cfg)
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(100)

	oldSocket := &safeBuffer{}
	newSocket := &safeBuffer{}
	conn.writer = bufio.NewWriter(oldSocket)

	connector := NewConnector(&ConnectorConfig{BaseConfig: cfg})
	connector.conn.rateLimiter.Stop()
	connector.conn = conn
	connector.running = true
	connector.ready = true
	conn.evidenceBarrier = &connector.evidenceBarrier
	conn.publicationBarrier = &connector.publicationBarrier

	gate := PaperOrderGate{
		Mode: "paper", Account: cfg.Account, Host: cfg.Host, Port: cfg.Port, ClientID: cfg.ClientID,
	}
	return conn, connector, oldSocket, newSocket, gate
}

func readyBrokerEvidenceTestConnector(t *testing.T) *Connector {
	t.Helper()
	connector := NewConnector(&ConnectorConfig{})
	t.Cleanup(func() { connector.conn.rateLimiter.Stop() })
	connector.conn.setStatus(StatusConnected)
	connector.conn.resetOrderIDReadiness()
	connector.mu.Lock()
	connector.ready = true
	connector.mu.Unlock()
	return connector
}

func assertBrokerEvidenceMutationBlocked(t *testing.T, connector *Connector, mutate func(), assertAfter func()) {
	t.Helper()
	binding, ok := connector.CaptureBrokerEvidence()
	if !ok {
		t.Fatal("ready connector did not produce broker evidence binding")
	}
	commitEntered := make(chan struct{})
	releaseCommit := make(chan struct{})
	commitDone := make(chan bool, 1)
	go func() {
		commitDone <- connector.WithStableBrokerEvidence(binding, func() bool {
			close(commitEntered)
			<-releaseCommit
			return true
		})
	}()
	<-commitEntered
	mutationDone := make(chan struct{})
	go func() {
		mutate()
		close(mutationDone)
	}()
	select {
	case <-mutationDone:
		t.Fatal("broker evidence mutation crossed an in-progress stable commit")
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseCommit)
	if committed := <-commitDone; !committed {
		t.Fatal("exact broker evidence binding did not commit")
	}
	select {
	case <-mutationDone:
	case <-time.After(time.Second):
		t.Fatal("broker evidence mutation remained blocked after commit")
	}
	assertAfter()
}

func TestBrokerEvidenceBarrierBlocksLifecyclePortfolioAndSessionMutationAtCommit(t *testing.T) {
	connector := readyBrokerEvidenceTestConnector(t)

	orderGeneration := connector.OrderLifecycleGeneration()
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.dispatchOrderLifecycle(OrderLifecycleEvent{Type: OrderLifecycleEventStatus, OrderID: 101, Status: "Submitted"})
	}, func() {
		if got := connector.OrderLifecycleGeneration(); got != orderGeneration+1 {
			t.Fatalf("order lifecycle generation=%d, want %d", got, orderGeneration+1)
		}
	})

	portfolioGeneration := connector.PortfolioProjectionGeneration()
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.conn.handlePortfolioValue([]string{
			"7", "8", "265598", "AAA", "STK", "", "0", "", "1",
			"NASDAQ", "USD", "AAA", "AAA", "10", "24", "240", "25", "0", "0", "DU123",
		})
	}, func() {
		if got := connector.PortfolioProjectionGeneration(); got != portfolioGeneration+1 {
			t.Fatalf("portfolio projection generation=%d, want %d", got, portfolioGeneration+1)
		}
	})

	published := false
	assertBrokerEvidenceMutationBlocked(t, connector, func() {
		connector.WithBrokerEvidenceMutation(func() { published = true })
	}, func() {
		if !published {
			t.Fatal("external connector publication mutation did not run")
		}
	})

	session, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("ready connector did not produce session binding")
	}
	assertBrokerEvidenceMutationBlocked(t, connector, connector.conn.resetOrderIDReadiness, func() {
		if connector.SessionCurrent(session) {
			t.Fatal("socket epoch reset did not invalidate prior session")
		}
	})

	managedConnector := readyBrokerEvidenceTestConnector(t)
	assertBrokerEvidenceMutationBlocked(t, managedConnector, func() {
		managedConnector.conn.processMessage(managedConnector.conn.encodeMsg(msgManagedAccts, "1", "DU-MANAGED"))
	}, func() {
		if got := managedConnector.AccountID(); got != "DU-MANAGED" {
			t.Fatalf("managed-account mutation = %q, want DU-MANAGED", got)
		}
	})

	summaryConnector := readyBrokerEvidenceTestConnector(t)
	assertBrokerEvidenceMutationBlocked(t, summaryConnector, func() {
		summaryConnector.conn.handleAccountSummary([]string{"63", "2", "7", "DU-SUMMARY", "NetLiquidation", "100000", "USD"})
	}, func() {
		if got := summaryConnector.AccountID(); got != "DU-SUMMARY" {
			t.Fatalf("account-summary seed mutation = %q, want DU-SUMMARY", got)
		}
	})
}

func TestSnapshotOpenOrdersPartialWriteAttemptsOnceAndPoisonsEpoch(t *testing.T) {
	c := NewConnector(&ConnectorConfig{})
	conn := c.conn
	t.Cleanup(conn.rateLimiter.Stop)
	conn.status = StatusConnected
	setServerVersionReady(conn, maxClientVersion)
	conn.observeNextValidOrderID(100)
	writer := &partialOrderWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	before := conn.rateLimiter.GetMetrics().TotalRequests
	if _, err := c.SnapshotOpenOrders(context.Background()); err == nil || !brokerSendMayHaveBeenWritten(err) {
		t.Fatalf("partial reqAllOpenOrders err=%v, want uncertain send", err)
	}
	if got := conn.rateLimiter.GetMetrics().TotalRequests - before; got != 1 {
		t.Fatalf("reqAllOpenOrders dispatches=%d, want exactly one", got)
	}
	if got := writer.calls.Load(); got != 1 {
		t.Fatalf("reqAllOpenOrders underlying writes=%d, want exactly one", got)
	}
	if _, err := c.SnapshotOpenOrders(context.Background()); !errors.Is(err, ErrOpenOrderSnapshotPoisoned) {
		t.Fatalf("same-epoch retry err=%v, want poisoned socket generation", err)
	}
}

func validOptionExerciseRequestForTest() OptionExerciseRequest {
	return OptionExerciseRequest{
		TickerID: 41,
		Contract: &Contract{
			ConID:        12345,
			Symbol:       "TEST",
			SecType:      "OPT",
			Expiry:       "20260717",
			Strike:       100,
			Right:        "C",
			Multiplier:   100,
			Exchange:     "SMART",
			Currency:     "USD",
			TradingClass: "TEST",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 1,
		Account:          "TEST-ACCOUNT",
	}
}

func assertExerciseSendDisposition(t *testing.T, err error, want SendDisposition, cause error) {
	t.Helper()
	if err == nil {
		t.Fatalf("error=nil, want disposition %q", want)
	}
	if got := SendDispositionOf(err); got != want {
		t.Fatalf("SendDispositionOf(%v)=%q, want %q", err, got, want)
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("error=%v, want errors.Is(..., %v)", err, cause)
	}
}

type partialExerciseWriteError struct {
	calls atomic.Int32
}

func (w *partialExerciseWriteError) Write(p []byte) (int, error) {
	w.calls.Add(1)
	if len(p) == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := len(p) / 2
	if n == 0 {
		n = 1
	}
	return n, io.ErrUnexpectedEOF
}

func newExerciseTransportFixture(t *testing.T, namespaceReady bool) (*Connection, *Connector, ConnectorSessionBinding, *partialExerciseWriteError) {
	t.Helper()
	conn := NewConnection(&ConnectionConfig{
		Host:     "127.0.0.1",
		Port:     7497,
		ClientID: 41,
		Account:  "TEST-ACCOUNT",
	})
	t.Cleanup(conn.rateLimiter.Stop)
	conn.serverVersion = 99
	conn.signalHandshakeReady()
	if namespaceReady {
		conn.observeNextValidOrderID(1)
	}
	conn.setStatus(StatusConnected)
	writer := &partialExerciseWriteError{}
	conn.writer = bufio.NewWriterSize(writer, 64*1024)

	connector := &Connector{conn: conn, ready: true}
	binding, ok := connector.CaptureSession()
	if !ok {
		t.Fatal("capture exercise transport session")
	}
	return conn, connector, binding, writer
}

func TestExerciseOptionsTransportDispositionIsPreserved(t *testing.T) {
	if !tradingEnabled {
		t.Skip("trading-build wire contract")
	}
	for _, tc := range []struct {
		name string
		run  func(context.Context, *Connection, *Connector, ConnectorSessionBinding, OptionExerciseRequest) error
	}{
		{name: "connection", run: func(_ context.Context, conn *Connection, _ *Connector, _ ConnectorSessionBinding, req OptionExerciseRequest) error {
			return conn.ExerciseOptions(req)
		}},
		{name: "connector", run: func(ctx context.Context, _ *Connection, connector *Connector, _ ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptions(ctx, req)
		}},
		{name: "session", run: func(ctx context.Context, _ *Connection, connector *Connector, binding ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptionsForSession(ctx, binding, req)
		}},
		{name: "guarded_session", run: func(ctx context.Context, _ *Connection, connector *Connector, binding ConnectorSessionBinding, req OptionExerciseRequest) error {
			req.TickerID = 0
			return connector.ExerciseOptionsForSessionGuarded(ctx, binding, req, func() error { return nil })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, connector, binding, writer := newExerciseTransportFixture(t, true)
			err := tc.run(context.Background(), conn, connector, binding, validOptionExerciseRequestForTest())
			assertExerciseSendDisposition(t, err, SendDispositionMayHaveWritten, io.ErrUnexpectedEOF)
			if got := writer.calls.Load(); got != 1 {
				t.Fatalf("wire writes=%d, want 1", got)
			}
		})
	}
}

func TestValidateOptionExerciseRequest(t *testing.T) {
	t.Parallel()
	valid := OptionExerciseRequest{
		TickerID: 1,
		Contract: &Contract{
			Symbol:   "AAPL",
			SecType:  "OPT",
			Expiry:   "20260619",
			Strike:   100,
			Right:    "C",
			Currency: "USD",
		},
		ExerciseAction:   OptionExerciseActionExercise,
		ExerciseQuantity: 1,
		Account:          "DU123",
	}
	if err := validateOptionExerciseRequest(valid); err != nil {
		t.Fatalf("valid exercise request failed: %v", err)
	}
	invalid := valid
	invalid.Override = 2
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "override") {
		t.Fatalf("invalid override err=%v, want override", err)
	}
	invalid = valid
	invalid.ExerciseAction = 9
	if err := validateOptionExerciseRequest(invalid); err == nil || !strings.Contains(err.Error(), "action") {
		t.Fatalf("invalid action err=%v, want action", err)
	}
}

func TestPreviewOrderWhatIfModernServerSendsProtobufWhatIfAndWaitsForOpenOrder(t *testing.T) {
	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)
	conn.observeNextValidOrderID(77)

	var buf safeBuffer
	conn.writer = bufio.NewWriter(&buf)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	type outcome struct {
		result OrderWhatIfResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := conn.PreviewOrderWhatIf(ctx, &IBKROrder{
			Symbol:    "MSFT",
			SecType:   "STK",
			Exchange:  "SMART",
			Currency:  "USD",
			Action:    "BUY",
			TotalQty:  2,
			OrderType: "LMT",
			LmtPrice:  425.50,
			TIF:       "DAY",
			Account:   "DU123456",
			OrderRef:  "preview-test",
			Transmit:  false,
		})
		done <- outcome{result: result, err: err}
	}()

	waitForWhatIfFrame(t, &buf)
	payload := extractFramePayload(t, &buf)
	if got := binary.BigEndian.Uint32(payload[:4]); got != uint32(protoPlaceOrderMsgID) {
		t.Fatalf("protobuf msgID = %d, want %d", got, protoPlaceOrderMsgID)
	}
	if bytes.Contains(payload, []byte("1.7976931348623157e+308")) {
		t.Fatalf("protobuf placeOrder payload contains ASCII max-float sentinel: %x", payload)
	}
	maxFloat := make([]byte, 8)
	binary.LittleEndian.PutUint64(maxFloat, math.Float64bits(math.MaxFloat64))
	if bytes.Contains(payload, maxFloat) {
		t.Fatalf("protobuf placeOrder payload contains binary max-float sentinel: %x", payload)
	}

	summary, err := parsePlaceOrderProtoSummary(payload[4:])
	if err != nil {
		t.Fatalf("parse protobuf placeOrder summary: %v", err)
	}
	if summary.orderID != 77 || summary.symbol != "MSFT" || summary.secType != "STK" {
		t.Fatalf("protobuf contract summary = %+v, want order 77 MSFT STK", summary)
	}
	if summary.action != "BUY" || summary.quantity != "2" || summary.orderType != "LMT" || summary.lmtPrice != 425.5 || summary.tif != "DAY" {
		t.Fatalf("protobuf order summary = %+v, want BUY 2 LMT 425.5 DAY", summary)
	}
	if summary.account != "DU123456" || summary.orderRef != "preview-test" {
		t.Fatalf("protobuf account/ref summary = %+v, want DU123456 preview-test", summary)
	}
	if !summary.whatIf || !summary.transmit {
		t.Fatalf("protobuf flags whatIf=%v transmit=%v, want true true", summary.whatIf, summary.transmit)
	}

	expected := loadHexFixture(t, "place_order_whatif_v203.hex")
	if !bytes.Equal(payload, expected) {
		t.Fatalf("protobuf placeOrder fixture mismatch\n got: %x\nwant: %x", payload, expected)
	}

	logFields := conn.decodeOutboundMessage(payload)
	if logFields[0] != strconv.Itoa(protoPlaceOrderMsgID) || logFields[1] != "protobuf" {
		t.Fatalf("outbound log fields = %#v, want protobuf summary", logFields)
	}
	if summaryFieldValue(logFields, "orderId=") != "77" || summaryFieldValue(logFields, "symbol=") != "MSFT" {
		t.Fatalf("outbound log fields missing order summary: %#v", logFields)
	}

	conn.processMessage(encodeOpenOrderProtoCallbackForTest(testOpenOrderProtoCallback{
		OrderID:            77,
		PermID:             987654,
		ClientID:           31,
		Symbol:             "MSFT",
		SecType:            "STK",
		Exchange:           "SMART",
		PrimaryExch:        "NASDAQ",
		Currency:           "USD",
		LocalSymbol:        "MSFT",
		TradingClass:       "MSFT",
		Action:             "BUY",
		Quantity:           "2",
		OrderType:          "LMT",
		LimitPrice:         425.5,
		TIF:                "DAY",
		Account:            "DU123456",
		OrderRef:           "preview-test",
		WhatIf:             true,
		Transmit:           true,
		Status:             "Submitted",
		InitMarginBefore:   1000,
		MaintMarginBefore:  500,
		EquityBefore:       10000,
		InitMarginAfter:    1025,
		MaintMarginAfter:   510,
		EquityAfter:        9574.5,
		Commission:         1.25,
		MinCommission:      1.25,
		MaxCommission:      1.25,
		CommissionCurrency: "USD",
		MarginCurrency:     "USD",
	}))

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("PreviewOrderWhatIf err = %v", got.err)
		}
		if got.result.Status != OrderWhatIfStatusAccepted || got.result.BrokerStatus != "Submitted" {
			t.Fatalf("result status = %+v, want accepted Submitted", got.result)
		}
	case <-time.After(time.Second):
		t.Fatal("PreviewOrderWhatIf did not return after matching openOrder")
	}
}

func waitForWhatIfFrame(t *testing.T, buf *safeBuffer) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if buf.Len() > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for outbound whatIf frame")
}

func extractFramePayload(t *testing.T, buf *safeBuffer) []byte {
	t.Helper()
	data := buf.Bytes()
	if len(data) < 4 {
		t.Fatalf("payload too short: %d bytes", len(data))
	}
	msgLen := binary.BigEndian.Uint32(data[:4])
	if uint32(len(data[4:])) < msgLen {
		t.Fatalf("payload length = %d, want at least %d", len(data[4:]), msgLen)
	}
	return data[4 : 4+msgLen]
}

func loadHexFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/wire/" + name)
	if err != nil {
		t.Fatalf("read hex fixture %s: %v", name, err)
	}
	compact := strings.Join(strings.Fields(string(raw)), "")
	decoded, err := hex.DecodeString(compact)
	if err != nil {
		t.Fatalf("decode hex fixture %s: %v", name, err)
	}
	return decoded
}

func TestSessionBoundWhatIfRejectsRolloverBeforeBrokerIDClaim(t *testing.T) {
	for _, tc := range []struct {
		name    string
		orderID int
	}{
		{name: "allocate"},
		{name: "explicit_modify_id", orderID: 700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, connector, oldSocket, newSocket, gate := newQueuedInstructionReconnectFixture(t)
			binding, ok := connector.CaptureSession()
			if !ok {
				t.Fatal("capture session A")
			}
			entered := make(chan struct{})
			release := make(chan struct{})
			connector.whatIfBeforeBrokerIDClaim = func() {
				close(entered)
				<-release
			}
			type outcome struct {
				result OrderWhatIfResult
				err    error
			}
			done := make(chan outcome, 1)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			go func() {
				contract := &Contract{ConID: 1, Symbol: "TEST", SecType: "STK", Exchange: "SMART", Currency: "USD"}
				order := &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "LMT", LmtPrice: 1, TIF: "DAY", Account: gate.Account}
				var result OrderWhatIfResult
				var err error
				if tc.orderID > 0 {
					result, err = connector.PreviewOrderWhatIfWithOrderIDForSession(ctx, binding, contract, order, tc.orderID)
				} else {
					result, err = connector.PreviewOrderWhatIfForSession(ctx, binding, contract, order)
				}
				done <- outcome{result: result, err: err}
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("WhatIf did not reach broker ID claim seam")
			}
			conn.resetOrderIDReadiness()
			conn.writer = bufio.NewWriter(newSocket)
			conn.observeNextValidOrderIDAtEpoch(500, conn.BrokerSessionEpoch())
			close(release)
			select {
			case got := <-done:
				if got.err == nil && got.result.Status != OrderWhatIfStatusUnavailable {
					t.Fatalf("rolled WhatIf result=%+v err=%v, want refusal", got.result, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("rolled WhatIf did not return")
			}
			if oldSocket.Len() != 0 || newSocket.Len() != 0 {
				t.Fatalf("rolled WhatIf wrote bytes old=%d new=%d", oldSocket.Len(), newSocket.Len())
			}
		})
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *safeBuffer) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Len()
}

func (s *safeBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.buf.Bytes()...)
}

func setServerVersionReady(c *Connection, version int) {
	c.serverVersion = version
	c.signalHandshakeReady()
	c.observeNextValidOrderID(1)
}

func TestOrderMethodsDisabledByDefault(t *testing.T) {
	if tradingEnabled {
		t.Skip("default disabled guard is not active in trading-tag builds")
	}

	conn := NewConnection(DefaultConfig())
	defer conn.rateLimiter.Stop()
	conn.status = StatusConnected
	setServerVersionReady(conn, minServerVerProtoBufPlaceOrder)

	if err := conn.PlaceOrder(&IBKROrder{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD", Action: "BUY", TotalQty: 1, OrderType: "MKT", TIF: "DAY"}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connection.PlaceOrder err = %v, want ErrTradingDisabled", err)
	}
	if err := conn.CancelOrder(1); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connection.CancelOrder err = %v, want ErrTradingDisabled", err)
	}

	c := NewConnector(&ConnectorConfig{BaseConfig: DefaultConfig()})
	if err := c.SubmitOrder(&Contract{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"}, &RawOrder{Action: "BUY", TotalQty: 1, OrderType: "MKT", TIF: "DAY"}); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connector.SubmitOrder err = %v, want ErrTradingDisabled", err)
	}
	if err := c.CancelOrder(1); !errors.Is(err, ErrTradingDisabled) {
		t.Fatalf("Connector.CancelOrder err = %v, want ErrTradingDisabled", err)
	}
}
