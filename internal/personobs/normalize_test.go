package personobs

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNormalizeContentCaseFold(t *testing.T) {
	assert.Equal(t, "strasse", NormalizeContent("Straße"))
	assert.Equal(t, "strasse", NormalizeContent("STRASSE"))
	assert.Equal(t, "a b", NormalizeContent(" Ａ\tＢ .!?"))
}
func TestContentKeyEquivalence(t *testing.T) {
	assert.Equal(t, ContentKey(LevelExplicit, "", "[PERSON]  Runs  Marathons."), ContentKey(LevelExplicit, "", "[person] runs marathons"))
	assert.NotEqual(t, ContentKey(LevelExplicit, "daughter", "[PERSON] runs"), ContentKey(LevelExplicit, "", "[PERSON] runs"))
}
func TestContentKeyByLevel(t *testing.T) {
	assert.NotEqual(t, ContentKey(LevelExplicit, "", "[PERSON] runs"), ContentKey(LevelDeductive, "", "[PERSON] runs"))
}
func TestContentKeyShape(t *testing.T) {
	assert.Regexp(t, "^[0-9a-f]{64}$", ContentKey(LevelExplicit, "", "[PERSON] runs"))
}

func TestNormalizeContentIdempotent(t *testing.T) {
	for _, input := range []string{"x . .", " . , : ; ! ? ", "x!? \t . ,", " Straße . . ", "Ａ\nＢ . .", "[PERSON] runs", "", "\t\n", "x...word . !"} {
		normalized := NormalizeContent(input)
		assert.Equal(t, normalized, NormalizeContent(normalized), input)
		assert.Equal(t, ContentKey(LevelExplicit, "", input), ContentKey(LevelExplicit, "", normalized), input)
	}
}
