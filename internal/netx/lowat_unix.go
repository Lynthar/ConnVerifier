//go:build linux || darwin

package netx

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func setNotSentLowat(c net.Conn, n int) bool {
	sc, ok := c.(syscall.Conn)
	if !ok {
		return false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return false
	}
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT, n)
	}); err != nil {
		return false
	}
	return serr == nil
}
