//go:build !linux && !darwin && !windows

package netenv

import (
	"errors"
	"net/netip"
)

func resolvers() ([]netip.Addr, error) {
	return nil, errors.New("reading the DNS configuration is not supported on this platform")
}
