package ibkr

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// heartbeatPair binds a Connection to one end of a loopback socket, marked
// connected as startAPI leaves it, and returns the other end as the Gateway.
func heartbeatPair(t *testing.T, interval time.Duration) (*Connection, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	gateway := <-accepted
	conn := NewConnection(nil)
	conn.config.AutoReconnect = false
	conn.config.HeartbeatInterval = interval
	conn.conn = client
	conn.reader = bufio.NewReader(client)
	conn.writer = bufio.NewWriter(client)
	conn.status = StatusConnected
	conn.lastHeartbeatNano.Store(time.Now().UnixNano())
	t.Cleanup(func() {
		_ = conn.Disconnect()
		_ = gateway.Close()
	})
	return conn, gateway
}

// currentTimeFrame is the Gateway's answer to a heartbeat request.
func currentTimeFrame() []byte {
	body := []byte("49\x001\x00" + strconv.FormatInt(time.Now().Unix(), 10) + "\x00")
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))), body...)
}

// The heartbeat counts answers, not sends. A Gateway that accepts every
// request and answers none loses its session after two silent intervals, so
// its owner redials it; one that answers keeps its session.
func TestHeartbeatCountsAnswersNotSends(t *testing.T) {
	const interval = 100 * time.Millisecond
	t.Run("silent Gateway", func(t *testing.T) {
		buf := captureConnectorLogs(t)
		conn, gateway := heartbeatPair(t, interval)
		requests := make(chan struct{}, 64)
		go func() {
			r := bufio.NewReader(gateway)
			for {
				size := make([]byte, 4)
				if _, err := io.ReadFull(r, size); err != nil {
					return
				}
				if _, err := io.CopyN(io.Discard, r, int64(binary.BigEndian.Uint32(size))); err != nil {
					return
				}
				select {
				case requests <- struct{}{}:
				default:
				}
			}
		}()
		lost := make(chan time.Time, 1)
		conn.SetOnDisconnect(func(error) {
			select {
			case lost <- time.Now():
			default:
			}
		})
		start := time.Now()
		conn.wg.Add(2)
		go conn.heartbeatMonitor()
		go conn.readMessages()
		select {
		case at := <-lost:
			if waited := at.Sub(start); waited < 2*interval || waited > 3*interval+time.Second {
				t.Fatalf("session dropped after %s, want two silent %s intervals", waited, interval)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a Gateway that answers nothing kept its session")
		}
		select {
		case <-requests:
		default:
			t.Fatal("no heartbeat request reached the Gateway: the test proves nothing about sends")
		}
		if lines := logLines(buf, "Heartbeat timeout: the Gateway answered nothing"); len(lines) != 1 {
			t.Fatalf("heartbeat timeout lines = %q, want one", lines)
		}
		if conn.IsConnected() {
			t.Fatal("dropped session still reads connected")
		}
	})
	t.Run("answering Gateway", func(t *testing.T) {
		conn, gateway := heartbeatPair(t, interval)
		go func() { _, _ = io.Copy(io.Discard, gateway) }()
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			tick := time.NewTicker(interval / 3)
			defer tick.Stop()
			for {
				select {
				case <-stop:
					return
				case <-tick.C:
					if _, err := gateway.Write(currentTimeFrame()); err != nil {
						return
					}
				}
			}
		}()
		lost := make(chan struct{}, 1)
		conn.SetOnDisconnect(func(error) {
			select {
			case lost <- struct{}{}:
			default:
			}
		})
		conn.wg.Add(2)
		go conn.heartbeatMonitor()
		go conn.readMessages()
		select {
		case <-lost:
			t.Fatal("an answering Gateway lost its session")
		case <-time.After(8 * interval):
		}
	})
}

func TestAnswerPathCountsAnswersTimeoutsAndInFlight(t *testing.T) {
	c := &Connector{historicalReqs: map[int]*historicalRequest{}, contractDetailsReqs: map[int]*contractDetailsRequest{}}
	now := time.Date(2026, 9, 28, 13, 0, 0, 0, time.UTC)
	c.historicalReqs[1] = &historicalRequest{sentAt: now.Add(-40 * time.Second)}
	c.contractDetailsReqs[2] = &contractDetailsRequest{sentAt: now.Add(-5 * time.Second)}
	c.answers.noteTimeout(now.Add(-20 * time.Minute))
	c.answers.noteTimeout(now.Add(-2 * time.Minute))
	c.noteContractDetailsOutcome(ErrContractDetailsTimeout, now.Add(-time.Minute))
	c.noteContractDetailsOutcome(errors.New("coded refusal"), now.Add(-30*time.Second))
	got := c.AnswerPath(now)
	if got.InFlight != 2 || !got.OldestSentAt.Equal(now.Add(-40*time.Second)) || got.Timeouts != 2 || !got.LastAnswerAt.Equal(now.Add(-30*time.Second)) {
		t.Fatalf("answer path = %+v", got)
	}
	for range answerTimeoutsKept + 10 {
		c.answers.noteTimeout(now)
	}
	if got := c.AnswerPath(now); got.Timeouts != answerTimeoutsKept {
		t.Fatalf("timeouts kept = %d, want the bound %d", got.Timeouts, answerTimeoutsKept)
	}
}
