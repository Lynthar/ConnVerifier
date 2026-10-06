package protocol

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	invitePrefix = "cvi1_"
	// MaxInviteLen bounds a pasted invite, prefix included.
	MaxInviteLen   = 2048
	maxInviteAddrs = 4
	maxLabelLen    = 64
)

// Invite carries a node's addresses, the pin of its key and an access token.
// It is a credential: never put it in results, logs or shared reports — use
// Label. String and GoString print only the label, so %v and %#v cannot leak it.
type Invite struct {
	Label string
	Addrs []string
	Pin   [32]byte
	Token [32]byte
}

type inviteJSON struct {
	Label string   `json:"l"`
	Addrs []string `json:"a"`
	Pin   string   `json:"p"`
	Token string   `json:"t"`
}

func (inv Invite) String() string   { return "invite " + strconv.Quote(inv.Label) }
func (inv Invite) GoString() string { return inv.String() }

// Encode returns the invite string to hand to a user.
func (inv Invite) Encode() string {
	data, _ := json.Marshal(inviteJSON{
		Label: inv.Label,
		Addrs: inv.Addrs,
		Pin:   base64.RawURLEncoding.EncodeToString(inv.Pin[:]),
		Token: base64.RawURLEncoding.EncodeToString(inv.Token[:]),
	})
	return invitePrefix + base64.RawURLEncoding.EncodeToString(data)
}

// ParseInvite decodes and validates an invite string; surrounding whitespace from
// pasting is ignored. Unknown JSON fields are ignored so later versions can add
// optional ones.
func ParseInvite(s string) (Invite, error) {
	s = strings.TrimSpace(s)
	if len(s) > MaxInviteLen {
		return Invite{}, fmt.Errorf("invite longer than %d characters", MaxInviteLen)
	}
	body, ok := strings.CutPrefix(s, invitePrefix)
	if !ok {
		return Invite{}, fmt.Errorf("not an invite: missing %q prefix", invitePrefix)
	}
	data, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	var j inviteJSON
	if err := json.Unmarshal(data, &j); err != nil {
		return Invite{}, fmt.Errorf("invite: %w", err)
	}
	inv := Invite{Label: j.Label, Addrs: j.Addrs}
	if err := decodeFixed(j.Pin, inv.Pin[:]); err != nil {
		return Invite{}, fmt.Errorf("invite pin: %w", err)
	}
	if err := decodeFixed(j.Token, inv.Token[:]); err != nil {
		return Invite{}, fmt.Errorf("invite token: %w", err)
	}
	if err := inv.Validate(); err != nil {
		return Invite{}, err
	}
	return inv, nil
}

// Validate checks the label and every address.
func (inv Invite) Validate() error {
	if err := ValidateLabel(inv.Label); err != nil {
		return err
	}
	if len(inv.Addrs) == 0 || len(inv.Addrs) > maxInviteAddrs {
		return fmt.Errorf("invite needs 1 to %d addresses, has %d", maxInviteAddrs, len(inv.Addrs))
	}
	for _, a := range inv.Addrs {
		if err := ValidateHostPort(a); err != nil {
			return err
		}
	}
	return nil
}

// ValidateLabel accepts 1 to 64 bytes of printable UTF-8.
func ValidateLabel(l string) error {
	if l == "" || len(l) > maxLabelLen || !utf8.ValidString(l) {
		return fmt.Errorf("label must be 1 to %d bytes of UTF-8", maxLabelLen)
	}
	for _, r := range l {
		if unicode.IsControl(r) {
			return fmt.Errorf("label contains a control character")
		}
	}
	return nil
}

// ValidateHostPort accepts "host:port" where host is an IP literal or a DNS name.
func ValidateHostPort(s string) error {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return fmt.Errorf("address %q: %w", s, err)
	}
	if p, err := strconv.Atoi(port); err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("address %q: port must be 1 to 65535", s)
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("address %q: host is neither an IP address nor a host name", s)
	}
	return nil
}

func validHostname(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// decodeFixed decodes unpadded base64url into dst, which it must fill exactly.
func decodeFixed(s string, dst []byte) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	if len(b) != len(dst) {
		return fmt.Errorf("want %d bytes, got %d", len(dst), len(b))
	}
	copy(dst, b)
	return nil
}
