package futu

import (
	"time"

	"google.golang.org/protobuf/proto"
)

// FixtureConfig is the client identity every frame fixture is recorded with.
var FixtureConfig = Config{ClientID: "canary-fixture", ClientVersion: 333}

// FixtureRequest renders the request of one scripted exchange exactly as the
// client's own builders do, with the serial the client would give it.
func FixtureRequest(protoID, serial uint32, now time.Time) []byte {
	var message proto.Message
	switch protoID {
	case ProtoInitConnect:
		message = initConnectRequest(FixtureConfig)
	case ProtoKeepAlive:
		message = keepAliveRequest(now)
	case ProtoGetGlobalState:
		message = globalStateRequest()
	default:
		panic("no fixture request for this protocol")
	}
	body, err := marshalDeterministic(message)
	if err != nil {
		panic(err)
	}
	return EncodeFrame(Frame{ProtoID: protoID, Serial: serial, Body: body})
}
