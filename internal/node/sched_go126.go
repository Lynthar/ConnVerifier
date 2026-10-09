//go:build !go1.27

package node

import "net/http"

// roundRobin is a no-op: before Go 1.27 HTTP/2 interleaves streams by default.
func roundRobin(*http.Server) {}
