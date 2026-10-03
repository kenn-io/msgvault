package identityindex

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChatwootMessageClassification(t *testing.T) {
	assert.True(t, IsChat("chatwoot", "direct_chat"))
	assert.True(t, IsChat("chatwoot", ""))
	assert.False(t, IsChat("meeting_transcript", "meeting"))
}
