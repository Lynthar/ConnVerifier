package protocol

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func validRequest() string {
	return `{"client":{"name":"connverifier","version":"v0"},"check":"tcp-capacity",` +
		`"want":{"connections":100,"dial_rate":10,"duration_s":0,"idle_timeout_s":60}}`
}

func TestDecodeSessionRequest(t *testing.T) {
	var r SessionRequest
	if err := DecodeJSON(strings.NewReader(validRequest()), &r); err != nil {
		t.Fatal(err)
	}
	if r.Want.Connections != 100 || r.Check != "tcp-capacity" {
		t.Fatalf("decoded %+v", r)
	}
}

func TestDecodeJSONRejects(t *testing.T) {
	tests := map[string]string{
		"oversized":            `{"check":"` + strings.Repeat("x", MaxBody) + `"}`,
		"trailing data":        validRequest() + `{}`,
		"not JSON":             `{`,
		"zero connections":     strings.Replace(validRequest(), `"connections":100`, `"connections":0`, 1),
		"too many connections": strings.Replace(validRequest(), `"connections":100`, `"connections":1000001`, 1),
		"negative duration":    strings.Replace(validRequest(), `"duration_s":0`, `"duration_s":-1`, 1),
		"long version":         strings.Replace(validRequest(), `"version":"v0"`, `"version":"`+strings.Repeat("v", 65)+`"`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			var r SessionRequest
			if err := DecodeJSON(strings.NewReader(body), &r); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestSessionResponseValidate(t *testing.T) {
	ok := SessionResponse{
		SessionID:    EncodeID(make([]byte, 16)),
		Secret:       EncodeID(make([]byte, 32)),
		ExpiresAt:    time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ObservedAddr: "198.51.100.7:51234",
		Granted:      Limits{Connections: 5, DialRate: 5, DurationS: 60, IdleTimeoutS: 60},
	}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	mutations := map[string]func(*SessionResponse){
		"short session id":      func(r *SessionResponse) { r.SessionID = EncodeID(make([]byte, 15)) },
		"short secret":          func(r *SessionResponse) { r.Secret = EncodeID(make([]byte, 31)) },
		"observed without port": func(r *SessionResponse) { r.ObservedAddr = "198.51.100.7" },
		"granted zero duration": func(r *SessionResponse) { r.Granted.DurationS = 0 },
		"negative load":         func(r *SessionResponse) { r.Node.Load.Sessions = -1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			r := ok
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

// Only the certificate whose key matches the pin may complete the handshake; a
// certificate with an identical name but another key must fail.
func TestClientTLSTrustsOnlyThePinnedKey(t *testing.T) {
	node, err := NewIdentity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	impostor, err := NewIdentity(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pin := Pin(node.Leaf)

	handshake := func(serverCert tls.Certificate) error {
		c, s := net.Pipe()
		defer c.Close()
		defer s.Close()
		go tls.Server(s, ServerTLS(serverCert)).Handshake()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		return tls.Client(c, ClientTLS(pin)).Handshake()
	}
	if err := handshake(node); err != nil {
		t.Fatalf("handshake with the pinned node failed: %v", err)
	}
	if err := handshake(impostor); !errors.Is(err, errPinMismatch) {
		t.Fatalf("handshake with another key = %v, want pin mismatch", err)
	}
}

func FuzzDecodeSessionRequest(f *testing.F) {
	f.Add(validRequest())
	f.Add(`{}`)
	f.Add(`{"want":{"connections":1e9}}`)
	f.Fuzz(func(t *testing.T, body string) {
		var r SessionRequest
		if err := DecodeJSON(strings.NewReader(body), &r); err != nil {
			return
		}
		data, err := json.Marshal(&r)
		if err != nil {
			t.Fatal(err)
		}
		var again SessionRequest
		if err := DecodeJSON(strings.NewReader(string(data)), &again); err != nil || again != r {
			t.Fatalf("re-encoded request did not round trip: %v", err)
		}
	})
}
