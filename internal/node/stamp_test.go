package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const baselineBody = `{"client":{"name":"t","version":"v"},"check":"baseline",` +
	`"want":{"connections":1,"dial_rate":1,"duration_s":60,"idle_timeout_s":60,"stamp_rate":%d}}`

// reflectorFixture is a node with a running reflector on an in-memory UDP socket
// and one baseline session granted through the control plane.
type reflectorFixture struct {
	s     *server
	pc    *memnet.PacketConn
	token string
	resp  protocol.SessionResponse
	key   []byte
	stop  func()
}

func newReflectorFixture(t *testing.T, stampRate int) *reflectorFixture {
	t.Helper()
	s, inv := testServer(t, 10)
	s.udp = true
	f := &reflectorFixture{s: s, pc: memnet.NewPacketConn("192.0.2.10:7443"), token: protocol.EncodeID(inv.Token[:])}
	w := apiRequest(t, s, http.MethodPost, protocol.PathSessions, f.token, fmt.Sprintf(baselineBody, stampRate))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &f.resp); err != nil || f.resp.Stamp == nil {
		t.Fatalf("session response %s: %v", w.Body, err)
	}
	secret, _ := f.resp.SecretBytes()
	f.key = protocol.StampKey(secret)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.reflect(ctx, f.pc); close(done) }()
	f.stop = func() { cancel(); f.pc.Close(); <-done }
	t.Cleanup(f.stop)
	return f
}

func (f *reflectorFixture) send(t *testing.T, c *memnet.Conn, seq uint32, key []byte, ssid uint16) {
	t.Helper()
	b := protocol.StampSender{Seq: seq, Timestamp: protocol.NTPTime(time.Now()), SSID: ssid}.Marshal(key)
	c.Write(b[:])
}

// reply reads one reflected packet, or reports that none came within 100 ms.
func reply(t *testing.T, c *memnet.Conn, key []byte) (protocol.StampReflector, bool) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	buf := make([]byte, 256)
	n, err := c.Read(buf)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return protocol.StampReflector{}, false
	}
	if err != nil || n != protocol.StampLen {
		t.Fatalf("read %d bytes: %v", n, err)
	}
	r, err := protocol.ParseStampReflector(buf[:n], key)
	if err != nil {
		t.Fatalf("reply does not verify: %v", err)
	}
	return r, true
}

func (f *reflectorFixture) end(t *testing.T) protocol.SessionEnd {
	t.Helper()
	w := apiRequest(t, f.s, http.MethodDelete, protocol.PathSessions+"/"+f.resp.SessionID, f.token, "")
	var e protocol.SessionEnd
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &e) != nil || e.Stamp == nil {
		t.Fatalf("end = %d %s", w.Code, w.Body)
	}
	return e
}

// A stateful reflector numbers what it received itself, echoes the sender's
// fields and timestamps its receive and send, so the client can split loss by
// direction and see the node's own delay.
func TestReflectorAnswersItsSession(t *testing.T) {
	f := newReflectorFixture(t, 100)
	c := f.pc.Dial()
	for i, seq := range []uint32{7, 9} {
		f.send(t, c, seq, f.key, f.resp.Stamp.SSID)
		r, ok := reply(t, c, f.key)
		if !ok {
			t.Fatalf("packet %d not answered", seq)
		}
		if r.Seq != uint32(i) || r.SenderSeq != seq || r.SSID != f.resp.Stamp.SSID {
			t.Fatalf("reply %+v: want reflector seq %d for sender seq %d", r, i, seq)
		}
		if d := r.Timestamp.Sub(r.Received); d < 0 || d > time.Second {
			t.Fatalf("residence %v", d)
		}
	}
	if got := f.end(t).Stamp; got.Received != 2 || got.OverRate != 0 || got.OtherAddr != 0 {
		t.Fatalf("counts %+v", got)
	}
}

// The reflector answers nothing it cannot authenticate, so it cannot be used to
// reflect traffic at anyone who does not hold the session's key.
func TestReflectorIgnoresWhatItCannotAuthenticate(t *testing.T) {
	f := newReflectorFixture(t, 100)
	c := f.pc.Dial()
	f.send(t, c, 1, protocol.StampKey(make([]byte, 32)), f.resp.Stamp.SSID)
	f.send(t, c, 2, f.key, f.resp.Stamp.SSID+1)
	c.Write(make([]byte, protocol.StampLen-1))
	if r, ok := reply(t, c, f.key); ok {
		t.Fatalf("answered %+v", r)
	}
	if got := f.end(t).Stamp; got.Received != 0 {
		t.Fatalf("counts %+v", got)
	}
}

// After the first packet the session belongs to that source address; another
// address is counted, not answered — a changed NAT mapping must not look like loss.
func TestReflectorLatchesTheFirstSourceAddress(t *testing.T) {
	f := newReflectorFixture(t, 100)
	first, other := f.pc.Dial(), f.pc.Dial()
	f.send(t, first, 0, f.key, f.resp.Stamp.SSID)
	if _, ok := reply(t, first, f.key); !ok {
		t.Fatal("first packet not answered")
	}
	f.send(t, other, 1, f.key, f.resp.Stamp.SSID)
	if _, ok := reply(t, other, f.key); ok {
		t.Fatal("another address was answered")
	}
	if got := f.end(t).Stamp; got.Received != 1 || got.OtherAddr != 1 {
		t.Fatalf("counts %+v", got)
	}
}

// Packets beyond the granted rate are counted as such, so the client can tell
// them from network loss.
func TestReflectorCountsPacketsOverTheRate(t *testing.T) {
	f := newReflectorFixture(t, 1)
	c := f.pc.Dial()
	for seq := range uint32(3) {
		f.send(t, c, seq, f.key, f.resp.Stamp.SSID)
	}
	if _, ok := reply(t, c, f.key); !ok {
		t.Fatal("first packet not answered")
	}
	if r, ok := reply(t, c, f.key); ok {
		t.Fatalf("a packet over the rate was answered: %+v", r)
	}
	if got := f.end(t).Stamp; got.Received != 1 || got.OverRate != 2 {
		t.Fatalf("counts %+v", got)
	}
}

func TestReflectorStopsAnsweringAnEndedSession(t *testing.T) {
	f := newReflectorFixture(t, 100)
	f.end(t)
	c := f.pc.Dial()
	f.send(t, c, 0, f.key, f.resp.Stamp.SSID)
	if r, ok := reply(t, c, f.key); ok {
		t.Fatalf("ended session answered %+v", r)
	}
}

// Only a baseline session on a node that runs a reflector gets a STAMP grant.
func TestStampGrantOnlyWithAReflector(t *testing.T) {
	for _, tt := range []struct {
		name  string
		udp   bool
		body  string
		stamp bool
	}{
		{"baseline with reflector", true, fmt.Sprintf(baselineBody, 50), true},
		{"baseline without reflector", false, fmt.Sprintf(baselineBody, 50), false},
		{"capacity asking for STAMP", true, `{"client":{"name":"t","version":"v"},"check":"tcp-capacity",` +
			`"want":{"connections":1,"dial_rate":1,"duration_s":60,"idle_timeout_s":60,"stamp_rate":50}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, inv := testServer(t, 10)
			s.udp = tt.udp
			w := apiRequest(t, s, http.MethodPost, protocol.PathSessions, protocol.EncodeID(inv.Token[:]), tt.body)
			var r protocol.SessionResponse
			if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &r) != nil {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			if (r.Stamp != nil) != tt.stamp || (r.Granted.StampRate > 0) != tt.stamp {
				t.Fatalf("stamp %+v granted %+v", r.Stamp, r.Granted)
			}
		})
	}
}
