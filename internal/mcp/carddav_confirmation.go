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
		if vcard.IsInlineMedia(*property) {
			property.RawValue = fmt.Sprintf("[inline %s, %d encoded bytes]", property.Name, len(property.RawValue))
		}
	}
	var preview strings.Builder
	if err := vcard.Encode(&preview, document); err != nil {
		return unavailable
	}
	return preview.String()
}
