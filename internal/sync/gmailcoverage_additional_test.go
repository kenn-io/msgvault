package sync

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
)

func gmailCoverageTransportEnv(t *testing.T) (*TestEnv, *gmailCoverageTransport) {
	t.Helper()
	e := gmailCoverageEnv(t)
	f := &gmailCoverageTransport{history: 1000}
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: f}
	t.Cleanup(func() { http.DefaultClient = old })
	e.Syncer = New(gmail.NewClient(nil, gmail.WithConcurrency(1)), e.Store, nil)
	return e, f
}

func TestGmailCoverageBackfillAndQuery(t *testing.T) {
	require := require.New(t)

	e, f := gmailCoverageTransportEnv(t)
	// Simulate the provider's pre-fix restricted listing, then widen it.
	f.omitSpecial = true
	_, err := e.Syncer.Full(e.Context, testEmail)
	require.NoError(err)
	gmailCoverageAbsent(t, e, "spam")
	gmailCoverageAbsent(t, e, "trash")
	f.omitSpecial = false
	gmailCoverageReopen(t, e)
	idle, err := e.Syncer.Incremental(e.Context, gmailCoverageSource(t, e))
	require.NoError(err)
	require.Zero(idle.MessagesAdded)
	full, err := e.Syncer.Full(e.Context, testEmail)
	require.NoError(err)
	require.Equal(int64(2), full.MessagesAdded)
	gmailCoverageRaw(t, e, "spam")
	gmailCoverageRaw(t, e, "trash")
	again, err := e.Syncer.Full(e.Context, testEmail)
	require.NoError(err)
	require.Zero(again.MessagesAdded)
	// An explicit query still chooses a narrower acquisition scope.
	opts := DefaultOptions()
	opts.Query = "-in:spam -in:trash"
	e.Syncer = New(e.Syncer.client, e.Store, opts)
	_, err = e.Syncer.Full(e.Context, testEmail)
	require.NoError(err)
	seen := 0
	for _, request := range f.requests {
		u, err := url.Parse(request)
		require.NoError(err)
		if strings.HasSuffix(u.Path, "/messages") && u.Query().Get("q") == opts.Query {
			require.Equal("true", u.Query().Get("includeSpamTrash"))
			seen++
		}
	}
	require.Equal(2, seen)
	// The query neither removes already archived messages nor resets debt/cursor.
	gmailCoverageRaw(t, e, "spam")
	gmailCoverageRaw(t, e, "trash")
}

func TestGmailCoverageIncrementalAndReplay(t *testing.T) {
	require := require.New(t)

	e, f := gmailCoverageTransportEnv(t)
	e.CreateSourceWithHistory(t, "1000")
	for i, id := range []string{"spam", "trash"} {
		f.history = uint64(1001 + i)
		f.event = id
		if id == "spam" {
			f.failGet = true
		}
		first, err := e.Syncer.Incremental(e.Context, gmailCoverageSource(t, e))
		require.NoError(err)
		if id == "spam" {
			require.Equal(int64(1), first.Errors)
			f.failGet = false
			f.event = ""
			gmailCoverageReopen(t, e)
			retry, err := e.Syncer.Incremental(e.Context, gmailCoverageSource(t, e))
			require.NoError(err)
			require.Equal(int64(1), retry.MessagesAdded)
		} else {
			require.Equal(int64(1), first.MessagesAdded)
		}
		gmailCoverageRaw(t, e, id)
	}
	f.event = ""
	gmailCoverageReopen(t, e)
	idle, err := e.Syncer.Incremental(e.Context, gmailCoverageSource(t, e))
	require.NoError(err)
	require.Zero(idle.MessagesAdded)
}

func TestGmailCoverageRejectsOldCheckpoint(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fingerprinted", true: "untyped"}[legacy], func(t *testing.T) {
			require := require.New(t)

			e, f := gmailCoverageTransportEnv(t)
			f.failPage = true
			_, err := e.Syncer.Full(e.Context, testEmail)
			require.Error(err)
			// Model an interrupted pre-coverage traversal whose next-page token is unsafe.
			// This is the pre-coverage fingerprint for an unfiltered, unlimited Gmail full.
			fingerprint := any("full:v1:864b224bbaf1812b29969fde37ac8c7bb08b34ded81e0bc47e35023e9d480d18")
			if legacy {
				fingerprint = nil
			}
			_, err = e.Store.DB().Exec("UPDATE sync_runs SET request_fingerprint=?", fingerprint)
			require.NoError(err)
			f.requests = nil
			gmailCoverageReopen(t, e)
			result, err := e.Syncer.Full(e.Context, testEmail)
			require.NoError(err)
			require.False(result.WasResumed)
			gmailCoverageRaw(t, e, "spam")
			gmailCoverageRaw(t, e, "trash")
			firstPage := false
			for _, request := range f.requests {
				u, err := url.Parse(request)
				require.NoError(err)
				if strings.HasSuffix(u.Path, "/messages") && u.Query().Get("pageToken") == "" {
					firstPage = true
				}
			}
			require.True(firstPage)
		})
	}
}

func TestGmailCoverageInterruptedLegacyRecoveryRestartsAndCatchesUp(t *testing.T) {
	require := require.New(t)

	env, source, prior := setupInterruptedHistoryRecoveryWithPrefixChange(t)
	_, err := env.Store.DB().Exec("UPDATE sync_runs SET request_fingerprint=? WHERE id=?", store.GmailHistoryRecoveryRequestFingerprint, prior.ID)
	require.NoError(err)
	// The prior run's page token was generated before Spam/Trash were included.
	var oldToken sql.NullString
	require.NoError(env.Store.DB().QueryRow("SELECT cursor_before FROM sync_runs WHERE id=?", prior.ID).Scan(&oldToken))
	require.True(oldToken.Valid)

	listed := env.Mock.ListMessagesCalls
	env.Mock.HistoryError = nil
	env.Syncer = New(env.Mock, env.Store, nil)
	summary, err := env.Syncer.Full(env.Context, testEmail)
	require.NoError(err)
	require.True(summary.WasResumed)
	require.Empty(summary.ResumedFromToken, "legacy page token must not be reused")
	require.Greater(env.Mock.ListMessagesCalls, listed)
	assertRawDataExists(t, env.Store, "arrived-before-resume")
	refreshed, err := env.Store.GetSourceByID(source.ID)
	require.NoError(err)
	require.Equal("20000", refreshed.SyncCursor.String, "recovery catches up from pinned cursor")
	require.Equal([]uint64{15000}, env.Mock.HistoryCalls, "History starts at the inherited handoff, even though the full also found the new message")
}

func TestGmailCoverageFailedRecoveryDoesNotAdvanceSourceCursor(t *testing.T) {
	env := newTestEnv(t)
	source := env.CreateSourceWithHistory(t, "1000")
	env.Syncer = New(&recoveryProfileSequenceAPI{MockAPI: env.Mock, historyIDs: []uint64{2000, 2001}}, env.Store, nil)
	env.Mock.HistoryError = errors.New("synthetic history catch-up failure")
	_, err := env.Syncer.RecoverExpiredHistory(env.Context, source)
	require.ErrorContains(t, err, "synthetic history catch-up failure")
	refreshed, err := env.Store.GetSourceByID(source.ID)
	require.NoError(t, err)
	require.Equal(t, "1000", refreshed.SyncCursor.String)
}
