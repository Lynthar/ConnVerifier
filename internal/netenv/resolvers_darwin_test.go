package netenv

import (
	"net/netip"
	"reflect"
	"testing"
)

func TestMainResolvers(t *testing.T) {
	out := `DNS configuration

resolver #1
  search domain[0] : lan
  nameserver[0] : 192.168.1.1
  nameserver[1] : fe80::1%en0
  if_index : 13 (en0)
  reach    : 0x00020002 (Reachable,Directly Reachable Address)

resolver #2
  domain   : local
  nameserver[0] : 224.0.0.251
  options  : mdns

DNS configuration (for scoped queries)

resolver #1
  nameserver[0] : 203.0.113.53
`
	want := []netip.Addr{netip.MustParseAddr("192.168.1.1"), netip.MustParseAddr("fe80::1%en0")}
	if got := mainResolvers(out); !reflect.DeepEqual(got, want) {
		t.Fatalf("mainResolvers = %v, want %v", got, want)
	}
	if got := mainResolvers("DNS configuration\n\nNo DNS configuration available\n"); len(got) != 0 {
		t.Fatalf("no configuration gave %v", got)
	}
}
