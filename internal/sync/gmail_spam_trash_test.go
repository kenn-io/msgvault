package sync

import (
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
	assertGmailAcquisitionAndPresenceLists(t, provider.requests, []string{"false", "false"})

	// Seed an archive that previously included Spam and Trash.
	env.Syncer = New(client, env.Store, nil)
	runFullSync(t, env)
	gmailCoverageRaw(t, env, "spam")
	gmailCoverageRaw(t, env, "trash")

	// Resume with the exclusion enabled after Gmail expires the History cursor.
	provider.requests = nil
	provider.history, provider.expire = 2000, true
	env.Syncer = New(client, env.Store, opts)
	summary, err := env.Syncer.IncrementalWithHistoryRecovery(t.Context(), gmailCoverageSource(t.Context(), t, env), nil)
	require.NoError(t, err)
	assert.Zero(t, summary.Errors)
	assertGmailAcquisitionAndPresenceLists(t, provider.requests, []string{"false", "false", "true", "true"})
	assertDeletedFromSource(t, env.Store, "spam", false)
	assertDeletedFromSource(t, env.Store, "trash", false)
}

func assertGmailAcquisitionAndPresenceLists(t *testing.T, requests, want []string) {
	t.Helper()
	var got []string
	for _, request := range requests {
		u, err := url.ParseRequestURI(request)
		require.NoError(t, err)
		if strings.HasSuffix(u.Path, "/messages") {
			got = append(got, u.Query().Get("includeSpamTrash"))
		}
	}
	assert.Equal(t, want, got)
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
