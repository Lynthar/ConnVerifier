//go:build unix

package netx

import (
	"context"
	"net"
	"syscall"
	"testing"
	"time"
)

// Only the socket option proves keepalive is off: package net's zero value enables
// it silently, so reading the code path is not enough.
func TestKeepAliveSocketOption(t *testing.T) {
	tests := []struct {
		name      string
		keepAlive time.Duration
		want      int
	}{
		{"zero disables", 0, 0},
		{"negative disables", -time.Second, 0},
		{"positive enables", 30 * time.Second, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			ln, err := Listen(ctx, "127.0.0.1:0", tt.keepAlive)
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			defer ln.Close()

			accepted := make(chan net.Conn, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					close(accepted)
					return
				}
				accepted <- conn
			}()

			dialed, err := Dialer(time.Second, tt.keepAlive).DialContext(ctx, "tcp", ln.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer dialed.Close()
			server, ok := <-accepted
			if !ok {
				t.Fatal("accept failed")
			}
			defer server.Close()

			for side, conn := range map[string]net.Conn{"dialed": dialed, "accepted": server} {
				if got := soKeepAlive(t, conn); got != tt.want {
					t.Errorf("%s SO_KEEPALIVE = %d, want %d", side, got, tt.want)
				}
			}
		})
	}
}

func soKeepAlive(t *testing.T, conn net.Conn) int {
	t.Helper()
	raw, err := conn.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatalf("syscall conn: %v", err)
	}
	var v int
	var sockErr error
	if err := raw.Control(func(fd uintptr) {
		v, sockErr = syscall.GetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_KEEPALIVE)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if sockErr != nil {
		t.Fatalf("getsockopt: %v", sockErr)
	}
	if v != 0 {
		return 1 // some platforms report the option as a non-zero flag value
	}
	return 0
}
