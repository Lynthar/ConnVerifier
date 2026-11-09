package netenv

import (
	"net"
	"net/netip"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

// defaultGateway reads the routing table from the kernel. Every interface also
// has a default route scoped to it; only the unscoped one is the system's default.
func defaultGateway(family string) (netip.Addr, error) {
	af := unix.AF_INET
	if family == "ipv6" {
		af = unix.AF_INET6
	}
	rib, err := route.FetchRIB(af, route.RIBTypeRoute, 0)
	if err != nil {
		return netip.Addr{}, err
	}
	msgs, err := route.ParseRIB(route.RIBTypeRoute, rib)
	if err != nil {
		return netip.Addr{}, err
	}
	var routes []defaultRoute
	for _, m := range msgs {
		rm, ok := m.(*route.RouteMessage)
		if !ok || rm.Flags&unix.RTF_UP == 0 || rm.Flags&unix.RTF_IFSCOPE != 0 || len(rm.Addrs) <= unix.RTAX_NETMASK ||
			rm.Addrs[unix.RTAX_DST] == nil || !zeroAddr(rm.Addrs[unix.RTAX_DST]) || !zeroAddr(rm.Addrs[unix.RTAX_NETMASK]) {
			continue
		}
		var r defaultRoute
		if rm.Flags&unix.RTF_GATEWAY != 0 {
			r.gateway = routeAddr(rm.Addrs[unix.RTAX_GATEWAY], rm.Index)
		}
		routes = append(routes, r)
	}
	return best(routes)
}

func zeroAddr(a route.Addr) bool {
	switch a := a.(type) {
	case nil:
		return true
	case *route.Inet4Addr:
		return a.IP == [4]byte{}
	case *route.Inet6Addr:
		return a.IP == [16]byte{}
	}
	return false
}

// routeAddr converts a gateway; one that is a link, not an address, is invalid.
func routeAddr(a route.Addr, index int) netip.Addr {
	switch a := a.(type) {
	case *route.Inet4Addr:
		return netip.AddrFrom4(a.IP)
	case *route.Inet6Addr:
		ip := netip.AddrFrom16(a.IP)
		if !ip.IsLinkLocalUnicast() {
			return ip
		}
		if a.ZoneID != 0 {
			index = a.ZoneID
		}
		if ifi, err := net.InterfaceByIndex(index); err == nil {
			return ip.WithZone(ifi.Name)
		}
		return ip
	}
	return netip.Addr{}
}
