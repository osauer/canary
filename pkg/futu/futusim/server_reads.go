package futusim

import (
	"google.golang.org/protobuf/proto"

	"github.com/osauer/canary/v2/pkg/futu"
)

// answerRead answers the reads of the next layer. Until a protocol has its
// handler, the simulator answers nothing, as OpenD answers nothing to a
// protocol it does not know.
func (s *Server) answerRead(_ futu.Frame) proto.Message {
	return nil
}
