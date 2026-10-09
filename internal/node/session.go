package node

import (
	"crypto/rand"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// bucket is a token bucket refilled by elapsed time. Callers hold the lock of
// whatever owns it.
type bucket struct {
	tokens, rate, burst float64
	last                time.Time
}

func newBucket(rate, burst float64, now time.Time) bucket {
	return bucket{tokens: burst, rate: rate, burst: burst, last: now}
}

func (b *bucket) allow(now time.Time) bool {
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// dataConn is one admitted data-plane connection. Writes are serialized because
// the session's expiry may send CLOSE while the connection's own loop sends PONG.
type dataConn struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *dataConn) send(f protocol.Frame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return protocol.WriteFrame(c.conn, f)
}

type session struct {
	id      [16]byte
	secret  []byte
	invite  string // token hash of the invite that created it
	granted protocol.Limits
	expires time.Time
	dials   bucket
	conns   map[*dataConn]struct{} // live; guarded by store.mu
	stamp   *stampState            // nil unless granted a STAMP rate
	load    *loadState             // nil unless granted load
	loadAns *protocol.LoadGrant    // what to answer about load; nil when not asked or not offered
}

// store holds sessions and enforces every count: sessions per invite and in
// total, live connections per session and in total, new connections per second,
// sessions with load at once and load bytes per invite per day.
type store struct {
	mu              sync.Mutex
	sessions        map[[16]byte]*session
	bySSID          map[uint16]*session
	perInvite       map[string]int
	daily           map[string]*dayUse // load granted per invite today; lost on restart
	liveConns       int
	maxConns        int
	maxSessions     int
	loadSessions    int
	maxLoadSessions int
	started         time.Time
	closing         bool // set by close: no new sessions or connections
}

func newStore(maxConns, maxSessions, maxLoadSessions int, now time.Time) *store {
	return &store{
		sessions:        make(map[[16]byte]*session),
		bySSID:          make(map[uint16]*session),
		perInvite:       make(map[string]int),
		daily:           make(map[string]*dayUse),
		maxConns:        maxConns,
		maxSessions:     maxSessions,
		maxLoadSessions: maxLoadSessions,
		started:         now,
	}
}

func (s *store) load() protocol.Load {
	s.mu.Lock()
	defer s.mu.Unlock()
	return protocol.Load{
		Sessions:       len(s.sessions),
		Connections:    s.liveConns,
		MaxConnections: s.maxConns,
		UptimeS:        int64(time.Since(s.started).Seconds()),
	}
}

// create grants want within the invite's limits; ip is where the request came
// from. It returns the API error reason when the invite or the node has no room
// for another session. Load that cannot be granted now does not refuse the session.
func (s *store) create(invite string, lim InviteLimits, want protocol.Limits, ip netip.Addr, now time.Time) (*session, string) {
	g := protocol.Limits{
		Connections:  min(want.Connections, lim.MaxConnections),
		DialRate:     min(want.DialRate, lim.MaxDialRate),
		DurationS:    lim.MaxDurationS,
		IdleTimeoutS: min(want.IdleTimeoutS, lim.MaxIdleTimeoutS),
		StampRate:    min(want.StampRate, lim.MaxStampRate),
	}
	if want.DurationS > 0 {
		g.DurationS = min(want.DurationS, lim.MaxDurationS)
	}
	sess := &session{
		invite:  invite,
		granted: g,
		expires: now.Add(time.Duration(g.DurationS) * time.Second),
		dials:   newBucket(float64(g.DialRate), float64(g.DialRate), now),
		conns:   make(map[*dataConn]struct{}),
		secret:  make([]byte, 32),
	}
	rand.Read(sess.id[:])
	rand.Read(sess.secret)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return nil, protocol.ErrReasonBusy
	}
	if s.perInvite[invite] >= lim.MaxSessions {
		return nil, protocol.ErrReasonQuota
	}
	if len(s.sessions) >= s.maxSessions {
		return nil, protocol.ErrReasonBusy
	}
	if g.StampRate > 0 {
		sess.stamp = newStampState(s.freeSSID(), protocol.StampKey(sess.secret), g.StampRate, now)
		s.bySSID[sess.stamp.ssid] = sess
	}
	if want.LoadBytes > 0 && lim.MaxLoadBytes > 0 && s.maxLoadSessions > 0 {
		s.grantLoad(sess, lim, want, ip, now)
	}
	s.sessions[sess.id] = sess
	s.perInvite[invite]++
	return sess, ""
}

// grantLoad charges the grant to the invite's day at once, so sessions in
// parallel cannot overrun it; end returns what was not used. Callers hold s.mu.
func (s *store) grantLoad(sess *session, lim InviteLimits, want protocol.Limits, ip netip.Addr, now time.Time) {
	day := now.UTC().Truncate(24 * time.Hour)
	d := s.daily[sess.invite]
	if d == nil || !d.day.Equal(day) {
		d = &dayUse{day: day}
		s.daily[sess.invite] = d
	}
	switch left := lim.MaxLoadBytesPerDay - d.used; {
	case s.loadSessions >= s.maxLoadSessions:
		sess.loadAns = &protocol.LoadGrant{Refused: protocol.ErrReasonBusy, RetryAfterS: loadRetryS}
	case left <= 0:
		retry := int(day.Add(24*time.Hour).Sub(now).Seconds()) + 1
		sess.loadAns = &protocol.LoadGrant{Refused: protocol.ErrReasonQuota, RetryAfterS: retry}
	default:
		bytes := min(want.LoadBytes, lim.MaxLoadBytes, left)
		conns := min(want.LoadConnections, lim.MaxLoadConnections)
		d.used += bytes
		s.loadSessions++
		sess.load = newLoadState(ip, bytes, conns, day, now)
		sess.loadAns = &protocol.LoadGrant{Bytes: bytes, Connections: conns}
	}
}

// loadAllowance is how many more connections, beyond the per-address limits, may
// come from ip: the load connections of its live sessions.
func (s *store) loadAllowance(ip netip.Addr) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, sess := range s.sessions {
		if sess.load != nil && sess.load.ip == ip {
			n += sess.load.conns
		}
	}
	return n
}

// loadSession returns the live session id with a load grant that the invite with
// token hash invite created, or nil.
func (s *store) loadSession(id [16]byte, invite string, now time.Time) *session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil || sess.load == nil || sess.invite != invite || !now.Before(sess.expires) {
		return nil
	}
	return sess
}

// admit decides one data-plane HELLO. On success the connection counts as live
// until release; a rejected connection is never counted, not even briefly.
func (s *store) admit(h protocol.Hello, c *dataConn, now time.Time) protocol.Reason {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[h.SessionID]
	switch {
	case s.closing:
		return protocol.ReasonShuttingDown
	case sess == nil || !now.Before(sess.expires) || !h.Verify(sess.secret):
		return protocol.ReasonAuth
	case s.liveConns >= s.maxConns:
		return protocol.ReasonBusy
	case len(sess.conns) >= sess.granted.Connections || !sess.dials.allow(now):
		return protocol.ReasonQuota
	}
	sess.conns[c] = struct{}{}
	s.liveConns++
	return 0
}

func (s *store) release(id [16]byte, c *dataConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[id]; sess != nil {
		if _, ok := sess.conns[c]; ok {
			delete(sess.conns, c)
			s.liveConns--
		}
	}
}

func (s *store) idleTimeout(id [16]byte) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sessions[id]; sess != nil {
		return time.Duration(sess.granted.IdleTimeoutS) * time.Second
	}
	return 0
}

// end removes sessions matching pick and returns their live connections, so the
// caller can tell them why they are closing without holding the lock on writes.
func (s *store) end(pick func(*session) bool) []*dataConn {
	s.mu.Lock()
	defer s.mu.Unlock()
	var conns []*dataConn
	for id, sess := range s.sessions {
		if !pick(sess) {
			continue
		}
		for c := range sess.conns {
			conns = append(conns, c)
		}
		s.liveConns -= len(sess.conns)
		if sess.stamp != nil {
			delete(s.bySSID, sess.stamp.ssid)
		}
		if sess.load != nil {
			sess.load.finish()
			s.loadSessions--
			if d := s.daily[sess.invite]; d != nil && d.day.Equal(sess.load.day) {
				d.used -= sess.load.unused()
			}
		}
		s.perInvite[sess.invite]--
		if s.perInvite[sess.invite] == 0 {
			delete(s.perInvite, sess.invite)
		}
		delete(s.sessions, id)
	}
	return conns
}

// close ends every session and refuses new ones and new connections from then on,
// so a HELLO racing the shutdown hears shutting_down instead of auth.
func (s *store) close() []*dataConn {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	return s.end(func(*session) bool { return true })
}

// closeAll sends CLOSE(reason) to each connection and closes it.
func closeAll(conns []*dataConn, reason protocol.Reason) {
	for _, c := range conns {
		c.send(protocol.Frame{Type: protocol.TypeClose, Reason: reason})
		c.conn.Close()
	}
}
