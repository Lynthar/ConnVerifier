package main

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestValidateServerConfig(t *testing.T) {
	cfg := ServerConfig{address: "127.0.0.1:9000"}
	if err := validateServerConfig(cfg); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cfg.address = ""
	if err := validateServerConfig(cfg); err == nil {
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
	go handle(server, ServerConfig{idleTimeout: time.Second}, func() {
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
	go handle(server, ServerConfig{idleTimeout: 10 * time.Millisecond}, func() {
		close(done)
	})

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("handler did not exit after idle timeout")
	}
}
