package loops

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseCommitmentIDRejectsAmbiguousSpellings(t *testing.T) {
	for _, id := range []string{"12", "c012"} {
		_, err := ParseCommitmentID(id)
		assert.Error(t, err, id)
	}
}
