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
