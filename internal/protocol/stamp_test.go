package protocol

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

var stampTestKey = StampKey(bytes.Repeat([]byte{7}, 32))

func testReflector() StampReflector {
	return StampReflector{
		Seq: 41, Timestamp: NTPTime(time.Unix(1_800_000_000, 2_000_000)), SSID: 0xbeef,
		Received: NTPTime(time.Unix(1_800_000_000, 1_000_000)), SenderSeq: 42,
		SenderTime: NTPTime(time.Unix(1_799_999_999, 0)), SenderTTL: 64,
	}
}

// Field offsets are the ones drawn in RFC 8972 Figs. 3 and 4; a peer built from
// the RFC reads them there.
func TestStampLayoutFollowsRFC(t *testing.T) {
	b := StampSender{Seq: 0x01020304, Timestamp: 0x1112131415161718, SSID: 0xbeef}.Marshal(stampTestKey)
	if got := binary.BigEndian.Uint32(b[0:]); got != 0x01020304 {
		t.Errorf("sequence at 0 = %#x", got)
	}
	if !bytes.Equal(b[4:16], make([]byte, 12)) || !bytes.Equal(b[28:96], make([]byte, 68)) {
		t.Error("MBZ fields are not zero")
	}
	if got := binary.BigEndian.Uint64(b[16:]); got != 0x1112131415161718 {
		t.Errorf("timestamp at 16 = %#x", got)
	}
	if got := binary.BigEndian.Uint16(b[26:]); got != 0xbeef {
		t.Errorf("SSID at 26 = %#x", got)
	}
	r := testReflector().Marshal(stampTestKey)
	for _, f := range []struct {
		name string
		off  int
		want uint64
		size int
	}{
		{"sequence", 0, 41, 4}, {"SSID", 26, 0xbeef, 2}, {"receive timestamp", 32, uint64(testReflector().Received), 8},
		{"sender sequence", 48, 42, 4}, {"sender timestamp", 64, uint64(testReflector().SenderTime), 8},
		{"sender TTL", 80, 64, 1},
	} {
		var got uint64
		for _, c := range r[f.off : f.off+f.size] {
			got = got<<8 | uint64(c)
		}
		if got != f.want {
			t.Errorf("%s at %d = %#x, want %#x", f.name, f.off, got, f.want)
		}
	}
}

func TestStampRoundTrip(t *testing.T) {
	s := StampSender{Seq: 9, Timestamp: NTPTime(time.Now()), SSID: 3}
	b := s.Marshal(stampTestKey)
	if ssid, ok := StampSSID(b[:]); !ok || ssid != 3 {
		t.Fatalf("StampSSID = %d, %v", ssid, ok)
	}
	if got, err := ParseStampSender(b[:], stampTestKey); err != nil || got != s {
		t.Fatalf("sender = %+v, %v; want %+v", got, err, s)
	}
	r := testReflector()
	rb := r.Marshal(stampTestKey)
	if got, err := ParseStampReflector(rb[:], stampTestKey); err != nil || got != r {
		t.Fatalf("reflector = %+v, %v; want %+v", got, err, r)
	}
}

// Every authenticated byte matters: a packet altered anywhere, signed with another
// key, or of another length is refused, so the node never answers it.
func TestStampRefusesAlteredPackets(t *testing.T) {
	good := StampSender{Seq: 1, SSID: 5}.Marshal(stampTestKey)
	for i := range StampLen {
		b := good
		b[i] ^= 0x01
		if _, err := ParseStampSender(b[:], stampTestKey); !errors.Is(err, ErrStampAuth) {
			t.Fatalf("byte %d flipped: err = %v", i, err)
		}
	}
	if _, err := ParseStampSender(good[:], StampKey(make([]byte, 32))); !errors.Is(err, ErrStampAuth) {
		t.Fatalf("other key: err = %v", err)
	}
	if _, err := ParseStampSender(append(good[:], 0), stampTestKey); !errors.Is(err, ErrStampAuth) {
		t.Fatalf("longer packet: err = %v", err)
	}
	if _, ok := StampSSID(good[:StampLen-1]); ok {
		t.Fatal("StampSSID accepted a short packet")
	}
}

func TestNTPTime(t *testing.T) {
	if got := NTPTime(time.Unix(0, 0)); got != 2_208_988_800<<32 {
		t.Fatalf("Unix epoch = %#x", uint64(got))
	}
	a := time.Unix(1_800_000_000, 0)
	if d := NTPTime(a.Add(1500 * time.Microsecond)).Sub(NTPTime(a)); d < 1499*time.Microsecond || d > 1501*time.Microsecond {
		t.Fatalf("1.5 ms apart = %v", d)
	}
	if d := NTP(5 << 31).Sub(NTP(1<<64 - 1<<31)); d != 3*time.Second {
		t.Fatalf("across the era wrap = %v, want 3s", d)
	}
}

func FuzzParseStampSender(f *testing.F) {
	good := StampSender{Seq: 1, SSID: 2}.Marshal(stampTestKey)
	f.Add(good[:])
	f.Add(make([]byte, StampLen))
	f.Add([]byte{1, 2, 3})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseStampSender(data, stampTestKey)
		if err != nil {
			return
		}
		if again, err := ParseStampSender(func() []byte { b := p.Marshal(stampTestKey); return b[:] }(), stampTestKey); err != nil || again != p {
			t.Fatalf("re-marshalled packet did not round trip: %v", err)
		}
	})
}

func FuzzParseStampReflector(f *testing.F) {
	good := testReflector().Marshal(stampTestKey)
	f.Add(good[:])
	f.Add(make([]byte, StampLen))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParseStampReflector(data, stampTestKey)
		if err != nil {
			return
		}
		if again, err := ParseStampReflector(func() []byte { b := p.Marshal(stampTestKey); return b[:] }(), stampTestKey); err != nil || again != p {
			t.Fatalf("re-marshalled packet did not round trip: %v", err)
		}
	})
}
