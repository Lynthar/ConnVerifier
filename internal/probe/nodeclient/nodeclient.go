// Package nodeclient opens and ends sessions on a node over its HTTPS control
// plane, for every check that runs against one.
package nodeclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// InviteEnv names the variable read when -node is not given; an invite there stays
// out of shell history and process listings.
const InviteEnv = "CONNVERIFIER_NODE"

// ResolveInvite reads a -node value: an invite, @FILE holding one, or empty for
// the InviteEnv variable.
func ResolveInvite(s string) (protocol.Invite, error) {
	if s == "" {
		s = os.Getenv(InviteEnv)
	}
	if s == "" {
		return protocol.Invite{}, fmt.Errorf("no node given: use -node INVITE, -node @FILE or set %s", InviteEnv)
	}
	if path, ok := strings.CutPrefix(s, "@"); ok {
		data, err := os.ReadFile(path)
		if err != nil {
			return protocol.Invite{}, err
		}
		s = string(data)
	}
	return protocol.ParseInvite(s)
}

// endTimeout bounds ending a session; it runs after the run's context has ended.
const endTimeout = 5 * time.Second

// DialFunc opens a connection; checks pass netx's dialer, tests an in-memory one.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// controlClient talks to one node address over HTTPS. Its connections come from
// the run's dialer, its TLS trusts only the invite's pin, and it follows no
// redirects and no proxy: a node cannot send the client anywhere else.
type controlClient struct {
	http  *http.Client
	tr    *http.Transport
	base  string
	token string
}

func newControl(addr string, inv protocol.Invite, dial DialFunc, timeout time.Duration) *controlClient {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dial(ctx, network, addr)
		},
		TLSClientConfig:        protocol.ClientTLS(inv.Pin),
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    timeout,
		ResponseHeaderTimeout:  timeout,
		MaxResponseHeaderBytes: 8 << 10,
		DisableCompression:     true,
	}
	return &controlClient{
		http: &http.Client{
			Transport:     tr,
			Timeout:       2 * timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		tr:    tr,
		base:  "https://" + addr,
		token: protocol.EncodeID(inv.Token[:]),
	}
}

func (c *controlClient) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.http.Do(req)
}

func (c *controlClient) close() { c.tr.CloseIdleConnections() }

// refusal reads a non-success response as the node's ErrorResponse; a body that
// does not decode still yields the HTTP status as the reason.
func refusal(resp *http.Response) *protocol.ErrorResponse {
	var e protocol.ErrorResponse
	if protocol.DecodeJSON(resp.Body, &e) != nil {
		e = protocol.ErrorResponse{Reason: fmt.Sprintf("http_%d", resp.StatusCode)}
	}
	return &e
}

// Session is a granted session on one node address.
type Session struct {
	Addr    string // the invite address that answered
	Resp    protocol.SessionResponse
	ID      [16]byte
	Secret  []byte
	Created time.Time
	ctl     *controlClient
}

// Error kinds: why no session was granted.
const (
	KindUnreachable = "unreachable"
	KindIdentity    = "identity"
	KindRefused     = "refused"
	KindBusy        = "busy"
)

// Error explains why no session was granted; Kind selects the message.
type Error struct {
	Kind    string
	Refusal *protocol.ErrorResponse
	Err     error
}

func (e *Error) Error() string {
	if e.Refusal != nil {
		return e.Kind + ": " + e.Refusal.Reason
	}
	return e.Kind + ": " + e.Err.Error()
}

// Open asks each invite address in turn until one answers. A key that does not
// match the pin stops the search at once: that node is not the one invited.
func Open(ctx context.Context, inv protocol.Invite, dial DialFunc, timeout time.Duration, version string, req protocol.SessionRequest) (*Session, error) {
	req.Client = protocol.ClientInfo{Name: "connverifier", Version: version}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var last error
	for _, addr := range inv.Addrs {
		ctl := newControl(addr, inv, dial, timeout)
		created := time.Now()
		resp, err := ctl.do(ctx, http.MethodPost, protocol.PathSessions, body)
		if err != nil {
			ctl.close()
			if errors.Is(err, protocol.ErrPinMismatch) {
				return nil, &Error{Kind: KindIdentity, Err: err}
			}
			last = err
			continue
		}
		s, serr := readSession(resp, addr, ctl, created)
		resp.Body.Close()
		if serr != nil {
			ctl.close()
			return nil, serr
		}
		return s, nil
	}
	return nil, &Error{Kind: KindUnreachable, Err: last}
}

func readSession(resp *http.Response, addr string, ctl *controlClient, created time.Time) (*Session, error) {
	if resp.StatusCode != http.StatusCreated {
		r := refusal(resp)
		kind := KindRefused
		if r.Reason == protocol.ErrReasonBusy || r.Reason == protocol.ErrReasonQuota {
			kind = KindBusy
		}
		return nil, &Error{Kind: kind, Refusal: r}
	}
	s := &Session{Addr: addr, ctl: ctl, Created: created}
	if err := protocol.DecodeJSON(resp.Body, &s.Resp); err != nil {
		return nil, &Error{Kind: KindRefused, Err: err,
			Refusal: &protocol.ErrorResponse{Reason: protocol.ErrReasonBadRequest, Message: "unreadable session response"}}
	}
	s.ID, _ = s.Resp.ID()
	s.Secret, _ = s.Resp.SecretBytes()
	return s, nil
}

// End releases the session on the node and returns what the node reported, or
// nil. It uses its own deadline, so it still runs after the run's context ended.
func (s *Session) End() *protocol.SessionEnd {
	defer s.ctl.close()
	ctx, cancel := context.WithTimeout(context.Background(), endTimeout)
	defer cancel()
	resp, err := s.ctl.do(ctx, http.MethodDelete, protocol.PathSessions+"/"+s.Resp.SessionID, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var e protocol.SessionEnd
	if resp.StatusCode != http.StatusOK || protocol.DecodeJSON(resp.Body, &e) != nil {
		return nil
	}
	return &e
}

// Failed records a check that never got a session: there is no measurement, so
// the status is ERROR, and the message names the cause.
func Failed(c *result.Check, e *Error) {
	c.Status = result.Error
	var m result.Message
	switch e.Kind {
	case KindIdentity:
		m.Key = "session.error.node_identity"
		m.Params = map[string]any{"addr": c.Path.Node}
	case KindUnreachable:
		m.Key = "session.error.node_unreachable"
		m.Params = map[string]any{"error": fmt.Sprint(e.Err)}
	case KindBusy:
		m.Key = "session.error.node_busy"
		m.Params = map[string]any{"reason": e.Refusal.Reason, "message": e.Refusal.Message, "retry_after_ms": e.Refusal.RetryAfterS * 1000}
	default:
		m.Key = "session.error.session_refused"
		m.Params = map[string]any{"reason": e.Refusal.Reason, "message": e.Refusal.Message}
	}
	c.Error = &m
}

// Load converts what the node reported about its load, or nil, for a result.
func Load(n *protocol.NodeInfo) *result.NodeLoad {
	if n == nil {
		return nil
	}
	l := n.Load
	return &result.NodeLoad{Sessions: l.Sessions, Connections: l.Connections, MaxConnections: l.MaxConnections, UptimeS: l.UptimeS}
}

// EndLoad is Load of what End returned.
func EndLoad(e *protocol.SessionEnd) *result.NodeLoad {
	if e == nil {
		return nil
	}
	return Load(&e.Node)
}

// Family is "ipv4" or "ipv6" for an IP-literal host:port, and "" for a host name.
func Family(hostPort string) string {
	ap, err := netip.ParseAddrPort(hostPort)
	switch {
	case err != nil:
		return ""
	case ap.Addr().Unmap().Is4():
		return "ipv4"
	}
	return "ipv6"
}
