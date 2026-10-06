// Package result is the versioned record a run produces. It holds structured values
// and message keys only; turning them into text is the report package's job, so
// every output format reads the same facts.
package result

import "time"

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

// Unit values used by Metric.
const (
	UnitCount = "count"
	UnitMs    = "ms"
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
}

type Path struct {
	Node     string `json:"node"`
	Family   string `json:"family,omitempty"`
	Protocol string `json:"protocol"`
}

// Metric is one observed value. An Insufficient metric has too few samples for its
// statistic and carries no Value, so it can never be read as zero.
type Metric struct {
	ID           string   `json:"id"`
	Unit         string   `json:"unit"`
	Value        *float64 `json:"value,omitempty"`
	Samples      uint64   `json:"samples,omitempty"`
	Insufficient bool     `json:"insufficient,omitempty"`
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
