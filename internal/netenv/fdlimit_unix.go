//go:build linux || darwin

package netenv

import "syscall"

func fdLimit() (uint64, bool) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, false
	}
	return r.Cur, true
}
