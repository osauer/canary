package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/osauer/canary/v2/pkg/futu"
)

func TestRunRefusesBadFlags(t *testing.T) {
	var stderr bytes.Buffer
	if got := run([]string{"-clock", "sometimes"}, io.Discard, &stderr); got != 2 || !strings.Contains(stderr.String(), "-clock") {
		t.Fatalf("exit %d: %s", got, stderr.String())
	}
	if got := run([]string{"-book", filepath.Join(t.TempDir(), "missing.json")}, io.Discard, io.Discard); got != 1 {
		t.Fatalf("a missing book exited %d", got)
	}
	if got := run([]string{"-nope"}, io.Discard, io.Discard); got != 2 {
		t.Fatalf("an unknown flag exited %d", got)
	}
	for _, listen := range []string{"127.0.0.1:7496", "127.0.0.1:4001", "0.0.0.0:0", "192.0.2.1:9999", "nonsense"} {
		var refused bytes.Buffer
		if got := run([]string{"-listen", listen}, io.Discard, &refused); got != 2 {
			t.Errorf("-listen %s exited %d: %s", listen, got, refused.String())
		}
	}
}

// The process serves the real wire on its printed address, follows the wall clock,
// honours its tier, and writes the frames it saw when it stops.
func TestRunServesAndRecords(t *testing.T) {
	out, stdout := io.Pipe()
	record := filepath.Join(t.TempDir(), "session.hex")
	done := make(chan int, 1)
	go func() {
		done <- run([]string{"-clock", "now", "-tier", "300", "-record", record}, stdout, io.Discard)
	}()
	addr, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	addr = strings.TrimSpace(addr)
	go func() { _, _ = io.Copy(io.Discard, out) }()

	conn := futu.NewConnector(futu.Config{Addr: addr, Account: 1000001, ClientVersion: 333, RedialDelay: 20 * time.Millisecond})
	conn.Start(context.Background())
	deadline := time.Now().Add(5 * time.Second)
	for !conn.IsReady() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !conn.IsReady() {
		t.Fatalf("no session: %+v", conn.Health())
	}
	load, err := conn.RefreshLoad(context.Background())
	if err != nil || load.SubscriptionQuota != 300 {
		t.Fatalf("tier %+v %v", load, err)
	}
	conn.Stop()

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != 0 {
			t.Fatalf("exit %d", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no stop on the signal")
	}
	raw, err := os.ReadFile(record)
	if err != nil || !strings.Contains(string(raw), "> ") {
		t.Fatalf("recording %q %v", raw, err)
	}
	if nc, err := net.DialTimeout("tcp", addr, 100*time.Millisecond); err == nil {
		_ = nc.Close()
		t.Fatal("the port stayed open after the stop")
	}
}
