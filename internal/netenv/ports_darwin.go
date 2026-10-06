package netenv

import "syscall"

func ephemeralPorts() (int, bool) {
	first, err1 := syscall.SysctlUint32("net.inet.ip.portrange.first")
	last, err2 := syscall.SysctlUint32("net.inet.ip.portrange.last")
	if err1 != nil || err2 != nil || last < first {
		return 0, false
	}
	return int(last-first) + 1, true
}
