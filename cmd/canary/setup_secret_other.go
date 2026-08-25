//go:build !darwin && !linux

package main

import (
	"bufio"
	"errors"
	"os"
)

func readSetupSecret(_ *os.File, _ *bufio.Reader) ([]byte, error) {
	return nil, errors.New("hidden terminal input is unavailable on this platform")
}
