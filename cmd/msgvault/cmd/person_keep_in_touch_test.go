package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/spf13/cobra"
	Assert "github.com/stretchr/testify/assert" //nolint:importas // Keep package constructors available to assertion helpers in nested scopes.
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/keepintouch"
)

func TestKeepInTouchManualPreviewUsesSameRunner(t *testing.T) {
	assert := Assert.New(t)

	var dry bool
	calls := 0
	deps := keepInTouchCommandDeps{isDaemon: func() bool { return true }, run: func(_ context.Context, preview bool) (keepintouch.Result, error) {
		calls++
		dry = preview
		return keepintouch.Result{Items: []keepintouch.Item{{PersonID: 7, Status: "preview"}}}, nil
	}}
	root := &cobra.Command{Use: "test"}
	person := &cobra.Command{Use: "person"}
	keepInTouch := newPersonKeepInTouchCommand(deps)
	person.AddCommand(keepInTouch)
	root.AddCommand(person)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"person", "keep-in-touch", "check", "--dry-run", "--json"})
	require.NoError(t, root.Execute())
	assert.True(dry)
	assert.Equal(1, calls)
	assert.Contains(output.String(), `"person_id":7`)
}

func TestKeepInTouchSummaryFormatsSkippedReasons(t *testing.T) {
	assert := Assert.New(t)
	var output bytes.Buffer
	command := newPersonKeepInTouchCommand(keepInTouchCommandDeps{
		isDaemon: func() bool { return true },
		run: func(context.Context, bool) (keepintouch.Result, error) {
			return keepintouch.Result{
				Items:         []keepintouch.Item{{PersonID: 1}, {PersonID: 2}},
				DraftAttempts: 4,
				Skipped:       map[string]int{"stale": 2, "not_due": 3},
			}, nil
		},
	})
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"check"})
	require.NoError(t, command.Execute())
	assert.Contains(output.String(), "2 actionable; 4 draft attempts; Skipped: 3 not due; 2 outdated")
	assert.NotContains(output.String(), "map[")
}
func TestKeepInTouchManualForwardsThroughDaemon(t *testing.T) {
	proxied := false
	deps := keepInTouchCommandDeps{isDaemon: func() bool { return false }, proxy: func(_ *cobra.Command, _ []string) error { proxied = true; return nil }, run: func(context.Context, bool) (keepintouch.Result, error) {
		require.FailNow(t, "local archive runner must not execute in the client")
		return keepintouch.Result{}, nil
	}}
	root := newPersonKeepInTouchCommand(deps)
	root.SetArgs([]string{"check", "--dry-run"})
	require.NoError(t, root.Execute())
	Assert.True(t, proxied)
}
