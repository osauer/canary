package futu

import (
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
)

// HeaderSize is the length of every OpenD packet header in bytes. The
// layout, little-endian, from the public client's MESSAGE_HEAD_FMT
// ("<1s1sI2B2I20s8s"):
//
//	offset  size  field
//	0       2     header flag, the bytes 'F' 'T'
//	2       4     protocol id
//	6       1     body format: 0 protobuf, 1 JSON
//	7       1     protocol version, 0
//	8       4     serial number, the client's, echoed in the reply
//	12      4     body length
//	16      20    SHA-1 of the plaintext body
//	36      8     reserved, zero
const HeaderSize = 44

// MaxBodySize caps the body length a header may declare before the reader
// allocates for it. OpenD's largest documented replies (a 400-instrument
// snapshot, a 1000-candle history page) are far below it; a header above
// the cap is a malformed frame, not a large reply.
const MaxBodySize = 16 * 1024 * 1024

// BodyFormat is the header's body format byte.
type BodyFormat uint8

const (
	// BodyProtobuf is the only format this client speaks.
	BodyProtobuf BodyFormat = 0
	// BodyJSON is OpenD's alternative; a frame carrying it is refused.
	BodyJSON BodyFormat = 1
)

// headerFlag is the two-byte prefix of every packet.
var headerFlag = [2]byte{'F', 'T'}

// Frame is one packet: the protocol it belongs to, the client's serial
// number and the body bytes. Encoding fills the header's format, version,
// length and SHA-1 from these.
type Frame struct {
	ProtoID uint32
	Serial  uint32
	Body    []byte
}

// EncodeFrame renders the frame as header plus body, ready for the socket.
func EncodeFrame(f Frame) []byte {
	out := make([]byte, HeaderSize+len(f.Body))
	copy(out, headerFlag[:])
	binary.LittleEndian.PutUint32(out[2:], f.ProtoID)
	out[6] = byte(BodyProtobuf)
	out[7] = 0
	binary.LittleEndian.PutUint32(out[8:], f.Serial)
	binary.LittleEndian.PutUint32(out[12:], uint32(len(f.Body)))
	sum := sha1.Sum(f.Body)
	copy(out[16:], sum[:])
	copy(out[HeaderSize:], f.Body)
	return out
}

// WriteFrame encodes the frame and writes it whole.
func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Body) > MaxBodySize {
		return fmt.Errorf("%w: body of %d bytes exceeds %d", ErrFrame, len(f.Body), MaxBodySize)
	}
	_, err := w.Write(EncodeFrame(f))
	return err
}

// ReadFrame reads one packet. A short read of the header or the body is
// io.ErrUnexpectedEOF (a clean close before any byte is io.EOF); a header
// without the FT flag, a JSON body format, a declared length above
// [MaxBodySize] or a body whose SHA-1 differs from the header's is an
// [ErrFrame], after which the session is not trustworthy.
func ReadFrame(r io.Reader) (Frame, error) {
	var header [HeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	if header[0] != headerFlag[0] || header[1] != headerFlag[1] {
		return Frame{}, fmt.Errorf("%w: header flag %q, want \"FT\"", ErrFrame, header[:2])
	}
	if format := BodyFormat(header[6]); format != BodyProtobuf {
		return Frame{}, fmt.Errorf("%w: body format %d, this client speaks protobuf (0) only", ErrFrame, format)
	}
	f := Frame{
		ProtoID: binary.LittleEndian.Uint32(header[2:]),
		Serial:  binary.LittleEndian.Uint32(header[8:]),
	}
	length := binary.LittleEndian.Uint32(header[12:])
	if length > MaxBodySize {
		return Frame{}, fmt.Errorf("%w: protocol %d declares a body of %d bytes, above %d", ErrFrame, f.ProtoID, length, MaxBodySize)
	}
	f.Body = make([]byte, length)
	if _, err := io.ReadFull(r, f.Body); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, fmt.Errorf("%w: protocol %d declares %d body bytes: %v", ErrFrame, f.ProtoID, length, err)
	}
	if sum := sha1.Sum(f.Body); sum != [20]byte(header[16:36]) {
		return Frame{}, fmt.Errorf("%w: protocol %d body SHA-1 does not match its header", ErrFrame, f.ProtoID)
	}
	return f, nil
}
