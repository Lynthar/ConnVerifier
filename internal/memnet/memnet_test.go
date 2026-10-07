package memnet

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestDeadlineInterruptsABlockedRead(t *testing.T) {
	c := NewPacketConn("192.0.2.10:7443").Dial()
	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 8)); done <- err }()
	time.Sleep(10 * time.Millisecond)
	c.SetReadDeadline(time.Now())
	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read not interrupted")
	}
	c.SetReadDeadline(time.Time{})
	c.p.WriteTo([]byte("x"), c.LocalAddr())
	if n, err := c.Read(make([]byte, 8)); n != 1 || err != nil {
		t.Fatalf("after clearing the deadline: %d, %v", n, err)
	}
}

func TestFiltersDropAndDuplicate(t *testing.T) {
	p := NewPacketConn("192.0.2.10:7443")
	c := p.Dial()
	c.Out = func(b []byte) [][]byte {
		if b[0] == 'd' {
			return nil
		}
		return [][]byte{b, b}
	}
	c.Write([]byte("drop"))
	c.Write([]byte("keep"))
	buf := make([]byte, 8)
	for range 2 {
		n, from, err := p.ReadFrom(buf)
		if err != nil || string(buf[:n]) != "keep" || from.String() != c.LocalAddr().String() {
			t.Fatalf("got %q from %v, %v", buf[:n], from, err)
		}
	}
	p.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
	if _, _, err := p.ReadFrom(buf); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("a dropped datagram arrived: %v", err)
	}
}
