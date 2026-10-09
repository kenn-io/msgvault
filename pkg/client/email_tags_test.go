package client_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/emailtags"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMessageTagsGeneratedClientSupportsFullUIDRange(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	native := emailtags.MessageTagResult{Provider: "imap", UID: ^uint32(0), UIDValidity: ^uint32(0), Tags: []string{}, Before: []string{}, AvailableTags: []emailtags.MessageTag{}, Verified: true}
	encoded, err := json.Marshal(native)
	require.NoError(err)
	var wire generated.MessageTagResult
	require.NoError(json.Unmarshal(encoded, &wire), "the generated client must accept the complete IMAP unsigned 32-bit identity range")
	require.NotNil(wire.UID)
	require.NotNil(wire.Uidvalidity)
	assert.Equal(int64(4294967295), *wire.UID)
	assert.Equal(int64(4294967295), *wire.Uidvalidity)
}
