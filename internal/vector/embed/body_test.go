package embed

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBodyTextForEmbedding(t *testing.T) {
	t.Run("prefers plain text", func(t *testing.T) {
		assert.Equal(t, "plain body", BodyTextForEmbedding("plain body", "<p>HTML body</p>"))
	})

	t.Run("converts HTML-only body with MIME pipeline", func(t *testing.T) {
		assert.Equal(t, "semantic needle", BodyTextForEmbedding("", "<p>semantic <strong>needle</strong></p>"))
	})
}

func TestHydrationBodyText_MatchesPerFamilyAssembly(t *testing.T) {
	// Chat chunks were assembled with the whitespace-aware HTML fallback;
	// every other family stored offsets against plain BodyTextForEmbedding.
	assert.Equal(t, "hello",
		HydrationBodyText("beeper", " \n\t", "<p>hello</p>"),
		"chat hydration must apply the whitespace-aware HTML fallback")
	assert.Equal(t, " \n\t",
		HydrationBodyText("email", " \n\t", "<p>hello</p>"),
		"non-chat hydration must keep the plain derivation its offsets used")
	assert.Equal(t, "plain",
		HydrationBodyText("beeper", "plain", "<p>hello</p>"))
	assert.Equal(t, "hello",
		HydrationBodyText("meeting_transcript", "", "<p>hello</p>"),
		"empty body_text falls back to HTML in both derivations")
}
