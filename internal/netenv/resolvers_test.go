package netenv

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestNameservers(t *testing.T) {
	conf := "# generated\nsearch lan\nnameserver 192.168.1.1\nnameserver  fe80::1%eth0 \nnameserver 192.168.1.1\n" +
		"nameserver not-an-address\noptions edns0\n;nameserver 203.0.113.9\nnameserver ::ffff:203.0.113.1\n"
	want := []netip.Addr{
		netip.MustParseAddr("192.168.1.1"),
		netip.MustParseAddr("fe80::1%eth0"),
		netip.MustParseAddr("203.0.113.1"),
	}
	if got := nameservers(conf); !reflect.DeepEqual(got, want) {
		t.Fatalf("nameservers = %v, want %v", got, want)
	}
}
