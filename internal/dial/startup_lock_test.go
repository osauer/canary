package dial

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestStartupReservationScopeAndCancellation(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "private", "daemon.sock")
	ctx, release, err := WithStartupLock(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	info, err := os.Stat(filepath.Dir(socket))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("daemon directory must remain private: info=%v err=%v", info, err)
	}
	_, borrowedRelease, err := WithStartupLock(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	borrowedRelease()
	waiting, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	// The daemon instance lock is shared by all socket names in one directory.
	if _, _, err := WithStartupLock(waiting, socket+"-other"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contending startup must wait, got %v", err)
	}
	_, otherRelease, err := WithStartupLock(t.Context(), filepath.Join(t.TempDir(), "daemon.sock"))
	if err != nil {
		t.Fatal(err)
	}
	otherRelease()
}

func TestStartupReservationInheritedBySelectedChild(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	ctx, release, err := WithStartupLock(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for _, mode := range []string{"inherited", "independent", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			childCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestStartupReservationChild$")
			cmd.Env = append(os.Environ(), "CANARY_STARTUP_LOCK_TEST="+mode, "CANARY_SOCKET="+socket)
			if mode == "inherited" {
				cmd.ExtraFiles = []*os.File{ctx.Value(startupLockKey{}).(*startupReservation).file}
			} else if mode == "unrelated" {
				file, err := os.CreateTemp(t.TempDir(), "unrelated")
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				cmd.ExtraFiles = []*os.File{file}
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child: %v\n%s", err, out)
			}
			// Child release must not unlock its parent's shared file description.
			wait, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			if _, _, err := WithStartupLock(wait, socket); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("child released parent's reservation: %v", err)
			}
		})
	}
}

func TestStartupReservationSurvivesOwnerCrash(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestStartupReservationChild$")
	cmd.Env = append(os.Environ(), "CANARY_STARTUP_LOCK_TEST=owner", "CANARY_SOCKET="+socket)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "reserved\n" {
		t.Fatalf("child reservation: %q %v", line, err)
	}
	wait, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := WithStartupLock(wait, socket); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owner did not exclude contender: %v", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	_, release, err := WithStartupLock(t.Context(), socket)
	if err != nil {
		t.Fatalf("crashed owner stranded reservation: %v", err)
	}
	release()
}

func TestStartupReservationRechecksStopIntent(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "daemon.sock")
	_, release, err := WithStartupLock(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result := make(chan error, 1)
	go func() {
		_, err := AutospawnAndConnectContextFromExecutable(t.Context(), socket, "/must-not-execute")
		result <- err
	}()
	if err := InhibitAutostart(socket); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-result; !errors.Is(err, ErrStopped) {
		t.Fatalf("stop intent lost: %v", err)
	}
}

// This subprocess exercises real inherited descriptors and flock ownership.
func TestStartupReservationChild(t *testing.T) {
	mode := os.Getenv("CANARY_STARTUP_LOCK_TEST")
	if mode == "" {
		return
	}
	if mode == "owner" {
		_, release, err := WithStartupLock(t.Context(), DefaultSocketPath())
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		fmt.Println("reserved")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	release, err := LockDaemonStartup(ctx, DefaultSocketPath())
	if mode == "inherited" {
		if err != nil {
			t.Fatalf("selected child blocked: %v", err)
		}
		release()
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unselected child bypassed reservation: %v", err)
	}
	if mode == "unrelated" {
		var stat syscall.Stat_t
		if err := syscall.Fstat(3, &stat); err != nil {
			t.Fatalf("unrelated descriptor was closed: %v", err)
		}
	}
}
