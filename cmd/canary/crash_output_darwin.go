//go:build darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func redirectStderr(f *os.File) error {
	return unix.Dup2(int(f.Fd()), 2)
}
