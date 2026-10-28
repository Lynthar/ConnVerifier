//go:build linux || darwin

package netx

import (
	"context"
	"io"
	"net"
	"testing"
)

func TestTCPSent(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		s, err := ln.Accept()
		if err == nil {
			io.Copy(io.Discard, s)
			s.Close()
		}
	}()
	c, err := Dialer(0, 0).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	sent, resent, ok := TCPSent(c)
	if !ok || sent == 0 || resent > sent {
		t.Fatalf("TCPSent = %d, %d, %v after a 1 MiB write; want some segments sent", sent, resent, ok)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if _, _, ok := TCPSent(a); ok {
		t.Fatal("a pipe has no TCP counters")
	}
}
