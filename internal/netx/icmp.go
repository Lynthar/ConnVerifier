//go:build !windows

package netx

import "golang.org/x/net/icmp"

// ListenICMP opens an unprivileged ICMP echo socket for family "ipv4" or "ipv6".
// Linux grants it only to groups in net.ipv4.ping_group_range and replaces the
// echo identifier with its own, so replies must be matched by sequence number.
func ListenICMP(family string) (*icmp.PacketConn, error) {
	if family == "ipv6" {
		return icmp.ListenPacket("udp6", "::")
	}
	return icmp.ListenPacket("udp4", "0.0.0.0")
}
