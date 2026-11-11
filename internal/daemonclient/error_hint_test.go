package daemonclient

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPIRecoveryHintRoundTrip(t *testing.T) {
	assert := assert.New(t)
	for _, cli := range []bool{false, true} {
		var err error
		body := []byte(`{"error":"message_not_found","message":"missing","hint":"run msgvault search"}`)
		if cli {
			err = handleCLIErrorBody(404, body)
		} else {
			err = handleErrorBody(404, body)
		}
		var got *APIError
		require.ErrorAs(t, err, &got)
		assert.Equal("message_not_found", got.Code)
		assert.Equal("run msgvault search", got.Hint())
		if cli {
			assert.EqualError(err, "missing")
		} else {
			assert.EqualError(err, "API error (404): missing")
		}
	}
}

func TestCLIStreamCodeRequiresMetadata(t *testing.T) {
	err := &CLIRunError{err: errors.New("not_permitted")}
	assert.Empty(t, err.APIErrorCode(), "plain error text is not structured code metadata")
}
