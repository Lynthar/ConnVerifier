package netx

import "syscall"

// Winsock2.h: WSAEMFILE 10024, WSAEADDRNOTAVAIL 10049, WSAENOBUFS 10055; package
// syscall does not name them on Windows.
var resourceErrnos = []error{syscall.Errno(10024), syscall.Errno(10049), syscall.Errno(10055)}
