//go:build !linux && !darwin && !windows

package netenv

import "net"

func kindOf(net.Interface) string { return KindUnknown }
