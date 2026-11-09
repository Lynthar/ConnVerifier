package netenv

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"reflect"
	"testing"
)

// hexAddr prints a as /proc/net/route does: an integer in the host's byte order.
func hexAddr(a string) string {
	b := netip.MustParseAddr(a).As4()
	return fmt.Sprintf("%08X", binary.NativeEndian.Uint32(b[:]))
}

func TestParseIPv4Routes(t *testing.T) {
	table := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
		"wlan0\t00000000\t" + hexAddr("192.168.1.1") + "\t0003\t0\t0\t600\t00000000\t0\t0\t0\n" +
		"eth0\t00000000\t" + hexAddr("192.168.5.2") + "\t0003\t0\t0\t100\t00000000\t0\t0\t0\n" +
		"wg0\t00000000\t00000000\t0001\t0\t0\t50\t00000000\t0\t0\t0\n" +
		"eth0\t0005A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n" +
		"down0\t00000000\t" + hexAddr("192.0.2.1") + "\t0002\t0\t0\t1\t00000000\t0\t0\t0\n"
	want := []defaultRoute{
		{netip.MustParseAddr("192.168.1.1"), 600},
		{netip.MustParseAddr("192.168.5.2"), 100},
		{netip.Addr{}, 50},
	}
	if got := parseIPv4Routes(table); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIPv4Routes = %v, want %v", got, want)
	}
}

func TestParseIPv6Routes(t *testing.T) {
	zero := "00000000000000000000000000000000"
	table := zero + " 00 " + zero + " 00 fe800000000000000000000000000001 00000400 00000001 00000000 00450003 eth0\n" +
		zero + " 00 " + zero + " 00 20010db8000000000000000000000001 00000064 00000001 00000000 00000003 eth1\n" +
		zero + " 00 " + zero + " 00 " + zero + " 00000032 00000001 00000000 00000001 wg0\n" +
		zero + " 00 " + zero + " 00 " + zero + " ffffffff 00000001 00000000 00200200 lo\n" +
		"20010db8000000000000000000000000 40 " + zero + " 00 " + zero + " 00000100 00000001 00000000 00000001 eth0\n"
	want := []defaultRoute{
		{netip.MustParseAddr("fe80::1%eth0"), 0x400},
		{netip.MustParseAddr("2001:db8::1"), 0x64},
		{netip.Addr{}, 0x32},
	}
	if got := parseIPv6Routes(table); !reflect.DeepEqual(got, want) {
		t.Fatalf("parseIPv6Routes = %v, want %v", got, want)
	}
}
