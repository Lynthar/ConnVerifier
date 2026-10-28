//go:build !linux && !darwin

package netx

import "net"

func tcpSent(net.Conn) (sent, resent uint64, ok bool) { return 0, 0, false }
