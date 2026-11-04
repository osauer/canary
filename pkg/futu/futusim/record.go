package futusim

import (
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/osauer/canary/v2/pkg/futu"
)

// Direction says which way a recorded frame travelled, as the simulator sees
// it.
type Direction string

const (
	// DirectionIn is a frame the client sent.
	DirectionIn Direction = ">"
	// DirectionOut is a frame the simulator sent.
	DirectionOut Direction = "<"
)

// Step is one recorded frame, its bytes exact.
type Step struct {
	Direction Direction
	Raw       []byte
}

// Recorder collects a scripted session's frames as a [Server] sees them.
type Recorder struct {
	mu    sync.Mutex
	steps []Step
}

// Add records one frame.
func (r *Recorder) Add(direction Direction, frame futu.Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.steps = append(r.steps, Step{Direction: direction, Raw: futu.EncodeFrame(frame)})
}

// Steps is everything recorded so far.
func (r *Recorder) Steps() []Step {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Step(nil), r.steps...)
}

// fixtureHeader opens every frame fixture, so a reader knows what the file
// proves and what it does not.
const fixtureHeader = `# Frames of a scripted simulator session, byte for byte: ">" the client sent,
# "<" the simulator sent. A regression fixture for the client against the
# simulator, both built from the same definitions; not conformance evidence
# against OpenD. Recorded with: go test ./pkg/futu -run Fixtures -record
`

// WriteScript writes steps as a frame fixture.
func WriteScript(path string, steps []Step) error {
	var b strings.Builder
	b.WriteString(fixtureHeader)
	for _, step := range steps {
		fmt.Fprintf(&b, "%s %s\n", step.Direction, hex.EncodeToString(step.Raw))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// ReadScript reads a frame fixture.
func ReadScript(path string) ([]Step, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var steps []Step
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		direction, encoded, ok := strings.Cut(line, " ")
		if !ok || (direction != string(DirectionIn) && direction != string(DirectionOut)) {
			return nil, fmt.Errorf("futusim: %s: malformed line %q", path, line)
		}
		frame, err := hex.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("futusim: %s: %w", path, err)
		}
		steps = append(steps, Step{Direction: Direction(direction), Raw: frame})
	}
	if len(steps) == 0 {
		return nil, errors.New("futusim: " + path + ": no frames")
	}
	return steps, nil
}
