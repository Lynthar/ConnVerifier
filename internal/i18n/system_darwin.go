package i18n

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// SystemLanguage returns the user's first preferred language from the macOS
// settings, or "" when it cannot be read.
func SystemLanguage() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return ""
	}
	return firstAppleLanguage(string(out))
}

// firstAppleLanguage returns the first element of an array as `defaults read` prints
// it: one element per line between "(" and ")", quoted only when it needs quotes.
func firstAppleLanguage(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "(" {
		return ""
	}
	return strings.Trim(strings.TrimSpace(lines[1]), `",`)
}
