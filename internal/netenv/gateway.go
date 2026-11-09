package netenv

import (
	"cmp"
	"errors"
	"net/netip"
	"slices"
)

// Errors from DefaultGateway.
var (
	ErrNoDefaultRoute = errors.New("no default route")
	ErrNoGateway      = errors.New("the default route leaves by an interface without a gateway")
)

// DefaultGateway returns the next hop of family's ("ipv4" or "ipv6") default route,
// the one with the lowest metric when there are several. A link-local gateway is
// zoned with its interface, without which no socket can reach it.
func DefaultGateway(family string) (netip.Addr, error) { return defaultGateway(family) }

// defaultRoute is a default route as the platform lists it; gateway is invalid or
// unspecified when the route has none.
type defaultRoute struct {
	gateway netip.Addr
	metric  uint64
}

func best(routes []defaultRoute) (netip.Addr, error) {
	if len(routes) == 0 {
		return netip.Addr{}, ErrNoDefaultRoute
	}
	r := slices.MinFunc(routes, func(a, b defaultRoute) int { return cmp.Compare(a.metric, b.metric) })
	if !r.gateway.IsValid() || r.gateway.IsUnspecified() {
		return netip.Addr{}, ErrNoGateway
	}
	return r.gateway, nil
}

// Address classes: what a result may say about an address it must not name.
const (
	ClassLoopback     = "loopback"
	ClassLinkLocal    = "link_local"
	ClassPrivate      = "private"      // RFC 1918, IPv6 unique local
	ClassShared       = "shared"       // RFC 6598, carrier-grade NAT
	ClassBenchmarking = "benchmarking" // 198.18.0.0/15, where proxy tools put fake-ip
	ClassPublic       = "public"
	ClassOther        = "other"
)

var (
	sharedSpace  = netip.MustParsePrefix("100.64.0.0/10")
	benchmarking = netip.MustParsePrefix("198.18.0.0/15")
)

// AddrClass returns the class of a.
func AddrClass(a netip.Addr) string {
	a = a.Unmap().WithZone("")
	switch {
	case a.IsLoopback():
		return ClassLoopback
	case a.IsLinkLocalUnicast():
		return ClassLinkLocal
	case a.IsPrivate():
		return ClassPrivate
	case sharedSpace.Contains(a):
		return ClassShared
	case benchmarking.Contains(a):
		return ClassBenchmarking
	case a.IsGlobalUnicast():
		return ClassPublic
	}
	return ClassOther
}
