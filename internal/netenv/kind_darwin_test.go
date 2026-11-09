package netenv

import (
	"net"
	"testing"
)

func TestKindOfByName(t *testing.T) {
	for name, want := range map[string]string{
		"utun3": KindTunnel, "ipsec0": KindTunnel, "gif0": KindTunnel, "stf0": KindTunnel,
		"ppp0": KindPPP, "lo0": KindOther, "nosuch9": KindUnknown,
	} {
		if got := kindOf(net.Interface{Name: name}); got != want {
			t.Errorf("kindOf(%s) = %q, want %q", name, got, want)
		}
	}
}
