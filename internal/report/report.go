// Package report renders a result.Run as JSON for programs or as text for people.
// Both read the same Run, so they cannot disagree about a status or a value.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/Lynthar/ConnVerifier/internal/i18n"
	"github.com/Lynthar/ConnVerifier/internal/result"
)

// JSON writes r as indented JSON followed by a newline.
func JSON(w io.Writer, r result.Run) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// Text writes r as label-value lines in cat's language. Lines are not aligned into
// columns: mixed-width CJK text would misalign them in many terminals.
func Text(w io.Writer, r result.Run, cat *i18n.Catalog) error {
	var b strings.Builder
	for i, c := range r.Checks {
		if i > 0 {
			b.WriteString("\n")
		}
		writeCheck(&b, c, cat)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeCheck(b *strings.Builder, c result.Check, cat *i18n.Catalog) {
	field := func(indent, label, value string) {
		b.WriteString(indent + cat.Text("line.field", map[string]any{"label": label, "value": value}) + "\n")
	}

	b.WriteString(cat.Text("line.header", map[string]any{
		"title":  cat.Text("check."+c.ID+".title", nil),
		"status": cat.Text("status."+string(c.Status), nil),
		"code":   string(c.Status),
	}) + "\n")
	field("", cat.Text("label.node", nil), pathText(c.Path))
	field("", cat.Text("label.method_version", nil), strconv.Itoa(c.MethodVersion))
	field("", cat.Text("label.elapsed", nil), formatMs(float64(c.ElapsedMs)))

	if len(c.Params) > 0 {
		b.WriteString(cat.Text("section.params", nil) + "\n")
		names := make([]string, 0, len(c.Params))
		for name := range c.Params {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			field("  ", cat.Text("param."+name, nil), paramText(cat, name, c.Params[name]))
		}
	}

	if n := c.Node; n != nil {
		writeNode(b, field, n, cat)
	}

	b.WriteString(cat.Text("section.metrics", nil) + "\n")
	for _, m := range c.Metrics {
		field("  ", cat.Text("metric."+m.ID, nil), metricText(cat, m))
	}

	messages := func(section string, msgs []result.Message) {
		if len(msgs) == 0 {
			return
		}
		b.WriteString(cat.Text(section, nil) + "\n")
		for _, m := range msgs {
			b.WriteString("  - " + Message(cat, m) + "\n")
		}
	}
	messages("section.inferences", c.Inferences)
	messages("section.not_proven", c.NotProven)
	messages("section.warnings", c.Warnings)
	if c.Error != nil {
		messages("section.error", []result.Message{*c.Error})
	}
}

// Message renders one message in cat's language, formatting its parameters the
// same way as everywhere else in the report.
func Message(cat *i18n.Catalog, m result.Message) string {
	return cat.Text(m.Key, messageParams(m.Params))
}

// messageParams formats numeric "_ms" parameters as durations, the same way
// parameters and metrics are shown, so messages do not print raw milliseconds.
func messageParams(params map[string]any) map[string]any {
	out := make(map[string]any, len(params))
	for name, v := range params {
		if f, ok := number(v); ok && strings.HasSuffix(name, "_ms") {
			out[name] = formatMs(f)
			continue
		}
		out[name] = v
	}
	return out
}

// writeNode renders what the node reported about itself, under its own heading so
// it is never read as something the client measured.
func writeNode(b *strings.Builder, field func(indent, label, value string), n *result.NodeReport, cat *i18n.Catalog) {
	b.WriteString(cat.Text("section.node", nil) + "\n")
	field("  ", cat.Text("node.label", nil), n.Label)
	if n.Version != "" {
		field("  ", cat.Text("node.version", nil), n.Version)
	}
	if n.ObservedAddr != "" {
		field("  ", cat.Text("node.observed_addr", nil), n.ObservedAddr)
	}
	if g := n.Granted; g != nil {
		stamp := ""
		if g.StampRate > 0 {
			stamp = cat.Text("node.grant_stamp", map[string]any{"rate": g.StampRate})
		}
		field("  ", cat.Text("node.granted", nil), Message(cat, result.Message{Key: "node.grant_value", Params: map[string]any{
			"connections": g.Connections, "rate": g.DialRate,
			"duration_ms": g.DurationS * 1000, "idle_ms": g.IdleTimeoutS * 1000, "stamp": stamp,
		}}))
	}
	for _, l := range []struct {
		key  string
		load *result.NodeLoad
	}{{"node.load_start", n.LoadStart}, {"node.load_end", n.LoadEnd}} {
		if l.load != nil {
			field("  ", cat.Text(l.key, nil), cat.Text("node.load_value", map[string]any{
				"sessions": l.load.Sessions, "connections": l.load.Connections, "max": l.load.MaxConnections,
			}))
		}
	}
}

func pathText(p result.Path) string {
	parts := []string{p.Node}
	switch p.Family {
	case "ipv4":
		parts = append(parts, "IPv4")
	case "ipv6":
		parts = append(parts, "IPv6")
	}
	if p.Protocol != "" {
		parts = append(parts, strings.ToUpper(p.Protocol))
	}
	return strings.Join(parts, " · ")
}

// paramText renders a parameter; a zero value uses the catalog's "<param>.zero"
// text when one exists, because zero means "disabled" or "unlimited" there.
func paramText(cat *i18n.Catalog, name string, v any) string {
	f, ok := number(v)
	if !ok {
		return fmt.Sprint(v)
	}
	if f == 0 && cat.Has("param."+name+".zero") {
		return cat.Text("param."+name+".zero", nil)
	}
	if strings.HasSuffix(name, "_ms") {
		return formatMs(f)
	}
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func metricText(cat *i18n.Catalog, m result.Metric) string {
	if m.Insufficient || m.Value == nil {
		return cat.Text("label.insufficient", map[string]any{"samples": m.Samples})
	}
	var v string
	switch m.Unit {
	case result.UnitMs:
		v = formatMs(*m.Value)
	case result.UnitPercent:
		v = sig3(*m.Value) + "%"
		if m.Low != nil && m.High != nil {
			return cat.Text("value.with_interval", map[string]any{"value": v, "low": sig3(*m.Low), "high": sig3(*m.High) + "%", "samples": m.Samples})
		}
	default:
		v = strconv.FormatFloat(*m.Value, 'f', -1, 64)
	}
	if m.Samples > 0 && m.Unit != result.UnitCount {
		return cat.Text("value.with_samples", map[string]any{"value": v, "samples": m.Samples})
	}
	return v
}

// formatMs renders milliseconds in the largest unit below the value (µs, ms, s or
// min) with about three significant digits and no trailing zeros.
func formatMs(ms float64) string {
	switch {
	case ms < 1:
		return sig3(ms*1000) + " µs"
	case ms < 1000:
		return sig3(ms) + " ms"
	case ms < 60_000:
		return sig3(ms/1000) + " s"
	default:
		return sig3(ms/60_000) + " min"
	}
}

func sig3(v float64) string {
	decimals := 0
	switch {
	case v < 10:
		decimals = 2
	case v < 100:
		decimals = 1
	}
	s := strconv.FormatFloat(v, 'f', decimals, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, true
	}
	return 0, false
}
