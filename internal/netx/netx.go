// Package netx is the only place the tool dials or listens, so TCP keepalive is
// decided once: its probes refresh NAT mappings and would corrupt the idle-timeout
// measurements the tool exists to make.
package netx

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"time"
)

// quic-go writes to stderr when it cannot raise a UDP buffer to 7 MiB; the load
// result reports the buffers instead (UDPBuffers), and stdout/stderr stay clean.
func init() { os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "true") }

// Dialer returns a TCP dialer with the given dial timeout. keepAlive is the probe
// period on dialed connections; keepAlive <= 0 disables keepalive.
func Dialer(timeout, keepAlive time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: keepAlivePeriod(keepAlive)}
}

// Listen opens a TCP listener on address. keepAlive is the probe period on accepted
// connections; keepAlive <= 0 disables keepalive.
func Listen(ctx context.Context, address string, keepAlive time.Duration) (net.Listener, error) {
	lc := net.ListenConfig{KeepAlive: keepAlivePeriod(keepAlive)}
	return lc.Listen(ctx, "tcp", address)
}

// ListenPacket opens a UDP socket on address: the node answers STAMP and QUIC on
// one each, and each QUIC connection of a client sends from its own (":0").
func ListenPacket(ctx context.Context, address string) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, "udp", address)
}

// DialUDP opens a UDP socket connected to address, so only its replies arrive.
func DialUDP(ctx context.Context, address string) (*net.UDPConn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	return c.(*net.UDPConn), nil
}

// keepAlivePeriod maps "<= 0 disables" onto package net, where zero silently
// enables 15s probes and only a negative value disables them.
func keepAlivePeriod(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
}

// UDPBuffers reports the receive and send buffer sizes pc got, as the kernel
// reports them (Linux doubles what was asked for); ok is false where they cannot
// be read.
func UDPBuffers(pc net.PacketConn) (recv, send int, ok bool) { return udpBuffers(pc) }

// SetNotSentLowat caps the data the kernel holds unsent for c at n bytes, so a
// sender's own buffer does not add to the round trips it measures
// (draft-ietf-ippm-responsiveness §6.1.1). It reports false where the platform
// or the connection has no such option.
func SetNotSentLowat(c net.Conn, n int) bool { return setNotSentLowat(c, n) }

// WriteFull writes all of data to conn and returns the first error; a write that
// makes no progress without an error is reported as io.ErrShortWrite.
func WriteFull(conn net.Conn, data []byte) error {
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// IsClientResource reports whether a dial error means this host ran out of file
// descriptors, ephemeral ports or socket buffers: a limit of the tool host, not
// of the network under test.
func IsClientResource(err error) bool {
	for _, e := range resourceErrnos {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
