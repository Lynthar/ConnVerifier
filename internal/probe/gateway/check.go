package gateway

import (
	"time"

	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func msg(key string, params map[string]any, basis ...string) result.Message {
	return result.Message{Key: key, Params: params, Basis: basis}
}

func skip(c *result.Check, key string) {
	c.Status = result.Skip
	c.Warnings = append(c.Warnings, msg(key, map[string]any{"family": c.Path.Family}))
}

// finish turns what the probes saw into the result. interrupted is whether the
// run ended before the check did.
func finish(c *result.Check, o outcome, interrupted bool) {
	c.Path.Protocol = o.protocol
	var sent, ok uint64
	var rtt []time.Duration
	for _, r := range o.replies {
		if r.cut {
			continue
		}
		sent++
		if r.ok {
			ok++
			rtt = append(rtt, r.rtt)
		}
	}
	if o.protocol != "" {
		c.Metrics = append([]result.Metric{
			result.Count("sent", sent),
			result.Count("replies", ok),
			result.Count("lost", sent-ok),
			result.Ratio("loss", sent-ok, sent),
		}, echo.DelayMetrics("", rtt)...)
	}
	if o.protocol == "tcp" {
		c.Params["tcp_port"] = o.port
		if o.icmpErr != nil {
			c.NotProven = append(c.NotProven, msg("gateway.not_proven.tcp_no_icmp", map[string]any{"port": o.port, "error": o.icmpErr.Error()}))
		} else {
			c.NotProven = append(c.NotProven, msg("gateway.not_proven.tcp", map[string]any{"port": o.port}))
		}
	}
	evaluate(c, o, sent-ok, interrupted)
}

// evaluate applies the status rules that follow the probing, in order (method
// document, "Status rules"); TestStatusRules mirrors that table.
func evaluate(c *result.Check, o outcome, lost uint64, interrupted bool) {
	switch {
	case o.protocol == "" && interrupted:
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("gateway.warning.interrupted", nil))
	case o.protocol == "" && o.icmpErr != nil:
		c.Status = result.Unsupported
		c.Warnings = append(c.Warnings, msg("gateway.unsupported.no_answer_no_icmp", map[string]any{"error": o.icmpErr.Error()}))
	case o.protocol == "":
		c.Status = result.Unsupported
		c.Warnings = append(c.Warnings, msg("gateway.unsupported.no_answer", nil))
	case lost > 0 && o.protocol == "icmp":
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("gateway.inference.loss_icmp", map[string]any{"count": lost, "wait_ms": replyWait.Milliseconds()}, "lost", "loss"))
	case lost > 0:
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("gateway.inference.loss_tcp", map[string]any{"count": lost, "wait_ms": replyWait.Milliseconds()}, "lost", "loss"))
	default:
		c.Status = result.Pass
	}
}
