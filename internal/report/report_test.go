package report

import "testing"

func TestFormatMs(t *testing.T) {
	tests := []struct {
		ms   float64
		want string
	}{
		{0.0005, "0.5 µs"},
		{0.131, "131 µs"},
		{1.5, "1.5 ms"},
		{12.34, "12.3 ms"},
		{255, "255 ms"},
		{999.6, "1000 ms"},
		{1000, "1 s"},
		{30000, "30 s"},
		{60000, "1 min"},
		{90000, "1.5 min"},
	}
	for _, tt := range tests {
		if got := formatMs(tt.ms); got != tt.want {
			t.Errorf("formatMs(%v) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[float64]string{
		0: "0 B", 999: "999 B", 1500: "1.5 kB", 500e6: "500 MB", 1.5e9: "1.5 GB", 20e9: "20 GB",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%v) = %q, want %q", n, got, want)
		}
	}
}
