//go:build !linux && !darwin && !windows

package netenv

import (
	"errors"
	"net/netip"
)

func defaultGateway(string) (netip.Addr, error) {
	return netip.Addr{}, errors.New("reading the routing table is not supported on this platform")
}
