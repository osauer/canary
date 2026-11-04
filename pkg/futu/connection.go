package futu

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"
)

// writeTimeout bounds one frame's write, so a stalled OpenD cannot hold a
// request past its caller: the heartbeat drops the session behind it.
const writeTimeout = 10 * time.Second

// envelope is what every OpenD reply carries around its answer.
type envelope interface {
	proto.Message
	GetRetType() int32
	GetRetMsg() string
	GetErrCode() int32
}

// marshalDeterministic encodes a message so that one request always has one
// byte form, which the recorded frame fixtures rely on.
func marshalDeterministic(m proto.Message) ([]byte, error) {
	return proto.MarshalOptions{Deterministic: true}.Marshal(m)
}

// connection owns one socket: the serial numbers, the framing, the matching
// of replies to requests by serial and the handing of everything else to the
// push handler. It knows nothing of the session above it.
type connection struct {
	nc     net.Conn
	pacer  *Pacer
	onPush func(Frame)

	serial   atomic.Uint32
	writeMu  sync.Mutex
	received atomic.Int64

	mu      sync.Mutex
	pending map[uint32]*pendingCall
	err     error
	done    chan struct{}
}

type pendingCall struct {
	protoID uint32
	reply   chan []byte
}

// newConnection wraps a dialled socket; serve runs its read loop. The pacer
// belongs to the connector, because OpenD's windows outlive a socket.
func newConnection(nc net.Conn, pacer *Pacer, onPush func(Frame)) *connection {
	c := &connection{
		nc:      nc,
		pacer:   pacer,
		onPush:  onPush,
		pending: make(map[uint32]*pendingCall),
		done:    make(chan struct{}),
	}
	c.touch()
	return c
}

// touch records that a frame arrived. It reads the monotonic wall clock, not
// the connector's injected one: the heartbeat is real elapsed time, whatever
// clock the wire's timestamps come from.
func (c *connection) touch() { c.received.Store(time.Now().UnixNano()) }

// sinceReceived is how long ago the last frame arrived.
func (c *connection) sinceReceived() time.Duration {
	return time.Duration(time.Now().UnixNano() - c.received.Load())
}

// serve reads frames until the socket fails. A malformed frame ends the
// session: it is not trustworthy after one.
func (c *connection) serve() {
	for {
		frame, err := ReadFrame(c.nc)
		if err != nil {
			c.close(err)
			return
		}
		c.touch()
		c.mu.Lock()
		call, ok := c.pending[frame.Serial]
		if ok && call.protoID == frame.ProtoID {
			delete(c.pending, frame.Serial)
			c.mu.Unlock()
			call.reply <- frame.Body
			continue
		}
		c.mu.Unlock()
		if c.onPush != nil {
			c.onPush(frame)
		}
	}
}

// close ends the connection with its reason; the first reason stands.
func (c *connection) close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	c.err = err
	close(c.done)
	_ = c.nc.Close()
}

// failure is the reason the connection ended, nil while it is open.
func (c *connection) failure() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// write sends one frame with the next serial and returns that serial. The
// serial is taken under the write lock, so frames leave in serial order.
func (c *connection) write(protoID uint32, body []byte, register func(serial uint32) error) (uint32, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	serial := c.serial.Add(1)
	if register != nil {
		if err := register(serial); err != nil {
			return 0, err
		}
	}
	if err := c.nc.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return serial, err
	}
	return serial, WriteFrame(c.nc, Frame{ProtoID: protoID, Serial: serial, Body: body})
}

// send writes a request without waiting for its answer; keep-alive uses it,
// since any received frame keeps the heartbeat.
func (c *connection) send(protoID uint32, req proto.Message) error {
	body, err := marshalDeterministic(req)
	if err != nil {
		return fmt.Errorf("opend protocol %d: encode: %w", protoID, err)
	}
	if _, err := c.write(protoID, body, nil); err != nil {
		c.close(err)
		return err
	}
	return nil
}

// request paces, sends and waits for the reply, decoding it into resp. A
// reply OpenD answered with a code other than success is an [*Error]; a
// session-level code or an undecodable body also ends the connection.
func (c *connection) request(ctx context.Context, protoID uint32, req proto.Message, resp envelope) error {
	if c.pacer != nil {
		if err := c.pacer.Admit(protoID); err != nil {
			return err
		}
	}
	return c.requestUnpaced(ctx, protoID, req, resp)
}

// requestUnpaced is a request the pacing table does not count: a read OpenD
// answers from its own cache, or a later page of a paged read.
func (c *connection) requestUnpaced(ctx context.Context, protoID uint32, req proto.Message, resp envelope) error {
	body, err := marshalDeterministic(req)
	if err != nil {
		return fmt.Errorf("opend protocol %d: encode: %w", protoID, err)
	}
	call := &pendingCall{protoID: protoID, reply: make(chan []byte, 1)}
	serial, err := c.write(protoID, body, func(serial uint32) error {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.err != nil {
			return c.err
		}
		c.pending[serial] = call
		return nil
	})
	if err != nil {
		c.forget(serial)
		if c.failure() == nil {
			c.close(err)
		}
		return err
	}
	select {
	case raw := <-call.reply:
		if err := proto.Unmarshal(raw, resp); err != nil {
			decodeErr := fmt.Errorf("%w: protocol %d reply body: %v", ErrFrame, protoID, err)
			c.close(decodeErr)
			return decodeErr
		}
		if resp.GetRetType() == RetTypeSucceed {
			return nil
		}
		replyErr := &Error{ProtoID: protoID, RetType: resp.GetRetType(), ErrCode: resp.GetErrCode(), Message: resp.GetRetMsg()}
		if replyErr.Session() {
			c.close(replyErr)
		}
		return replyErr
	case <-c.done:
		c.forget(serial)
		return c.failure()
	case <-ctx.Done():
		c.forget(serial)
		return ctx.Err()
	}
}

func (c *connection) forget(serial uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.pending, serial)
}
