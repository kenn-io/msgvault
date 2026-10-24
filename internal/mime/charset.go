package mime

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"github.com/jhillyerd/enmime/v2"
)

var declaredCharsetParser = enmime.NewParser(
	enmime.SetCustomParseMediaType(parseMediaTypeLenient),
	enmime.DisableCharacterDetection(true),
	enmime.SetReadPartErrorPolicy(func(*enmime.Part, error) bool { return true }),
)

// restoreDeclaredUTF8 keeps valid UTF-8 content when charset detection prefers
// a legacy encoding. Detection still recovers parts with incorrect declarations.
func restoreDeclaredUTF8(raw []byte, root *enmime.Part) {
	candidates := root.BreadthMatchAll(func(part *enmime.Part) bool {
		return isUTF8Charset(part.OrigCharset) && !isUTF8Charset(part.Charset)
	})
	if len(candidates) == 0 {
		return
	}

	// Reparse only actual overrides, retaining enmime's transport decoding. Reverse
	// transcoding could not recover original bytes lost by a legacy charset decoder.
	declared, err := declaredCharsetParser.ReadParts(bytes.NewReader(raw))
	if err != nil {
		return
	}
	byID := make(map[string]*enmime.Part)
	for _, part := range declared.BreadthMatchAll(func(*enmime.Part) bool { return true }) {
		byID[part.PartID] = part
	}
	for _, part := range candidates {
		source := byID[part.PartID]
		if source == nil || !utf8.Valid(source.Content) {
			continue
		}
		part.Content = source.Content
	}
}

func isUTF8Charset(charset string) bool {
	switch strings.ToLower(charset) {
	case "utf-8", "utf8", "unicode-1-1-utf-8", "utf8mb4":
		return true
	default:
		return false
	}
}
