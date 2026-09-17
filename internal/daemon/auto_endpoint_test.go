package daemon

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestAutoTradingRequiresConfirmedAccountNotPort(t *testing.T) {
	for _, tc := range []struct {
		name, mode, account, connected, blocker string
		port                                    int
	}{
		{"live Gateway", "live", "U1234567", "U1234567", "", 4001},
		{"live TWS", "live", "U1234567", "U1234567", "", 7496},
		{"live custom paper-numbered port", "live", "U1234567", "U1234567", "", 4002},
		{"paper custom live-numbered port", "paper", "DU1234567", "DU1234567", "", 4001},
		{"paper TWS", "paper", "DU1234567", "DU1234567", "", 7497},
		{"managed account membership", "live", "U1234567", "U7654321,U1234567,", "", 7496},
		{"wrong account", "live", "U1234567", "U7654321", "gateway_account_mismatch", 7496},
		{"unknown account", "live", "U1234567", "", "gateway_account_unconfirmed", 7496},
		{"aggregate account", "live", "U1234567", "All", "gateway_account_unconfirmed", 7496},
		{"malformed membership", "live", "U1234567", "All,U1234567", "gateway_account_mismatch", 7496},
		{"missing account pin", "live", "", "U1234567", "gateway_account_unpinned", 7496},
		{"paper in live mode", "live", "DU1234567", "DU1234567", "live_endpoint_unconfirmed", 4001},
		{"live in paper mode", "paper", "U1234567", "U1234567", "paper_endpoint_unconfirmed", 4002},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newOrderPreviewTestServer(t, config.Trading{Mode: tc.mode})
			s.cfg.Gateway.Port = nil
			s.cfg.Gateway.Account = tc.account
			s.endpoint.Port = tc.port
			s.endpoint.Account = tc.account
			s.endpoint.PortOrigin = discover.OriginDiscovered
			s.gatewayAccountForTrading = func() string { return tc.connected }
			status := s.tradingStatus(s.endpoint)
			if tc.blocker == "" {
				if status.Blocked || !status.CanPreview {
					t.Fatalf("expected eligible Auto gate, got %+v", status.Blockers)
				}
				if mode := accountModeForStatus(tc.port, tc.account); mode != tc.mode {
					t.Fatalf("mode=%s want=%s", mode, tc.mode)
				}
			} else {
				found := false
				for _, b := range status.Blockers {
					found = found || b.Code == tc.blocker
				}
				if !found || status.CanPreview {
					t.Fatalf("missing blocker %s: %+v", tc.blocker, status.Blockers)
				}
			}
		})
	}
}

func TestAutoTradingStillRequiresClientPin(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: "paper"})
	s.cfg.Gateway.Port = nil
	s.cfg.Gateway.ClientID = nil
	status := s.tradingStatus(s.endpoint)
	for _, b := range status.Blockers {
		if b.Code == "gateway_client_id_unpinned" && !status.CanPreview {
			return
		}
	}
	t.Fatalf("client pin was bypassed: %+v", status.Blockers)
}

func TestAutoPortSwitchRejectsPreviousPreview(t *testing.T) {
	s := newOrderPreviewTestServer(t, config.Trading{Mode: "paper"})
	s.cfg.Gateway.Port = nil
	s.endpoint.PortOrigin = discover.OriginDiscovered
	token := mintPreviewTokenForConfirmTest(t, s, rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted})
	if _, err := s.verifyPreviewTokenForPlace(token); err != nil {
		t.Fatalf("initial preview: %v", err)
	}
	s.endpoint.Port = 7497
	if _, err := s.verifyPreviewTokenForPlace(token); err == nil || !strings.Contains(err.Error(), "current gate") {
		t.Fatalf("previous endpoint preview accepted: %v", err)
	}
}

func TestAutoBackendRediscoveryWaitsAndPreservesPins(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name         string
		pinned, down bool
		age          time.Duration
		want         bool
	}{
		{"brief interruption", false, true, 29 * time.Second, false},
		{"sustained interruption", false, true, 30 * time.Second, true},
		{"pinned endpoint", true, true, time.Hour, false},
		{"restored", false, false, time.Hour, false},
		{"future timestamp", false, true, -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoBackendRediscoveryDue(tc.pinned, ibkrlib.BackendLinkReport{Down: tc.down, ChangedAt: now.Add(-tc.age)}, now); got != tc.want {
				t.Fatalf("rediscovery=%v want=%v", got, tc.want)
			}
		})
	}
	if autoBackendRediscoveryDue(false, ibkrlib.BackendLinkReport{Down: true}, now) {
		t.Fatal("unknown onset triggered failover")
	}
}

func TestAutoBackendFailoverTriesTWSBeforeStillListeningGateway(t *testing.T) {
	s := newTestServer(t)
	ep, ok := preferAlternateEndpoint(discover.Endpoint{Host: "127.0.0.1", Port: 4001, Alternates: []int{7496}, PortOrigin: discover.OriginDiscovered}, 4001)
	if !ok {
		t.Fatal("TWS alternate not selected")
	}
	var attempted []int
	s.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		attempted = append(attempted, ep.Port)
		return &fakeAttempter{startErr: errors.New("synthetic handshake failure")}
	}
	s.connectWithFailover(context.Background(), ep)
	if !reflect.DeepEqual(attempted, []int{7496, 4001}) {
		t.Fatalf("attempted %v", attempted)
	}
	for _, ep := range []discover.Endpoint{
		{Port: 4001, PortOrigin: discover.OriginDiscovered},
		{Port: 4001, PortOrigin: discover.OriginPinned, Alternates: []int{7496}},
	} {
		if _, ok := preferAlternateEndpoint(ep, 4001); ok {
			t.Fatal("no eligible alternate must retain current socket")
		}
	}
}

func TestAutoDiscoverySkipsWrongAccountAndBackendDownCandidates(t *testing.T) {
	for _, tc := range []struct {
		name, account string
		down          bool
	}{
		{"wrong account", "DU1234567", false},
		{"unconfirmed aggregate", "All", false},
		{"backend down", "U1234567", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			s.cfg.Gateway.Port = nil
			s.cfg.Gateway.Account = "U1234567"
			wrong := &fakeAttempter{connectOk: true, account: tc.account, backend: ibkrlib.BackendLinkReport{Down: tc.down}}
			correct := &fakeAttempter{connectOk: true, account: "U1234567"}
			var attempted []int
			s.attempterFactory = func(ep discover.Endpoint) connectAttempter {
				attempted = append(attempted, ep.Port)
				if ep.Port == 4002 {
					return wrong
				}
				return correct
			}
			s.connectWithFailover(t.Context(), discover.Endpoint{Host: "127.0.0.1", Port: 4002, Alternates: []int{7496}, Account: "U1234567", PortOrigin: discover.OriginDiscovered})
			if !reflect.DeepEqual(attempted, []int{4002, 7496}) || s.endpoint.Port != 7496 || wrong.stopCalls.Load() != 1 {
				t.Fatalf("wrong session selected: %v stopped=%d", attempted, wrong.stopCalls.Load())
			}
		})
	}
}

func TestAutoBackendLossDrivesRediscoveryThroughGatewayRead(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	sendLoss := make(chan struct{})
	done := make(chan struct{})
	socketErrors := make(chan error, 1)
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			socketErrors <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
		frame := func(body []byte) error {
			packet := make([]byte, 4)
			binary.BigEndian.PutUint32(packet, uint32(len(body)))
			_, err := conn.Write(append(packet, body...))
			return err
		}
		readFrame := func() error {
			header := make([]byte, 4)
			if _, err := io.ReadFull(conn, header); err != nil {
				return err
			}
			n := binary.BigEndian.Uint32(header)
			if n > 65536 {
				return errors.New("oversized synthetic request")
			}
			_, err := io.CopyN(io.Discard, conn, int64(n))
			return err
		}
		prefix := make([]byte, 4)
		if _, err := io.ReadFull(conn, prefix); err != nil {
			socketErrors <- err
			return
		}
		if string(prefix) != "API\x00" {
			socketErrors <- errors.New("missing API handshake")
			return
		}
		if err := readFrame(); err != nil {
			socketErrors <- err
			return
		}
		if err := frame([]byte("131\x0020260917 12:00:00\x00")); err != nil {
			socketErrors <- err
			return
		}
		if err := readFrame(); err != nil {
			socketErrors <- err
			return
		}
		message := func(id uint32, body []byte) error {
			b := make([]byte, 4)
			binary.BigEndian.PutUint32(b, id)
			return frame(append(b, body...))
		}
		if err := message(15, []byte("1\x00DU1234567\x00")); err != nil {
			socketErrors <- err
			return
		}
		if err := message(9, []byte("1\x001000\x00")); err != nil {
			socketErrors <- err
			return
		}
		select {
		case <-sendLoss:
		case <-t.Context().Done():
			return
		}
		text := []byte("synthetic upstream loss")
		payload := append([]byte{0x18, 0xcc, 0x08, 0x22, byte(len(text))}, text...)
		if err := message(204, payload); err != nil {
			socketErrors <- err
			return
		}
		_, _ = io.Copy(io.Discard, conn)
	}()
	s := newTestServer(t)
	s.cfg.Gateway.Port = nil
	s.endpoint = discover.Endpoint{Host: "127.0.0.1", Port: port, ClientID: 91, PortOrigin: discover.OriginDiscovered}
	c := s.newConnector(s.endpoint)
	t.Cleanup(func() { _ = c.Stop() })
	if err := c.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.IsReady() {
		t.Fatalf("synthetic connector not ready: %s", c.LastError())
	}
	close(sendLoss)
	deadline := time.Now().Add(5 * time.Second)
	for !c.BackendLink().Down && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !c.BackendLink().Down {
		t.Fatal("synthetic loss was not observed")
	}
	s.connector = c
	s.serverCtx = t.Context()
	s.now = func() time.Time { return c.BackendLink().ChangedAt.Add(31 * time.Second) }
	oldProbe, oldPorts := discover.Probe, discover.StandardPorts
	discover.StandardPorts = []int{port, 7496}
	discover.Probe = func(context.Context, string, int, time.Duration) error { return nil }
	defer func() { discover.Probe, discover.StandardPorts = oldProbe, oldPorts }()
	var mu sync.Mutex
	var attempted []int
	s.attempterFactory = func(ep discover.Endpoint) connectAttempter {
		mu.Lock()
		attempted = append(attempted, ep.Port)
		mu.Unlock()
		return &fakeAttempter{startErr: errors.New("synthetic alternative unavailable")}
	}
	if got := s.gatewayConnector(); got != nil {
		t.Fatal("backend-down connector remained usable")
	}
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		running := s.connectInFlight
		s.mu.Unlock()
		if !running {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	s.mu.Lock()
	running := s.connectInFlight
	s.mu.Unlock()
	if running {
		t.Fatal("rediscovery did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(attempted, []int{7496, port}) {
		t.Fatalf("actual recovery attempted %v", attempted)
	}
	select {
	case err := <-socketErrors:
		t.Fatal(err)
	default:
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retired socket not closed")
	}
}
