package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSlackMediaBackfillExitPreservesOperationAndCacheErrors(t *testing.T) {
	operationErr := errors.New("workspace backfill failed")
	cacheErr := errors.New("cache refresh failed")

	err := slackMediaBackfillExit(nil, []error{operationErr}, cacheErr)

	require.ErrorIs(t, err, operationErr)
	require.ErrorIs(t, err, cacheErr)
	require.ErrorContains(t, err, "1 workspace(s) failed")
}

func TestSlackMediaBackfillExitPreservesInterruptionAndCacheError(t *testing.T) {
	cacheErr := errors.New("cache refresh failed")

	err := slackMediaBackfillExit(context.Canceled, nil, cacheErr)

	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, err, cacheErr)
	require.ErrorContains(t, err, "interrupted")
}

func TestSlackMediaBackfillExitReturnsCacheErrorAfterSuccessfulRun(t *testing.T) {
	cacheErr := errors.New("cache refresh failed")

	err := slackMediaBackfillExit(nil, nil, cacheErr)

	require.ErrorIs(t, err, cacheErr)
}
