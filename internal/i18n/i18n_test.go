package i18n

import (
	"reflect"
	"testing"
)

func TestCatalogsHaveSameKeys(t *testing.T) {
	base, err := Load(Default)
	if err != nil {
		t.Fatal(err)
	}
	for _, lang := range Supported {
		c, err := Load(lang)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(c.Keys(), base.Keys()) {
			t.Errorf("%s keys differ from %s", lang, Default)
		}
	}
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"nothing set", nil, "zh-CN"},
		{"LANG english", map[string]string{"LANG": "en_US.UTF-8"}, "en"},
		{"LANG chinese", map[string]string{"LANG": "zh_CN.UTF-8"}, "zh-CN"},
		{"LC_ALL wins", map[string]string{"LC_ALL": "en_GB.UTF-8", "LANG": "zh_CN.UTF-8"}, "en"},
		{"LC_MESSAGES before LANG", map[string]string{"LC_MESSAGES": "en", "LANG": "zh_TW"}, "en"},
		{"C locale falls back", map[string]string{"LC_ALL": "C", "LANG": "en_US"}, "zh-CN"},
		{"unsupported falls back", map[string]string{"LANG": "de_DE.UTF-8"}, "zh-CN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Detect(func(k string) string { return tt.env[k] }); got != tt.want {
				t.Fatalf("Detect = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalize(t *testing.T) {
	for tag, want := range map[string]string{"zh": "zh-CN", "zh-CN": "zh-CN", "zh_Hans": "zh-CN", "EN": "en", "en-US": "en"} {
		if got, ok := Normalize(tag); !ok || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", tag, got, ok, want)
		}
	}
	for _, tag := range []string{"", "C", "fr_FR"} {
		if _, ok := Normalize(tag); ok {
			t.Errorf("Normalize(%q) accepted an unsupported tag", tag)
		}
	}
}

func TestText(t *testing.T) {
	c := &Catalog{msgs: map[string]string{"k": "{a} and {b}"}}
	if got := c.Text("k", map[string]any{"a": 1, "b": "{a}"}); got != "1 and {a}" {
		t.Fatalf("Text = %q; substituted values must not be substituted again", got)
	}
	if got := c.Text("missing.key", nil); got != "missing.key" {
		t.Fatalf("missing key rendered as %q, want the key itself", got)
	}
}
