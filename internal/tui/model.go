package tui

import (
	"time"

	"github.com/osauer/canary/v2/internal/app/live"
	"github.com/osauer/canary/v2/internal/cli"
)

const maxOutputBlocks = 200

type outputBlock struct {
	Command  string
	Stdout   string
	Stderr   string
	ExitCode int
	Started  time.Time
	Finished time.Time
}

type activeCommand struct {
	line   string
	cancel func()
}

type confirmation struct {
	line    string
	message string
}

type model struct {
	size        Size
	editor      *lineEditor
	catalog     []cli.CommandSpec
	outputs     []outputBlock
	scroll      int
	snapshot    live.Snapshot
	liveErr     string
	active      *activeCommand
	confirm     *confirmation
	message     string
	tickerIndex int
	tickerSig   string
	lastTick    time.Time
	quitting    bool
}

func newModel(catalog []cli.CommandSpec, size Size) *model {
	return &model{
		size:    size,
		editor:  newLineEditor(),
		catalog: catalog,
		outputs: []outputBlock{{
			Command:  "welcome",
			Stdout:   canaryWelcomeText(),
			ExitCode: 0,
			Started:  time.Now(),
			Finished: time.Now(),
		}},
	}
}

func (m *model) addOutput(block outputBlock) {
	m.outputs = append(m.outputs, block)
	if len(m.outputs) > maxOutputBlocks {
		m.outputs = append([]outputBlock(nil), m.outputs[len(m.outputs)-maxOutputBlocks:]...)
	}
	m.scroll = 0
}

func (m *model) setMessage(msg string) {
	m.message = msg
}
