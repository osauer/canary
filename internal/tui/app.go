package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/app/daemonclient"
	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/cli"
	"github.com/osauer/canary/v2/internal/dial"
	"github.com/osauer/canary/v2/internal/productidentity"
)

// Options carries the process dependencies wired by the Canary command entry
// point. The TUI owns terminal UX; command behavior and live data sources stay
// in cli/live.
type Options struct {
	Stdin      *os.File
	Stdout     *os.File
	Stderr     *os.File
	Version    string
	SocketPath string
}

// Run starts the interactive terminal cockpit and blocks until the user exits
// or ctx is cancelled.
func Run(ctx context.Context, opts Options) int {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.SocketPath == "" {
		opts.SocketPath = dial.DefaultSocketPath()
	}
	term, err := openTerminal(opts.Stdin, opts.Stdout)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "%s: TUI: %v\n", productidentity.Executable, err)
		return 1
	}
	defer term.close()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	events := make(chan uiEvent, 64)
	go readInput(ctx, opts.Stdin, events)
	go watchResize(ctx, opts.Stdout, events)

	client := daemonclient.Real{SocketPath: opts.SocketPath, AutoSpawn: true}
	service := live.New(client, 5*time.Second, time.Minute)
	liveCh, release := service.Subscribe()
	defer release()
	go service.Start(ctx)
	go forwardLiveEvents(ctx, service, liveCh, events)

	catalog := cli.Catalog()
	model := newModel(catalog, term.size())
	runner := commandRunner{version: opts.Version, socketPath: opts.SocketPath, color: cli.ShouldColor(opts.Stdout)}
	frame := render(model)
	term.write(frame)

	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case at := <-tick.C:
				events <- uiEvent{kind: eventTick, at: at}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return 0
		case ev := <-events:
			handleEvent(ctx, cancel, model, runner, events, ev)
			next := render(model)
			if next != frame {
				term.write(next)
				frame = next
			}
			if model.quitting {
				return 0
			}
		}
	}
}

func forwardLiveEvents(ctx context.Context, service *live.Service, liveCh <-chan live.Event, events chan<- uiEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-liveCh:
			if !ok {
				return
			}
			select {
			case events <- uiEvent{kind: eventLiveSnapshot, snap: service.Snapshot()}:
			case <-ctx.Done():
				return
			}
		}
	}
}

func handleEvent(ctx context.Context, cancel context.CancelFunc, m *model, runner commandRunner, events chan<- uiEvent, ev uiEvent) {
	switch ev.kind {
	case eventResize:
		m.size = ev.size
	case eventLiveSnapshot:
		if sig := tickerSignature(ev.snap); sig != m.tickerSig {
			m.tickerIndex = 0
			m.tickerSig = sig
		}
		m.snapshot = ev.snap
		m.liveErr = ""
	case eventLiveError:
		if ev.err != nil {
			m.liveErr = ev.err.Error()
		}
	case eventTick:
		m.tickerIndex++
		m.lastTick = ev.at
	case eventCommandDone:
		m.addOutput(ev.result.block)
		m.active = nil
		m.setMessage("")
	case eventKey:
		handleKey(ctx, cancel, m, runner, events, ev.key)
	}
}

func handleKey(ctx context.Context, appCancel context.CancelFunc, m *model, runner commandRunner, events chan<- uiEvent, key keyEvent) {
	switch key.kind {
	case keyCtrlD, keyCtrlQ:
		if m.active == nil {
			m.quitting = true
			appCancel()
			return
		}
	case keyCtrlC:
		if m.active != nil && m.active.cancel != nil {
			m.active.cancel()
			m.setMessage("command cancelled")
			return
		}
		if m.confirm == nil && m.editor.line() == "" {
			m.quitting = true
			appCancel()
			return
		}
		m.editor.clear()
		m.confirm = nil
		m.setMessage("")
	case keyCtrlL:
		m.outputs = nil
		m.scroll = 0
		m.setMessage("output cleared")
	case keyEsc:
		m.confirm = nil
		m.editor.suggestions = nil
		m.setMessage("cancelled")
	case keyPageUp:
		m.scroll += max(1, m.size.Rows/2)
	case keyPageDown:
		m.scroll = max(0, m.scroll-max(1, m.size.Rows/2))
	case keyUp:
		m.editor.historyUp()
	case keyDown:
		m.editor.historyDown()
	case keyLeft:
		m.editor.left()
	case keyRight:
		m.editor.right()
	case keyHome:
		m.editor.home()
	case keyEnd:
		m.editor.end()
	case keyBackspace:
		m.editor.backspace()
	case keyDelete:
		m.editor.delete()
	case keyTab:
		m.editor.complete(m.catalog, dynamicSymbols(m.snapshot))
	case keyCtrlR:
		m.setMessage("history search is not implemented yet; use arrow keys")
	case keyRune:
		if key.r >= 32 {
			m.editor.insert(key.r)
		}
	case keyEnter:
		acceptLine(ctx, m, runner, events)
	}
}

func acceptLine(ctx context.Context, m *model, runner commandRunner, events chan<- uiEvent) {
	line := strings.TrimSpace(m.editor.line())
	if m.confirm != nil {
		answer := strings.ToLower(line)
		pending := m.confirm.line
		m.confirm = nil
		m.editor.clear()
		if answer == "yes" || answer == "y" {
			startCommand(ctx, m, runner, events, pending)
			return
		}
		m.setMessage("confirmation cancelled")
		return
	}
	if line == "" {
		return
	}
	if handleMeta(m, line) {
		m.editor.acceptHistory(line)
		m.editor.clear()
		return
	}
	if m.active != nil {
		m.setMessage("command already running; Ctrl-C cancels it")
		return
	}
	conf, err := confirmationFor(line, m.catalog)
	if err != nil {
		m.addOutput(outputBlock{Command: line, Stderr: err.Error(), ExitCode: 2, Started: time.Now(), Finished: time.Now()})
		m.editor.clear()
		return
	}
	if conf != nil {
		m.confirm = conf
		m.editor.clear()
		return
	}
	m.editor.acceptHistory(line)
	m.editor.clear()
	startCommand(ctx, m, runner, events, line)
}

func startCommand(parent context.Context, m *model, runner commandRunner, events chan<- uiEvent, line string) {
	started := time.Now()
	cmdCtx, cancel := context.WithCancel(parent)
	m.active = &activeCommand{line: line, cancel: cancel}
	m.setMessage("")
	go func() {
		result := runner.run(cmdCtx, line, started, m.catalog)
		select {
		case events <- uiEvent{kind: eventCommandDone, result: result}:
		case <-parent.Done():
		}
	}()
}
