package netenv

import (
	"net/netip"

	"golang.org/x/sys/windows"
)

// siteLocal is fec0::/10. Windows lists fec0:0:0:ffff::1 to ::3 on every adapter
// with no IPv6 resolver configured; nothing answers there.
var siteLocal = netip.MustParsePrefix("fec0::/10")

// resolvers collects the resolvers of every adapter that is up, in adapter order.
func resolvers() ([]netip.Addr, error) {
	aa, err := adapters(windows.GAA_FLAG_SKIP_UNICAST | windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_FRIENDLY_NAME)
	if err != nil {
		return nil, err
	}
	var list []netip.Addr
	for a := aa; a != nil; a = a.Next {
		if a.OperStatus != windows.IfOperStatusUp || a.IfType == windows.IF_TYPE_SOFTWARE_LOOPBACK {
			continue
		}
		for d := a.FirstDnsServerAddress; d != nil; d = d.Next {
			ip, ok := netip.AddrFromSlice(d.Address.IP())
			if !ok || siteLocal.Contains(ip.Unmap()) {
				continue
			}
			list = appendNew(list, ip.Unmap())
		}
	}
	return list, nil
}
