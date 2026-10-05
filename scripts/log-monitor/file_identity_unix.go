//go:build darwin || linux

package main

import (
	"os"
	"strconv"
	"syscall"
)

// fileIdentity is the inode alone. macOS renumbers APFS volumes at boot, so a
// device number in the identity turned every reboot into a whole-file replay
// (2026-10-04 and 2026-10-05); the content digests still guard inode reuse.
func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return strconv.FormatUint(stat.Ino, 10)
	}
	return ""
}
