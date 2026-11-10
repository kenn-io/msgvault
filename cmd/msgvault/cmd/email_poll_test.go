package cmd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestPollingSourceSelection(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	st := storetest.New(t).Store
	imapSource, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(imapSource.ID, "user@example.test"))
	state := invocationFromContext(testInvocationContext(t.Context(), config.NewDefaultConfig(), invocationOptions{}))
	selected, err := pollingSource(t.Context(), "user@example.test", st, state)
	requirements.NoError(err)
	checks.Equal(imapSource.ID, selected.ID)
	state.cfg.Sync.IMAPSkipFolders = map[string][]string{imapSource.Identifier: {"Archive"}}
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.ErrorContains(err, "folder filter")
	state.cfg.Sync.IMAPSkipFolders = nil
	gmailSource, err := st.GetOrCreateSource(sourceTypeGmail, "user@example.test")
	requirements.NoError(err)
	selected, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.NoError(err)
	checks.Equal(gmailSource.ID, selected.ID)
	second, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://second@example.test")
	requirements.NoError(err)
	requirements.NoError(st.UpdateSourceDisplayName(second.ID, "shared@example.test"))
	requirements.NoError(st.UpdateSourceDisplayName(imapSource.ID, "shared@example.test"))
	_, err = pollingSource(t.Context(), "shared@example.test", st, state)
	requirements.ErrorContains(err, "multiple sources")
	_, err = st.GetOrCreateSource(sourceTypeIMAP, "user@example.test")
	requirements.NoError(err)
	_, err = pollingSource(t.Context(), "user@example.test", st, state)
	requirements.ErrorContains(err, "multiple sources")
	_, err = pollingSource(t.Context(), "missing@example.test", st, state)
	requirements.ErrorContains(err, "no active")
	retired, err := st.GetOrCreateSource(sourceTypeGmail, "retired@example.test")
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO source_settings (source_id, merged_into_source_id) VALUES (?, ?)`), retired.ID, gmailSource.ID)
	requirements.NoError(err)
	_, err = pollingSource(t.Context(), retired.Identifier, st, state)
	requirements.ErrorContains(err, "no active")
}

func TestPollingTargetsRejectDuplicateSource(t *testing.T) {
	st := storetest.New(t).Store
	cfg := config.NewDefaultConfig()
	state := invocationFromContext(testInvocationContext(t.Context(), cfg, invocationOptions{}))
	source, err := st.GetOrCreateSource(sourceTypeIMAP, "imaps://user@example.test")
	require.NoError(t, err)
	cfg.Accounts = []config.AccountSchedule{{Email: source.Identifier, Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second}}
	require.NoError(t, st.UpdateSourceDisplayName(source.ID, "mailbox@example.test"))
	cfg.Accounts = append(cfg.Accounts, config.AccountSchedule{Email: "mailbox@example.test", Enabled: true, Schedule: "0 * * * *", PollInterval: 15 * time.Second})
	require.ErrorContains(t, validatePollingTargets(t.Context(), cfg, st, state)[1], "already has a polling account")
}
