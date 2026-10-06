//go:build !linux && !darwin

package netenv

func ephemeralPorts() (int, bool) { return 0, false }
