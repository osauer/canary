package flexstmt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseContextCancelsWhileWaitingForParser(t *testing.T) {
	parseSlot <- struct{}{}
	defer func() { <-parseSlot }()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := ParseContext(ctx, []byte("<FlexQueryResponse/>")); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled parser waiter stuck")
	}
}

// Cancel after a deterministic number of reader checks, while XML decoding
// is in progress, rather than relying on CPU timing.
type parsingCancellation struct {
	context.Context
	reads int
}

func (c *parsingCancellation) Err() error {
	c.reads++
	if c.reads > 3 {
		return context.Canceled
	}
	return nil
}
func TestParseContextCancelsInsideXML(t *testing.T) {
	ctx := &parsingCancellation{Context: context.Background()}
	data := []byte(`<FlexQueryResponse><FlexStatements><FlexStatement accountId="DU1234567" fromDate="20260901" toDate="20260902" whenGenerated="20260903">` + strings.Repeat(`<Unused value="abcdefghij"/>`, 100000) + `</FlexStatement></FlexStatements></FlexQueryResponse>`)
	if _, err := ParseContext(ctx, data); !errors.Is(err, context.Canceled) {
		t.Fatalf("parse did not cancel: %v", err)
	}
}
