package netenv

import (
	"bufio"
	"net/netip"
	"slices"
	"strings"
)

// Resolvers returns the DNS resolvers this host is configured to ask, in its own
// order, without duplicates. A link-local one is zoned with its interface.
func Resolvers() ([]netip.Addr, error) { return resolvers() }

// nameservers reads the "nameserver" lines of a resolv.conf.
func nameservers(conf string) []netip.Addr {
	var out []netip.Addr
	s := bufio.NewScanner(strings.NewReader(conf))
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 2 || f[0] != "nameserver" {
			continue
		}
		if a, err := netip.ParseAddr(f[1]); err == nil {
			out = appendNew(out, a.Unmap())
		}
	}
	return out
}

func appendNew(list []netip.Addr, a netip.Addr) []netip.Addr {
	if slices.Contains(list, a) {
		return list
	}
	return append(list, a)
}
