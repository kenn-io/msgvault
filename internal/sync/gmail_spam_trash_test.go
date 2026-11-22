package sync

import (
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/gmail"
)

func TestGmailSkipSpamTrashFullAndRecovery(t *testing.T) {
	// Use the real Gmail client to verify acquisition and presence requests
	// differ at the provider boundary, including across pages.
	env := gmailCoverageEnv(t)
	provider := &gmailCoverageTransport{history: 1000}
	client := gmailTransportClient(t, provider)
	opts := DefaultOptions()
	opts.GmailSkipSpamTrash = true
	env.Syncer = New(client, env.Store, opts)

	summary := runFullSync(t, env)
	assert.Equal(t, int64(2), summary.MessagesAdded)
	gmailCoverageAbsent(t, env, "spam")
	gmailCoverageAbsent(t, env, "trash")
	assert.Equal(t, []string{"false ", "false "}, gmailMessageLists(t, provider.requests))

	// Seed an archive that previously included Spam and Trash.
	env.Syncer = New(client, env.Store, nil)
	runFullSync(t, env)
	gmailCoverageRaw(t, env, "spam")
	gmailCoverageRaw(t, env, "trash")

	// An archived message moves to Trash while the History cursor expires.
	provider.requests = nil
	provider.labels = map[string]string{"normal1": "TRASH"}
	provider.history, provider.expire = 2000, true
	env.Syncer = New(client, env.Store, opts)
	summary, err := env.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t.Context(), t, env), nil)
	require.NoError(t, err)
	assert.Zero(t, summary.Errors)
	assert.Equal(t, []string{
		"false ", "false ",
		"true " + gmailSpamTrashQuery, "true " + gmailSpamTrashQuery,
		"true ", "true ",
	}, gmailMessageLists(t, provider.requests))
	assertMessageHasLabel(t, env.Store, "normal1", "TRASH")
	assertMessageNotHasLabel(t, env.Store, "normal1", "INBOX")
	for _, id := range []string{"normal1", "spam", "trash"} {
		assertDeletedFromSource(t, env.Store, id, false)
	}
}

// gmailMessageLists returns "includeSpamTrash q" for each message listing.
func gmailMessageLists(t *testing.T, requests []string) []string {
	t.Helper()
	var got []string
	for _, request := range requests {
		u, err := url.ParseRequestURI(request)
		require.NoError(t, err)
		if strings.HasSuffix(u.Path, "/messages") {
			got = append(got, u.Query().Get("includeSpamTrash")+" "+u.Query().Get("q"))
		}
	}
	return got
}

func TestGmailSkipSpamTrashFullSkipsMessageMovedAfterListing(t *testing.T) {
	env := gmailCoverageEnv(t)
	provider := &gmailCoverageTransport{history: 1000, moveOnList: map[string]string{"normal2": "SPAM"}}
	opts := DefaultOptions()
	opts.GmailSkipSpamTrash = true
	env.Syncer = New(gmailTransportClient(t, provider), env.Store, opts)

	summary := runFullSync(t, env)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	gmailCoverageRaw(t, env, "normal1")
	gmailCoverageAbsent(t, env, "normal2")
}

func TestGmailSkipSpamTrashReplayStopsRetrying(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	seedMessages(env, 1, 1000, "archived")
	runFullSync(t, env)
	env.SetOptions(t, func(opts *Options) { opts.GmailSkipSpamTrash = true })
	source := gmailCoverageSource(t.Context(), t, env)

	env.Mock.AddMessage("lost", testMIME(), []string{"INBOX"})
	env.Mock.GetMessageError["lost"] = errors.New("synthetic fetch failure")
	env.SetHistory(1001, historyAdded("lost"))
	runIncrementalSync(t, env)
	items, err := env.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, items, 1)

	// The retry finds the message in Spam and drops it from the retry list.
	delete(env.Mock.GetMessageError, "lost")
	env.Mock.Messages["lost"].LabelIDs = []string{"SPAM"}
	summary := runIncrementalSync(t, env)
	assert.Zero(t, summary.MessagesAdded)
	assertMessageCount(t, env.Store, 1)
	items, err = env.Store.ListUnresolvedSyncItemErrors(t.Context(), source.ID)
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestGmailSkipSpamTrashHistory(t *testing.T) {
	t.Parallel()
	env := newTestEnv(t)
	env.Mock.Labels = []*gmail.Label{
		{ID: "INBOX", Name: "INBOX", Type: "system"},
		{ID: "TRASH", Name: "TRASH", Type: "system"},
		{ID: "SPAM", Name: "SPAM", Type: "system"},
	}
	seedMessages(env, 1, 1000, "archived")
	runFullSync(t, env)
	env.SetOptions(t, func(opts *Options) { opts.GmailSkipSpamTrash = true })

	env.Mock.AddMessage("spam", testMIME(), []string{"SPAM"})
	env.Mock.AddMessage("trash", testMIME(), []string{"TRASH"})
	env.Mock.AddMessage("kept", testMIME(), []string{"INBOX"})
	env.SetHistory(1001,
		gmail.HistoryRecord{MessagesAdded: []gmail.HistoryMessage{
			{Message: gmail.MessageID{ID: "spam"}},
			{Message: gmail.MessageID{ID: "kept", ThreadID: "thread_kept"}},
		}},
		historyLabelAdded("trash", "TRASH"),
		historyLabelAdded("archived", "TRASH"),
	)
	summary := runIncrementalSync(t, env)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assert.Equal(t, int64(2), summary.MessagesSkipped)
	assert.Zero(t, summary.Errors)
	assertMessageCount(t, env.Store, 2)
	assertMessageHasLabel(t, env.Store, "archived", "TRASH")
	assertDeletedFromSource(t, env.Store, "archived", false)

	// A removal-only History event can make a previously skipped message eligible.
	env.Mock.Messages["spam"].LabelIDs = []string{"INBOX"}
	env.SetHistory(1002, historyLabelRemoved("spam", "SPAM"))
	summary = runIncrementalSync(t, env)
	assert.Equal(t, int64(1), summary.MessagesAdded)
	assertMessageHasLabel(t, env.Store, "spam", "INBOX")
}
