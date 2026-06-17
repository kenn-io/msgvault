package imap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestXOAuth2Client_Start(t *testing.T) {
	tests := []struct {
		name     string
		username string
		token    string
		wantMech string
		wantIR   string
	}{
		{
			name:     "basic",
			username: "user@example.com",
			token:    "ya29.access-token",
			wantMech: "XOAUTH2",
			wantIR:   "user=user@example.com\x01auth=Bearer ya29.access-token\x01\x01",
		},
		{
			name:     "empty token",
			username: "user@example.com",
			token:    "",
			wantMech: "XOAUTH2",
			wantIR:   "user=user@example.com\x01auth=Bearer \x01\x01",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewXOAuth2Client(tt.username, tt.token)
			mech, ir, err := c.Start()
			require.NoError(t, err, "Start()")
			assert.Equal(t, tt.wantMech, mech, "mech")
			assert.Equal(t, tt.wantIR, string(ir), "ir")
		})
	}
}

func TestXOAuth2Client_Next(t *testing.T) {
	c := NewXOAuth2Client("user@example.com", "token")
	// On auth failure the server sends a JSON error challenge.  The correct
	// XOAUTH2 response is an empty byte slice; the server then sends NO and
	// the IMAP AUTHENTICATE command returns the server's error message.
	resp, err := c.Next([]byte(`{"status":"401","schemes":"bearer","scope":"..."}`))
	require.NoError(t, err, "Next() returned unexpected error")
	assert.Empty(t, resp, "Next() should return empty response")
}
