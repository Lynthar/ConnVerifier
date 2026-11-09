package i18n

import "testing"

func TestFirstAppleLanguage(t *testing.T) {
	tests := []struct {
		name, out, want string
	}{
		{"quoted", "(\n    \"zh-Hans-US\",\n    \"en-US\",\n    \"zh-Hant-HK\"\n)\n", "zh-Hans-US"},
		{"bare", "(\n    en,\n    \"zh-Hans-CN\"\n)\n", "en"},
		{"single", "(\n    \"en-GB\"\n)\n", "en-GB"},
		{"empty array", "(\n)\n", ""},
		{"not an array", "en-US\n", ""},
		{"no output", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstAppleLanguage(tt.out); got != tt.want {
				t.Fatalf("firstAppleLanguage = %q, want %q", got, tt.want)
			}
		})
	}
}
