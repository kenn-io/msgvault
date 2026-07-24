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
