package netx

import (
	"errors"

	"golang.org/x/net/icmp"
)

// ListenICMP fails: Windows has no unprivileged ICMP socket.
func ListenICMP(string) (*icmp.PacketConn, error) {
	return nil, errors.New("unprivileged ICMP sockets are not available on Windows")
}
