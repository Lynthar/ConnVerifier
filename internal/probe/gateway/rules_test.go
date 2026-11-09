package gateway

import (
	"errors"
	"slices"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/result"
)

func messageKeys(c result.Check) []string {
	var k []string
	for _, group := range [][]result.Message{c.Inferences, c.NotProven, c.Warnings} {
		for _, m := range group {
			k = append(k, m.Key)
		}
	}
	return k
}

// Rows follow the "Status rules" table of docs/methods/gateway.md, in order; rules
// 0a to 0c are decided before probing and covered by TestRun.
func TestStatusRules(t *testing.T) {
	noICMP := errors.New("permission denied")
	for _, tt := range []struct {
		name        string
		o           outcome
		lost        uint64
		interrupted bool
		status      result.Status
		key         string
	}{
		{"1 nothing answered", outcome{}, 0, false, result.Unsupported, "gateway.unsupported.no_answer"},
		{"1 no ICMP, no handshake answered", outcome{icmpErr: noICMP}, 0, false, result.Unsupported, "gateway.unsupported.no_answer_no_icmp"},
		{"1 interrupted before an answer", outcome{}, 0, true, result.Invalid, "gateway.warning.interrupted"},
		{"2 ICMP loss", outcome{protocol: "icmp"}, 1, false, result.Warn, "gateway.inference.loss_icmp"},
		{"2 handshake loss", outcome{protocol: "tcp", port: 80}, 1, false, result.Warn, "gateway.inference.loss_tcp"},
		{"3 clean", outcome{protocol: "icmp"}, 0, false, result.Pass, ""},
		{"3 clean, interrupted after answers", outcome{protocol: "tcp", port: 53}, 0, true, result.Pass, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var c result.Check
			evaluate(&c, tt.o, tt.lost, tt.interrupted)
			if c.Status != tt.status || (tt.key != "" && !slices.Contains(messageKeys(c), tt.key)) {
				t.Fatalf("status %s messages %v; want %s with %s", c.Status, messageKeys(c), tt.status, tt.key)
			}
		})
	}
}
