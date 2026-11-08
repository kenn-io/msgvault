package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/store"
)

func TestKnownMessageTypesAreWellFormed(t *testing.T) {
	t.Parallel()
	// The search --message-type flag accepts any well-formed type, so every
	// type msgvault writes must pass the same rule.
	for _, mt := range KnownMessageTypes {
		assert.True(t, store.IsWellFormedMessageType(mt), "%q should be well formed", mt)
	}
}
