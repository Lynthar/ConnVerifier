package netenv

import (
	"errors"
	"net/netip"
	"testing"
)

func TestAddrClass(t *testing.T) {
	for addr, want := range map[string]string{
		"127.0.0.1": ClassLoopback, "::1": ClassLoopback,
		"169.254.1.1": ClassLinkLocal, "fe80::1%en0": ClassLinkLocal,
		"192.168.1.1": ClassPrivate, "10.0.0.1": ClassPrivate, "fd00::1": ClassPrivate, "::ffff:172.16.0.1": ClassPrivate,
		"100.64.0.1": ClassShared, "198.18.0.1": ClassBenchmarking, "198.19.255.254": ClassBenchmarking,
		"203.0.113.1": ClassPublic, "2001:db8::1": ClassPublic,
		"224.0.0.1": ClassOther, "0.0.0.0": ClassOther,
	} {
		if got := AddrClass(netip.MustParseAddr(addr)); got != want {
			t.Errorf("AddrClass(%s) = %q, want %q", addr, got, want)
		}
	}
}

func TestBest(t *testing.T) {
	gw := func(s string) netip.Addr { return netip.MustParseAddr(s) }
	tests := []struct {
		name   string
		routes []defaultRoute
		want   netip.Addr
		err    error
	}{
		{"none", nil, netip.Addr{}, ErrNoDefaultRoute},
		{"lowest metric wins", []defaultRoute{{gw("192.0.2.1"), 600}, {gw("192.0.2.2"), 100}}, gw("192.0.2.2"), nil},
		{"lowest has no gateway", []defaultRoute{{gw("192.0.2.1"), 600}, {netip.Addr{}, 50}}, netip.Addr{}, ErrNoGateway},
		{"unspecified is no gateway", []defaultRoute{{gw("0.0.0.0"), 0}}, netip.Addr{}, ErrNoGateway},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := best(tt.routes)
			if got != tt.want || !errors.Is(err, tt.err) {
				t.Fatalf("best = %v, %v; want %v, %v", got, err, tt.want, tt.err)
			}
		})
	}
}
