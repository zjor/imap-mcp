package mailparse

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const maxFilenameBytes = 200

// CleanFilename makes an attachment name safe to show and to use in a header:
// base name only, no control characters or quotes, no edge dots or spaces,
// at most 200 bytes. An empty result becomes "attachment".
func CleanFilename(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r == '"' || unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.ToValidUTF8(name, "")
	name = strings.Trim(name, ". ")
	if len(name) > maxFilenameBytes {
		cut := maxFilenameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.Trim(name[:cut], ". ")
	}
	if name == "" {
		return "attachment"
	}
	return name
}
