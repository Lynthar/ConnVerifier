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

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+protocol.PathSessions, s.createSession)
	mux.HandleFunc("DELETE "+protocol.PathSessions+"/{id}", s.endSession)
	return s.requests.wrap(mux)
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
	if req.Check != "tcp-capacity" {
		writeError(w, http.StatusBadRequest, protocol.ErrReasonBadRequest, 0, "unsupported check")
		return
	}
	sess, reason := s.store.create(inv.TokenSHA256, inv.Limits, req.Want, time.Now())
	switch reason {
	case protocol.ErrReasonQuota:
		writeError(w, http.StatusTooManyRequests, reason, 60, "this invite already has its maximum number of sessions")
		return
	case protocol.ErrReasonBusy:
		writeError(w, http.StatusServiceUnavailable, reason, 60, "the node has its maximum number of sessions")
		return
	}
	observed := ""
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		observed = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port()).String()
	}
	if s.cfg.logConnections {
		log.Printf("session for %q from %s granted %+v", inv.Label, observed, sess.granted)
	}
	writeJSON(w, http.StatusCreated, protocol.SessionResponse{
		SessionID:    protocol.EncodeID(sess.id[:]),
		Secret:       protocol.EncodeID(sess.secret),
		ExpiresAt:    sess.expires.UTC(),
		ObservedAddr: observed,
		Granted:      sess.granted,
		Node:         s.nodeInfo(),
	})
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
	found := false
	conns := s.store.end(func(sess *session) bool {
		match := sess.id == id && sess.invite == inv.TokenSHA256
		found = found || match
		return match
	})
	if !found {
		writeError(w, http.StatusNotFound, protocol.ErrReasonBadRequest, 0, "no such session")
		return
	}
	closeAll(conns, protocol.ReasonSessionEnded)
	writeJSON(w, http.StatusOK, protocol.SessionEnd{Node: s.nodeInfo()})
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
		var a netip.Addr
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			a = ap.Addr().Unmap()
		}
		if !l.allow(a, time.Now()) {
			writeError(w, http.StatusTooManyRequests, protocol.ErrReasonQuota, 1, "too many requests")
			return
		}
		h.ServeHTTP(w, r)
	})
}
