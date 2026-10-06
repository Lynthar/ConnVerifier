package main

import (
	"runtime/debug"
	"testing"
)

func TestBuildVersion(t *testing.T) {
	setting := func(kv ...string) []debug.BuildSetting {
		var s []debug.BuildSetting
		for i := 0; i < len(kv); i += 2 {
			s = append(s, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return s
	}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{"no build info", nil, "unknown"},
		{"module version wins", &debug.BuildInfo{
			Main:     debug.Module{Version: "v1.2.3"},
			Settings: setting("vcs.revision", "0123456789abcdef"),
		}, "v1.2.3"},
		{"devel falls back to revision", &debug.BuildInfo{
			Main:     debug.Module{Version: "(devel)"},
			Settings: setting("vcs.revision", "0123456789abcdef", "vcs.modified", "false"),
		}, "devel-0123456789ab"},
		{"dirty tree is marked", &debug.BuildInfo{
			Settings: setting("vcs.revision", "0123456789abcdef", "vcs.modified", "true"),
		}, "devel-0123456789ab+dirty"},
		{"no vcs info", &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, "devel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildVersion(tt.info); got != tt.want {
				t.Fatalf("buildVersion = %q, want %q", got, tt.want)
			}
		})
	}
}
