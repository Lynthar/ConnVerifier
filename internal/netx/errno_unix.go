//go:build !windows

package netx

import "syscall"

var resourceErrnos = []error{syscall.EMFILE, syscall.ENFILE, syscall.EADDRNOTAVAIL, syscall.ENOBUFS}
