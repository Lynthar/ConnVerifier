package node

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/memnet"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

func loadWant(bytes int64, conns int) protocol.Limits {
	w := want(1)
	w.LoadBytes, w.LoadConnections = bytes, conns
	return w
}

var clientIP = netip.MustParseAddr("192.0.2.1")

func TestLoadGrantsOneSessionAtATime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	st := newStore(100, 8, 1, now)
	first, _ := st.create("inv", DefaultInviteLimits, loadWant(10<<30, 100), clientIP, now)
	if g := first.loadAns; g == nil || g.Bytes != DefaultInviteLimits.MaxLoadBytes || g.Connections != DefaultInviteLimits.MaxLoadConnections {
		t.Fatalf("first grant %+v, want the invite's caps", g)
	}
	second, reason := st.create("inv", DefaultInviteLimits, loadWant(1<<20, 4), clientIP, now)
	if reason != "" || second.load != nil || second.loadAns.Refused != protocol.ErrReasonBusy || second.loadAns.RetryAfterS != loadRetryS {
		t.Fatalf("second session: %q %+v; want the session, with load refused busy", reason, second.loadAns)
	}
	st.end(func(s *session) bool { return s == first })
	third, _ := st.create("inv", DefaultInviteLimits, loadWant(1<<20, 4), clientIP, now)
	if third.load == nil {
		t.Fatalf("after the first ended: %+v, want a grant", third.loadAns)
	}
}

func TestLoadWithoutOfferOrAsking(t *testing.T) {
	now := time.Now()
	noLoad := DefaultInviteLimits
	noLoad.MaxLoadBytes = 0
	for name, tt := range map[string]struct {
		lim     InviteLimits
		maxLoad int
		want    protocol.Limits
	}{
		"invite without load":      {noLoad, 1, loadWant(1<<20, 4)},
		"node offers no load":      {DefaultInviteLimits, 0, loadWant(1<<20, 4)},
		"session asks for no load": {DefaultInviteLimits, 1, want(1)},
	} {
		st := newStore(100, 8, tt.maxLoad, now)
		sess, _ := st.create("inv", tt.lim, tt.want, clientIP, now)
		if sess.load != nil || sess.loadAns != nil {
			t.Errorf("%s: load %+v", name, sess.loadAns)
		}
	}
}

// A grant is charged to the day when made and what went unused is returned at
// the end; when the day's budget is gone the refusal points at UTC midnight.
func TestLoadDailyQuota(t *testing.T) {
	now := time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	lim := DefaultInviteLimits
	lim.MaxSessions, lim.MaxLoadBytes, lim.MaxLoadBytesPerDay = 8, 60, 100
	st := newStore(100, 8, 4, now)

	a, _ := st.create("inv", lim, loadWant(1000, 4), clientIP, now)
	a.load.sent.Add(10)
	st.end(func(s *session) bool { return s == a }) // 50 of 60 unused: back to the day
	b, _ := st.create("inv", lim, loadWant(1000, 4), clientIP, now)
	c, _ := st.create("inv", lim, loadWant(1000, 4), clientIP, now)
	if b.loadAns.Bytes != 60 || c.loadAns.Bytes != 30 {
		t.Fatalf("grants %d and %d after 10 used, want 60 and 30", b.loadAns.Bytes, c.loadAns.Bytes)
	}
	d, _ := st.create("inv", lim, loadWant(1000, 4), clientIP, now)
	if d.loadAns.Refused != protocol.ErrReasonQuota || d.loadAns.RetryAfterS != 3601 {
		t.Fatalf("over the day: %+v, want quota with retry at midnight", d.loadAns)
	}
	other, _ := st.create("other-inv", lim, loadWant(1000, 4), clientIP, now)
	if other.load == nil {
		t.Fatalf("another invite's day is its own: %+v", other.loadAns)
	}
	st.end(func(s *session) bool { return true })
	next, _ := st.create("inv", lim, loadWant(1000, 4), clientIP, now.Add(2*time.Hour))
	if next.loadAns.Bytes != 60 {
		t.Fatalf("next day: %+v, want a fresh 60", next.loadAns)
	}
}

func TestLoadAllowancePerAddress(t *testing.T) {
	now := time.Now()
	st := newStore(100, 8, 2, now)
	st.create("inv", DefaultInviteLimits, loadWant(1<<20, 20), clientIP, now)
	if a, b := st.loadAllowance(clientIP), st.loadAllowance(netip.MustParseAddr("192.0.2.2")); a != 20 || b != 0 {
		t.Fatalf("allowance %d for the session's address and %d for another; want 20 and 0", a, b)
	}
	p := newPending(1, 10)
	if !p.acquire(clientIP, 0) || p.acquire(clientIP, 0) || !p.acquire(clientIP, 1) {
		t.Fatal("an allowance of 1 should admit exactly one more connection")
	}
}

func loadServer(t *testing.T, budget int64) (*server, string, protocol.SessionResponse) {
	t.Helper()
	s, _ := testServer(t, 10)
	lim := DefaultInviteLimits
	lim.MaxLoadBytes = budget
	inv, err := CreateInvite(s.cfg.stateDir, "load", []string{"192.0.2.10:7443"}, 0, lim)
	if err != nil {
		t.Fatal(err)
	}
	token := protocol.EncodeID(inv.Token[:])
	body := fmt.Sprintf(`{"client":{"name":"t","version":"v"},"check":"load",`+
		`"want":{"connections":1,"dial_rate":1,"duration_s":60,"idle_timeout_s":60,"load_bytes":%d,"load_connections":8}}`, 10*budget)
	w := apiRequest(t, s, "POST", protocol.PathSessions, token, body)
	var resp protocol.SessionResponse
	if w.Code != http.StatusCreated || protocol.DecodeJSON(w.Body, &resp) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if resp.Load == nil || resp.Load.Bytes != budget || resp.Load.Connections != 8 {
		t.Fatalf("load grant %+v, want %d bytes and 8 connections", resp.Load, budget)
	}
	return s, token, resp
}

// lastRecord is the final read-progress record of an upload's response.
func lastRecord(t *testing.T, body []byte) uint64 {
	t.Helper()
	if len(body) == 0 || len(body)%8 != 0 {
		t.Fatalf("upload response of %d bytes is not a run of 8-byte records", len(body))
	}
	return binary.BigEndian.Uint64(body[len(body)-8:])
}

// The large object ends normally where the budget does, so the client can tell
// a spent budget from a failure; an upload reports what it read, and past the
// budget it reads nothing more.
func TestLoadEndpointsSpendTheBudget(t *testing.T) {
	const budget = 300 << 10
	s, token, resp := loadServer(t, budget)
	path := func(endpoint string) string { return protocol.LoadPath(resp.SessionID, endpoint) }

	if w := apiRequest(t, s, "GET", path(protocol.LoadSmall), token, ""); w.Code != 200 || w.Body.Len() != 1 {
		t.Fatalf("small = %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := apiRequest(t, s, "POST", path(protocol.LoadUpload), token, strings.Repeat("x", 100<<10)); w.Code != 200 || lastRecord(t, w.Body.Bytes()) != 100<<10 {
		t.Fatalf("upload within the budget = %d %v", w.Code, w.Body.Bytes())
	}
	w := apiRequest(t, s, "GET", path(protocol.LoadLarge), token, "")
	if w.Code != 200 || w.Body.Len() != budget-1-100<<10 {
		t.Fatalf("large = %d, %d bytes; want the rest of the budget, %d", w.Code, w.Body.Len(), budget-1-100<<10)
	}
	if w := apiRequest(t, s, "POST", path(protocol.LoadUpload), token, "more"); w.Code != 200 || lastRecord(t, w.Body.Bytes()) != 0 {
		t.Fatalf("upload past the budget = %d %v, want nothing read", w.Code, w.Body.Bytes())
	}

	time.Sleep(5 * lagPeriod) // so the node's lag timer has woken
	w = apiRequest(t, s, "DELETE", protocol.PathSessions+"/"+resp.SessionID, token, "")
	var end protocol.SessionEnd
	if w.Code != 200 || protocol.DecodeJSON(w.Body, &end) != nil || end.Load == nil {
		t.Fatalf("end = %d %s", w.Code, w.Body)
	}
	if end.Load.Sent != budget-100<<10 || end.Load.Received != 100<<10 || end.Load.LagTicks == 0 {
		t.Fatalf("end counts %+v", end.Load)
	}
}

func TestLoadEndpointsNeedTheSessionsInvite(t *testing.T) {
	s, token, resp := loadServer(t, 1<<20)
	other, _ := CreateInvite(s.cfg.stateDir, "other", []string{"192.0.2.10:7443"}, 0, DefaultInviteLimits)
	for name, tt := range map[string]struct{ path, token string }{
		"other invite":    {protocol.LoadPath(resp.SessionID, protocol.LoadSmall), protocol.EncodeID(other.Token[:])},
		"no token":        {protocol.LoadPath(resp.SessionID, protocol.LoadSmall), ""},
		"unknown session": {protocol.LoadPath(protocol.EncodeID(make([]byte, 16)), protocol.LoadSmall), token},
	} {
		if w := apiRequest(t, s, "GET", tt.path, tt.token, ""); w.Code != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", name, w.Code)
		}
	}
	apiRequest(t, s, "DELETE", protocol.PathSessions+"/"+resp.SessionID, token, "")
	if w := apiRequest(t, s, "GET", protocol.LoadPath(resp.SessionID, protocol.LoadLarge), token, ""); w.Code != http.StatusNotFound {
		t.Fatalf("after the session ended: %d, want 404", w.Code)
	}
}

// slowBody trickles a kilobyte every 100 ms until its time is up.
type slowBody struct{ until time.Time }

func (b *slowBody) Read(p []byte) (int, error) {
	if time.Now().After(b.until) {
		return 0, io.EOF
	}
	time.Sleep(100 * time.Millisecond)
	return copy(p, make([]byte, 1024)), nil
}

// Load streams outlive the server's 10 s per-request timeouts. On HTTP/2 Go
// resets a stream whose write timeout fires even if the handler wrote nothing
// yet, so an upload was cut at 10 s.
func TestLoadStreamsOutliveRequestTimeouts(t *testing.T) {
	if testing.Short() {
		t.Skip("runs past the server's timeouts")
	}
	s, token, resp := loadServer(t, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := memnet.NewListener()
	go s.serve(ctx, ln, nil)
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return ln.Dial(ctx, "", "") },
		TLSClientConfig:   protocol.ClientTLS(protocol.Pin(s.tls.Certificates[0].Leaf)),
		ForceAttemptHTTP2: true,
	}
	defer tr.CloseIdleConnections()
	request := func(method, endpoint string, body io.Reader) (*http.Response, error) {
		r, _ := http.NewRequestWithContext(ctx, method, "https://node"+protocol.LoadPath(resp.SessionID, endpoint), body)
		r.Header.Set("Authorization", "Bearer "+token)
		return tr.RoundTrip(r)
	}
	until := time.Now().Add(handshakeTimeout + time.Second)
	errs := make(chan error, 2)
	go func() {
		r, err := request(http.MethodPost, protocol.LoadUpload, &slowBody{until: until})
		if err != nil {
			errs <- err
			return
		}
		defer r.Body.Close()
		body, err := io.ReadAll(r.Body) // the progress records, until the upload ends
		if err == nil && (r.StatusCode != http.StatusOK || time.Now().Before(until)) {
			err = fmt.Errorf("upload answered %d and ended early (%d bytes of records)", r.StatusCode, len(body))
		}
		errs <- err
	}()
	go func() {
		r, err := request(http.MethodGet, protocol.LoadLarge, nil)
		if err != nil {
			errs <- err
			return
		}
		defer r.Body.Close()
		buf := make([]byte, 1024)
		for time.Now().Before(until) && err == nil {
			_, err = r.Body.Read(buf)
			time.Sleep(10 * time.Millisecond)
		}
		errs <- err
	}()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("a load stream was cut: %v", err)
		}
	}
}
