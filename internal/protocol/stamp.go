package protocol

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"
)

// STAMP test packets (RFC 8762) in authenticated mode with the session-sender
// identifier of RFC 8972 — the only STAMP form a node answers. Sender and
// reflector packets are both StampLen bytes; offsets follow RFC 8972 Figs. 3 and 4.
const StampLen = 112

const (
	stampSeq         = 0
	stampTimestamp   = 16
	stampErrEst      = 24
	stampSSID        = 26
	stampReceived    = 32
	stampSenderSeq   = 48
	stampSenderTime  = 64
	stampSenderErr   = 72
	stampSenderTTL   = 80
	stampHMAC        = 96
	stampHMACLen     = 16
	stampKeyInfo     = "connverifier stamp v1"
	stampErrEstimate = 32<<8 | 1 // unsynchronized, NTP format, error 1 s (RFC 4656 §4.1.2)
)

// ErrStampAuth means a STAMP packet has the wrong length or its HMAC does not
// verify under the session's key; such packets are dropped unanswered.
var ErrStampAuth = errors.New("stamp: packet length or HMAC invalid")

// NTP is a 64-bit NTPv4 timestamp: seconds since 1900 and a 32-bit fraction.
type NTP uint64

// NTPTime converts t to NTP format.
func NTPTime(t time.Time) NTP {
	const unixToNTP = 2_208_988_800
	sec := uint64(t.Unix() + unixToNTP)
	frac := uint64(t.Nanosecond()) << 32 / 1_000_000_000
	return NTP(sec<<32 | frac)
}

// Sub returns a − b. Both must come from the same clock and lie within about 68
// years of each other; the difference wraps correctly across an NTP era.
func (a NTP) Sub(b NTP) time.Duration {
	return time.Duration(float64(int64(a-b)) / (1 << 32) * float64(time.Second))
}

// StampKey derives a session's HMAC key from its secret, so the key never
// travels on the wire and differs from every other use of the secret.
func StampKey(secret []byte) []byte {
	key, _ := hkdf.Key(sha256.New, secret, nil, stampKeyInfo, 32)
	return key
}

// StampSender is a session-sender test packet.
type StampSender struct {
	Seq       uint32
	Timestamp NTP // T1
	SSID      uint16
}

// StampReflector is a session-reflector test packet. A stateful reflector counts
// Seq itself, so gaps in Seq versus SenderSeq tell forward from return loss.
type StampReflector struct {
	Seq        uint32
	Timestamp  NTP // T3, taken just before sending
	SSID       uint16
	Received   NTP // T2
	SenderSeq  uint32
	SenderTime NTP
	SenderTTL  uint8
}

// StampSSID returns the session-sender identifier of a packet of the right
// length, before any authentication, so the receiver can find the key.
func StampSSID(b []byte) (uint16, bool) {
	if len(b) != StampLen {
		return 0, false
	}
	return binary.BigEndian.Uint16(b[stampSSID:]), true
}

// Marshal returns the packet signed with key.
func (p StampSender) Marshal(key []byte) [StampLen]byte {
	var b [StampLen]byte
	binary.BigEndian.PutUint32(b[stampSeq:], p.Seq)
	binary.BigEndian.PutUint64(b[stampTimestamp:], uint64(p.Timestamp))
	binary.BigEndian.PutUint16(b[stampErrEst:], stampErrEstimate)
	binary.BigEndian.PutUint16(b[stampSSID:], p.SSID)
	sign(&b, key)
	return b
}

// ParseStampSender verifies b under key and decodes it.
func ParseStampSender(b, key []byte) (StampSender, error) {
	if !verify(b, key) {
		return StampSender{}, ErrStampAuth
	}
	return StampSender{
		Seq:       binary.BigEndian.Uint32(b[stampSeq:]),
		Timestamp: NTP(binary.BigEndian.Uint64(b[stampTimestamp:])),
		SSID:      binary.BigEndian.Uint16(b[stampSSID:]),
	}, nil
}

// Marshal returns the packet signed with key.
func (p StampReflector) Marshal(key []byte) [StampLen]byte {
	var b [StampLen]byte
	binary.BigEndian.PutUint32(b[stampSeq:], p.Seq)
	binary.BigEndian.PutUint64(b[stampTimestamp:], uint64(p.Timestamp))
	binary.BigEndian.PutUint16(b[stampErrEst:], stampErrEstimate)
	binary.BigEndian.PutUint16(b[stampSSID:], p.SSID)
	binary.BigEndian.PutUint64(b[stampReceived:], uint64(p.Received))
	binary.BigEndian.PutUint32(b[stampSenderSeq:], p.SenderSeq)
	binary.BigEndian.PutUint64(b[stampSenderTime:], uint64(p.SenderTime))
	binary.BigEndian.PutUint16(b[stampSenderErr:], stampErrEstimate)
	b[stampSenderTTL] = p.SenderTTL
	sign(&b, key)
	return b
}

// ParseStampReflector verifies b under key and decodes it.
func ParseStampReflector(b, key []byte) (StampReflector, error) {
	if !verify(b, key) {
		return StampReflector{}, ErrStampAuth
	}
	return StampReflector{
		Seq:        binary.BigEndian.Uint32(b[stampSeq:]),
		Timestamp:  NTP(binary.BigEndian.Uint64(b[stampTimestamp:])),
		SSID:       binary.BigEndian.Uint16(b[stampSSID:]),
		Received:   NTP(binary.BigEndian.Uint64(b[stampReceived:])),
		SenderSeq:  binary.BigEndian.Uint32(b[stampSenderSeq:]),
		SenderTime: NTP(binary.BigEndian.Uint64(b[stampSenderTime:])),
		SenderTTL:  b[stampSenderTTL],
	}, nil
}

// sign writes HMAC-SHA-256 over the first 96 octets, truncated to 16 (RFC 8762 §4.4).
func sign(b *[StampLen]byte, key []byte) {
	copy(b[stampHMAC:], mac(b[:stampHMAC], key))
}

func verify(b, key []byte) bool {
	return len(b) == StampLen && hmac.Equal(b[stampHMAC:], mac(b[:stampHMAC], key))
}

func mac(data, key []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)[:stampHMACLen]
}
