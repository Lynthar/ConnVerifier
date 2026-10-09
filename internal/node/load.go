package node

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netenv"
	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

const (
	loadChunk  = 64 << 10
	writeChunk = 16 << 10 // one HTTP/2 frame, so a probe's answer waits behind at most one
	// h2MaxFrame is the smallest frame size HTTP/2 allows. Clients send DATA frames
	// up to what the server advertises (Go's default 1 MiB) and hold the connection
	// while writing one, so a probe's request would wait behind up to half a megabyte.
	h2MaxFrame    = 16 << 10
	smallRate     = 200 // small-object requests per second per session
	loadRetryS    = 60  // retry hint when another session holds the load slots
	lagPeriod     = 10 * time.Millisecond
	lagLimit      = 10 * time.Millisecond
	progressEvery = 50 * time.Millisecond // how often an upload reports what it has read
	h2ReceiveSize = 16 << 20              // per stream and per connection; Go's default 1 MiB caps uploads at about 84 Mbit/s over 100 ms
	// A download stream's kernel keeps at most 5 ms of its throughput unsent, so
	// the node's own buffer does not delay the probes that share the connection
	// (draft-ietf-ippm-responsiveness §6.1.1); before a throughput is known,
	// firstLowat bytes.
	lowatTime  = 5 * time.Millisecond
	lowatEvery = time.Second
	firstLowat = 256 << 10
	minLowat   = 4 << 10
)

// connKey holds the connection a request arrived on, for socket options.
type connKey struct{}

// socketOf returns the TCP connection under a request's TLS, or nil.
func socketOf(r *http.Request) net.Conn {
	tc, ok := r.Context().Value(connKey{}).(*tls.Conn)
	if !ok {
		return nil
	}
	c := tc.NetConn()
	if pc, ok := c.(*peekedConn); ok {
		return pc.Conn
	}
	return c
}

var zeros = make([]byte, loadChunk)

// loadState is a session's allowance on the load endpoints. remaining gates every
// body byte in either direction before it moves; sent and received count what did.
type loadState struct {
	ip             netip.Addr // the address the session was created from
	conns          int
	granted        int64
	day            time.Time // the UTC day the grant was charged to
	remaining      atomic.Int64
	sent, received atomic.Uint64
	smallMu        sync.Mutex
	small          bucket
	lag            *netenv.Lag
	ended          chan struct{}
	once           sync.Once
	counts         protocol.LoadCounts
}

func newLoadState(ip netip.Addr, granted int64, conns int, day, now time.Time) *loadState {
	l := &loadState{
		ip: ip, conns: conns, granted: granted, day: day,
		small: newBucket(smallRate, smallRate, now),
		lag:   netenv.StartLag(lagPeriod, lagLimit),
		ended: make(chan struct{}),
	}
	l.remaining.Store(granted)
	return l
}

// take reserves up to n bytes of the budget and returns how many it got.
func (l *loadState) take(n int) int {
	for {
		r := l.remaining.Load()
		if r <= 0 {
			return 0
		}
		got := min(int64(n), r)
		if l.remaining.CompareAndSwap(r, r-got) {
			return int(got)
		}
	}
}

func (l *loadState) giveBack(n int) { l.remaining.Add(int64(n)) }

func (l *loadState) allowSmall(now time.Time) bool {
	l.smallMu.Lock()
	defer l.smallMu.Unlock()
	return l.small.allow(now)
}

// finish stops the lag timer and wakes every load handler of the session; it
// returns the session's final counts and may be called more than once.
func (l *loadState) finish() protocol.LoadCounts {
	l.once.Do(func() {
		ticks, over := l.lag.Stop()
		close(l.ended)
		l.counts = protocol.LoadCounts{Sent: l.sent.Load(), Received: l.received.Load(), LagTicks: ticks, LagOver: over}
	})
	return l.counts
}

// unused is the part of the grant no byte moved through.
func (l *loadState) unused() int64 {
	return max(0, l.granted-int64(l.sent.Load()+l.received.Load()))
}

// dayUse is what one invite's sessions have been granted on one UTC day.
type dayUse struct {
	day  time.Time
	used int64
}

// outliveTimeouts moves a load stream's read and write deadlines to its session's
// end. The server's 10 s timeouts apply per HTTP/2 stream, and on expiry Go resets
// the stream even when the handler has not written yet: an upload would be cut.
func outliveTimeouts(w http.ResponseWriter, sess *session) {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(sess.expires)
	rc.SetWriteDeadline(sess.expires)
}

// loadSession finds the live session id with a load grant, created by the invite
// whose token the request bears; anything else is answered "no such session".
func (s *server) loadSession(w http.ResponseWriter, r *http.Request) (*session, bool) {
	now := time.Now()
	var sess *session
	id, err := protocol.DecodeID(r.PathValue("id"))
	b64, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	token, terr := base64.RawURLEncoding.DecodeString(b64)
	if err == nil && ok && terr == nil && len(token) == 32 {
		sess = s.store.loadSession(id, tokenHash(token), now)
	}
	if sess != nil {
		return sess, true
	}
	// Load requests skip the per-address request limit; failed ones pay into it.
	if !s.requests.allow(requestIP(r), now) {
		writeError(w, http.StatusTooManyRequests, protocol.ErrReasonQuota, 1, "too many requests")
		return nil, false
	}
	writeError(w, http.StatusNotFound, protocol.ErrReasonBadRequest, 0, "no such session with a load grant")
	return nil, false
}

func (s *server) loadSmall(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	if !sess.load.allowSmall(time.Now()) {
		writeError(w, http.StatusTooManyRequests, protocol.ErrReasonQuota, 1, "too many requests")
		return
	}
	if sess.load.take(1) == 0 {
		writeError(w, http.StatusRequestEntityTooLarge, protocol.ErrReasonQuota, 0, "load budget spent")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	n, _ := w.Write(zeros[:1])
	sess.load.sent.Add(uint64(n))
	sess.load.giveBack(1 - n)
}

// loadLarge sends until the session's budget is spent, then ends the response
// normally, so the client can tell a spent budget from a failure.
func (s *server) loadLarge(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ld := sess.load
	outliveTimeouts(w, sess)
	sock := socketOf(r)
	if sock != nil {
		netx.SetNotSentLowat(sock, firstLowat)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	mark, marked := time.Now(), 0
	for {
		if sock != nil && time.Since(mark) >= lowatEvery {
			bps := float64(marked) / time.Since(mark).Seconds()
			netx.SetNotSentLowat(sock, max(int(bps*lowatTime.Seconds()), minLowat))
			mark, marked = time.Now(), 0
		}
		select {
		case <-r.Context().Done():
			return
		case <-ld.ended:
			return
		default:
		}
		n := ld.take(writeChunk)
		if n == 0 {
			return
		}
		written, err := w.Write(zeros[:n])
		marked += written
		ld.sent.Add(uint64(written))
		ld.giveBack(n - written)
		if err != nil {
			return
		}
	}
}

// loadUpload reads and discards the body and reports, while it reads, how many
// body bytes it has read so far: the client counts upload goodput from these
// reports, because bytes written on its side include whatever is still in
// flight. The response ends before the body does once the budget is spent.
func (s *server) loadUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.loadSession(w, r)
	if !ok {
		return
	}
	ld := sess.load
	outliveTimeouts(w, sess)
	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	var read uint64
	var record [8]byte
	report := func() error {
		binary.BigEndian.PutUint64(record[:], read)
		if _, err := w.Write(record[:]); err != nil {
			return err
		}
		return rc.Flush()
	}
	if report() != nil {
		return
	}
	buf := make([]byte, loadChunk)
	last := time.Now()
	for {
		select {
		case <-ld.ended:
			return
		default:
		}
		n := ld.take(len(buf))
		if n == 0 {
			report()
			return
		}
		got, err := r.Body.Read(buf[:n])
		ld.received.Add(uint64(got))
		ld.giveBack(n - got)
		read += uint64(got)
		if err != nil {
			report()
			return
		}
		if time.Since(last) >= progressEvery {
			if report() != nil {
				return
			}
			last = time.Now()
		}
	}
}

func requestIP(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}
