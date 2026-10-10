package mcpevents

import (
	"path/filepath"
	"testing"
	"time"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestRetentionCannotExceedSevenDayHardBound(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	f := storetest.New(t)
	opts := Options{Enabled: true, Sources: []string{"gmail"}, OwnerKey: "synthetic-owner", KeyPath: filepath.Join(t.TempDir(), "key"), Retention: 169 * time.Hour}
	_, err := New(t.Context(), f.Store, opts)
	require.Error(err)
	var eventErr *Error
	require.ErrorAs(err, &eventErr)
	assert.Equal("invalid_retention", eventErr.Reason)
	opts.Retention = -time.Hour
	_, err = New(t.Context(), f.Store, opts)
	require.Error(err)
	for _, retention := range []time.Duration{0, time.Hour, 168 * time.Hour} {
		opts.Retention = retention
		s, err := New(t.Context(), f.Store, opts)
		require.NoError(err)
		if retention == 0 {
			assert.Equal(168*time.Hour, s.opts.Retention)
		} else {
			assert.Equal(retention, s.opts.Retention)
		}
	}
}
