// Package i18n turns message keys into text in the reader's language. Catalogs are
// flat JSON maps from key to text with {name} placeholders, embedded at build time,
// so every interface reads the same files.
package i18n

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed locales/*.json
var locales embed.FS

// Default is used whenever the reader's language cannot be determined or has no catalog.
const Default = "zh-CN"

// Supported lists the languages that have a catalog.
var Supported = []string{"zh-CN", "en"}

type Catalog struct {
	lang string
	msgs map[string]string
}

// Load returns the catalog for a supported language.
func Load(lang string) (*Catalog, error) {
	data, err := locales.ReadFile("locales/" + lang + ".json")
	if err != nil {
		return nil, fmt.Errorf("no catalog for language %q", lang)
	}
	var msgs map[string]string
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("catalog %s: %w", lang, err)
	}
	return &Catalog{lang: lang, msgs: msgs}, nil
}

func (c *Catalog) Lang() string { return c.lang }

// Has reports whether key exists in the catalog.
func (c *Catalog) Has(key string) bool {
	_, ok := c.msgs[key]
	return ok
}

// Keys returns every key in the catalog, sorted.
func (c *Catalog) Keys() []string {
	keys := make([]string, 0, len(c.msgs))
	for k := range c.msgs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Text renders key with params substituted for {name} placeholders. A missing key
// renders as the key itself, so a gap shows up in output instead of vanishing.
func (c *Catalog) Text(key string, params map[string]any) string {
	text, ok := c.msgs[key]
	if !ok {
		return key
	}
	if len(params) == 0 {
		return text
	}
	pairs := make([]string, 0, 2*len(params))
	for name, v := range params {
		pairs = append(pairs, "{"+name+"}", fmt.Sprint(v))
	}
	return strings.NewReplacer(pairs...).Replace(text)
}

// Normalize maps a language tag or POSIX locale ("zh_CN.UTF-8", "en-US", "zh") to a
// supported language, reporting false when it names none.
func Normalize(tag string) (string, bool) {
	base := strings.ToLower(tag)
	if i := strings.IndexAny(base, "_-.@"); i >= 0 {
		base = base[:i]
	}
	switch base {
	case "zh":
		return "zh-CN", true
	case "en":
		return "en", true
	}
	return "", false
}

// Detect picks the language from LC_ALL, LC_MESSAGES and LANG, first non-empty
// wins as in POSIX; unset, "C", "POSIX" or unsupported values give Default.
func Detect(getenv func(string) string) string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := getenv(name); v != "" {
			if lang, ok := Normalize(v); ok {
				return lang
			}
			return Default
		}
	}
	return Default
}
