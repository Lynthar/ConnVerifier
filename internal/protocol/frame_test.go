package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

var testSecret = bytes.Repeat([]byte{7}, 32)

func TestFrameRoundTrip(t *testing.T) {
	frames := []Frame{
		{Type: TypeAccept, IdleTimeoutMs: 0},
		{Type: TypeAccept, IdleTimeoutMs: ^uint32(0)},
		{Type: TypeReject, Reason: ReasonBusy, RetryAfterS: 30},
		{Type: TypeClose, Reason: ReasonIdleTimeout},
		{Type: TypeClose, Reason: Reason(200), RetryAfterS: ^uint16(0)},
		{Type: TypePing, Seq: 0},
		{Type: TypePong, Seq: ^uint64(0)},
		{Type: TypeProbe, Seq: 42},
		{Type: TypeProbeAck, Seq: 42},
	}
	var stream bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&stream, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range frames {
		got, err := ReadFrame(&stream)
		if err != nil {
			t.Fatalf("ReadFrame(%+v): %v", want, err)
		}
		if got != want {
			t.Fatalf("round trip = %+v, want %+v", got, want)
		}
	}
	if _, err := ReadFrame(&stream); err != io.EOF {
		t.Fatalf("read past the last frame = %v, want io.EOF", err)
	}
}

func TestReadFrameRejects(t *testing.T) {
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"unknown type", []byte{0x7f, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0}, ErrMalformed},
		{"hello is not a frame", []byte{0x01, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0}, ErrMalformed},
		{"length too long for type", []byte{byte(TypePing), 0, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0}, ErrMalformed},
		{"length too short for type", []byte{byte(TypePing), 0, 7, 0, 0, 0, 0, 0, 0, 0}, ErrMalformed},
		{"length high byte set", []byte{byte(TypePing), 1, 8}, ErrMalformed},
		{"truncated payload", []byte{byte(TypePing), 0, 8, 0, 0, 0}, io.ErrUnexpectedEOF},
		{"truncated header", []byte{byte(TypePing), 0}, io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadFrame(bytes.NewReader(tt.in)); !errors.Is(err, tt.want) {
				t.Fatalf("ReadFrame = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestHelloSignedAndParsed(t *testing.T) {
	sid, nonce := [16]byte{1, 2, 3}, [16]byte{9, 8, 7}
	h := NewHello(testSecret, sid, nonce)
	wire := h.MarshalBinary()
	if len(wire) != HelloSize || wire[0] != MagicByte {
		t.Fatalf("hello wire = %x", wire)
	}
	got, err := ReadHello(bytes.NewReader(wire))
	if err != nil || got != h {
		t.Fatalf("ReadHello = %+v, %v; want %+v", got, err, h)
	}
	if !got.Verify(testSecret) {
		t.Fatal("hello signed with the secret did not verify")
	}
	if got.Verify(bytes.Repeat([]byte{8}, 32)) {
		t.Fatal("hello verified under the wrong secret")
	}

	for _, i := range []int{4, 8, 24, 40} { // version, session, nonce, mac
		tampered := bytes.Clone(wire)
		tampered[i] ^= 1
		h2, err := ReadHello(bytes.NewReader(tampered))
		if err == nil && h2.Verify(testSecret) {
			t.Fatalf("hello with byte %d flipped still verified", i)
		}
	}
}

func TestReadHelloRejects(t *testing.T) {
	good := NewHello(testSecret, [16]byte{1}, [16]byte{2}).MarshalBinary()
	mutate := func(i int, v byte) []byte { b := bytes.Clone(good); b[i] = v; return b }
	tests := []struct {
		name string
		in   []byte
		want error
	}{
		{"bad magic", mutate(0, 'X'), ErrMalformed},
		{"wrong type", mutate(5, 0x02), ErrMalformed},
		{"reserved bits set", mutate(7, 1), ErrMalformed},
		{"other version", mutate(4, 2), ErrUnsupported},
		{"short", good[:HelloSize-1], io.ErrUnexpectedEOF},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ReadHello(bytes.NewReader(tt.in)); !errors.Is(err, tt.want) {
				t.Fatalf("ReadHello = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestReasonNames(t *testing.T) {
	if ReasonBusy.String() != "busy" || ReasonShuttingDown.String() != "shutting_down" {
		t.Fatal("known reasons misnamed")
	}
	if Reason(99).String() != "other" {
		t.Fatal("unknown reason must read as other")
	}
}

// Any frame ReadFrame accepts must re-encode to the bytes it was read from.
func FuzzReadFrame(f *testing.F) {
	f.Add(Frame{Type: TypePing, Seq: 7}.Append(nil))
	f.Add(Frame{Type: TypeReject, Reason: ReasonQuota, RetryAfterS: 5}.Append(nil))
	f.Add(Frame{Type: TypeAccept, IdleTimeoutMs: 120000}.Append(nil))
	f.Add([]byte{0xff, 0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		fr, err := ReadFrame(bytes.NewReader(data))
		if err != nil {
			return
		}
		enc := fr.Append(nil)
		if !bytes.Equal(enc, data[:len(enc)]) {
			t.Fatalf("re-encoded %x, read from %x", enc, data)
		}
	})
}

func FuzzReadHello(f *testing.F) {
	f.Add(NewHello(testSecret, [16]byte{1}, [16]byte{2}).MarshalBinary())
	f.Add(make([]byte, HelloSize))
	f.Fuzz(func(t *testing.T, data []byte) {
		h, err := ReadHello(bytes.NewReader(data))
		if err != nil {
			return
		}
		if enc := h.MarshalBinary(); !bytes.Equal(enc, data[:HelloSize]) {
			t.Fatalf("re-encoded %x, read from %x", enc, data[:HelloSize])
		}
	})
}
