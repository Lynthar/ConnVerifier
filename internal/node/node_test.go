package node

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestValidateConfig(t *testing.T) {
	cfg := Config{address: "127.0.0.1:9000"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cfg.address = ""
	if err := cfg.Validate(); err == nil {
		t.Fatalf("empty address accepted")
	}
}

func TestConnectionLimit(t *testing.T) {
	limit := makeConnectionLimit(1)
	if !acquireConnection(limit) {
		t.Fatalf("first connection should be accepted")
	}
	if acquireConnection(limit) {
		t.Fatalf("second connection should be rejected")
	}
	releaseConnection(limit)
	if !acquireConnection(limit) {
		t.Fatalf("connection should be accepted after release")
	}
}

func TestHandleEchoesBytesAndCallsDone(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go handle(server, Config{idleTimeout: time.Second}, func() {
		close(done)
	})

	payload := []byte("PING")
	if err := client.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	if _, err := client.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echo = %q, want %q", got, payload)
	}

	if err := client.Close(); err != nil {
		t.Fatalf("close client: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handler did not exit after client close")
	}
}

func TestHandleIdleTimeout(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	done := make(chan struct{})
	go handle(server, Config{idleTimeout: 10 * time.Millisecond}, func() {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handler did not exit after idle timeout")
	}
}

// chanListener hands out connections pushed into conns, so the accept loop can
// be driven without opening a socket.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{} }

func TestServeRejectsOverLimitAndStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := newChanListener()
	stopped := make(chan struct{})
	go func() {
		serve(ctx, ln, Config{maxConns: 1, idleTimeout: time.Second})
		close(stopped)
	}()

	first, firstServer := net.Pipe()
	defer first.Close()
	ln.conns <- firstServer
	first.SetDeadline(time.Now().Add(time.Second))
	if _, err := first.Write([]byte("PING")); err != nil {
		t.Fatalf("write to accepted connection: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(first, got); err != nil || string(got) != "PING" {
		t.Fatalf("echo = %q, %v; want PING", got, err)
	}

	second, secondServer := net.Pipe()
	defer second.Close()
	ln.conns <- secondServer
	second.SetDeadline(time.Now().Add(time.Second))
	if _, err := second.Read(got); err != io.EOF {
		t.Fatalf("over-limit connection read = %v, want io.EOF from an immediate close", err)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("serve did not return after cancel")
	}
	select {
	case <-ln.done:
	default:
		t.Fatal("listener not closed on shutdown")
	}
}
