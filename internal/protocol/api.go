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

// PathSessions is where sessions are created (POST) and ended (DELETE …/{id}).
const PathSessions = "/v2/sessions"

// Load endpoints, under PathSessions/{id}: GET LoadSmall answers 1 byte, GET
// LoadLarge sends until the session's load budget is spent, POST LoadUpload
// discards what it reads.
const (
	LoadSmall  = "/load/small"
	LoadLarge  = "/load/large"
	LoadUpload = "/load/upload"
)

// LoadPath is the path of a load endpoint of the session with the given ID.
func LoadPath(sessionID, endpoint string) string { return PathSessions + "/" + sessionID + endpoint }

// MaxBody bounds every control-plane request and response body.
const MaxBody = 4096

// Checks a session can be opened for.
const (
	CheckTCPCapacity = "tcp-capacity"
	CheckBaseline    = "baseline" // udp-baseline and tcp-baseline, side by side
	CheckLoad        = "load"     // tcp-load or quic-load: load endpoints, with STAMP and an echo connection beside them
)

// Transports a load session may ask for; "" is TCP.
const (
	TransportTCP  = "tcp"  // HTTP/2 over TLS on the node's TCP port
	TransportQUIC = "quic" // HTTP/3 on the node's QUIC port
)

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
	MaxStampRate    = 10_000
	MaxLoadBytes    = 1 << 40
	MaxLoadConns    = 1024
)

const (
	maxShortString = 64
	maxMessage     = 256
)

// Limits are what a session asks for (want) or is allowed (granted). In a request
// DurationS 0 means "as long as the node allows"; granted values are never 0.
// StampRate is STAMP packets per second, 0 when the session sends none. LoadBytes
// and LoadConnections are asked for only: a grant comes back as SessionResponse.Load.
type Limits struct {
	Connections     int   `json:"connections"`
	DialRate        int   `json:"dial_rate"`
	DurationS       int   `json:"duration_s"`
	IdleTimeoutS    int   `json:"idle_timeout_s"`
	StampRate       int   `json:"stamp_rate,omitempty"`
	LoadBytes       int64 `json:"load_bytes,omitempty"`
	LoadConnections int   `json:"load_connections,omitempty"`
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
	case l.StampRate < 0 || l.StampRate > MaxStampRate:
		return fmt.Errorf("stamp_rate must be 0 to %d", MaxStampRate)
	case l.LoadBytes < 0 || l.LoadBytes > MaxLoadBytes:
		return fmt.Errorf("load_bytes must be 0 to %d", int64(MaxLoadBytes))
	case l.LoadConnections < 0 || l.LoadConnections > MaxLoadConns:
		return fmt.Errorf("load_connections must be 0 to %d", MaxLoadConns)
	case l.LoadBytes > 0 && l.LoadConnections == 0:
		return errors.New("load_bytes needs load_connections")
	}
	return nil
}

type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type SessionRequest struct {
	Client    ClientInfo `json:"client"`
	Check     string     `json:"check"`
	Transport string     `json:"transport,omitempty"` // of a load session
	Want      Limits     `json:"want"`
}

func (r *SessionRequest) Validate() error {
	if err := shortStrings(r.Client.Name, r.Client.Version, r.Check); err != nil {
		return err
	}
	switch {
	case r.Transport == "" || r.Transport == TransportTCP:
	case r.Transport == TransportQUIC && r.Check == CheckLoad:
	default:
		return errors.New("transport must be tcp, or quic for a load session")
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
// saw it on the control connection, empty if unknown; sensitive, for local results only.
type SessionResponse struct {
	SessionID    string      `json:"session_id"`
	Secret       string      `json:"secret"`
	ExpiresAt    time.Time   `json:"expires_at"`
	ObservedAddr string      `json:"observed_addr"`
	Granted      Limits      `json:"granted"`
	Node         NodeInfo    `json:"node"`
	Stamp        *StampGrant `json:"stamp,omitempty"`
	Load         *LoadGrant  `json:"load,omitempty"`
}

// LoadGrant is a session's allowance on the load endpoints: Bytes is its budget
// in both directions together, Connections how many load connections it may hold.
// QUIC is set when the grant is for HTTP/3; a node that ignores the asked
// transport grants TCP and omits it. TCPCongestion names the node's TCP
// congestion control where it can tell. A node that offers load but cannot grant
// it now sets only Refused (ErrReasonBusy or ErrReasonQuota) and RetryAfterS; a
// node that offers none omits the grant.
type LoadGrant struct {
	Bytes         int64      `json:"bytes,omitempty"`
	Connections   int        `json:"connections,omitempty"`
	QUIC          *QUICGrant `json:"quic,omitempty"`
	TCPCongestion string     `json:"tcp_congestion,omitempty"`
	Refused       string     `json:"refused,omitempty"`
	RetryAfterS   int        `json:"retry_after_s,omitempty"`
}

// QUICGrant is where the node serves HTTP/3: Port, on the host of the address
// the session was opened on.
type QUICGrant struct {
	Port int `json:"port"`
}

func (g *LoadGrant) validate() error {
	if err := shortStrings(g.TCPCongestion); err != nil {
		return err
	}
	switch g.Refused {
	case "":
		if g.Bytes < 1 || g.Bytes > MaxLoadBytes || g.Connections < 1 || g.Connections > MaxLoadConns {
			return errors.New("load grant out of range")
		}
		if g.QUIC != nil && (g.QUIC.Port < 1 || g.QUIC.Port > 65535) {
			return errors.New("load grant: quic port out of range")
		}
	case ErrReasonBusy, ErrReasonQuota:
		if g.Bytes != 0 || g.Connections != 0 || g.QUIC != nil || g.RetryAfterS < 0 || g.RetryAfterS > 24*3600 {
			return errors.New("refused load grant carries a grant")
		}
	default:
		return errors.New("unknown load refusal")
	}
	return nil
}

// LoadCounts are what the node saw of a session's load: body bytes it sent and
// received on the load endpoints, and how many times its 10 ms timer woke during
// the session (LagTicks) and woke more than 10 ms late (LagOver).
type LoadCounts struct {
	Sent     uint64 `json:"sent"`
	Received uint64 `json:"received"`
	LagTicks uint64 `json:"lag_ticks"`
	LagOver  uint64 `json:"lag_over"`
}

// StampGrant identifies a session's STAMP packets (RFC 8972 SSID); never 0.
type StampGrant struct {
	SSID uint16 `json:"ssid"`
}

// StampCounts are what the node's reflector saw of one session. Received counts
// authenticated packets it answered; OverRate and OtherAddr are packets it dropped
// for exceeding the granted rate or for coming from another source address.
type StampCounts struct {
	Received  uint64 `json:"received"`
	OverRate  uint64 `json:"over_rate"`
	OtherAddr uint64 `json:"other_addr"`
}

func (r *SessionResponse) Validate() error {
	if _, err := r.ID(); err != nil {
		return fmt.Errorf("session_id: %w", err)
	}
	if _, err := r.SecretBytes(); err != nil {
		return fmt.Errorf("secret: %w", err)
	}
	if r.ObservedAddr != "" {
		if _, err := netip.ParseAddrPort(r.ObservedAddr); err != nil {
			return fmt.Errorf("observed_addr: %w", err)
		}
	}
	if err := r.Granted.validate(true); err != nil {
		return fmt.Errorf("granted: %w", err)
	}
	if r.Stamp != nil && r.Stamp.SSID == 0 {
		return errors.New("stamp: ssid must not be 0")
	}
	if r.Load != nil {
		if err := r.Load.validate(); err != nil {
			return err
		}
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

// SessionEnd answers DELETE on a session with the node's load at that moment, for
// a session that sent STAMP packets what the reflector saw of them, and for one
// granted load what the load endpoints saw.
type SessionEnd struct {
	Node  NodeInfo     `json:"node"`
	Stamp *StampCounts `json:"stamp,omitempty"`
	Load  *LoadCounts  `json:"load,omitempty"`
}

func (r *SessionEnd) Validate() error {
	if r.Load != nil && r.Load.LagOver > r.Load.LagTicks {
		return errors.New("load: lag_over above lag_ticks")
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
