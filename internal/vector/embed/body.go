package embed

import "go.kenn.io/msgvault/internal/mime"

// BodyTextForEmbedding returns the exact body representation used to build the
// vector corpus. Plain text wins when present; HTML-only messages use the
// MIME-aware HTML-to-text conversion before the configurable preprocessing
// pipeline runs.
func BodyTextForEmbedding(bodyText, bodyHTML string) string {
	if bodyText != "" {
		return bodyText
	}
	if bodyHTML != "" {
		return mime.StripHTML(bodyHTML)
	}
	return ""
}
