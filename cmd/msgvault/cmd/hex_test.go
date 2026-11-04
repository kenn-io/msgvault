package cmd

import (
	"context"
	"errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"testing"
	"time"
)

func TestHexWatchSchedulesEnabledSources(t *testing.T) {
	sources := []config.HexSource{{Identifier: "enabled", Enabled: true, Schedule: "* * * * *"}, {Identifier: "disabled", Schedule: "* * * * *"}}
	selected, err := hexWatchSources(sources)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	err = runHexWatch(ctx, selected, func(context.Context, config.HexSource) error {
		calls++
		if calls == 2 {
			cancel()
		}
		return errors.New("temporary failure")
	}, func(context.Context, time.Time) error { return nil }, func(error) {})
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 2, calls)
	for _, sources := range [][]config.HexSource{nil, {{Enabled: true}}, {{Enabled: true, Schedule: "invalid"}}, {{Enabled: true, Schedule: "0 0 31 2 *"}}} {
		_, err := hexWatchSources(sources)
		require.Error(t, err)
	}
}
