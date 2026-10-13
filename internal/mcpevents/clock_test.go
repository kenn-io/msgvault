package mcpevents

import (
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Assertion constructors keep failures local to this test.
	Require "github.com/stretchr/testify/require" //nolint:importas // Assertion constructors keep failures local to this test.
)

func TestConfiguredClockControlsSubscriptionAdmission(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	original, fixture, request := eventService(t)
	observed := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	options := original.opts
	options.Now = func() time.Time { return observed }
	service, err := New(t.Context(), fixture.Store, options)
	require.NoError(err)
	service.webhook = original.webhook
	result, err := service.Subscribe(t.Context(), service.principal, request)
	require.NoError(err)
	assert.Equal(observed.Add(24*time.Hour).UnixMilli(), result.RefreshBefore)
	row, err := fixture.Store.GetMCPSubscription(t.Context(), result.ID)
	require.NoError(err)
	require.NotNil(row)
	assert.True(row.CreatedAt.Equal(observed))
	assert.True(row.VerifiedAt.Equal(observed))
}
