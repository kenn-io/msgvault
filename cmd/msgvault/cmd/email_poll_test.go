package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPollingSourceSelection(t *testing.T) {
	st := storetest.New(t).Store
	imapSource, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(imapSource.ID, "user@example.test"))
	state := invocationFromContext(testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{}))
	selected, err := pollingSource(t.Context(), "user@example.test", st, state)
	require.NoError(t, err)
	assert.Equal(t, imapSource.ID, selected.ID)
	state.cfg.Sync.IMAPSkipFolders = map[string][]string{imapSource.Identifier: {"Archive"}}
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	assert.ErrorContains(t, err, "folder filter")
	state.cfg.Sync.IMAPSkipFolders = nil
	_, err = st.GetOrCreateSource(sourceTypeGmail, "user@example.test")
	require.NoError(t, err)
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	assert.ErrorContains(t, err, "multiple sources")
	_, err = pollingSource(t.Context(), "missing@example.test", st, state)
	assert.ErrorContains(t, err, "no active")
}
