//go:build !linux && !darwin

package netx

import "net"

func setNotSentLowat(net.Conn, int) bool { return false }
