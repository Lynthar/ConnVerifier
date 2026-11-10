package dns

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/probe/echo"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

func msg(key string, params map[string]any, basis ...string) result.Message {
	return result.Message{Key: key, Params: params, Basis: basis}
}

// rcodeNames are the response codes a resolver gives a valid query (RFC 1035 §4.1.1).
var rcodeNames = map[int]string{1: "FORMERR", 2: "SERVFAIL", 3: "NXDOMAIN", 4: "NOTIMP", 5: "REFUSED"}

// tally is what one resolver's answers add up to.
type tally struct {
	sent, replies, errors uint64
	rtt                   []time.Duration
	codes                 map[int]uint64 // error answers by response code
}

func count(o outcome) tally {
	t := tally{codes: map[int]uint64{}}
	for _, a := range o.answers {
		if !a.sent || a.cut {
			continue
		}
		t.sent++
		if !a.ok {
			continue
		}
		t.replies++
		t.rtt = append(t.rtt, a.rtt)
		if a.rcode != 0 {
			t.errors++
			t.codes[a.rcode]++
		}
	}
	return t
}

// codesText lists error answers by code, as "SERVFAIL ×3, REFUSED ×1".
func codesText(codes map[int]uint64) string {
	var parts []string
	for _, code := range slices.Sorted(maps.Keys(codes)) {
		name, ok := rcodeNames[code]
		if !ok {
			name = fmt.Sprintf("RCODE %d", code)
		}
		parts = append(parts, fmt.Sprintf("%s ×%d", name, codes[code]))
	}
	return strings.Join(parts, ", ")
}

// finish turns one resolver's answers into its result. interrupted is whether the
// run ended before the check did.
func finish(c *result.Check, o outcome, interrupted bool) {
	t := count(o)
	c.Metrics = append([]result.Metric{
		result.Count("sent", t.sent),
		result.Count("replies", t.replies),
		result.Count("lost", t.sent-t.replies),
		result.Ratio("loss", t.sent-t.replies, t.sent),
		result.Count("error_replies", t.errors),
	}, echo.DelayMetrics("", t.rtt)...)
	evaluate(c, t, o.sendErr, interrupted)
}

// evaluate applies the status rules that follow the queries, in order (method
// document, "Status rules"); TestStatusRules mirrors that table.
func evaluate(c *result.Check, t tally, sendErr error, interrupted bool) {
	lost := t.sent - t.replies
	switch {
	case t.sent == 0 && interrupted:
		c.Status = result.Invalid
		c.Warnings = append(c.Warnings, msg("dns.warning.interrupted", nil))
		return
	case t.replies == 0 && sendErr != nil:
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("dns.inference.unreachable", map[string]any{"error": sendErr.Error()}, "lost"))
		return
	}
	c.Status = result.Pass
	if lost > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("dns.inference.unanswered", map[string]any{"count": lost, "wait_ms": replyWait.Milliseconds()}, "lost", "loss"))
	}
	if t.errors > 0 {
		c.Status = result.Warn
		c.Inferences = append(c.Inferences, msg("dns.inference.error_replies", map[string]any{"count": t.errors, "codes": codesText(t.codes)}, "error_replies"))
	}
}
