package netenv

import (
	"net/netip"
	"strconv"
	"unsafe"

	"golang.org/x/sys/windows"
)

// defaultGateway ranks default routes as Windows does: by the route's metric plus
// its interface's.
func defaultGateway(family string) (netip.Addr, error) {
	af := uint16(windows.AF_INET)
	if family == "ipv6" {
		af = windows.AF_INET6
	}
	var table *windows.MibIpForwardTable2
	if err := windows.GetIpForwardTable2(af, &table); err != nil {
		return netip.Addr{}, err
	}
	defer windows.FreeMibTable(unsafe.Pointer(table))
	var routes []defaultRoute
	for _, row := range table.Rows() {
		if row.DestinationPrefix.PrefixLength != 0 {
			continue
		}
		ifc := windows.MibIpInterfaceRow{Family: af, InterfaceLuid: row.InterfaceLuid}
		if windows.GetIpInterfaceEntry(&ifc) != nil {
			continue
		}
		routes = append(routes, defaultRoute{
			gateway: nextHop(&row.NextHop, row.InterfaceIndex),
			metric:  uint64(row.Metric) + uint64(ifc.Metric),
		})
	}
	return best(routes)
}

// nextHop converts a route's next hop; a link-local one is zoned with the
// interface index, which package net accepts as a zone on Windows.
func nextHop(sa *windows.RawSockaddrInet, index uint32) netip.Addr {
	switch sa.Family {
	case windows.AF_INET:
		return netip.AddrFrom4((*windows.RawSockaddrInet4)(unsafe.Pointer(sa)).Addr)
	case windows.AF_INET6:
		ip := netip.AddrFrom16((*windows.RawSockaddrInet6)(unsafe.Pointer(sa)).Addr)
		if ip.IsLinkLocalUnicast() {
			ip = ip.WithZone(strconv.FormatUint(uint64(index), 10))
		}
		return ip
	}
	return netip.Addr{}
}
