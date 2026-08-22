package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCapPersonSemanticUTF8RepairsMalformedInputBeforeCapping(t *testing.T) {
	input := string([]byte{0xff}) + strings.Repeat("x", MaxPersonSemanticDocumentBytes+100)

	got := capPersonSemanticUTF8(input)

	require.True(t, utf8.ValidString(got))
	assert.Len(t, []byte(got), MaxPersonSemanticDocumentBytes)
	assert.True(t, strings.HasSuffix(got, "x"), "valid suffix should survive malformed leading bytes")
}

func TestCapPersonSemanticUTF8BacksOffPartialRune(t *testing.T) {
	input := strings.Repeat("x", MaxPersonSemanticDocumentBytes-1) + "雪"

	got := capPersonSemanticUTF8(input)

	require.True(t, utf8.ValidString(got))
	assert.Greater(t, len(got), MaxPersonSemanticDocumentBytes-4)
	assert.Less(t, len(got), MaxPersonSemanticDocumentBytes)
}

func TestRenderPersonSemanticNameFallsBackFromBlankOptionalValues(t *testing.T) {
	assert.Equal(t, "Alice Example", renderPersonSemanticName(PersonName{
		Formatted: new("  "), GivenName: new("Alice"), FamilyName: new("Example"),
	}))
	assert.Equal(t, "Original Name", renderPersonSemanticName(PersonName{
		SortAs: new("\n"), OriginalValue: "Original Name",
	}))
}
