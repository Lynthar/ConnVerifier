package netenv

import (
	"encoding/binary"
	"encoding/hex"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

func defaultGateway(family string) (netip.Addr, error) {
	path, parse := "/proc/net/route", parseIPv4Routes
	if family == "ipv6" {
		path, parse = "/proc/net/ipv6_route", parseIPv6Routes
	}
	table, err := os.ReadFile(path)
	if err != nil {
		return netip.Addr{}, err
	}
	return best(parse(string(table)))
}

// Route flags in both tables (RTF_* in linux/route.h).
const (
	rtfUp      = 0x1
	rtfGateway = 0x2
	rtfReject  = 0x200
)

// parseIPv4Routes reads the default routes of /proc/net/route: interface,
// destination, gateway, flags, refcount, use, metric, mask, all hex but metric.
func parseIPv4Routes(table string) []defaultRoute {
	var routes []defaultRoute
	for line := range strings.Lines(table) {
		f := strings.Fields(line)
		if len(f) < 8 || f[1] != "00000000" || f[7] != "00000000" {
			continue
		}
		gw, err1 := strconv.ParseUint(f[2], 16, 32)
		flags, err2 := strconv.ParseUint(f[3], 16, 32)
		metric, err3 := strconv.ParseUint(f[6], 10, 32)
		if err1 != nil || err2 != nil || err3 != nil || flags&rtfUp == 0 || flags&rtfReject != 0 {
			continue
		}
		r := defaultRoute{metric: metric}
		if flags&rtfGateway != 0 {
			var a [4]byte // printed as an integer in the host's byte order
			binary.NativeEndian.PutUint32(a[:], uint32(gw))
			r.gateway = netip.AddrFrom4(a)
		}
		routes = append(routes, r)
	}
	return routes
}

// parseIPv6Routes reads the default routes of /proc/net/ipv6_route: destination,
// its prefix length, source, its length, next hop, metric, refcount, use, flags
// and interface, all hex. The kernel's unreachable route on lo is a reject route.
func parseIPv6Routes(table string) []defaultRoute {
	var routes []defaultRoute
	for line := range strings.Lines(table) {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] != strings.Repeat("0", 32) || f[1] != "00" {
			continue
		}
		nh, err1 := hex.DecodeString(f[4])
		metric, err2 := strconv.ParseUint(f[5], 16, 32)
		flags, err3 := strconv.ParseUint(f[8], 16, 32)
		if err1 != nil || len(nh) != 16 || err2 != nil || err3 != nil || flags&rtfUp == 0 || flags&rtfReject != 0 {
			continue
		}
		r := defaultRoute{metric: metric}
		if flags&rtfGateway != 0 {
			r.gateway = netip.AddrFrom16([16]byte(nh))
			if r.gateway.IsLinkLocalUnicast() {
				r.gateway = r.gateway.WithZone(f[9])
			}
		}
		routes = append(routes, r)
	}
	return routes
}
