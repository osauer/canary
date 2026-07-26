package tui

import (
	"time"

	"github.com/osauer/canary/v2/internal/app/live"
)

type eventKind int

const (
	eventKey eventKind = iota
	eventResize
	eventCommandDone
	eventLiveSnapshot
	eventLiveError
	eventTick
)

type uiEvent struct {
	kind   eventKind
	key    keyEvent
	size   Size
	result commandResult
	snap   live.Snapshot
	err    error
	at     time.Time
}
