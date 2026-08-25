//go:build linux

package main

import (
	"bufio"
	"os"

	"golang.org/x/sys/unix"
)

func readSetupSecret(tty *os.File, reader *bufio.Reader) ([]byte, error) {
	fd := int(tty.Fd())
	state, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return nil, err
	}
	hidden := *state
	hidden.Lflag &^= unix.ECHO
	if err := unix.IoctlSetTermios(fd, unix.TCSETS, &hidden); err != nil {
		return nil, err
	}
	defer func() { _ = unix.IoctlSetTermios(fd, unix.TCSETS, state) }()
	return readSetupSecretLine(reader)
}
