package embed

import (
	"strings"

	"go.kenn.io/msgvault/internal/mime"
)

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

// ContextualBodyText is the body representation for contextual chat
// documents: like BodyTextForEmbedding, but body_text that is blank after
// trimming ASCII whitespace falls back to the HTML body. Chat assembly and
// search-time chunk hydration must both use this function — the stored chunk
// offsets only reconstruct correct excerpts when both sides canonicalize
// identically.
func ContextualBodyText(bodyText, bodyHTML string) string {
	if strings.Trim(bodyText, " \t\n\r") == "" {
		bodyText = ""
	}
	return BodyTextForEmbedding(bodyText, bodyHTML)
}

// HydrationBodyText selects the per-family body derivation for search-time
// chunk hydration, matching how each family's chunks were assembled: chat
// messages use the whitespace-aware HTML fallback (ContextualBodyText), and
// every other family — legacy generations, ordinary contextual documents,
// and meeting documents — keeps the plain BodyTextForEmbedding their offsets
// were stored against.
func HydrationBodyText(messageType, bodyText, bodyHTML string) string {
	if messageType == contextualChatMessageType {
		return ContextualBodyText(bodyText, bodyHTML)
	}
	return BodyTextForEmbedding(bodyText, bodyHTML)
}
