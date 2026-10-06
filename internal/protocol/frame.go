// Package protocol defines what client and node exchange: the data plane (a fixed
// HELLO, then small typed frames), the invite string, the control-plane JSON and the
// TLS pin. Every decoder rejects input it does not fully understand.
package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is the only data-plane version this build speaks.
const Version = 1

var magic = [4]byte{'C', 'V', 'D', '2'}

// MagicByte is the first byte of a data-plane connection, which a node uses to
// tell it from a TLS ClientHello (0x16) on the shared port.
const MagicByte = 'C'

// HelloSize is the fixed length of the HELLO that opens a data-plane connection.
const HelloSize = 56

const (
	helloType      = 0x01
	helloMACLabel  = "CVD2 hello"
	frameHeaderLen = 3
)

var (
	ErrMalformed   = errors.New("protocol: malformed message")
	ErrUnsupported = errors.New("protocol: unsupported version")
)

// Hello opens a data-plane connection. MAC authenticates the version, session and
// nonce under the session secret, so a ticket cannot be forged or downgraded.
type Hello struct {
	Version   uint8
	SessionID [16]byte
	Nonce     [16]byte
	MAC       [16]byte
}

// NewHello returns a HELLO for this build's version, signed with secret.
func NewHello(secret []byte, sessionID, nonce [16]byte) Hello {
	h := Hello{Version: Version, SessionID: sessionID, Nonce: nonce}
	h.MAC = helloMAC(secret, h.Version, sessionID, nonce)
	return h
}

func helloMAC(secret []byte, version uint8, sessionID, nonce [16]byte) [16]byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(helloMACLabel))
	m.Write([]byte{version})
	m.Write(sessionID[:])
	m.Write(nonce[:])
	var out [16]byte
	copy(out[:], m.Sum(nil))
	return out
}

// Verify reports whether the HELLO was signed with secret, in constant time.
func (h Hello) Verify(secret []byte) bool {
	want := helloMAC(secret, h.Version, h.SessionID, h.Nonce)
	return hmac.Equal(h.MAC[:], want[:])
}

func (h Hello) MarshalBinary() []byte {
	b := make([]byte, HelloSize)
	copy(b[0:4], magic[:])
	b[4] = h.Version
	b[5] = helloType
	copy(b[8:24], h.SessionID[:])
	copy(b[24:40], h.Nonce[:])
	copy(b[40:56], h.MAC[:])
	return b
}

// ReadHello reads and checks one HELLO. A well-formed HELLO of another version
// returns ErrUnsupported together with the parsed value, so the node can answer
// with a version rejection instead of dropping the connection.
func ReadHello(r io.Reader) (Hello, error) {
	var b [HelloSize]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return Hello{}, err
	}
	if [4]byte(b[0:4]) != magic || b[5] != helloType || b[6] != 0 || b[7] != 0 {
		return Hello{}, ErrMalformed
	}
	h := Hello{Version: b[4]}
	copy(h.SessionID[:], b[8:24])
	copy(h.Nonce[:], b[24:40])
	copy(h.MAC[:], b[40:56])
	if h.Version != Version {
		return h, ErrUnsupported
	}
	return h, nil
}

// FrameType identifies a data-plane frame after the HELLO.
type FrameType uint8

const (
	TypeAccept   FrameType = 0x02 // node → client: IdleTimeoutMs
	TypeReject   FrameType = 0x03 // node → client: Reason, RetryAfterS
	TypePing     FrameType = 0x10 // client → node: Seq
	TypePong     FrameType = 0x11 // node → client: Seq echoed
	TypeProbe    FrameType = 0x20 // node → client: Seq (node-initiated idle probe)
	TypeProbeAck FrameType = 0x21 // client → node: Seq
	TypeClose    FrameType = 0x30 // either way: Reason, RetryAfterS
)

// payloadLen is the exact payload length of each frame type; any other type or
// length is malformed.
var payloadLen = map[FrameType]int{
	TypeAccept: 4, TypeReject: 3, TypePing: 8, TypePong: 8,
	TypeProbe: 8, TypeProbeAck: 8, TypeClose: 3,
}

// MaxFrameSize bounds a frame on the wire, header included.
const MaxFrameSize = frameHeaderLen + 8

// Reason says why a node rejected or closed a connection.
type Reason uint8

const (
	ReasonBusy         Reason = 1 // node-wide connection limit
	ReasonQuota        Reason = 2 // session or invite quota
	ReasonAuth         Reason = 3 // bad ticket, unknown or expired session
	ReasonVersion      Reason = 4
	ReasonIdleTimeout  Reason = 5 // the agreed idle limit passed
	ReasonSessionEnded Reason = 6
	ReasonShuttingDown Reason = 7
)

var reasonNames = map[Reason]string{
	ReasonBusy: "busy", ReasonQuota: "quota", ReasonAuth: "auth", ReasonVersion: "version",
	ReasonIdleTimeout: "idle_timeout", ReasonSessionEnded: "session_ended", ReasonShuttingDown: "shutting_down",
}

// String names the reason; values this build does not know are "other", so a
// newer node can add reasons without breaking older clients.
func (r Reason) String() string {
	if n, ok := reasonNames[r]; ok {
		return n
	}
	return "other"
}

// Frame is one data-plane frame. Only the fields of its Type are carried.
type Frame struct {
	Type          FrameType
	Seq           uint64
	IdleTimeoutMs uint32
	Reason        Reason
	RetryAfterS   uint16
}

// Append encodes f onto dst. It panics on a type with no defined payload, which
// is a programming error, not input.
func (f Frame) Append(dst []byte) []byte {
	n, ok := payloadLen[f.Type]
	if !ok {
		panic(fmt.Sprintf("protocol: no frame type %#x", f.Type))
	}
	dst = append(dst, byte(f.Type), 0, byte(n))
	switch f.Type {
	case TypeAccept:
		dst = binary.BigEndian.AppendUint32(dst, f.IdleTimeoutMs)
	case TypeReject, TypeClose:
		dst = append(dst, byte(f.Reason))
		dst = binary.BigEndian.AppendUint16(dst, f.RetryAfterS)
	default:
		dst = binary.BigEndian.AppendUint64(dst, f.Seq)
	}
	return dst
}

// WriteFrame writes one frame in a single Write call.
func WriteFrame(w io.Writer, f Frame) error {
	var buf [MaxFrameSize]byte
	_, err := w.Write(f.Append(buf[:0]))
	return err
}

// ReadFrame reads exactly one frame. An unknown type or a length that does not
// match the type is ErrMalformed; the caller should close the connection.
func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [frameHeaderLen]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Type: FrameType(hdr[0])}
	want, ok := payloadLen[f.Type]
	if !ok || int(binary.BigEndian.Uint16(hdr[1:3])) != want {
		return Frame{}, ErrMalformed
	}
	var p [8]byte
	if _, err := io.ReadFull(r, p[:want]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	switch f.Type {
	case TypeAccept:
		f.IdleTimeoutMs = binary.BigEndian.Uint32(p[:4])
	case TypeReject, TypeClose:
		f.Reason = Reason(p[0])
		f.RetryAfterS = binary.BigEndian.Uint16(p[1:3])
	default:
		f.Seq = binary.BigEndian.Uint64(p[:8])
	}
	return f, nil
}
