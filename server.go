package main

import (
	"flag"
	"io"
	"log"
	"net"
)

func main() {
	addr := flag.String("addr", ":9000", "TCP listen address")
	flag.Parse()

	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen failed: %v", err)
	}
	log.Printf("echo server listening on %s", listener.Addr())

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Printf("accept failed: %v", err)
			continue
		}
		go handle(conn)
	}
}

func handle(conn net.Conn) {
	defer conn.Close()
	log.Printf("connection opened from %s", conn.RemoteAddr())
	defer log.Printf("connection closed from %s", conn.RemoteAddr())

	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if _, writeErr := conn.Write(buf[:n]); writeErr != nil {
				log.Printf("echo write error: %v", writeErr)
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				log.Printf("remote closed %s", conn.RemoteAddr())
			} else {
				log.Printf("read error from %s: %v", conn.RemoteAddr(), err)
			}
			return
		}
	}
}
