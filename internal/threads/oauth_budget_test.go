package threads

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThreadsOAuthBindsSourceBeforeLongExchange(t *testing.T) {
	fixture := newOAuthFixture(t)
	opts := fixture.options()
	called := false
	opts.WithOwnerLock = func(ctx context.Context, source string, exchange func() (TokenRecord, error)) (TokenRecord, error) {
		called = true
		assert.Equal(t, "threads:123:"+oauthTestOwner, source)
		assert.Equal(t, int32(1), fixture.calls.Load())
		return TokenRecord{}, errors.New("binding barrier")
	}
	_, err := ExchangeCredential(t.Context(), opts, testOAuthRequest())
	require.ErrorContains(t, err, "binding barrier")
	assert.True(t, called)
	assert.Equal(t, int32(1), fixture.calls.Load())
}
