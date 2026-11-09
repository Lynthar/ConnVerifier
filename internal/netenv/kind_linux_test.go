package netenv

import (
	"os"
	"path/filepath"
	"testing"
)

// The trees mirror what a kernel shows for each kind of link; tap is a tun device
// in Ethernet mode, so only its tun_flags tells it apart from a wired port.
func TestSysfsKind(t *testing.T) {
	root := t.TempDir()
	links := map[string]struct {
		typ, devtype string
		files        []string
		want         string
	}{
		"wlan0":  {"1", "", []string{"device", "phy80211"}, KindWiFi},
		"wlp2s0": {"1", "wlan", []string{"device"}, KindWiFi},
		"eth0":   {"1", "", []string{"device"}, KindWired},
		"ppp0":   {"512", "", nil, KindPPP},
		"tun0":   {"65534", "", []string{"tun_flags"}, KindTunnel},
		"tap0":   {"1", "", []string{"tun_flags"}, KindTunnel},
		"wg0":    {"65534", "wireguard", nil, KindTunnel},
		"gre1":   {"778", "", nil, KindTunnel},
		"sit1":   {"776", "", nil, KindTunnel},
		"br0":    {"1", "bridge", nil, KindOther},
		"veth0":  {"1", "", nil, KindOther},
		"lo":     {"772", "", nil, KindOther},
		"odd0":   {"not a number", "", nil, KindUnknown},
	}
	for name, l := range links {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"type": l.typ + "\n", "uevent": "INTERFACE=" + name + "\n"}
		if l.devtype != "" {
			files["uevent"] += "DEVTYPE=" + l.devtype + "\n"
		}
		for _, f := range l.files {
			files[f] = ""
		}
		for f, content := range files {
			if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for name, l := range links {
		if got := sysfsKind(root, name); got != l.want {
			t.Errorf("sysfsKind(%s) = %q, want %q", name, got, l.want)
		}
	}
	if got := sysfsKind(root, "missing0"); got != KindUnknown {
		t.Errorf("sysfsKind(missing0) = %q, want %q", got, KindUnknown)
	}
}
