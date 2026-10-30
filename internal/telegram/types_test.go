package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestConvTypeMapsEveryDocumentedChatType pins the raw export chat types
// Telegram Desktop documents for its machine-readable JSON export onto
// archive conversation types. A newly documented value belongs here and in
// the fixture, not in the "chat" fallback.

func TestConvTypeMapsEveryDocumentedChatType(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want string
	}{
		{"personal_chat", "direct_chat"},
		{"bot_chat", "direct_chat"},
		{"saved_messages", "direct_chat"},
		{"private_group", "group_chat"},
		{"private_supergroup", "group_chat"},
		{"public_supergroup", "group_chat"},
		{"private_channel", "channel"},
		{"public_channel", "channel"},
		{"replies", "direct_chat"},
		{"verification_codes", "direct_chat"},
		{"something_new", "chat"},
	} {
		chat := &Chat{ID: 1, Type: tc.raw}
		assert.Equal(t, tc.want, chat.ConvType(), "chat type %s", tc.raw)
	}
}
