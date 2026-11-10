package netenv

import (
	"net/netip"
	"os"
	"slices"
)

// resolvedStub is systemd-resolved's local stub. Behind it, the resolvers the host
// actually uses are in resolvedUpstream; asking the stub would time its cache.
var resolvedStub = netip.MustParseAddr("127.0.0.53")

const resolvedUpstream = "/run/systemd/resolve/resolv.conf"

func resolvers() ([]netip.Addr, error) {
	conf, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil, err
	}
	list := nameservers(string(conf))
	if slices.Contains(list, resolvedStub) {
		if up, err := os.ReadFile(resolvedUpstream); err == nil {
			return nameservers(string(up)), nil
		}
	}
	return list, nil
}
