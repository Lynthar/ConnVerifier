//go:build go1.27

package node

import "net/http"

// roundRobin makes HTTP/2 interleave streams. Go 1.27's default serves streams of
// equal priority one after another, so a probe would wait behind a whole load stream.
func roundRobin(hs *http.Server) { hs.DisableClientPriority = true }
