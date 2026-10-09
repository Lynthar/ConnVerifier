//go:build linux || darwin

package netx

import (
	"context"
	"net"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSetNotSentLowat(t *testing.T) {
	ln, err := Listen(context.Background(), "127.0.0.1:0", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	c, err := Dialer(0, 0).Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if !SetNotSentLowat(c, 65536) {
		t.Fatal("SetNotSentLowat failed on a TCP connection")
	}
	raw, _ := c.(syscall.Conn).SyscallConn()
	var got int
	raw.Control(func(fd uintptr) { got, _ = unix.GetsockoptInt(int(fd), unix.IPPROTO_TCP, unix.TCP_NOTSENT_LOWAT) })
	if got != 65536 {
		t.Fatalf("TCP_NOTSENT_LOWAT reads back %d, want 65536", got)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if SetNotSentLowat(a, 65536) {
		t.Fatal("a pipe has no such option")
	}
}
