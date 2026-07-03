package imap

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewClient_WithTokenSource(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	cfg := &Config{
		Host:       "outlook.office365.com",
		Port:       993,
		TLS:        true,
		Username:   "user@company.com",
		AuthMethod: AuthXOAuth2,
	}
	called := false
	ts := func(ctx context.Context) (string, error) {
		called = true
		return "test-token", nil
	}
	c := NewClient(cfg, "", WithTokenSource(ts))
	require.NotNil(c.tokenSource, "tokenSource should be set")
	// Verify the token source is callable
	token, err := c.tokenSource(context.Background())
	require.NoError(err)
	assert.Equal("test-token", token, "token")
	assert.True(called, "token source was not called")
}
