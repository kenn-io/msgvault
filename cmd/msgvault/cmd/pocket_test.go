package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/pocket"
)

func TestPocketSourceResolution(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	cfg := &config.Config{Pocket: []config.PocketSource{{Identifier: "personal"}, {Identifier: "work"}}}
	all, err := resolvePocketSources(nil, cfg)
	requirements.NoError(err)
	requirements.Len(all, 2)
	one, err := resolvePocketSources([]string{"work"}, cfg)
	requirements.NoError(err)
	requirements.Len(one, 1)
	assertions.Equal("work", one[0].Identifier)
	_, err = resolvePocketSources([]string{"missing"}, cfg)
	requirements.Error(err)
	_, err = resolvePocketSources(nil, &config.Config{})
	requirements.Error(err)
	_, err = resolvePocketSources(nil, nil)
	requirements.Error(err)
}

func TestPocketKeyEnvironmentAndFlags(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Setenv("POCKET_TEST_KEY", "synthetic-api-key")
	_, err := pocketClient(config.PocketSource{APIKeyEnv: "POCKET_TEST_KEY"})
	requirements.NoError(err)
	t.Setenv("POCKET_TEST_KEY", "")
	_, err = pocketClient(config.PocketSource{APIKeyEnv: "POCKET_TEST_KEY"})
	requirements.Error(err)
	assertions.NotContains(err.Error(), "synthetic-api-key")
	cmd := newSyncPocketCmd()
	requirements.NoError(cmd.Flags().Parse([]string{"--limit=-1"}))
	_, err = pocketImportOptions(cmd)
	requirements.Error(err)
	requirements.NoError(cmd.Flags().Set("limit", "2"))
	requirements.NoError(cmd.Flags().Set("after", "bad-date"))
	_, err = pocketImportOptions(cmd)
	requirements.Error(err)
	requirements.NoError(cmd.Flags().Set("after", "2026-09-01"))
	opts, err := pocketImportOptions(cmd)
	requirements.NoError(err)
	assertions.True(opts.Full)
	assertions.Equal(2, opts.Limit)
	assertions.Equal("2026-09-01", opts.StartedAfter.Format("2006-01-02"))
	assertions.True(manualSyncCLICommand([]string{"sync-pocket"}))
	assertions.NotNil(cmd.Flags().Lookup("build-cache"))
	assertions.NotNil(cmd.Flags().Lookup("no-build-cache"))
}

func TestPocketPartialFailureRefreshesWithoutCancellation(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	called := false
	err := finishPocketImport(ctx, &pocket.ImportSummary{MeetingsUpdated: 1}, errors.New("sync failed"), func(refreshCtx context.Context) error {
		called = true
		requirements.NoError(refreshCtx.Err())
		return errors.New("refresh failed")
	})
	requirements.Error(err)
	requirements.ErrorIs(err, context.Canceled)
	assertions.Contains(err.Error(), "refresh failed")
	assertions.True(called)
	called = false
	err = finishPocketImport(ctx, &pocket.ImportSummary{}, errors.New("before writes"), func(context.Context) error { called = true; return nil })
	requirements.Error(err)
	assertions.False(called)
}
