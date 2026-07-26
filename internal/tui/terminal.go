package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

// Size is the terminal viewport in character cells.
type Size struct {
	Rows int
	Cols int
}

type terminal struct {
	in      *os.File
	out     *os.File
	restore func() error
}

// IsInteractive reports whether stdin and stdout both point at terminal
// devices. Zero-arg `canary` only enters the TUI in this case.
func IsInteractive(in, out *os.File) bool {
	return isTerminalFile(in) && isTerminalFile(out)
}

func isTerminalFile(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func openTerminal(in, out *os.File) (*terminal, error) {
	if !IsInteractive(in, out) {
		return nil, fmt.Errorf("stdin/stdout are not interactive terminals")
	}
	restore, err := enterRawMode(int(in.Fd()))
	if err != nil {
		return nil, err
	}
	t := &terminal{in: in, out: out, restore: restore}
	_, _ = io.WriteString(out, "\x1b[?1049h\x1b[?25l\x1b[2J\x1b[H")
	return t, nil
}

func (t *terminal) close() {
	if t == nil {
		return
	}
	_, _ = io.WriteString(t.out, "\x1b[0m\x1b[?25h\x1b[?1049l")
	if t.restore != nil {
		_ = t.restore()
	}
}

func (t *terminal) write(s string) {
	_, _ = io.WriteString(t.out, s)
}

func (t *terminal) size() Size {
	return terminalSize(t.out)
}

func terminalSize(out *os.File) Size {
	if out != nil {
		ws, err := unix.IoctlGetWinsize(int(out.Fd()), unix.TIOCGWINSZ)
		if err == nil && ws != nil && ws.Col > 0 && ws.Row > 0 {
			return Size{Rows: int(ws.Row), Cols: int(ws.Col)}
		}
	}
	return Size{Rows: 24, Cols: 80}
}

func watchResize(ctx context.Context, out *os.File, events chan<- uiEvent) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			events <- uiEvent{kind: eventResize, size: terminalSize(out)}
		}
	}
}

func enterRawMode(fd int) (func() error, error) {
	old, err := unix.IoctlGetTermios(fd, termiosGetRequest)
	if err != nil {
		return nil, fmt.Errorf("read terminal mode: %w", err)
	}
	raw := *old
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	if err := unix.IoctlSetTermios(fd, termiosSetRequest, &raw); err != nil {
		return nil, fmt.Errorf("set raw terminal mode: %w", err)
	}
	return func() error {
		return unix.IoctlSetTermios(fd, termiosSetRequest, old)
	}, nil
}
