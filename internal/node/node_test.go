package node

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// stateDir is a fresh directory the node creates owner-only; t.TempDir itself is
// group-readable, which the node rightly refuses.
func stateDir(t *testing.T) string {
	return filepath.Join(t.TempDir(), "node")
}

func testServer(t *testing.T, maxConns int) (*server, protocol.Invite) {
	t.Helper()
	dir := stateDir(t)
	inv, err := CreateInvite(dir, "test", []string{"192.0.2.10:7443"}, DefaultInviteLimits)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{listen: ":0", stateDir: dir, maxConns: maxConns, maxSessions: 4}
	return newServer(cfg, "vtest", cert), inv
}

func want(conns int) protocol.Limits {
	return protocol.Limits{Connections: conns, DialRate: 1000, IdleTimeoutS: 60}
}

func TestValidateConfig(t *testing.T) {
	ok := Config{listen: ":7443", stateDir: "/tmp/x", maxConns: 10, maxSessions: 1}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"empty listen":      func(c *Config) { c.listen = "" },
		"empty state dir":   func(c *Config) { c.stateDir = "" },
		"zero max conns":    func(c *Config) { c.maxConns = 0 },
		"huge max conns":    func(c *Config) { c.maxConns = protocol.MaxConnections + 1 },
		"zero max sessions": func(c *Config) { c.maxSessions = 0 },
	} {
		c := ok
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestInviteLifecycle(t *testing.T) {
	dir := stateDir(t)
	inv, err := CreateInvite(dir, "alice", []string{"192.0.2.10:7443"}, DefaultInviteLimits)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := protocol.ParseInvite(inv.Encode())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := LoadIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Pin != protocol.Pin(cert.Leaf) {
		t.Fatal("invite pin does not match the node key")
	}
	if _, err := CreateInvite(dir, "alice", []string{"192.0.2.10:7443"}, DefaultInviteLimits); err == nil {
		t.Fatal("duplicate label accepted")
	}
	data, err := os.ReadFile(filepath.Join(dir, invitesFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), protocol.EncodeID(inv.Token[:])) {
		t.Fatal("invites file stores the token itself")
	}
	if err := RevokeInvite(dir, "alice"); err != nil {
		t.Fatal(err)
	}
	if list, _ := ListInvites(dir); len(list) != 0 {
		t.Fatalf("revoked invite still listed: %+v", list)
	}
	if err := RevokeInvite(dir, "alice"); err == nil {
		t.Fatal("revoking a missing invite succeeded")
	}
}

func TestStateRefusesFilesOthersCanRead(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no permission bits on Windows")
	}
	dir := stateDir(t)
	if _, err := CreateInvite(dir, "a", []string{"192.0.2.10:7443"}, DefaultInviteLimits); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, invitesFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadInvites(dir); err == nil {
		t.Fatal("world-readable invites file accepted")
	}
	if err := os.Chmod(filepath.Join(dir, keyFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(dir); err == nil {
		t.Fatal("world-readable key accepted")
	}
}

func TestStoreGrantsAndSessionLimits(t *testing.T) {
	now := time.Now()
	st := newStore(100, 3, now)
	lim := DefaultInviteLimits
	lim.MaxSessions, lim.MaxConnections = 2, 50
	sess, reason := st.create("inv-a", lim, protocol.Limits{Connections: 80, DialRate: 5, IdleTimeoutS: 7200}, now)
	if reason != "" {
		t.Fatalf("create = %q", reason)
	}
	g := sess.granted
	if g.Connections != 50 || g.DialRate != 5 || g.IdleTimeoutS != lim.MaxIdleTimeoutS || g.DurationS != lim.MaxDurationS {
		t.Fatalf("granted %+v", g)
	}
	if _, reason := st.create("inv-a", lim, want(1), now); reason != "" {
		t.Fatalf("second session = %q", reason)
	}
	if _, reason := st.create("inv-a", lim, want(1), now); reason != protocol.ErrReasonQuota {
		t.Fatalf("third session for one invite = %q, want quota", reason)
	}
	if _, reason := st.create("inv-b", lim, want(1), now); reason != "" {
		t.Fatalf("other invite = %q", reason)
	}
	if _, reason := st.create("inv-c", lim, want(1), now); reason != protocol.ErrReasonBusy {
		t.Fatalf("over node session limit = %q, want busy", reason)
	}
}

func TestAdmit(t *testing.T) {
	now := time.Now()
	st := newStore(2, 4, now)
	sess, _ := st.create("inv", DefaultInviteLimits, want(1), now)
	hello := func(secret []byte) protocol.Hello {
		return protocol.NewHello(secret, sess.id, [16]byte{byte(time.Now().UnixNano())})
	}
	if r := st.admit(hello(make([]byte, 32)), &dataConn{}, now); r != protocol.ReasonAuth {
		t.Fatalf("wrong secret = %v, want auth", r)
	}
	c1 := &dataConn{}
	if r := st.admit(hello(sess.secret), c1, now); r != 0 {
		t.Fatalf("first connection = %v", r)
	}
	if r := st.admit(hello(sess.secret), &dataConn{}, now); r != protocol.ReasonQuota {
		t.Fatalf("over session connections = %v, want quota", r)
	}
	if l := st.load(); l.Connections != 1 {
		t.Fatalf("rejected connection counted: %d live", l.Connections)
	}
	other, _ := st.create("inv2", DefaultInviteLimits, want(5), now)
	st.admit(protocol.NewHello(other.secret, other.id, [16]byte{1}), &dataConn{}, now)
	if r := st.admit(protocol.NewHello(other.secret, other.id, [16]byte{2}), &dataConn{}, now); r != protocol.ReasonBusy {
		t.Fatalf("over node connections = %v, want busy", r)
	}
	st.release(sess.id, c1)
	if l := st.load(); l.Connections != 1 {
		t.Fatalf("after release %d live, want 1", l.Connections)
	}
	if r := st.admit(hello(sess.secret), &dataConn{}, sess.expires); r != protocol.ReasonAuth {
		t.Fatalf("expired session = %v, want auth", r)
	}
}

// dataPipe opens a data-plane connection to s in memory and sends a HELLO.
func dataPipe(t *testing.T, s *server, sess *session) (net.Conn, chan struct{}) {
	t.Helper()
	client, srv := net.Pipe()
	done := make(chan struct{})
	go func() { s.serveData(srv, func() {}); close(done) }()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := client.Write(protocol.NewHello(sess.secret, sess.id, [16]byte{9}).MarshalBinary()); err != nil {
		t.Fatal(err)
	}
	return client, done
}

func readFrame(t *testing.T, c net.Conn, want protocol.FrameType) protocol.Frame {
	t.Helper()
	f, err := protocol.ReadFrame(c)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if f.Type != want {
		t.Fatalf("frame %+v, want type %#x", f, want)
	}
	return f
}

func TestDataPlaneEchoAndSessionEnd(t *testing.T) {
	s, inv := testServer(t, 10)
	sess, _ := s.store.create(tokenHash(inv.Token[:]), DefaultInviteLimits, want(5), time.Now())
	client, done := dataPipe(t, s, sess)
	defer client.Close()

	if f := readFrame(t, client, protocol.TypeAccept); f.IdleTimeoutMs != 60000 {
		t.Fatalf("idle timeout %d ms, want 60000", f.IdleTimeoutMs)
	}
	protocol.WriteFrame(client, protocol.Frame{Type: protocol.TypePing, Seq: 77})
	if f := readFrame(t, client, protocol.TypePong); f.Seq != 77 {
		t.Fatalf("pong seq %d, want 77", f.Seq)
	}

	go closeAll(s.store.end(func(*session) bool { return true }), protocol.ReasonSessionEnded)
	if f := readFrame(t, client, protocol.TypeClose); f.Reason != protocol.ReasonSessionEnded {
		t.Fatalf("close reason %v, want session_ended", f.Reason)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("data-plane handler did not return")
	}
	if l := s.store.load(); l.Connections != 0 || l.Sessions != 0 {
		t.Fatalf("load after end = %+v", l)
	}
}

func TestDataPlaneIdleTimeoutSaysWhy(t *testing.T) {
	s, inv := testServer(t, 10)
	w := want(5)
	w.IdleTimeoutS = 1
	sess, _ := s.store.create(tokenHash(inv.Token[:]), DefaultInviteLimits, w, time.Now())
	client, _ := dataPipe(t, s, sess)
	defer client.Close()
	readFrame(t, client, protocol.TypeAccept)
	if f := readFrame(t, client, protocol.TypeClose); f.Reason != protocol.ReasonIdleTimeout {
		t.Fatalf("close reason %v, want idle_timeout", f.Reason)
	}
}

func TestDataPlaneRejectsWithReason(t *testing.T) {
	s, inv := testServer(t, 10)
	sess, _ := s.store.create(tokenHash(inv.Token[:]), DefaultInviteLimits, want(1), time.Now())
	first, _ := dataPipe(t, s, sess)
	defer first.Close()
	readFrame(t, first, protocol.TypeAccept)
	second, done := dataPipe(t, s, sess)
	defer second.Close()
	if f := readFrame(t, second, protocol.TypeReject); f.Reason != protocol.ReasonQuota {
		t.Fatalf("reject reason %v, want quota", f.Reason)
	}
	<-done
	if _, err := second.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("after reject read = %v, want EOF", err)
	}
}

// chanListener hands out connections pushed into conns, so the accept loop can be
// driven without a socket. Every net.Pipe end has the same (unparseable) address.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeRoutesByFirstByte(t *testing.T) {
	s, inv := testServer(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	ln := newChanListener()
	stopped := make(chan struct{})
	go func() { s.serve(ctx, ln); close(stopped) }()
	dial := func() net.Conn {
		c, srv := net.Pipe()
		ln.conns <- srv
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return c
	}

	junk := dial()
	junk.Write([]byte("X"))
	if n, err := junk.Read(make([]byte, 16)); err != io.EOF || n != 0 {
		t.Fatalf("unknown first byte got %d bytes, %v; want an immediate close", n, err)
	}

	tc := tls.Client(dial(), protocol.ClientTLS(inv.Pin))
	if err := tc.Handshake(); err != nil {
		t.Fatalf("TLS through the shared port: %v", err)
	}
	tc.Close()

	sess, _ := s.store.create(tokenHash(inv.Token[:]), DefaultInviteLimits, want(1), time.Now())
	data := dial()
	data.Write(protocol.NewHello(sess.secret, sess.id, [16]byte{3}).MarshalBinary())
	readFrame(t, data, protocol.TypeAccept)

	cancel()
	if f := readFrame(t, data, protocol.TypeClose); f.Reason != protocol.ReasonShuttingDown {
		t.Fatalf("on shutdown close reason %v, want shutting_down", f.Reason)
	}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after cancel")
	}
}

func TestServeCapsUnprovenConnectionsPerAddress(t *testing.T) {
	s, _ := testServer(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newChanListener()
	go s.serve(ctx, ln)

	var held []net.Conn
	for range maxPendingPerIP {
		c, srv := net.Pipe()
		ln.conns <- srv
		held = append(held, c)
	}
	over, srv := net.Pipe()
	ln.conns <- srv
	over.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := over.Read(make([]byte, 1)); err != io.EOF || n != 0 {
		t.Fatalf("connection over the per-address limit read %d, %v; want closed", n, err)
	}
	for _, c := range held {
		c.Close()
	}
}

func apiRequest(t *testing.T, s *server, method, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

const sessionBody = `{"client":{"name":"t","version":"v"},"check":"tcp-capacity",` +
	`"want":{"connections":10,"dial_rate":10,"duration_s":60,"idle_timeout_s":60}}`

func TestCreateAndEndSession(t *testing.T) {
	s, inv := testServer(t, 10)
	token := protocol.EncodeID(inv.Token[:])

	w := apiRequest(t, s, "POST", protocol.PathSessions, token, sessionBody)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var resp protocol.SessionResponse
	if err := protocol.DecodeJSON(w.Body, &resp); err != nil {
		t.Fatalf("response does not validate: %v", err)
	}
	if resp.ObservedAddr != "192.0.2.1:1234" || resp.Granted.Connections != 10 {
		t.Fatalf("response %+v", resp)
	}

	other, err := CreateInvite(s.cfg.stateDir, "other", []string{"192.0.2.10:7443"}, DefaultInviteLimits)
	if err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(t, s, "DELETE", protocol.PathSessions+"/"+resp.SessionID, protocol.EncodeID(other.Token[:]), ""); w.Code != http.StatusNotFound {
		t.Fatalf("ending another invite's session = %d, want 404", w.Code)
	}
	w = apiRequest(t, s, "DELETE", protocol.PathSessions+"/"+resp.SessionID, token, "")
	var end protocol.SessionEnd
	if w.Code != http.StatusOK || protocol.DecodeJSON(w.Body, &end) != nil {
		t.Fatalf("end = %d %s", w.Code, w.Body)
	}
	if end.Node.Load.Sessions != 0 {
		t.Fatalf("load after end %+v", end.Node.Load)
	}
}

func TestCreateSessionRefusals(t *testing.T) {
	s, inv := testServer(t, 10)
	token := protocol.EncodeID(inv.Token[:])
	tests := []struct {
		name, token, body string
		code              int
		reason            string
	}{
		{"no token", "", sessionBody, 401, protocol.ErrReasonAuth},
		{"unknown token", protocol.EncodeID(make([]byte, 32)), sessionBody, 401, protocol.ErrReasonAuth},
		{"bad body", token, "{", 400, protocol.ErrReasonBadRequest},
		{"unsupported check", token, strings.Replace(sessionBody, "tcp-capacity", "dns", 1), 400, protocol.ErrReasonBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := apiRequest(t, s, "POST", protocol.PathSessions, tt.token, tt.body)
			var e protocol.ErrorResponse
			if w.Code != tt.code || protocol.DecodeJSON(w.Body, &e) != nil || e.Reason != tt.reason {
				t.Fatalf("= %d %s, want %d %s", w.Code, w.Body, tt.code, tt.reason)
			}
		})
	}

	if err := RevokeInvite(s.cfg.stateDir, "test"); err != nil {
		t.Fatal(err)
	}
	if w := apiRequest(t, s, "POST", protocol.PathSessions, token, sessionBody); w.Code != 401 {
		t.Fatalf("revoked invite = %d, want 401", w.Code)
	}
}

func TestRequestRateLimit(t *testing.T) {
	s, _ := testServer(t, 10)
	codes := map[int]int{}
	for range requestRate + 5 {
		codes[apiRequest(t, s, "POST", protocol.PathSessions, "", sessionBody).Code]++
	}
	if codes[http.StatusTooManyRequests] == 0 {
		t.Fatalf("no request was limited: %v", codes)
	}
}
