//go:build !darwin && !linux

package main

import "os"

// Without a dup2 the runtime's fatal output stays on the inherited stderr;
// Go-level writes through os.Stderr still reach the crash log.
func redirectStderr(*os.File) error { return nil }
