// Package result is the versioned record a run produces. It holds structured values
// and message keys only; turning them into text is the report package's job, so
// every output format reads the same facts.
package result

import (
	"math"
	"time"

	"github.com/Lynthar/ConnVerifier/internal/stats"
)

// Schema names the JSON layout; v0 makes no compatibility promise between builds.
const Schema = "connverifier.result/v0"

// Status is a check's outcome. ERROR means no valid measurement was obtained and
// never means the network is poor; INVALID means data exists but must not be judged.
type Status string

const (
	Pass        Status = "PASS"
	Warn        Status = "WARN"
	Fail        Status = "FAIL"
	Skip        Status = "SKIP"
	Unsupported Status = "UNSUPPORTED"
	Error       Status = "ERROR"
	Invalid     Status = "INVALID"
)

// Unit values used by Metric. A UnitLevel value is a confidence: 1 low, 2 medium,
// 3 high (draft-ietf-ippm-responsiveness §5.4.1).
const (
	UnitCount   = "count"
	UnitMs      = "ms"
	UnitPercent = "%"
	UnitMbps    = "Mbit/s"
	UnitRPM     = "RPM"
	UnitBytes   = "B"
	UnitLevel   = "level"
)

// Confidence levels for UnitLevel metrics.
const (
	Low    = 1
	Medium = 2
	High   = 3
)

type Run struct {
	Schema    string    `json:"schema"`
	Tool      Tool      `json:"tool"`
	RunID     string    `json:"run_id"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	ElapsedMs int64     `json:"elapsed_ms"`
	Checks    []Check   `json:"checks"`
}

type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// Check is one check over one path; the same check over another node, address
// family or protocol is a separate Check, never merged into this one.
type Check struct {
	ID            string         `json:"id"`
	MethodVersion int            `json:"method_version"`
	Status        Status         `json:"status"`
	Path          Path           `json:"path"`
	Params        map[string]any `json:"params"`
	ElapsedMs     int64          `json:"elapsed_ms"`
	Metrics       []Metric       `json:"metrics"`
	Inferences    []Message      `json:"inferences,omitempty"`
	NotProven     []Message      `json:"not_proven,omitempty"`
	Warnings      []Message      `json:"warnings,omitempty"`
	Error         *Message       `json:"error,omitempty"`
	Node          *NodeReport    `json:"node,omitempty"`
}

// NodeReport holds what the node said about itself, kept apart from the client's
// own metrics. ObservedAddr is the client's public address: sensitive.
type NodeReport struct {
	Label        string    `json:"label"`
	Version      string    `json:"version,omitempty"`
	ObservedAddr string    `json:"observed_addr,omitempty"`
	Granted      *Grant    `json:"granted,omitempty"`
	LoadStart    *NodeLoad `json:"load_start,omitempty"`
	LoadEnd      *NodeLoad `json:"load_end,omitempty"`
	Traffic      *Traffic  `json:"traffic,omitempty"`
}

// Traffic is what the node counted on its load endpoints: body bytes it sent and
// received, and how often its 10 ms timer woke more than 10 ms late (LagOver of
// LagTicks).
type Traffic struct {
	SentBytes     uint64 `json:"sent_bytes"`
	ReceivedBytes uint64 `json:"received_bytes"`
	LagTicks      uint64 `json:"lag_ticks"`
	LagOver       uint64 `json:"lag_over"`
}

type Grant struct {
	Connections     int   `json:"connections"`
	DialRate        int   `json:"dial_rate"`
	DurationS       int   `json:"duration_s"`
	IdleTimeoutS    int   `json:"idle_timeout_s"`
	StampRate       int   `json:"stamp_rate,omitempty"`
	LoadBytes       int64 `json:"load_bytes,omitempty"`
	LoadConnections int   `json:"load_connections,omitempty"`
}

type NodeLoad struct {
	Sessions       int   `json:"sessions"`
	Connections    int   `json:"connections"`
	MaxConnections int   `json:"max_connections"`
	UptimeS        int64 `json:"uptime_s"`
}

type Path struct {
	Node     string `json:"node"`
	Family   string `json:"family,omitempty"`
	Protocol string `json:"protocol"`
}

// Metric is one observed value. An Insufficient metric has too few samples for its
// statistic and carries no Value, so it can never be read as zero. Low and High,
// when present, bound a 95% interval around Value. AtLeast marks Value as a lower
// bound: the run stopped before it could show the true value is not higher.
type Metric struct {
	ID           string   `json:"id"`
	Unit         string   `json:"unit"`
	Value        *float64 `json:"value,omitempty"`
	Low          *float64 `json:"low,omitempty"`
	High         *float64 `json:"high,omitempty"`
	Samples      uint64   `json:"samples,omitempty"`
	Insufficient bool     `json:"insufficient,omitempty"`
	AtLeast      bool     `json:"at_least,omitempty"`
}

// Message is a statement for the reader as a catalog key plus parameters; Basis
// lists the metric IDs that support it.
type Message struct {
	Key    string         `json:"key"`
	Params map[string]any `json:"params,omitempty"`
	Basis  []string       `json:"basis,omitempty"`
}

// Count returns an exact count metric.
func Count(id string, n uint64) Metric {
	v := float64(n)
	return Metric{ID: id, Unit: UnitCount, Value: &v}
}

// Millis returns a duration metric in milliseconds at microsecond precision.
func Millis(id string, d time.Duration, samples uint64) Metric {
	v := float64(d.Microseconds()) / 1000
	return Metric{ID: id, Unit: UnitMs, Value: &v, Samples: samples}
}

// Mbps returns a rate given in bits per second as Mbit/s.
func Mbps(id string, bitsPerSecond float64) Metric {
	v := round4(bitsPerSecond / 1e6)
	return Metric{ID: id, Unit: UnitMbps, Value: &v}
}

// RPM returns round trips per minute, rounded to a whole number.
func RPM(id string, rpm float64, samples uint64) Metric {
	v := math.Round(rpm)
	return Metric{ID: id, Unit: UnitRPM, Value: &v, Samples: samples}
}

// Bytes returns a byte count.
func Bytes(id string, n uint64) Metric {
	v := float64(n)
	return Metric{ID: id, Unit: UnitBytes, Value: &v}
}

// Level returns a confidence: Low, Medium or High.
func Level(id string, level int) Metric {
	v := float64(level)
	return Metric{ID: id, Unit: UnitLevel, Value: &v}
}

// Ratio returns k of n as a percentage with its 95% Wilson interval; with n = 0
// it is insufficient.
func Ratio(id string, k, n uint64) Metric {
	if n == 0 {
		return Metric{ID: id, Unit: UnitPercent, Insufficient: true}
	}
	lo, hi := stats.Wilson(k, n)
	v, l, h := round4(100*float64(k)/float64(n)), round4(100*lo), round4(100*hi)
	return Metric{ID: id, Unit: UnitPercent, Value: &v, Low: &l, High: &h, Samples: n}
}

// round4 keeps four decimals. CPUs that fuse multiply-add compute the interval a
// few bits differently from those that do not, and a result must not depend on that.
func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

// ExitCode is 1 when any check ended in ERROR and 0 otherwise: the exit status
// reports whether measurements were obtained, not how good the network is.
func (r Run) ExitCode() int {
	for _, c := range r.Checks {
		if c.Status == Error {
			return 1
		}
	}
	return 0
}
