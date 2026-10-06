//go:build linux || darwin

package netenv

import "testing"

func TestHostFactsKnownOnUnix(t *testing.T) {
	if n, ok := FDLimit(); !ok || n == 0 {
		t.Errorf("FDLimit = %d, %v; want a positive known limit", n, ok)
	}
	if n, ok := EphemeralPorts(); !ok || n <= 0 || n > 65535 {
		t.Errorf("EphemeralPorts = %d, %v; want 1..65535", n, ok)
	}
}
