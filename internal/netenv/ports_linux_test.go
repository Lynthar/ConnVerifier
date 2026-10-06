package netenv

import "testing"

func TestParsePortRange(t *testing.T) {
	tests := []struct {
		in   string
		want int
		ok   bool
	}{
		{"32768\t60999\n", 28232, true},
		{"1024 65535", 64512, true},
		{"60999 32768", 0, false},
		{"garbage", 0, false},
	}
	for _, tt := range tests {
		if got, ok := parsePortRange(tt.in); got != tt.want || ok != tt.ok {
			t.Errorf("parsePortRange(%q) = %d, %v; want %d, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
