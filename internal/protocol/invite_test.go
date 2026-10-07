package protocol

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testInvite() Invite {
	return Invite{
		Label: "tokyo-alice",
		Addrs: []string{"203.0.113.5:7443", "[2001:db8::5]:7443"},
		Pin:   [32]byte{1, 2, 3},
		Token: [32]byte{4, 5, 6},
	}
}

func TestInviteRoundTrip(t *testing.T) {
	inv := testInvite()
	s := inv.Encode()
	if !strings.HasPrefix(s, invitePrefix) || len(s) > MaxInviteLen {
		t.Fatalf("encoded invite %q", s)
	}
	got, err := ParseInvite("  " + s + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, inv) {
		t.Fatalf("round trip = %#v fields %v, want %v", got, got.Addrs, inv.Addrs)
	}
}

func TestInviteNeverPrintsToken(t *testing.T) {
	inv := testInvite()
	token := base64.RawURLEncoding.EncodeToString(inv.Token[:])
	for _, out := range []string{fmt.Sprint(inv), fmt.Sprintf("%v %+v %#v %s", inv, inv, inv, inv)} {
		if strings.Contains(out, token) || strings.Contains(out, fmt.Sprint(inv.Token[:])) {
			t.Fatalf("formatted invite leaks the token: %q", out)
		}
		if !strings.Contains(out, inv.Label) {
			t.Fatalf("formatted invite lacks the label: %q", out)
		}
	}
}

func encodeRaw(j string) string {
	return invitePrefix + base64.RawURLEncoding.EncodeToString([]byte(j))
}

func TestParseInviteRejects(t *testing.T) {
	pin := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	short := base64.RawURLEncoding.EncodeToString(make([]byte, 31))
	valid := func(label, addrs, p, tok string) string {
		return encodeRaw(fmt.Sprintf(`{"l":%q,"a":%s,"p":%q,"t":%q}`, label, addrs, p, tok))
	}
	tests := map[string]string{
		"no prefix":           "cvi9_abc",
		"too long":            invitePrefix + strings.Repeat("A", MaxInviteLen),
		"not base64":          invitePrefix + "!!!",
		"not JSON":            encodeRaw("{"),
		"empty label":         valid("", `["203.0.113.5:7443"]`, pin, pin),
		"control in label":    valid("a\nb", `["203.0.113.5:7443"]`, pin, pin),
		"no address":          valid("x", `[]`, pin, pin),
		"five addresses":      valid("x", `["a.example:1","b.example:1","c.example:1","d.example:1","e.example:1"]`, pin, pin),
		"port zero":           valid("x", `["203.0.113.5:0"]`, pin, pin),
		"port missing":        valid("x", `["203.0.113.5"]`, pin, pin),
		"bad host":            valid("x", `["bad_host!:7443"]`, pin, pin),
		"pin wrong length":    valid("x", `["203.0.113.5:7443"]`, short, pin),
		"token wrong length":  valid("x", `["203.0.113.5:7443"]`, pin, short),
		"token not base64url": valid("x", `["203.0.113.5:7443"]`, pin, "+/+/"),
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInvite(in); err == nil {
				t.Fatalf("ParseInvite accepted %q", in)
			}
		})
	}
}

func TestParseInviteAcceptsHostnameAndUnknownFields(t *testing.T) {
	pin := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	s := encodeRaw(fmt.Sprintf(`{"l":"家","a":["node.example:443"],"p":%q,"t":%q,"z":["future"]}`, pin, pin))
	if _, err := ParseInvite(s); err != nil {
		t.Fatalf("ParseInvite: %v", err)
	}
}

func TestInviteUDPPort(t *testing.T) {
	inv := testInvite()
	if _, ok := inv.UDPAddr(inv.Addrs[0]); ok {
		t.Fatal("an invite without a UDP port gave a UDP address")
	}
	inv.UDPPort = 7443
	got, err := ParseInvite(inv.Encode())
	if err != nil || got.UDPPort != 7443 {
		t.Fatalf("UDP port did not round trip: %d, %v", got.UDPPort, err)
	}
	if addr, ok := got.UDPAddr("[2001:db8::1]:443"); !ok || addr != "[2001:db8::1]:7443" {
		t.Fatalf("UDPAddr = %q, %v", addr, ok)
	}
	inv.UDPPort = 70000
	if _, err := ParseInvite(inv.Encode()); err == nil {
		t.Fatal("UDP port 70000 accepted")
	}
}

func FuzzParseInvite(f *testing.F) {
	f.Add(testInvite().Encode())
	f.Add("cvi1_")
	f.Add(encodeRaw(`{"l":"x","a":["[::1]:1"],"p":"","t":""}`))
	f.Fuzz(func(t *testing.T, s string) {
		inv, err := ParseInvite(s)
		if err != nil {
			return
		}
		again, err := ParseInvite(inv.Encode())
		if err != nil || !reflect.DeepEqual(again, inv) {
			t.Fatalf("re-encoded invite did not round trip: %v", err)
		}
	})
}
