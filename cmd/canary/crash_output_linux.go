//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// Dup3 rather than Dup2: linux/arm64 and the newer ports have no dup2 syscall.
func redirectStderr(f *os.File) error {
	return unix.Dup3(int(f.Fd()), 2, 0)
}
