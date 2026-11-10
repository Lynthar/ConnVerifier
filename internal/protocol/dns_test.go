package protocol

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// The expected bytes are checked by hand against RFC 1035 §4.1 and RFC 6891 §6.1.2.
func TestRootNSQuery(t *testing.T) {
	want, _ := hex.DecodeString("abcd" + "0100" + "0001" + "0000" + "0000" + "0001" +
		"00" + "0002" + "0001" +
		"00" + "0029" + "04d0" + "00000000" + "0000")
	if got := RootNSQuery(0xabcd); !bytes.Equal(got, want) {
		t.Fatalf("RootNSQuery = %x, want %x", got, want)
	}
}

// answer turns a query into a response with rcode, as a resolver would, leaving
// the question in and the records out.
func answer(q []byte, rcode int) []byte {
	a := bytes.Clone(q[:dnsHeaderLen+5])
	a[2] |= 0x80
	a[3] = a[3]&0xf0 | byte(rcode)
	a[11] = 0 // no additional record
	return a
}

func TestParseRootNSAnswer(t *testing.T) {
	q := RootNSQuery(7)
	refusedBare := answer(q, 5)[:dnsHeaderLen]
	refusedBare[5] = 0
	otherType := answer(q, 0)
	otherType[dnsHeaderLen+2] = 1 // A, not NS
	notify := answer(q, 0)
	notify[2] |= 4 << 3
	for _, tt := range []struct {
		name  string
		b     []byte
		ok    bool
		rcode int
	}{
		{"answer", answer(q, 0), true, 0},
		{"answer with records after the question", append(answer(q, 0), 1, 2, 3), true, 0},
		{"server failure", answer(q, 2), true, 2},
		{"refusal without the question", refusedBare, true, 5},
		{"the query itself", q, false, 0},
		{"another question", otherType, false, 0},
		{"not a standard query", notify, false, 0},
		{"short", answer(q, 0)[:dnsHeaderLen+3], false, 0},
		{"no question and no error", func() []byte { b := answer(q, 0)[:dnsHeaderLen]; b[5] = 0; return b }(), false, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			id, rcode, err := ParseRootNSAnswer(tt.b)
			if (err == nil) != tt.ok || (tt.ok && (id != 7 || rcode != tt.rcode)) {
				t.Fatalf("ParseRootNSAnswer = %d, %d, %v; want ok %v rcode %d", id, rcode, err, tt.ok, tt.rcode)
			}
		})
	}
}

func FuzzParseRootNSAnswer(f *testing.F) {
	f.Add(answer(RootNSQuery(1), 0))
	f.Add(RootNSQuery(1))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		id, rcode, err := ParseRootNSAnswer(data)
		if err != nil {
			return
		}
		if len(data) < dnsHeaderLen || rcode < 0 || rcode > 15 || id != uint16(data[0])<<8|uint16(data[1]) {
			t.Fatalf("accepted %x as id %d rcode %d", data, id, rcode)
		}
	})
}
