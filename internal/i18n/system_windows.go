package i18n

import "golang.org/x/sys/windows"

// SystemLanguage returns the user's first preferred display language, or "" when
// it cannot be read.
func SystemLanguage() string {
	langs, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil || len(langs) == 0 {
		return ""
	}
	return langs[0]
}
