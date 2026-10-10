package node

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"log"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// routes serves the control plane behind the per-address request limit, and the
// load endpoints without it: a load check sends tens of requests a second.
func (s *server) routes() http.Handler {
	control := http.NewServeMux()
	control.HandleFunc("POST "+protocol.PathSessions, s.createSession)
	control.HandleFunc("DELETE "+protocol.PathSessions+"/{id}", s.endSession)
	limited := s.requests.wrap(control)
	load := s.loadRoutes()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := load.Handler(r); pattern != "" {
			load.ServeHTTP(w, r)
			return
		}
		limited.ServeHTTP(w, r)
	})
}

// authenticate finds the invite whose token the request bears. The invite list
// is reread on every call, so a revocation takes effect for the next session.
func (s *server) authenticate(r *http.Request) (InviteRecord, bool) {
	b64, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return InviteRecord{}, false
	}
	token, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil || len(token) != 32 {
		return InviteRecord{}, false
	}
	invites, err := loadInvites(s.cfg.stateDir)
	if err != nil {
		log.Printf("read invites: %v", err)
		return InviteRecord{}, false
	}
	h := tokenHash(token)
	for _, inv := range invites {
		if subtle.ConstantTimeCompare([]byte(inv.TokenSHA256), []byte(h)) == 1 {
			return inv, true
		}
	}
	return InviteRecord{}, false
}

func (s *server) createSession(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, protocol.ErrReasonAuth, 0, "unknown or revoked invite")
		return
	}
	var req protocol.SessionRequest
	if err := protocol.DecodeJSON(r.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, protocol.ErrReasonBadRequest, 0, err.Error())
		return
	}
	switch {
	case (req.Check == protocol.CheckBaseline || req.Check == protocol.CheckLoad) && s.udp:
	case req.Check == protocol.CheckBaseline, req.Check == protocol.CheckLoad, req.Check == protocol.CheckTCPCapacity:
		req.Want.StampRate = 0 // no reflector here, or a check that sends no STAMP
	default:
		writeError(w, http.StatusBadRequest, protocol.ErrReasonBadRequest, 0, "unsupported check")
		return
	}
	if req.Check != protocol.CheckLoad {
		req.Want.LoadBytes, req.Want.LoadConnections = 0, 0
	}
	quic := req.Check == protocol.CheckLoad && req.Transport == protocol.TransportQUIC && s.quicPort > 0
	sess, reason := s.store.create(inv.TokenSHA256, inv.Limits, req.Want, requestIP(r), quic, time.Now())
	switch reason {
	case protocol.ErrReasonQuota:
		writeError(w, http.StatusTooManyRequests, reason, 60, "this invite already has its maximum number of sessions")
		return
	case protocol.ErrReasonBusy:
		writeError(w, http.StatusServiceUnavailable, reason, 60, "the node has no room for another session")
		return
	}
	observed := ""
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		observed = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
	}
	if s.cfg.logConnections {
		log.Printf("session for %q from %s granted %+v", inv.Label, observed, sess.granted)
	}
	resp := protocol.SessionResponse{
		SessionID:    protocol.EncodeID(sess.id[:]),
		Secret:       protocol.EncodeID(sess.secret),
		ExpiresAt:    sess.expires.UTC(),
		ObservedAddr: observed,
		Granted:      sess.granted,
		Node:         s.nodeInfo(),
	}
	if sess.stamp != nil {
		resp.Stamp = &protocol.StampGrant{SSID: sess.stamp.ssid}
	}
	resp.Load = sess.loadAns
	if a := sess.loadAns; a != nil && a.Refused == "" {
		g := *a
		if sess.load.quic {
			g.QUIC = &protocol.QUICGrant{Port: s.quicPort}
		}
		if len(s.tcpCC) <= 64 {
			g.TCPCongestion = s.tcpCC
		}
		resp.Load = &g
	}
	writeJSON(w, http.StatusCreated, resp)
}

func (s *server) endSession(w http.ResponseWriter, r *http.Request) {
	inv, ok := s.authenticate(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, protocol.ErrReasonAuth, 0, "unknown or revoked invite")
		return
	}
	id, err := protocol.DecodeID(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, protocol.ErrReasonBadRequest, 0, "bad session id")
		return
	}
	var ended *session
	conns := s.store.end(func(sess *session) bool {
		match := sess.id == id && sess.invite == inv.TokenSHA256
		if match {
			ended = sess
		}
		return match
	})
	if ended == nil {
		writeError(w, http.StatusNotFound, protocol.ErrReasonBadRequest, 0, "no such session")
		return
	}
	closeAll(conns, protocol.ReasonSessionEnded)
	resp := protocol.SessionEnd{Node: s.nodeInfo()}
	if ended.stamp != nil {
		resp.Stamp = ended.stamp.counts()
	}
	if ended.load != nil {
		counts := ended.load.finish() // end already finished it; this reads the counts
		resp.Load = &counts
	}
	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, reason string, retryAfterS int, msg string) {
	if len(msg) > 200 {
		msg = msg[:200]
	}
	writeJSON(w, status, protocol.ErrorResponse{Reason: reason, RetryAfterS: retryAfterS, Message: msg})
}

// requestLimiter caps control-plane requests per source address. Entries idle
// for a minute are dropped so the map cannot grow without bound.
type requestLimiter struct {
	mu      sync.Mutex
	rate    float64
	buckets map[netip.Addr]*bucket
}

func newRequestLimiter(rate float64) *requestLimiter {
	return &requestLimiter{rate: rate, buckets: make(map[netip.Addr]*bucket)}
}

func (l *requestLimiter) allow(a netip.Addr, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buckets) > 4096 {
		for k, b := range l.buckets {
			if now.Sub(b.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[a]
	if b == nil {
		nb := newBucket(l.rate, l.rate, now)
		b = &nb
		l.buckets[a] = b
	}
	return b.allow(now)
}

func (l *requestLimiter) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(requestIP(r), time.Now()) {
			writeError(w, http.StatusTooManyRequests, protocol.ErrReasonQuota, 1, "too many requests")
			return
		}
		h.ServeHTTP(w, r)
	})
}
