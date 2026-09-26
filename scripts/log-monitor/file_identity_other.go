//go:build !darwin && !linux

package main

import "os"

// The checkpoint hashes remain active where a stable file ID is unavailable.
func fileIdentity(os.FileInfo) string { return "" }
