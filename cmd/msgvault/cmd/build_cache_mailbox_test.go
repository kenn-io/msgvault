package cmd

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/query"
	"go.kenn.io/msgvault/internal/store"
)

func TestBuildCacheProviderMailboxRepair(t *testing.T) {
	for _, csv := range []string{"", "1"} {
		t.Run(map[string]string{"": "scanner", "1": "csv"}[csv], func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			t.Setenv("MSGVAULT_FORCE_CSV_SNAPSHOT", csv)
			root := t.TempDir()
			path := filepath.Join(root, "archive.db")
			st, err := store.Open(path)
			requirements.NoError(err)
			t.Cleanup(func() { _ = st.Close() })
			requirements.NoError(st.InitSchema())
			var unreadID int64
			for _, provider := range []string{"gmail", "imap", "msmail"} {
				src, err := st.GetOrCreateSource(provider, provider+"@example.test")
				requirements.NoError(err)
				conv, err := st.EnsureConversation(src.ID, "thread", "Mailbox fixture")
				requirements.NoError(err)
				id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, SourceMessageID: "mail-1", ConversationID: conv, MessageType: "email", SentAt: sql.NullTime{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), Valid: true}})
				requirements.NoError(err)
				switch provider {
				case "gmail":
					var labels []int64
					for _, name := range []string{"INBOX", "UNREAD"} {
						label, err := st.EnsureLabel(src.ID, name, name, "system")
						requirements.NoError(err)
						labels = append(labels, label)
						if name == "UNREAD" {
							unreadID = label
						}
					}
					_, err = st.ReconcileMessageLabels(id, labels, true)
					requirements.NoError(err)
				case "imap":
					requirements.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "mail-1", Flags: []string{}}}}}))
				case "msmail":
					inbox, read := true, false
					requirements.NoError(st.ObserveMicrosoftMailboxContext(t.Context(), src.ID, "mail-1", &inbox, &read))
				}
			}
			analytics := filepath.Join(root, "analytics")
			_, err = buildCache(path, analytics, true)
			requirements.NoError(err)
			check := func(want int64) {
				t.Helper()
				engine, err := query.NewDuckDBEngine(analytics, path, st.DB())
				requirements.NoError(err)
				defer func() { _ = engine.Close() }()
				page, err := engine.Explore(t.Context(), query.ExploreRequest{Context: query.Context{InboxStates: []string{"inbox"}, ReadStates: []string{"unread"}}})
				requirements.NoError(err)
				assertions.Equal(want, page.TotalCount)
				for _, row := range page.Rows {
					assertions.Equal("inbox", row.InboxState)
					assertions.Equal("unread", row.ReadState)
					assertions.NotEmpty(row.SourceIdentifier)
				}
			}
			check(3)
			if csv == "" {
				assertProviderMailboxSavedViewMCP(t, st, path, analytics)
			}
			var gmailID int64
			requirements.NoError(st.DB().QueryRow("SELECT m.id FROM messages m JOIN sources s ON s.id=m.source_id WHERE s.source_type='gmail'").Scan(&gmailID))
			requirements.NoError(st.RemoveMessageLabels(gmailID, []int64{unreadID}))
			result, err := buildCache(path, analytics, false)
			requirements.NoError(err)
			assertions.True(result.IdentityOnly, "state-only changes repair child datasets")
			check(2)
			var imapSource int64
			requirements.NoError(st.DB().QueryRow("SELECT id FROM sources WHERE source_type='imap'").Scan(&imapSource))
			delta := []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: "mail-1", Flags: []string{"\\Seen"}}}}}
			requirements.NoError(st.ApplyIMAPMailboxDeltas(imapSource, delta))
			result, err = buildCache(path, analytics, false)
			requirements.NoError(err)
			assertions.True(result.IdentityOnly)
			check(1)
			var before, after int64
			requirements.NoError(st.DB().QueryRow("SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='mailbox_state'").Scan(&before))
			requirements.NoError(st.ApplyIMAPMailboxDeltas(imapSource, delta))
			requirements.NoError(st.DB().QueryRow("SELECT COALESCE(MAX(seq),0) FROM cache_related_change_journal WHERE dataset='mailbox_state'").Scan(&after))
			assertions.Equal(before, after, "unchanged observations do not churn mailbox cache")
		})
	}
}
