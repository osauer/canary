package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/pkg/futu/futusim"
)

const (
	dualIBKRAccount  = "DU1234567"
	dualOpenDAccount = "1000001"
)

// dualStack is one daemon's whole world: its scratch directory, environment and
// socket.
type dualStack struct {
	env    []string
	socket string
	dir    string
}

func newDualStack(t *testing.T, config string) dualStack {
	t.Helper()
	env, socket, _ := lifecycleEnv(t)
	dir := filepath.Dir(socket)
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	return dualStack{env: env, socket: socket, dir: dir}
}

// status runs the CLI's status under the stack's environment. The CLI starts the
// daemon if none serves its socket, so this is also the autospawn path.
func (s dualStack) status(t *testing.T) rpc.HealthResult {
	t.Helper()
	out, _ := runCLI(t, s.env, 60*time.Second, "status", "--json")
	var health rpc.HealthResult
	if err := json.Unmarshal([]byte(out), &health); err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	return health
}

// policies lists the stack's policy files with their bytes, to prove them untouched.
func (s dualStack) policies(t *testing.T) map[string]string {
	t.Helper()
	root := filepath.Join(s.dir, "config", "ibkr", "policies")
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[strings.TrimPrefix(path, root)] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mentions reports any regular file under root holding the text, skipping binary
// databases, which hold no paths of another stack anyway.
func mentions(t *testing.T, root, text string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil || bytes.IndexByte(raw, 0) >= 0 {
			return nil
		}
		if bytes.Contains(raw, []byte(text)) {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func healthOver(t *testing.T, socket string) rpc.HealthResult {
	t.Helper()
	conn, err := dial.Connect(socket)
	if err != nil {
		t.Fatalf("dial %s: %v", socket, err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var health rpc.HealthResult
	if err := conn.Call(ctx, rpc.MethodStatusHealth, rpc.HealthParams{}, &health); err != nil {
		t.Fatalf("health over %s: %v", socket, err)
	}
	return health
}

func waitForOpenDReady(t *testing.T, s dualStack) rpc.HealthResult {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		health := healthOver(t, s.socket)
		if health.Connected {
			return health
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready OpenD session: %+v", health.OpenD)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// The owner's ask of 2026-11-04 16:03 CET and the multi-broker review: an
// IBKR-shaped daemon and an OpenD daemon run at the same time with separate XDG
// roots and sockets. Neither sees the other's state, socket or policy files, each
// names its own broker and account, a client of the two sockets sees distinct
// fields, and a down OpenD socket under the OpenD stack's environment autospawns an
// OpenD daemon and never an IBKR one.
func TestLifecycle_TwoBrokersSideBySide(t *testing.T) {
	sim := futusim.Start(t, futusim.Options{})
	host, port, err := net.SplitHostPort(sim.Addr())
	if err != nil {
		t.Fatal(err)
	}
	ibkr := newDualStack(t, fmt.Sprintf("[gateway]\nhost = \"127.0.0.1\"\nport = 1\nclient_id = 199\ntls = false\naccount = %q\n", dualIBKRAccount))
	opend := newDualStack(t, fmt.Sprintf("[broker]\nkind = \"opend\"\n[opend]\nhost = %q\nport = %s\naccount = %q\n", host, port, dualOpenDAccount))
	if ibkr.dir == opend.dir || ibkr.socket == opend.socket {
		t.Fatal("the two stacks share a directory")
	}

	ibkrStatus := ibkr.status(t)
	ibkrPolicies := ibkr.policies(t)
	opend.status(t)
	opendHealth := waitForOpenDReady(t, opend)
	opendPolicies := opend.policies(t)

	// Each names its own broker kind and account.
	if ibkrStatus.Broker != "" || ibkrStatus.Account != dualIBKRAccount && ibkrStatus.ConnectedAccount != dualIBKRAccount && !strings.Contains(ibkrStatus.Trading.Account, dualIBKRAccount) {
		t.Fatalf("IBKR stack: broker %q account %q connected %q trading %q", ibkrStatus.Broker, ibkrStatus.Account, ibkrStatus.ConnectedAccount, ibkrStatus.Trading.Account)
	}
	if ibkrStatus.OpenD != nil {
		t.Fatal("the IBKR stack carries an OpenD block")
	}
	if opendHealth.Broker != "opend" || opendHealth.ConnectedAccount != dualOpenDAccount || opendHealth.AccountMode != rpc.AccountModePaper || opendHealth.OpenD == nil {
		t.Fatalf("OpenD stack: %+v", opendHealth)
	}

	// A client of the two sockets receives distinct fields.
	overIBKR, overOpenD := healthOver(t, ibkr.socket), healthOver(t, opend.socket)
	if overIBKR.Broker == overOpenD.Broker || overIBKR.ConnectedAccount == overOpenD.ConnectedAccount || overIBKR.DaemonStarted.Equal(overOpenD.DaemonStarted) && overIBKR.GatewayPort == overOpenD.GatewayPort {
		t.Fatalf("the two sockets answered alike: %q/%q %q/%q", overIBKR.Broker, overOpenD.Broker, overIBKR.ConnectedAccount, overOpenD.ConnectedAccount)
	}
	if overIBKR.GatewayPort != 1 || overOpenD.GatewayHost != host {
		t.Fatalf("endpoints %s:%d and %s:%d", overIBKR.GatewayHost, overIBKR.GatewayPort, overOpenD.GatewayHost, overOpenD.GatewayPort)
	}

	// Neither stack's files name the other's directory, and each has its own.
	if hits := mentions(t, ibkr.dir, opend.dir); len(hits) != 0 {
		t.Errorf("the IBKR stack names the OpenD stack: %v", hits)
	}
	if hits := mentions(t, opend.dir, ibkr.dir); len(hits) != 0 {
		t.Errorf("the OpenD stack names the IBKR stack: %v", hits)
	}
	if len(ibkrPolicies) == 0 || len(opendPolicies) == 0 {
		t.Fatalf("policy files: %d and %d", len(ibkrPolicies), len(opendPolicies))
	}
	for _, s := range []dualStack{ibkr, opend} {
		if _, err := os.Stat(filepath.Join(s.dir, "home", ".config", "ibkr", "policies")); !os.IsNotExist(err) {
			t.Errorf("a stack wrote policies under its home directory: %v", err)
		}
		if _, err := os.Stat(filepath.Join(s.dir, "state")); err != nil {
			t.Errorf("a stack has no state of its own: %v", err)
		}
	}

	// A read of the OpenD stack leaves the IBKR stack's policy files as they were.
	if _, code := runCLI(t, opend.env, 60*time.Second, "positions", "--json"); code != 0 {
		t.Errorf("positions under OpenD exited %d", code)
	}
	after := ibkr.policies(t)
	if len(after) != len(ibkrPolicies) {
		t.Fatalf("IBKR policy files changed: %d then %d", len(ibkrPolicies), len(after))
	}
	for name, body := range ibkrPolicies {
		if after[name] != body {
			t.Errorf("IBKR policy %s changed while the OpenD stack ran", name)
		}
	}

	// The autospawn hazard: the OpenD daemon stops and its socket goes. A status under
	// the OpenD stack's environment starts a daemon from that environment's config,
	// so it comes back as OpenD and never as IBKR. The IBKR daemon is untouched.
	ibkrPID := daemonPID(ibkr.socket)
	firstOpenD := daemonPID(opend.socket)
	stopLifecycleDaemon(t, opend.socket)
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(opend.socket); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the OpenD socket stayed after its daemon stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
	opend.status(t)
	respawned := waitForOpenDReady(t, opend)
	if respawned.Broker != "opend" || respawned.ConnectedAccount != dualOpenDAccount || respawned.GatewayPort == 1 {
		t.Fatalf("the respawned daemon is not the OpenD stack's: %q %q port %d", respawned.Broker, respawned.ConnectedAccount, respawned.GatewayPort)
	}
	if pid := daemonPID(opend.socket); pid == 0 || pid == firstOpenD {
		t.Fatalf("no new daemon: was %d, is %d", firstOpenD, pid)
	}
	if pid := daemonPID(ibkr.socket); pid != ibkrPID {
		t.Fatalf("the IBKR daemon changed: was %d, is %d", ibkrPID, pid)
	}
}
