package entrykey

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEntryKeyRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want Ref
	}{
		{MessageKey(3, 42, ""), Ref{Kind: Message, SourceID: 3, SourceMessageID: "42"}},
		{MessageKey(1, 0, "a:b:c"), Ref{Kind: Message, SourceID: 1, SourceMessageID: "a:b:c"}},
		{ConversationKey(2, 9), Ref{Kind: Conversation, SourceID: 2, ConversationID: 9}},
	} {
		ref, err := Parse(tc.key)
		require.NoError(t, err)
		assert.Equal(t, tc.want, ref)
	}
	for _, key := range []string{"", "source:0:message:x", "source:-1:message:x", "source:1:message:", "source:1:conversation:0", "source:01:message:x", "source:1:unknown:x", "source:1:conversation:2:x"} {
		_, err := Parse(key)
		assert.ErrorIs(t, err, ErrInvalid)
	}
}
