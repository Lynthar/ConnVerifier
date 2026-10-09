//go:build unix

package netenv

import (
	"syscall"
	"time"
)

func processCPU() (time.Duration, bool) {
	var r syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &r); err != nil {
		return 0, false
	}
	return time.Duration(r.Utime.Nano() + r.Stime.Nano()), true
}
