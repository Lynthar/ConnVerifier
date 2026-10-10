package netx

import (
	"net"
	"syscall"
)

func udpBuffers(pc net.PacketConn) (recv, send int, ok bool) {
	sc, isSys := pc.(syscall.Conn)
	if !isSys {
		return 0, 0, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var rerr, serr error
	if err := raw.Control(func(fd uintptr) {
		recv, rerr = syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF)
		send, serr = syscall.GetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF)
	}); err != nil {
		return 0, 0, false
	}
	return recv, send, rerr == nil && serr == nil
}
