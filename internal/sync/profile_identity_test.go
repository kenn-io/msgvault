package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
)

func TestSyncRefreshesOAuthEvidence(t *testing.T) {
	t.Parallel()
	for _, incremental := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "incremental_noop"}[incremental], func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			source, err := env.Store.GetOrCreateSource("gmail", testEmail)
			require.NoError(err)
			if incremental {
				runFullSync(t, env)
			}
			require.NoError(env.Store.AddAccountIdentity(source.ID, testEmail, "manual"))
			before, err := env.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			revision, err := env.Store.AccountIdentityRevision()
			require.NoError(err)
			if incremental {
				runIncrementalSync(t, env)
			} else {
				runFullSync(t, env)
			}
			after, err := env.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			require.Len(after, 1)
			assert.Equal("manual,oauth", after[0].SourceSignal)
			assert.Equal(before[0].ConfirmedAt, after[0].ConfirmedAt)
			unchangedRevision, err := env.Store.AccountIdentityRevision()
			require.NoError(err)
			assert.Equal(revision, unchangedRevision)
			_, err = env.Store.RemoveAccountIdentity(source.ID, testEmail)
			require.NoError(err)
			if incremental {
				runIncrementalSync(t, env)
			} else {
				runFullSync(t, env)
			}
			removed, err := env.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			assert.Empty(removed, "sync must not restore removed ownership")
		})
	}
}

func TestProfileIdentityAccountBoundaries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, account, profile string
		confirmed, wantOAuth         bool
	}{
		{"primary", "gmail", "owner@example.test", "owner@example.test", true, true},
		{"case", "gmail", "OWNER@example.test", "owner@example.test", true, true},
		{"Workspace is exact", "gmail", "alias@example.test", "owner@example.test", true, false},
		{"unconfirmed stays unconfirmed", "gmail", "owner@example.test", "owner@example.test", false, false},
		{"IMAP config is not OAuth", "imap", "owner@example.test", "owner@example.test", true, false},
		{"empty profile", "gmail", "owner@example.test", "", true, false},
		{"display name profile", "gmail", "owner@example.test", "Owner <owner@example.test>", true, false},
		{"invalid profile", "gmail", "owner@example.test", "invalid", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			source, err := env.Store.GetOrCreateSource(tc.kind, tc.account)
			require.NoError(err)
			if tc.confirmed {
				require.NoError(env.Store.AddAccountIdentity(source.ID, tc.account, "manual"))
			}
			err = env.Syncer.refreshProfileIdentity(t.Context(), source, &gmail.Profile{EmailAddress: tc.profile})
			require.NoError(err)
			identities, err := env.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			if !tc.confirmed {
				assert.Empty(identities)
				return
			}
			require.Len(identities, 1)
			assert.Equal(tc.account, identities[0].Address)
			want := "manual"
			if tc.wantOAuth {
				want = "manual,oauth"
			}
			assert.Equal(want, identities[0].SourceSignal)
		})
	}
}

func TestProfileIdentityMissingProfile(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	env := newTestEnv(t)
	source, err := env.Store.GetOrCreateSource("gmail", testEmail)
	require.NoError(err)
	require.Error(env.Syncer.refreshProfileIdentity(t.Context(), source, nil))
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err)
	assert.Empty(t, identities)
}

func TestSyncContinuesAfterProfileAddressChange(t *testing.T) {
	t.Parallel()
	for _, incremental := range []bool{false, true} {
		t.Run(map[bool]string{false: "full", true: "incremental"}[incremental], func(t *testing.T) {
			t.Parallel()
			require := require.New(t)
			assert := assert.New(t)
			env := newTestEnv(t)
			source, err := env.Store.GetOrCreateSource("gmail", testEmail)
			require.NoError(err)
			if incremental {
				runFullSync(t, env)
			}
			require.NoError(env.Store.AddAccountIdentity(source.ID, testEmail, "manual"))
			seedMessages(env, 1, 2000, "after-rename")
			env.Mock.Profile.EmailAddress = "renamed@example.com"
			if incremental {
				env.SetHistory(2000, gmail.HistoryRecord{
					ID:            2000,
					MessagesAdded: []gmail.HistoryMessage{{Message: gmail.MessageID{ID: "after-rename"}}},
				})
				runIncrementalSync(t, env)
			} else {
				runFullSync(t, env)
			}
			ids, err := env.Store.MessageExistsBatch(source.ID, []string{"after-rename"})
			require.NoError(err)
			assert.NotZero(ids["after-rename"])
			identities, err := env.Store.ListAccountIdentities(source.ID)
			require.NoError(err)
			require.Len(identities, 1)
			assert.Equal("manual", identities[0].SourceSignal)
		})
	}
}

func TestProfileIdentityWriteFailureRetriesOnNoopSync(t *testing.T) {
	t.Parallel()
	require := require.New(t)
	assert := assert.New(t)
	env := newTestEnv(t)
	runFullSync(t, env)
	source, err := env.Store.GetSourceByIdentifier(testEmail)
	require.NoError(err)
	require.NoError(env.Store.AddAccountIdentity(source.ID, testEmail, "manual"))
	// SQLite trigger exercises the real persistence failure without mocking Store.
	_, err = env.Store.DB().Exec(`CREATE TRIGGER fail_oauth_refresh BEFORE UPDATE ON account_identities BEGIN SELECT RAISE(ABORT, 'synthetic OAuth write failure'); END`)
	require.NoError(err)
	runIncrementalSync(t, env)
	identities, err := env.Store.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("manual", identities[0].SourceSignal)
	_, err = env.Store.DB().Exec("DROP TRIGGER fail_oauth_refresh")
	require.NoError(err)
	runIncrementalSync(t, env)
	identities, err = env.Store.ListAccountIdentities(source.ID)
	require.NoError(err)
	require.Len(identities, 1)
	assert.Equal("manual,oauth", identities[0].SourceSignal)
}
