//go:build !linux && !darwin

package netenv

func fdLimit() (uint64, bool) { return 0, false }
