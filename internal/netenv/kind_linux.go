package netenv

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func kindOf(ifi net.Interface) string { return sysfsKind("/sys/class/net", ifi.Name) }

// Link types in /sys/class/net/<name>/type (ARPHRD_* in linux/if_arp.h).
const (
	arphrdEther = 1
	arphrdPPP   = 512
)

// tunnelTypes are IP-in-IP, IPv6-in-IPv6, SIT, GRE, IPv6 GRE and NONE, the type of
// tun devices and WireGuard. A tap device is Ethernet and is told by its tun_flags.
var tunnelTypes = []int{768, 769, 776, 778, 823, 65534}

// sysfsKind classifies interface name from the sysfs tree at root. Wired means an
// Ethernet link backed by a device: bridges, veth and VLANs have none and are other.
func sysfsKind(root, name string) string {
	dir := filepath.Join(root, name)
	b, err := os.ReadFile(filepath.Join(dir, "type"))
	if err != nil {
		return KindUnknown
	}
	typ, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return KindUnknown
	}
	exists := func(p string) bool {
		_, err := os.Stat(filepath.Join(dir, p))
		return err == nil
	}
	switch {
	case exists("phy80211") || exists("wireless") || devType(dir) == "wlan":
		return KindWiFi
	case typ == arphrdPPP:
		return KindPPP
	case exists("tun_flags") || slices.Contains(tunnelTypes, typ):
		return KindTunnel
	case typ == arphrdEther && exists("device"):
		return KindWired
	}
	return KindOther
}

// devType returns the DEVTYPE line of dir's uevent, "" when it has none.
func devType(dir string) string {
	f, err := os.Open(filepath.Join(dir, "uevent"))
	if err != nil {
		return ""
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if v, ok := strings.CutPrefix(s.Text(), "DEVTYPE="); ok {
			return v
		}
	}
	return ""
}
