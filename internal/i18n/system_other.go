//go:build !darwin && !windows

package i18n

// SystemLanguage returns "": elsewhere the locale variables are the system setting.
func SystemLanguage() string { return "" }
