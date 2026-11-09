package netenv

import "testing"

func TestKindOfIfType(t *testing.T) {
	for typ, want := range map[uint32]string{
		71: KindWiFi, 6: KindWired, 23: KindPPP, 131: KindTunnel, 53: KindTunnel,
		24: KindOther, 243: KindOther,
	} {
		if got := kindOfIfType(typ); got != want {
			t.Errorf("kindOfIfType(%d) = %q, want %q", typ, got, want)
		}
	}
}
