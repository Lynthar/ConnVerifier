package baseline

import (
	"errors"
	"slices"
	"testing"

	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/protocol"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func messageKeys(c result.Check) []string {
	var k []string
	for _, group := range [][]result.Message{c.Inferences, c.NotProven, c.Warnings} {
		for _, m := range group {
			k = append(k, m.Key)
		}
	}
	if c.Error != nil {
		k = append(k, c.Error.Key)
	}
	return k
}

// Rows follow the "Status rules" table of docs/methods/udp-baseline.md, in order.
func TestUDPStatusRules(t *testing.T) {
	clean := echo.Outcome{Sent: 1000, Received: 1000}
	node := &protocol.StampCounts{Received: 1000}
	for _, tt := range []struct {
		name   string
		o      echo.Outcome
		counts *protocol.StampCounts
		status result.Status
		key    string
	}{
		{"0 nothing sent", echo.Outcome{}, node, result.Error, "udp_baseline.error.nothing_sent"},
		{"1 nothing reached the node", echo.Outcome{Sent: 1000}, &protocol.StampCounts{}, result.Error, "udp_baseline.error.no_udp_to_node"},
		{"1 no reply, node silent", echo.Outcome{Sent: 1000}, nil, result.Error, "udp_baseline.error.no_reply"},
		{"1 node received, nothing came back", echo.Outcome{Sent: 1000}, node, result.Warn, "udp_baseline.inference.return_blocked"},
		{"2 this host fell behind", echo.Outcome{Sent: 1000, Received: 1000, Slipped: 11}, node, result.Invalid, "baseline.warning.send_slip"},
		{"2 the node was slow", echo.Outcome{Sent: 1000, Received: 1000, SlowNode: 11}, node, result.Invalid, "udp_baseline.warning.node_slow"},
		{"2 over the granted rate", clean, &protocol.StampCounts{Received: 1000, OverRate: 1}, result.Invalid, "udp_baseline.warning.over_rate"},
		{"2 source address changed", clean, &protocol.StampCounts{Received: 1000, OtherAddr: 1}, result.Invalid, "udp_baseline.warning.address_changed"},
		{"3 loss", echo.Outcome{Sent: 1000, Received: 999}, node, result.Warn, "udp_baseline.inference.loss"},
		{"3 duplicates", echo.Outcome{Sent: 1000, Received: 1000, DupReturn: 1}, node, result.Warn, "udp_baseline.inference.duplicates"},
		{"3 reordering", echo.Outcome{Sent: 1000, Received: 1000, Reordered: 1}, node, result.Warn, "udp_baseline.inference.reordering"},
		{"4 clean", clean, node, result.Pass, ""},
		{"1% slip is still valid", echo.Outcome{Sent: 1000, Received: 1000, Slipped: 10}, node, result.Pass, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var c result.Check
			evaluateUDP(&c, tt.o, tt.counts)
			if c.Status != tt.status || (tt.key != "" && !slices.Contains(messageKeys(c), tt.key)) {
				t.Fatalf("status %s messages %v; want %s with %s", c.Status, messageKeys(c), tt.status, tt.key)
			}
		})
	}
}

// Rows follow the "Status rules" table of docs/methods/tcp-baseline.md, in order.
func TestTCPStatusRules(t *testing.T) {
	broke := errors.New("connection reset")
	for _, tt := range []struct {
		name   string
		o      echo.Outcome
		broke  error
		status result.Status
		key    string
	}{
		{"1 broke before any echo", echo.Outcome{Sent: 10}, broke, result.Error, "tcp_baseline.error.broke"},
		{"2 this host fell behind", echo.Outcome{Sent: 1000, Received: 1000, Slipped: 11}, nil, result.Invalid, "baseline.warning.send_slip"},
		{"3 broke during the run", echo.Outcome{Sent: 500, Received: 500}, broke, result.Warn, "tcp_baseline.inference.broke"},
		{"3 stalls", echo.Outcome{Sent: 1000, Received: 998}, nil, result.Warn, "tcp_baseline.inference.stalls"},
		{"4 clean", echo.Outcome{Sent: 1000, Received: 1000}, nil, result.Pass, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var c result.Check
			evaluateTCP(&c, tt.o, tt.broke)
			if c.Status != tt.status || (tt.key != "" && !slices.Contains(messageKeys(c), tt.key)) {
				t.Fatalf("status %s messages %v; want %s with %s", c.Status, messageKeys(c), tt.status, tt.key)
			}
		})
	}
}

func TestTCPRefusals(t *testing.T) {
	for _, tt := range []struct {
		f      echo.DataFailure
		status result.Status
		key    string
	}{
		{echo.DataFailure{Kind: "reject", Reject: protocol.Frame{Reason: protocol.ReasonBusy}}, result.Invalid, "tcp_baseline.warning.node_refused"},
		{echo.DataFailure{Kind: "reject", Reject: protocol.Frame{Reason: protocol.ReasonAuth}}, result.Error, "tcp_baseline.error.rejected"},
		{echo.DataFailure{Kind: "dial", Err: errors.New("refused")}, result.Error, "tcp_baseline.error.no_connection"},
		{echo.DataFailure{Kind: "handshake", Err: errors.New("eof")}, result.Error, "tcp_baseline.error.handshake_failed"},
	} {
		var c result.Check
		tcpFailed(&c, &tt.f)
		if c.Status != tt.status || !slices.Contains(messageKeys(c), tt.key) {
			t.Errorf("%s: status %s messages %v", tt.key, c.Status, messageKeys(c))
		}
	}
}
