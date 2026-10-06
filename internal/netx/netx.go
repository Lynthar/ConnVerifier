// Package netx is the only place the tool dials or listens, so TCP keepalive is
// decided once: its probes refresh NAT mappings and would corrupt the idle-timeout
// measurements the tool exists to make.
package netx

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

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

// keepAlivePeriod maps "<= 0 disables" onto package net, where zero silently
// enables 15s probes and only a negative value disables them.
func keepAlivePeriod(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
}

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
