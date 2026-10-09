// Package netenv reads facts about the host that bound what a check can measure.
// A fact the platform cannot provide is reported as unknown, never guessed.
package netenv

import "time"

// FDLimit returns the process's current open-file limit. The Go runtime raises
// the soft limit to the hard limit at startup, so this is the effective value.
func FDLimit() (limit uint64, ok bool) {
	limit, ok = fdLimit()
	if limit >= 1<<62 {
		return 0, false // unlimited: no bound to report
	}
	return limit, ok
}

// EphemeralPorts returns how many local ports the host assigns to outgoing
// connections, which caps connections from one address to one destination.
func EphemeralPorts() (int, bool) {
	return ephemeralPorts()
}

// ProcessCPU returns the CPU time this process has used, user and system together.
func ProcessCPU() (time.Duration, bool) { return processCPU() }
