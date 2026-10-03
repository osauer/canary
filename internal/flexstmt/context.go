package flexstmt

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
)

// A process parses one report at a time. Waiters can cancel without allocating
// a second full XML object graph; health/status never enter this gate.
var parseSlot = make(chan struct{}, 1)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
func unmarshalContext(ctx context.Context, data []byte, out any) error {
	return xml.NewDecoder(contextReader{ctx, bytes.NewReader(data)}).Decode(out)
}
