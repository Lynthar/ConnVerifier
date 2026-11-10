package dns

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

// Rows follow the "Status rules" table of docs/methods/dns.md, in order; rules 0a
// and 0b are decided before any query and covered by TestRun.
func TestStatusRules(t *testing.T) {
	refused := errors.New("connection refused")
	for _, tt := range []struct {
		name        string
		t           tally
		sendErr     error
		interrupted bool
		status      result.Status
		key         string
	}{
		{"1 could not send", tally{sent: 20}, refused, false, result.Warn, "dns.inference.unreachable"},
		{"1 unanswered", tally{sent: 20, replies: 19}, nil, false, result.Warn, "dns.inference.unanswered"},
		{"1 nothing answered", tally{sent: 20}, nil, false, result.Warn, "dns.inference.unanswered"},
		{"1 error answers", tally{sent: 20, replies: 20, errors: 2, codes: map[int]uint64{2: 2}}, nil, false, result.Warn, "dns.inference.error_replies"},
		{"2 clean", tally{sent: 20, replies: 20}, nil, false, result.Pass, ""},
		{"2 clean despite a send error", tally{sent: 20, replies: 20}, refused, false, result.Pass, ""},
		{"interrupted before any query", tally{}, nil, true, result.Invalid, "dns.warning.interrupted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var c result.Check
			evaluate(&c, tt.t, tt.sendErr, tt.interrupted)
			if c.Status != tt.status || (tt.key != "" && !slices.Contains(messageKeys(c), tt.key)) {
				t.Fatalf("status %s messages %v; want %s with %s", c.Status, messageKeys(c), tt.status, tt.key)
			}
		})
	}
}

func TestCodesText(t *testing.T) {
	if got := codesText(map[int]uint64{5: 1, 2: 3, 9: 1}); got != "SERVFAIL ×3, REFUSED ×1, RCODE 9 ×1" {
		t.Fatalf("codesText = %q", got)
	}
}
