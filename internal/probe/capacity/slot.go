package capacity

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log"
	mrand "math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// pool is everything the slots share for one run against one granted session.
type pool struct {
	cfg       Config
	addr      string
	sessionID [16]byte
	secret    []byte
	target    int
	rate      int
	dial      dialFunc
	limiter   *RateLimiter
	dialLog   *dialLogger
	stats     *Stats
	wg        sync.WaitGroup // slot goroutines
}

type Slot struct {
	id      int
	backoff time.Duration
}

// errBadAck marks a heartbeat reply that did not echo the sequence number sent.
var errBadAck = errors.New("heartbeat reply does not match")

// nodeClosedError is a CLOSE frame from the node: the node ended the connection
// and said why, so it is not a network drop.
type nodeClosedError struct{ reason protocol.Reason }

func (e *nodeClosedError) Error() string { return "node closed the connection: " + e.reason.String() }

// rejectError is a REJECT frame: the node refused to admit the connection.
type rejectError struct {
	reason     protocol.Reason
	retryAfter time.Duration
}

func (e *rejectError) Error() string { return "node rejected the connection: " + e.reason.String() }

// hostLimits are the tool host's own bounds on connection count; a zero field
// means the platform could not report it.
type hostLimits struct {
	fdLimit        uint64
	ephemeralPorts int
}

func readHostLimits() hostLimits {
	var h hostLimits
	if n, ok := netenv.FDLimit(); ok {
		h.fdLimit = n
	}
	if n, ok := netenv.EphemeralPorts(); ok {
		h.ephemeralPorts = n
	}
	return h
}

// run holds the slot pool until ctx ends and returns the final counters once
// every slot goroutine has returned.
func (p *pool) run(ctx context.Context) finalStats {
	p.limiter = newRateLimiter(ctx, p.rate)
	p.dialLog = newDialLogger()
	final := make(chan finalStats, 1)
	go reportStats(ctx, p.target, p.stats, final)

	readySlots := make(chan *Slot, p.target)
	for i := range p.target {
		readySlots <- &Slot{id: i + 1, backoff: p.cfg.minBackoff}
	}
	go p.connectionManager(ctx, readySlots)

	f := <-final
	p.wg.Wait()
	return f
}

func (p *pool) connectionManager(ctx context.Context, readySlots chan *Slot) {
	for {
		select {
		case <-ctx.Done():
			return
		case slot := <-readySlots:
			if !p.limiter.Wait(ctx) {
				return
			}
			p.wg.Add(1)
			go p.runSlot(ctx, slot, readySlots)
		}
	}
}

func (p *pool) runSlot(ctx context.Context, slot *Slot, readySlots chan *Slot) {
	defer p.wg.Done()
	conn, err := p.openConn(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slot.backoff = increaseBackoff(slot.backoff, p.cfg.maxBackoff)
		wait := jitter(slot.backoff)
		var rej *rejectError
		if errors.As(err, &rej) {
			wait = max(wait, rej.retryAfter) // the node's hint is a floor, not a replacement
		}
		scheduleSlot(ctx, slot, wait, readySlots)
		return
	}

	stats := p.stats
	atomic.AddInt64(&stats.activeConnections, 1)
	start := time.Now()
	survived, err := maintain(ctx, conn, p.cfg, stats)
	atomic.AddInt64(&stats.activeConnections, -1)
	conn.Close()

	if ctx.Err() != nil {
		return
	}

	duration := time.Since(start)
	atomic.AddUint64(&stats.sessionNanos, uint64(duration))
	recordDropReason(err, stats)

	if p.cfg.logDrops {
		log.Printf("slot %d dropped after %s: %v", slot.id, duration.Round(time.Millisecond), err)
	}
	if survived {
		slot.backoff = p.cfg.minBackoff
	} else {
		slot.backoff = increaseBackoff(slot.backoff, p.cfg.maxBackoff)
	}
	scheduleSlot(ctx, slot, jitter(slot.backoff), readySlots)
}

func scheduleSlot(ctx context.Context, slot *Slot, delay time.Duration, readySlots chan *Slot) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	if delay <= 0 {
		select {
		case readySlots <- slot:
		case <-ctx.Done():
		}
		return
	}

	go func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			select {
			case <-ctx.Done():
				return
			default:
			}
			select {
			case readySlots <- slot:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}
	}()
}

// openConn dials the node and presents the session ticket. It returns the
// connection only once the node has admitted it; a REJECT is a *rejectError.
func (p *pool) openConn(ctx context.Context) (net.Conn, error) {
	stats := p.stats
	atomic.AddUint64(&stats.dialAttempts, 1)
	conn, err := p.dial(ctx, "tcp", p.addr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err // shutdown cancellation, not a dial failure
		}
		atomic.AddUint64(&stats.dialErrors, 1)
		if netx.IsClientResource(err) {
			atomic.AddUint64(&stats.dialErrorsHost, 1)
		}
		p.noteError(err)
		return nil, err
	}

	var nonce [16]byte
	rand.Read(nonce[:])
	conn.SetDeadline(time.Now().Add(p.cfg.ioTimeout))
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) }) // after, or it is overwritten
	defer stop()
	_, err = conn.Write(protocol.NewHello(p.secret, p.sessionID, nonce).MarshalBinary())
	var f protocol.Frame
	if err == nil {
		f, err = protocol.ReadFrame(conn)
	}
	switch {
	case ctx.Err() != nil:
		conn.Close()
		return nil, ctx.Err()
	case err == nil && f.Type == protocol.TypeAccept:
		conn.SetDeadline(time.Time{})
		atomic.AddUint64(&stats.connections, 1)
		p.noteFamily(conn)
		return conn, nil
	case err == nil && f.Type == protocol.TypeReject:
		conn.Close()
		p.countReject(f.Reason)
		return nil, &rejectError{reason: f.Reason, retryAfter: time.Duration(f.RetryAfterS) * time.Second}
	case err == nil:
		err = protocol.ErrMalformed
	}
	conn.Close()
	atomic.AddUint64(&stats.handshakeErrors, 1)
	p.noteError(err)
	return nil, err
}

func (p *pool) countReject(r protocol.Reason) {
	switch r {
	case protocol.ReasonBusy:
		atomic.AddUint64(&p.stats.rejectedBusy, 1)
	case protocol.ReasonQuota:
		atomic.AddUint64(&p.stats.rejectedQuota, 1)
	case protocol.ReasonAuth, protocol.ReasonVersion:
		atomic.AddUint64(&p.stats.rejectedAuth, 1)
	default:
		atomic.AddUint64(&p.stats.rejectedOther, 1)
	}
}

func (p *pool) noteError(err error) {
	msg := err.Error()
	p.stats.lastError.Store(&msg)
	if p.dialLog.shouldLog(msg, time.Now()) {
		log.Printf("connect failed: %v", err)
	}
}

func (p *pool) noteFamily(conn net.Conn) {
	if p.stats.family.Load() != nil {
		return
	}
	family := "ipv6"
	if addr, ok := conn.RemoteAddr().(*net.TCPAddr); ok && addr.IP.To4() != nil {
		family = "ipv4"
	}
	p.stats.family.CompareAndSwap(nil, &family)
}

// maintain heartbeats an admitted connection until it breaks or ctx ends and reports
// whether an interval heartbeat was acked (it survived a full interval: that resets
// the backoff). Reading never pauses, so only a silent drop waits for a reply timeout.
func maintain(ctx context.Context, conn net.Conn, cfg Config, stats *Stats) (survived bool, err error) {
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()

	var (
		fr       protocol.FrameReader
		seq      uint64
		awaiting bool
		initial  = true
		sentAt   time.Time
		nextBeat = time.Now()
	)
	for {
		now := time.Now()
		if !awaiting && !now.Before(nextBeat) {
			seq++
			conn.SetWriteDeadline(now.Add(cfg.ioTimeout))
			if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.TypePing, Seq: seq}); err != nil {
				return survived, err
			}
			atomic.AddUint64(&stats.heartbeatsSent, 1)
			awaiting, sentAt = true, now
		}
		deadline := nextBeat
		if awaiting {
			deadline = sentAt.Add(cfg.ioTimeout)
		}
		conn.SetReadDeadline(deadline)
		if ctx.Err() != nil { // a cancel before this deadline would be overwritten by it
			return survived, ctx.Err()
		}
		f, err := fr.Next(conn)
		if ctx.Err() != nil {
			return survived, ctx.Err()
		}
		if err != nil {
			if isTimeout(err) && !awaiting {
				continue // the next heartbeat is due
			}
			return survived, err
		}
		switch f.Type {
		case protocol.TypePong:
			if !awaiting || f.Seq != seq {
				return survived, errBadAck
			}
			atomic.AddUint64(&stats.heartbeatsAck, 1)
			stats.rtt.Record(time.Since(sentAt))
			if !initial {
				atomic.AddUint64(&stats.intervalAcks, 1)
				survived = true
			}
			initial, awaiting = false, false
			nextBeat = sentAt.Add(cfg.heartbeat)
		case protocol.TypeClose:
			return survived, &nodeClosedError{reason: f.Reason}
		case protocol.TypeProbe:
			conn.SetWriteDeadline(time.Now().Add(cfg.ioTimeout))
			if err := protocol.WriteFrame(conn, protocol.Frame{Type: protocol.TypeProbeAck, Seq: f.Seq}); err != nil {
				return survived, err
			}
		default:
			return survived, protocol.ErrMalformed
		}
	}
}

// recordDropReason records one drop in exactly one bucket: bad ack, closed by the
// node with a reason, timeout (likely a silent NAT/middlebox drop), closed (peer
// EOF/FIN) or error (the rest).
func recordDropReason(err error, stats *Stats) {
	var nc *nodeClosedError
	switch {
	case errors.Is(err, errBadAck):
		atomic.AddUint64(&stats.badAcks, 1)
	case errors.As(err, &nc):
		atomic.AddUint64(&stats.dropNodeClosed, 1)
		switch nc.reason {
		case protocol.ReasonIdleTimeout:
			atomic.AddUint64(&stats.nodeClosedIdle, 1)
		case protocol.ReasonSessionEnded:
			atomic.AddUint64(&stats.nodeClosedSession, 1)
		default:
			atomic.AddUint64(&stats.nodeClosedOther, 1)
		}
	case isTimeout(err):
		atomic.AddUint64(&stats.dropTimeouts, 1)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		atomic.AddUint64(&stats.dropClosed, 1)
	default:
		atomic.AddUint64(&stats.dropErrors, 1)
	}
}

func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// jitter returns a random duration in [d/2, d] (equal jitter): spread-out waits
// desynchronize the reconnect wave after a mass drop. Callers must keep
// slot.backoff itself un-jittered so the exponential progression stays clean.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	half := d / 2
	return half + time.Duration(mrand.Int63n(int64(half)+1))
}

func increaseBackoff(current, maximum time.Duration) time.Duration {
	if current <= 0 {
		current = 100 * time.Millisecond
	}
	current *= 2
	if current > maximum {
		return maximum
	}
	return current
}
