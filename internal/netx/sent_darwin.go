package netx

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func tcpSent(c net.Conn) (sent, resent uint64, ok bool) {
	sc, isSys := c.(syscall.Conn)
	if !isSys {
		return 0, 0, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0, 0, false
	}
	var info *unix.TCPConnectionInfo
	var serr error
	if err := raw.Control(func(fd uintptr) {
		info, serr = unix.GetsockoptTCPConnectionInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_CONNECTION_INFO)
	}); err != nil || serr != nil {
		return 0, 0, false
	}
	return info.Txpackets, info.Txretransmitpackets, true
}
