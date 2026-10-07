// Package memnet is an in-memory network for tests: a node-side PacketConn with
// connected client Conns, and a pipe-fed stream Listener. Only tests import it,
// so it never reaches the binary.
package memnet

import (
	"context"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// Filter sees each datagram on its way and returns the copies to deliver: none
// drops it, two duplicate it. A nil Filter delivers every datagram once.
type Filter func(b []byte) [][]byte

type datagram struct {
	data []byte
	from net.Addr
}

// PacketConn is the node's end. Datagrams written to an address that is not a
// live Conn are dropped, as UDP would.
type PacketConn struct {
	addr  *net.UDPAddr
	in    chan datagram
	dl    deadline
	once  sync.Once
	done  chan struct{}
	mu    sync.Mutex // guards conns and next
	conns map[netip.AddrPort]*Conn
	next  uint16
	// Out filters node → client datagrams; set it before traffic flows.
	Out Filter
}

// NewPacketConn returns a node end listening on addr.
func NewPacketConn(addr string) *PacketConn {
	return &PacketConn{
		addr:  net.UDPAddrFromAddrPort(netip.MustParseAddrPort(addr)),
		in:    make(chan datagram, 4096),
		dl:    newDeadline(),
		done:  make(chan struct{}),
		conns: make(map[netip.AddrPort]*Conn),
		next:  40000,
	}
}

// Dial returns a client Conn connected to p with a fresh source address.
func (p *PacketConn) Dial() *Conn {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	ap := netip.AddrPortFrom(netip.MustParseAddr("192.0.2.200"), p.next)
	c := &Conn{p: p, addr: net.UDPAddrFromAddrPort(ap), in: make(chan []byte, 4096), dl: newDeadline(), done: make(chan struct{})}
	p.conns[ap] = c
	return c
}

func (p *PacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case d := <-p.in:
		return copy(b, d.data), d.from, nil
	case <-p.done:
		return 0, nil, net.ErrClosed
	case <-p.dl.wait():
		return 0, nil, os.ErrDeadlineExceeded
	}
}

func (p *PacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	select {
	case <-p.done:
		return 0, net.ErrClosed
	default:
	}
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	c := p.conns[ap]
	p.mu.Unlock()
	if c != nil {
		for _, d := range apply(p.Out, b) {
			c.deliver(d)
		}
	}
	return len(b), nil
}

func (p *PacketConn) Close() error {
	p.once.Do(func() { close(p.done) })
	return nil
}

func (p *PacketConn) LocalAddr() net.Addr               { return p.addr }
func (p *PacketConn) SetDeadline(t time.Time) error     { p.dl.set(t); return nil }
func (p *PacketConn) SetReadDeadline(t time.Time) error { p.dl.set(t); return nil }
func (p *PacketConn) SetWriteDeadline(time.Time) error  { return nil }

// Conn is a client's connected datagram socket.
type Conn struct {
	p    *PacketConn
	addr *net.UDPAddr
	in   chan []byte
	dl   deadline
	once sync.Once
	done chan struct{}
	// Out filters client → node datagrams; set it before traffic flows.
	Out Filter
}

func (c *Conn) deliver(b []byte) {
	select {
	case c.in <- b:
	default: // a full socket buffer drops, as UDP would
	}
}

func (c *Conn) Read(b []byte) (int, error) {
	select {
	case d := <-c.in:
		return copy(b, d), nil
	case <-c.done:
		return 0, net.ErrClosed
	case <-c.dl.wait():
		return 0, os.ErrDeadlineExceeded
	}
}

func (c *Conn) Write(b []byte) (int, error) {
	select {
	case <-c.done:
		return 0, net.ErrClosed
	default:
	}
	for _, d := range apply(c.Out, b) {
		select {
		case c.p.in <- datagram{data: d, from: c.addr}:
		default:
		}
	}
	return len(b), nil
}

func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.done)
		c.p.mu.Lock()
		delete(c.p.conns, c.addr.AddrPort())
		c.p.mu.Unlock()
	})
	return nil
}

func (c *Conn) LocalAddr() net.Addr               { return c.addr }
func (c *Conn) RemoteAddr() net.Addr              { return c.p.addr }
func (c *Conn) SetDeadline(t time.Time) error     { c.dl.set(t); return nil }
func (c *Conn) SetReadDeadline(t time.Time) error { c.dl.set(t); return nil }
func (c *Conn) SetWriteDeadline(time.Time) error  { return nil }

func apply(f Filter, b []byte) [][]byte {
	cp := append([]byte(nil), b...)
	if f == nil {
		return [][]byte{cp}
	}
	return f(cp)
}

// deadline is a resettable read deadline: setting it wakes a blocked reader, so a
// deadline moved to now interrupts a read as it does on a real socket.
type deadline struct {
	mu    sync.Mutex
	timer *time.Timer
	ch    chan struct{} // closed while the deadline has passed
}

func newDeadline() deadline { return deadline{ch: make(chan struct{})} }

func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.timer != nil {
		d.timer.Stop()
		d.timer = nil
	}
	if isClosed(d.ch) {
		d.ch = make(chan struct{})
	}
	if t.IsZero() {
		return
	}
	ch := d.ch
	dur := time.Until(t)
	if dur <= 0 {
		close(ch)
		return
	}
	var tm *time.Timer
	tm = time.AfterFunc(dur, func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.timer == tm && !isClosed(ch) { // a stopped timer may still fire once
			close(ch)
		}
	})
	d.timer = tm
}

func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.ch
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// Listener is a stream listener fed by net.Pipe ends, so a real node can serve
// TCP in memory.
type Listener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func NewListener() *Listener {
	return &Listener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *Listener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *Listener) Addr() net.Addr { return &net.TCPAddr{} }

// Dial hands the listener the far end of a new pipe; network and address are
// ignored, so it fits wherever a dial function is expected.
func (l *Listener) Dial(ctx context.Context, _, _ string) (net.Conn, error) {
	c, s := net.Pipe()
	select {
	case l.conns <- s:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}
