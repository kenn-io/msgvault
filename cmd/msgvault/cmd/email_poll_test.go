package cmd

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/gmail"
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
	gmailSource, err := st.GetOrCreateSource(sourceTypeGmail, "user@example.test")
	require.NoError(t, err)
	selected, err = pollingSource(t.Context(), "user@example.test", st, state)
	require.NoError(t, err)
	assert.Equal(t, gmailSource.ID, selected.ID)
	second, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://second@example.test")
	require.NoError(t, err)
	require.NoError(t, st.UpdateSourceDisplayName(second.ID, "shared@example.test"))
	require.NoError(t, st.UpdateSourceDisplayName(imapSource.ID, "shared@example.test"))
	_, err = pollingSource(t.Context(), "shared@example.test", st, state)
	assert.ErrorContains(t, err, "multiple sources")
	_, err = st.GetOrCreateSource(sourceTypeIMAP, "user@example.test")
	require.NoError(t, err)
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	assert.ErrorContains(t, err, "multiple sources")
	_, err = pollingSource(t.Context(), "missing@example.test", st, state)
	assert.ErrorContains(t, err, "no active")
	retired, err := st.GetOrCreateSource(sourceTypeGmail, "retired@example.test")
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?)`), retired.ID, gmailSource.ID)
	require.NoError(t, err)
	_, err = pollingSource(t.Context(), retired.Identifier, st, state)
	assert.ErrorContains(t, err, "no active")
}

func TestDeferredQuotaKeepsOtherFailures(t *testing.T) {
	deferred := fmt.Errorf("profile: %w", gmail.ErrQuotaDeferred)
	assert.True(t, gmail.IsQuotaDeferred(deferred))
	assert.False(t, gmail.IsQuotaDeferred(errors.Join(deferred, errors.New("other failure"))))
}

func TestPollingTargetsRejectInvalidConfiguration(t *testing.T) {
	st := storetest.New(t).Store
	cfg := config.NewDefaultConfig()
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	_, err := st.GetOrCreateSource(sourceTypeMSMail, "other@example.test")
	require.NoError(t, err)
	cfg.Accounts = []config.AccountSchedule{{Email: "other@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	assert.ErrorContains(t, validatePollingTargets(t.Context(), cfg, st, state), "no active")
	source, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	require.NoError(t, err)
	cfg.Accounts[0].Email = source.Identifier
	cfg.Sync.IMAPSkipFolders = map[string][]string{source.Identifier: {"Archive"}}
	assert.ErrorContains(t, validatePollingTargets(t.Context(), cfg, st, state), "folder filter")
	cfg.Sync.IMAPSkipFolders = nil
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "mailbox@example.test"))
	cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: "mailbox@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second})
	assert.ErrorContains(t, validatePollingTargets(t.Context(), cfg, st, state), "already has a polling account")
}
