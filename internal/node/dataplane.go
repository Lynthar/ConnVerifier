package node

import (
	"errors"
	"net"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// serveData runs one data-plane connection from its HELLO, which must arrive
// within the deadline route already set, to its close. release ends the
// connection's unauthenticated status once the HELLO has been decided.
func (s *server) serveData(conn net.Conn, release func()) {
	defer conn.Close()
	c := &dataConn{conn: conn}
	h, err := protocol.ReadHello(conn)
	release()
	if errors.Is(err, protocol.ErrUnsupported) {
		s.stats.rejectedVersion.Add(1)
		c.send(protocol.Frame{Type: protocol.TypeReject, Reason: protocol.ReasonVersion})
		return
	}
	if err != nil {
		return
	}
	if reason := s.store.admit(h, c, time.Now()); reason != 0 {
		s.countReject(reason)
		c.send(protocol.Frame{Type: protocol.TypeReject, Reason: reason, RetryAfterS: retryAfter(reason)})
		return
	}
	defer s.store.release(h.SessionID, c)

	idle := s.store.idleTimeout(h.SessionID)
	if idle <= 0 {
		return // the session ended between admission and here
	}
	if c.send(protocol.Frame{Type: protocol.TypeAccept, IdleTimeoutMs: uint32(idle / time.Millisecond)}) != nil {
		return
	}
	for {
		conn.SetReadDeadline(time.Now().Add(idle))
		f, err := protocol.ReadFrame(conn)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				c.send(protocol.Frame{Type: protocol.TypeClose, Reason: protocol.ReasonIdleTimeout})
			}
			return
		}
		switch f.Type {
		case protocol.TypePing:
			if c.send(protocol.Frame{Type: protocol.TypePong, Seq: f.Seq}) != nil {
				return
			}
		case protocol.TypeProbeAck:
		default: // a client CLOSE, or a frame only a node may send
			return
		}
	}
}

func (s *server) countReject(r protocol.Reason) {
	switch r {
	case protocol.ReasonBusy:
		s.stats.rejectedBusy.Add(1)
	case protocol.ReasonQuota:
		s.stats.rejectedQuota.Add(1)
	case protocol.ReasonShuttingDown: // the node's doing, not a refusal of the client
	default:
		s.stats.rejectedAuth.Add(1)
	}
}

// retryAfter is a hint: capacity frees up when other connections end, a dial-rate
// refusal clears within a second, and an auth failure will not clear by waiting.
func retryAfter(r protocol.Reason) uint16 {
	switch r {
	case protocol.ReasonBusy:
		return 30
	case protocol.ReasonQuota:
		return 1
	}
	return 0
}
