package mcp

import (
	"fmt"
	"strings"

	"go.kenn.io/msgvault/internal/vcard"
)

// cardDAVConfirmationPreview summarizes media in a display-only copy. The
// original vCard and its approval token continue through the daemon unchanged.
func cardDAVConfirmationPreview(body string) string {
	const unavailable = "Contact preview unavailable."
	document, err := vcard.DecodeWithOptions(strings.NewReader(body), vcard.DecodeOptions{MaxCards: 1})
	if err != nil || len(document.Cards) != 1 {
		return unavailable
	}
	for i := range document.Cards[0].Properties {
		property := &document.Cards[0].Properties[i]
		switch property.Name {
		case "PHOTO", "LOGO", "SOUND", "KEY":
		default:
			continue
		}
		value := strings.TrimLeft(property.RawValue, " \t")
		inline := len(value) >= 5 && strings.EqualFold(value[:5], "data:")
		for _, encoding := range property.Parameters {
			if encoding.Name != "ENCODING" && !encoding.Bare {
				continue
			}
			for _, value := range encoding.Values {
				token := strings.TrimSpace(value.Decoded)
				if strings.EqualFold(token, "b") || strings.EqualFold(token, "BASE64") || strings.EqualFold(token, "QUOTED-PRINTABLE") {
					inline = true
				}
			}
		}
		if inline {
			property.RawValue = fmt.Sprintf("[inline %s, %d encoded bytes]", property.Name, len(property.RawValue))
		}
	}
	var preview strings.Builder
	if err := vcard.Encode(&preview, document); err != nil {
		return unavailable
	}
	return preview.String()
}
