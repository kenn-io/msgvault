package store_test

import (
	"database/sql"
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestImportDependencySnapshotsRejectIdentityChangesAndUnretainedRepeatedKeys(t *testing.T) {
	for _, mode := range []string{"wrong-source", "wrong-key", "repeated-new-key", "missing-builder", "producer-revoked"} {
		t.Run(mode, func(t *testing.T) {
			require := Require.New(t)

			st := testutil.NewTestStore(t)
			_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-component", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
			require.NoError(err)
			source, err := st.GetOrCreateSource("eml", "synthetic-import@example.net")
			require.NoError(err)
			sync, err := st.StartSyncContext(t.Context(), source.ID, "full")
			require.NoError(err)
			scoped := st.ScopedToSync(source.ID, sync)
			run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, "eml", true, sync, 0)
			require.NoError(err)
			view, err := scoped.WithImportRun(t.Context(), run.ID)
			require.NoError(err)
			actualKey := "first"
			actualSource := source.ID
			if mode == "wrong-key" {
				actualKey = "undeclared"
			}
			if mode == "wrong-source" {
				other, e := st.GetOrCreateSource("eml", "synthetic-other@example.net")
				require.NoError(e)
				actualSource = other.ID
			}
			makeSnapshot := func(declared, actual string, sid int64) store.ParticipantMessageSnapshot {
				return store.ParticipantMessageSnapshot{SourceMessageID: declared, Participants: []store.ParticipantPersistData{{EmailAddress: "synthetic-sender@example.net", DisplayName: "Synthetic Sender"}}, Build: func(ids []int64) *store.MessagePersistData {
					return &store.MessagePersistData{Message: &store.Message{SourceID: sid, SourceMessageID: actual, MessageType: "email", SenderID: sql.NullInt64{Int64: ids[0], Valid: true}}, Conversation: &store.ConversationPersistData{SourceConversationID: "synthetic-thread", ConversationType: "email_thread"}, BodyText: sql.NullString{String: "Synthetic body", Valid: true}, RawMIME: []byte("Synthetic raw"), ReplaceReply: true}
				}}
			}
			first := makeSnapshot("first", actualKey, actualSource)
			second := makeSnapshot("second", "second", source.ID)
			want := "import alias snapshot identity mismatch"
			switch mode {
			case "repeated-new-key":
				second = makeSnapshot("first", "first", source.ID)
				want = "import alias decision is unavailable"
			case "missing-builder":
				second.Build = nil
				want = "participant builders"
			case "producer-revoked":
				_, err = st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-component"})
				require.NoError(err)
				want = "import_notifications_unavailable"
			}
			_, err = view.PersistImportMessageSnapshotsContext(t.Context(), source.ID, []store.ParticipantMessageSnapshot{first, second})
			require.ErrorContains(err, want)
			for _, table := range []string{"participants", "messages", "conversations", "message_bodies", "message_raw", "import_message_decisions", "mcp_event_log"} {
				var count int
				require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
				Assert.Zero(t, count, "atomic rejection must roll back "+table)
			}
		})
	}
}

func TestImportCompleteReplyRejectsParentOutsideCanonicalConversation(t *testing.T) {
	require := Require.New(t)

	st := testutil.NewTestStore(t)
	_, err := st.ConfigureMCPEvents(t.Context(), store.MCPEventsConfig{Enabled: true, ImportNotifications: true, Principal: "owner:synthetic-parent-scope", Capabilities: []store.MCPEventCapability{{Family: "msgvault.import_message_archived", SourceType: "eml", Kinds: []string{"message"}}}})
	require.NoError(err)
	source, err := st.GetOrCreateSource("eml", "synthetic-import@example.net")
	require.NoError(err)
	sync, err := st.StartSyncContext(t.Context(), source.ID, "full")
	require.NoError(err)
	scoped := st.ScopedToSync(source.ID, sync)
	run, err := scoped.BeginNativeImportAttempt(t.Context(), source.ID, "eml", true, sync, 0)
	require.NoError(err)
	view, err := scoped.WithImportRun(t.Context(), run.ID)
	require.NoError(err)
	_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "parent", MessageType: "email"}, Conversation: &store.ConversationPersistData{SourceConversationID: "different-native-thread", ConversationType: "email_thread"}, BodyText: sql.NullString{String: "Synthetic parent", Valid: true}})
	require.NoError(err)
	_, err = view.PersistMessageContext(t.Context(), &store.MessagePersistData{Message: &store.Message{SourceID: source.ID, SourceMessageID: "child", MessageType: "email"}, Conversation: &store.ConversationPersistData{SourceConversationID: "child-native-thread", ConversationType: "email_thread"}, BodyText: sql.NullString{String: "Synthetic child", Valid: true}, ReplaceReply: true, ReplyToSourceMessageID: "parent"})
	require.ErrorContains(err, "native reply parent is unavailable")
	for _, table := range []string{"messages", "conversations", "message_bodies", "import_message_decisions", "mcp_event_log"} {
		var count int
		require.NoError(st.DB().QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count))
		Assert.Equal(t, 1, count, "only the valid parent remains in "+table)
	}
}
