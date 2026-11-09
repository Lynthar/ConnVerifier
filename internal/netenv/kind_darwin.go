package netenv

import (
	"net"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// tunnelPrefixes name macOS tunnel interfaces: utun is what VPN apps and network
// extensions create, ipsec the built-in IKEv2 client, gif and stf the kernel's own.
var tunnelPrefixes = []string{"utun", "ipsec", "gif", "stf"}

func kindOf(ifi net.Interface) string {
	for _, p := range tunnelPrefixes {
		if strings.HasPrefix(ifi.Name, p) {
			return KindTunnel
		}
	}
	if strings.HasPrefix(ifi.Name, "ppp") {
		return KindPPP
	}
	switch t, ok := functionalType(ifi.Name); {
	case !ok:
		return KindUnknown
	case t == ifrTypeWired:
		return KindWired
	case t == ifrTypeWiFiInfra:
		return KindWiFi
	}
	return KindOther
}

// IFRTYPE_FUNCTIONAL_* in net/if.h.
const (
	ifrTypeWired     = 2
	ifrTypeWiFiInfra = 3
)

// ifreqFunctional is struct ifreq with the functional-type member of its union;
// the ioctl copies all 32 bytes of struct ifreq, so the padding must be there.
type ifreqFunctional struct {
	name [unix.IFNAMSIZ]byte
	typ  uint32
	_    [12]byte
}

func functionalType(name string) (uint32, bool) {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return 0, false
	}
	defer unix.Close(fd)
	var req ifreqFunctional
	copy(req.name[:], name)
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(unix.SIOCGIFFUNCTIONALTYPE), uintptr(unsafe.Pointer(&req)))
	return req.typ, errno == 0
}
