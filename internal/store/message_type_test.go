package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsEmailMessageType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		messageType string
		want        bool
	}{
		{name: "typed email", messageType: "email", want: true},
		{name: "legacy blank", messageType: "", want: true},
		{name: "chat", messageType: "imessage", want: false},
		{name: "calendar", messageType: "calendar_event", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsEmailMessageType(tc.messageType))
		})
	}
}

func TestIsWellFormedMessageType(t *testing.T) {
	for _, tc := range []struct {
		messageType string
		want        bool
	}{
		{messageType: "email", want: true},
		{messageType: "google_voice_text", want: true},
		{messageType: "feed_item", want: true},
		{messageType: "v2", want: true},
		{messageType: "", want: false},
		{messageType: "SMS", want: false},
		{messageType: "feed-item", want: false},
		{messageType: "feed item", want: false},
		{messageType: "_feed", want: false},
		{messageType: "2fa", want: false},
		{messageType: "flüssig", want: false},
	} {
		t.Run(tc.messageType, func(t *testing.T) {
			assert.Equal(t, tc.want, IsWellFormedMessageType(tc.messageType))
		})
	}
}
