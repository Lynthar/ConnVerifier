package capacity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/protocol"
)

// controlClient talks to one node address over HTTPS. Its connections come from
// the run's dialer, its TLS trusts only the invite's pin, and it follows no
// redirects and no proxy: a node cannot send the client anywhere else.
type controlClient struct {
	http  *http.Client
	tr    *http.Transport
	base  string
	token string
}

func newControl(addr string, inv protocol.Invite, dial dialFunc, timeout time.Duration) *controlClient {
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

// refusal reads a non-success response as the node's ErrorResponse; a body that
// does not decode still yields the HTTP status as the reason.
func refusal(resp *http.Response) *protocol.ErrorResponse {
	var e protocol.ErrorResponse
	if protocol.DecodeJSON(resp.Body, &e) != nil {
		e = protocol.ErrorResponse{Reason: fmt.Sprintf("http_%d", resp.StatusCode)}
	}
	return &e
}

// session is a granted session on one node address.
type session struct {
	addr    string
	ctl     *controlClient
	resp    protocol.SessionResponse
	id      [16]byte
	secret  []byte
	created time.Time
}

// sessionError explains why no session was granted; kind selects the message.
type sessionError struct {
	kind    string // "unreachable", "identity", "refused" or "busy"
	refusal *protocol.ErrorResponse
	err     error
}

func (e *sessionError) Error() string {
	if e.refusal != nil {
		return e.kind + ": " + e.refusal.Reason
	}
	return e.kind + ": " + e.err.Error()
}

// openSession asks each invite address in turn until one answers. A key that does
// not match the pin stops the search at once: that node is not the one invited.
func openSession(ctx context.Context, inv protocol.Invite, dial dialFunc, timeout time.Duration, version string, want protocol.Limits) (*session, error) {
	body, err := json.Marshal(protocol.SessionRequest{
		Client: protocol.ClientInfo{Name: "connverifier", Version: version},
		Check:  "tcp-capacity",
		Want:   want,
	})
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
				return nil, &sessionError{kind: "identity", err: err}
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
	return nil, &sessionError{kind: "unreachable", err: last}
}

func readSession(resp *http.Response, addr string, ctl *controlClient, created time.Time) (*session, error) {
	if resp.StatusCode != http.StatusCreated {
		r := refusal(resp)
		kind := "refused"
		if r.Reason == protocol.ErrReasonBusy || r.Reason == protocol.ErrReasonQuota {
			kind = "busy"
		}
		return nil, &sessionError{kind: kind, refusal: r}
	}
	s := &session{addr: addr, ctl: ctl, created: created}
	if err := protocol.DecodeJSON(resp.Body, &s.resp); err != nil {
		return nil, &sessionError{kind: "refused", err: err,
			refusal: &protocol.ErrorResponse{Reason: protocol.ErrReasonBadRequest, Message: "unreadable session response"}}
	}
	s.id, _ = s.resp.ID()
	s.secret, _ = s.resp.SecretBytes()
	return s, nil
}

// end releases the session on the node and returns the node's load. It uses its
// own deadline, so it still runs after the run's context has been cancelled.
func (s *session) end() *protocol.NodeInfo {
	defer s.ctl.close()
	ctx, cancel := context.WithTimeout(context.Background(), endTimeout)
	defer cancel()
	resp, err := s.ctl.do(ctx, http.MethodDelete, protocol.PathSessions+"/"+s.resp.SessionID, nil)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var e protocol.SessionEnd
	if resp.StatusCode != http.StatusOK || protocol.DecodeJSON(resp.Body, &e) != nil {
		return nil
	}
	return &e.Node
}

func (c *controlClient) close() { c.tr.CloseIdleConnections() }
