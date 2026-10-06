package protocol

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"
)

// Control-plane paths.
const (
	PathSessions = "/v2/sessions"
	PathStatus   = "/v2/status"
)

// MaxBody bounds every control-plane request and response body.
const MaxBody = 4096

// Error reasons in an ErrorResponse.
const (
	ErrReasonBadRequest = "bad_request"
	ErrReasonAuth       = "auth"
	ErrReasonVersion    = "version"
	ErrReasonQuota      = "quota"
	ErrReasonBusy       = "busy"
)

// Bounds on Limits; a node grants at most these whatever it is configured to.
const (
	MaxConnections  = 1_000_000
	MaxDialRate     = 100_000
	MaxDurationS    = 7 * 24 * 3600
	MaxIdleTimeoutS = 24 * 3600
)

const (
	maxShortString = 64
	maxMessage     = 256
)

// Limits are what a session asks for (want) or is allowed (granted). In a request
// DurationS 0 means "as long as the node allows"; granted values are never 0.
type Limits struct {
	Connections  int `json:"connections"`
	DialRate     int `json:"dial_rate"`
	DurationS    int `json:"duration_s"`
	IdleTimeoutS int `json:"idle_timeout_s"`
}

func (l Limits) validate(granted bool) error {
	minDuration := 0
	if granted {
		minDuration = 1
	}
	switch {
	case l.Connections < 1 || l.Connections > MaxConnections:
		return fmt.Errorf("connections must be 1 to %d", MaxConnections)
	case l.DialRate < 1 || l.DialRate > MaxDialRate:
		return fmt.Errorf("dial_rate must be 1 to %d", MaxDialRate)
	case l.DurationS < minDuration || l.DurationS > MaxDurationS:
		return fmt.Errorf("duration_s must be %d to %d", minDuration, MaxDurationS)
	case l.IdleTimeoutS < 1 || l.IdleTimeoutS > MaxIdleTimeoutS:
		return fmt.Errorf("idle_timeout_s must be 1 to %d", MaxIdleTimeoutS)
	}
	return nil
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type SessionRequest struct {
	Client ClientInfo `json:"client"`
	Check  string     `json:"check"`
	Want   Limits     `json:"want"`
}

func (r *SessionRequest) Validate() error {
	if err := shortStrings(r.Client.Name, r.Client.Version, r.Check); err != nil {
		return err
	}
	return r.Want.validate(false)
}

type Load struct {
	Sessions       int   `json:"sessions"`
	Connections    int   `json:"connections"`
	MaxConnections int   `json:"max_connections"`
	UptimeS        int64 `json:"uptime_s"`
}

type NodeInfo struct {
	Version string `json:"version"`
	Load    Load   `json:"load"`
}

func (n NodeInfo) validate() error {
	if err := shortStrings(n.Version); err != nil {
		return err
	}
	l := n.Load
	if l.Sessions < 0 || l.Connections < 0 || l.MaxConnections < 0 || l.UptimeS < 0 ||
		l.Connections > MaxConnections || l.MaxConnections > MaxConnections {
		return errors.New("node load out of range")
	}
	return nil
}

// SessionResponse grants a session. ObservedAddr is the client's address as the node
// saw it on the control connection; it is sensitive and belongs only in local results.
type SessionResponse struct {
	SessionID    string    `json:"session_id"`
	Secret       string    `json:"secret"`
	ExpiresAt    time.Time `json:"expires_at"`
	ObservedAddr string    `json:"observed_addr"`
	Granted      Limits    `json:"granted"`
	Node         NodeInfo  `json:"node"`
}

func (r *SessionResponse) Validate() error {
	if _, err := r.ID(); err != nil {
		return fmt.Errorf("session_id: %w", err)
	}
	if _, err := r.SecretBytes(); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if _, err := netip.ParseAddrPort(r.ObservedAddr); err != nil {
		return fmt.Errorf("observed_addr: %w", err)
	}
	if err := r.Granted.validate(true); err != nil {
		return fmt.Errorf("granted: %w", err)
	}
	return r.Node.validate()
}

func (r *SessionResponse) ID() ([16]byte, error) {
	var id [16]byte
	return id, decodeFixed(r.SessionID, id[:])
}

func (r *SessionResponse) SecretBytes() ([]byte, error) {
	secret := make([]byte, 32)
	return secret, decodeFixed(r.Secret, secret)
}

// EncodeID renders a session ID or secret for the JSON fields above.
func EncodeID(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// DecodeID parses a session ID from a URL path segment or JSON field.
func DecodeID(s string) ([16]byte, error) {
	var id [16]byte
	return id, decodeFixed(s, id[:])
}

type StatusResponse struct {
	Node   NodeInfo `json:"node"`
	Limits Limits   `json:"limits"`
}

func (r *StatusResponse) Validate() error {
	if err := r.Limits.validate(true); err != nil {
		return fmt.Errorf("limits: %w", err)
	}
	return r.Node.validate()
}

// ErrorResponse explains a refused request. Message is for the reader only and
// never decides a status.
type ErrorResponse struct {
	Reason      string `json:"reason"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
	Message     string `json:"message,omitempty"`
}

func (e *ErrorResponse) Validate() error {
	if err := shortStrings(e.Reason); err != nil {
		return err
	}
	if e.RetryAfterS < 0 || e.RetryAfterS > 24*3600 || len(e.Message) > maxMessage {
		return errors.New("error response out of range")
	}
	return nil
}

func shortStrings(ss ...string) error {
	for _, s := range ss {
		if len(s) > maxShortString {
			return fmt.Errorf("string field longer than %d bytes", maxShortString)
		}
	}
	return nil
}

// DecodeJSON reads exactly one JSON value of at most MaxBody bytes from r into v
// and validates it. Larger bodies, trailing data and invalid values are refused.
func DecodeJSON(r io.Reader, v interface{ Validate() error }) error {
	data, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil {
		return err
	}
	if len(data) > MaxBody {
		return fmt.Errorf("%w: body over %d bytes", ErrMalformed, MaxBody)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("%w: trailing data", ErrMalformed)
	}
	return v.Validate()
}
