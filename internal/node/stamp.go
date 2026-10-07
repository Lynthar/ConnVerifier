package node

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// stampState is one session's STAMP reflector: stateful, so its own sequence
// number lets the client tell forward from return loss (RFC 8762 §4). Only the
// reflector goroutine touches from and rate; the counters are read at session end.
type stampState struct {
	ssid uint16
	key  []byte
	from netip.AddrPort // latched on the first authenticated packet
	rate bucket

	received, overRate, otherAddr atomic.Uint64
}

func newStampState(ssid uint16, key []byte, rate int, now time.Time) *stampState {
	return &stampState{ssid: ssid, key: key, rate: newBucket(float64(rate), float64(rate), now)}
}

func (st *stampState) counts() *protocol.StampCounts {
	return &protocol.StampCounts{Received: st.received.Load(), OverRate: st.overRate.Load(), OtherAddr: st.otherAddr.Load()}
}

// freeSSID picks a non-zero identifier no live session holds (RFC 8972 §3 forbids
// reuse across sessions). The caller holds s.mu; sessions are capped far below 2^16.
func (s *store) freeSSID() uint16 {
	for {
		var b [2]byte
		rand.Read(b[:])
		if id := binary.BigEndian.Uint16(b[:]); id != 0 && s.bySSID[id] == nil {
			return id
		}
	}
}

// stampSession returns the reflector state of the live session holding ssid.
func (s *store) stampSession(ssid uint16, now time.Time) *stampState {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.bySSID[ssid]
	if s.closing || sess == nil || !now.Before(sess.expires) {
		return nil
	}
	return sess.stamp
}

// reflect answers STAMP packets on pc until it is closed. A packet is answered
// only when its SSID names a live session, its HMAC verifies, it comes from the
// session's source address and fits the granted rate; the reply has its length.
func (s *server) reflect(ctx context.Context, pc net.PacketConn) {
	buf := make([]byte, protocol.StampLen+1) // one spare byte exposes oversized packets
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. Windows reports an ICMP port unreachable here
		}
		received := time.Now()
		ssid, ok := protocol.StampSSID(buf[:n])
		if !ok {
			continue
		}
		st := s.store.stampSession(ssid, received)
		if st == nil {
			continue
		}
		p, err := protocol.ParseStampSender(buf[:n], st.key)
		if err != nil {
			continue
		}
		addr := udpAddrPort(from)
		switch {
		case !st.from.IsValid():
			st.from = addr
		case st.from != addr:
			st.otherAddr.Add(1)
			continue
		}
		if !st.rate.allow(received) {
			st.overRate.Add(1)
			continue
		}
		seq := st.received.Add(1) - 1
		reply := protocol.StampReflector{
			Seq: uint32(seq), SSID: ssid, Received: protocol.NTPTime(received),
			SenderSeq: p.Seq, SenderTime: p.Timestamp, Timestamp: protocol.NTPTime(time.Now()),
		}.Marshal(st.key)
		pc.WriteTo(reply[:], from)
	}
}

func udpAddrPort(a net.Addr) netip.AddrPort {
	if u, ok := a.(*net.UDPAddr); ok {
		ap := u.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}
