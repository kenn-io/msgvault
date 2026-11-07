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

func TestKnownMessageTypesIncludesTextTypes(t *testing.T) {
	t.Parallel()
	// Every text message type must also be a known message type, so the
	// search --message-type help names every text filter.
	for _, mt := range TextMessageTypes {
		assert.Contains(t, KnownMessageTypes, mt, "text type %q must be a known message type", mt)
	}
}
