package netenv

import (
	"context"
	"net"
	"net/netip"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/netx"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// Interface kinds. A PPP link is broadband dial-up or a VPN, which nothing on the
// host tells apart; Unknown means the platform could not tell the kind.
const (
	KindWiFi    = "wifi"
	KindWired   = "wired"
	KindTunnel  = "tunnel"
	KindPPP     = "ppp"
	KindOther   = "other"
	KindUnknown = "unknown"
)

// Route is where traffic to one address leaves this host.
type Route struct {
	Family    string // "ipv4" or "ipv6"
	Interface string // the interface's name, "" when no interface holds the source address
	Kind      string
}

// RouteTo finds the interface the kernel routes traffic to hostPort by: it connects
// a UDP socket, which sends nothing, and looks up which interface holds the source
// address the kernel chose. A host name is resolved first.
func RouteTo(ctx context.Context, hostPort string) (Route, error) {
	c, err := netx.DialUDP(ctx, hostPort)
	if err != nil {
		return Route{}, err
	}
	local := c.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap().WithZone("")
	c.Close()
	r := Route{Family: "ipv6", Kind: KindUnknown}
	if local.Is4() {
		r.Family = "ipv4"
	}
	if ifi, ok := interfaceWith(local); ok {
		r.Interface, r.Kind = ifi.Name, kindOf(ifi)
	}
	return r, nil
}

func interfaceWith(addr netip.Addr) (net.Interface, bool) {
	ifs, err := net.Interfaces()
	if err != nil {
		return net.Interface{}, false
	}
	for _, ifi := range ifs {
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				if ip, ok := netip.AddrFromSlice(n.IP); ok && ip.Unmap() == addr {
					return ifi, true
				}
			}
		}
	}
	return net.Interface{}, false
}

// routeTimeout bounds the lookups Host makes, resolving a host name included.
const routeTimeout = 2 * time.Second

// Host is the result's host object: for each address family the checks ran on,
// the kind of interface the route to their node leaves by, or nil when none ran on
// a node. It keeps its own deadline, so it answers after an interrupted run too.
func Host(checks []result.Check) *result.Host {
	ctx, cancel := context.WithTimeout(context.Background(), routeTimeout)
	defer cancel()
	var h result.Host
	nodes, families := map[string]bool{}, map[string]bool{}
	for _, c := range checks {
		if c.Status == result.Skip || c.Path.Node == "" || nodes[c.Path.Node] {
			continue
		}
		nodes[c.Path.Node] = true
		r, err := RouteTo(ctx, c.Path.Node)
		if err != nil {
			r = Route{Family: c.Path.Family, Kind: KindUnknown}
		}
		if !families[r.Family] {
			families[r.Family] = true
			h.Interfaces = append(h.Interfaces, result.Interface{Family: r.Family, Kind: r.Kind})
		}
	}
	if len(h.Interfaces) == 0 {
		return nil
	}
	return &h
}
