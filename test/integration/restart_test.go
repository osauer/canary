package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/update"
)

// Exercise the actual executable and instance-lock boundary: old clients
// starting daemon mode directly must queue behind the chosen replacement,
// just as current readers do. All state and the unreachable Gateway are local
// to lifecycleEnv; this test never uses the operator's daemon or app.
func TestLifecycle_RestartReservationOwnsReplacement(t *testing.T) {
	env, socket, _ := lifecycleEnv(t)
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch name {
		case "CANARY_SOCKET", "CANARY_LOG", "CANARY_CONFIG", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "HOME":
			t.Setenv(name, value)
		}
	}
	if out, code := runCLI(t, env, 15*time.Second, "status", "--json"); code != 0 {
		t.Fatalf("initial start exit=%d: %s", code, out)
	}
	oldPID := daemonPID(socket)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	ctx, release, err := dial.WithStartupLock(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := update.StopDaemon(oldPID, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	type contender struct {
		cmd *exec.Cmd
		out bytes.Buffer
	}
	var contenders []*contender
	for i := range 6 {
		args := []string{"daemon"} // Models a reader built before startup reservations.
		if i%2 == 0 {
			args = []string{"status", "--json"}
		}
		c := &contender{cmd: exec.CommandContext(ctx, sharedCLI, args...)}
		c.cmd.Env = env
		c.cmd.Stdout, c.cmd.Stderr = &c.out, &c.out
		if err := c.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.cmd.Process.Kill(); _ = c.cmd.Wait() })
		contenders = append(contenders, c)
	}
	conn, err := dial.AutospawnAndConnectContextFromExecutableWithTimeout(ctx, socket, sharedCLI, 10*time.Second)
	if err != nil {
		t.Fatalf("chosen executable lost handover: %v", err)
	}
	_ = conn.Close()
	chosenPID := daemonPID(socket)
	if chosenPID == 0 || chosenPID == oldPID {
		t.Fatalf("replacement PID=%d, old=%d", chosenPID, oldPID)
	}
	release()
	for _, c := range contenders {
		if err := c.cmd.Wait(); err != nil {
			t.Fatalf("contender %v: %v\n%s", c.cmd.Args, err, c.out.String())
		}
	}
	if got := daemonPID(socket); got != chosenPID {
		t.Fatalf("contender replaced chosen daemon: got %d want %d", got, chosenPID)
	}

	// Keeping strict identity checking is essential: an existing daemon must
	// still be refused by the exact-start API, even if it looks healthy.
	if conn, err := dial.AutospawnAndConnectContextFromExecutableWithTimeout(t.Context(), socket, sharedCLI, time.Second); err == nil || !strings.Contains(err.Error(), "refusing exact-executable") {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("existing owner accepted as exact replacement: %v", err)
	}

	// Finally exercise the public command, including its reservation context
	// reaching the selected subprocess and the default scoped app exclusion.
	out, code := runCLI(t, env, 20*time.Second, "restart", "--json")
	if code != 0 {
		t.Fatalf("restart exit=%d: %s", code, out)
	}
	var result struct {
		OldPID  int  `json:"old_pid"`
		NewPID  int  `json:"new_pid"`
		Started bool `json:"started"`
		App     struct {
			Reason string `json:"reason"`
		} `json:"app"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("restart JSON: %v\n%s", err, out)
	}
	if !result.Started || result.OldPID != chosenPID || result.NewPID == chosenPID || result.NewPID != daemonPID(socket) || result.App.Reason != "socket_overridden" {
		t.Fatalf("restart did not own exact replacement: %+v", result)
	}
}
