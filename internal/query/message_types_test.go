package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsKnownMessageType(t *testing.T) {
	known := []string{"email", "calendar_event", "sms", "mms", "whatsapp", "imessage", "teams"}
	for _, mt := range known {
		assert.True(t, IsKnownMessageType(mt), "%q should be known", mt)
	}

	unknown := []string{"carrier_pigeon", "", "EMAIL", "telegram"}
	for _, mt := range unknown {
		assert.False(t, IsKnownMessageType(mt), "%q should be unknown", mt)
	}
}

func TestKnownMessageTypesIncludesTextTypes(t *testing.T) {
	// Every text message type must also be a known message type, otherwise
	// the search --message-type validation would reject valid text filters.
	for _, mt := range TextMessageTypes {
		assert.True(t, IsKnownMessageType(mt), "text type %q must be a known message type", mt)
	}
}

func TestDiscordIsKnownTextMessageType(t *testing.T) {
	assert.True(t, IsKnownMessageType("discord"))
	assert.True(t, IsTextMessageType("discord"))
}
