package store_test

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestInboxContextBoundedExactMessage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("context-message")
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "context-message"}
	text := strings.Repeat("aé🙂", 20000)
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, text)
	requirements.NoError(err)
	for _, limit := range []int{1, 2, 3, 4, 5, 6, 7, 8, 16384, 65536} {
		result, err := f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MaxBytes: limit})
		requirements.NoError(err)
		assertions.Equal(target, result.Target)
		assertions.Equal(mid, result.MessageID)
		assertions.LessOrEqual(len(result.Text), limit)
		assertions.True(utf8.ValidString(result.Text))
		assertions.True(strings.HasPrefix(text, result.Text))
		assertions.True(result.Truncated)
		assertions.False(result.Unavailable)
		if len(result.Text) < limit {
			_, width := utf8.DecodeRuneInString(text[len(result.Text):])
			assertions.Greater(len(result.Text)+width, limit)
		}
	}
	result, err := f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
	requirements.NoError(err)
	assertions.LessOrEqual(len(result.Text), 16384)
	wrong := target
	wrong.AccountID = "foreign@example.test"
	_, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: wrong})
	require.ErrorIs(t, err, inboxcontrol.ErrDenied)
	wrong = target
	wrong.ProviderID = "wrong-native-id"
	_, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: wrong})
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	for _, limit := range []int{-1, 65537} {
		_, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MaxBytes: limit})
		require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.InboxContext(ctx, inboxcontrol.ContextRequest{Target: target})
	assertions.ErrorIs(err, context.Canceled)
}

func TestInboxContextMissingEmptyAndExactBound(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("context-message")
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "context-message"}
	result, err := f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
	requirements.NoError(err)
	assertions.True(result.Unavailable)
	assertions.Empty(result.Text)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "")
	requirements.NoError(err)
	result, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
	requirements.NoError(err)
	assertions.False(result.Unavailable)
	assertions.Empty(result.Text)
	assertions.False(result.Truncated)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE message_bodies SET body_text=? WHERE message_id=?`), "exact", mid)
	requirements.NoError(err)
	result, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MaxBytes: 5})
	requirements.NoError(err)
	assertions.Equal("exact", result.Text)
	assertions.False(result.Truncated)
	_, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MessageID: mid})
	assertions.ErrorIs(err, inboxcontrol.ErrInvalid)
}

func TestInboxContextChatRequiresExactMessageMembership(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("beeper", "synthetic-account")
	requirements.NoError(err)
	chat, err := st.EnsureConversation(source.ID, "native-chat", "Synthetic chat")
	requirements.NoError(err)
	other, err := st.EnsureConversation(source.ID, "other-chat", "Synthetic other chat")
	requirements.NoError(err)
	target := inboxcontrol.Target{SourceID: source.ID, SourceType: "beeper", SourceIdentifier: source.Identifier, AccountID: source.Identifier, Scope: inboxcontrol.ScopeChat, ItemID: chat, ProviderID: "native-chat"}
	var mids []int64
	for _, conv := range []int64{chat, other} {
		mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: strings.Repeat("m", len(mids)+1), MessageType: "beeper"})
		requirements.NoError(err)
		_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic message")
		requirements.NoError(err)
		mids = append(mids, mid)
	}
	_, err = st.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
	require.ErrorIs(t, err, inboxcontrol.ErrInvalid)
	result, err := st.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MessageID: mids[0]})
	requirements.NoError(err)
	assertions.Equal("Synthetic message", result.Text)
	_, err = st.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MessageID: mids[1]})
	assertions.ErrorIs(err, inboxcontrol.ErrDenied)
}

func TestInboxContextSQLitePreservesNUL(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	testutil.SkipIfPostgres(t, "PostgreSQL TEXT rejects NUL bytes; SQLite accepts them")
	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("nul-body")
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "nul-body"}
	_, err := f.Store.DB().Exec(f.Store.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "before\x00after")
	requirements.NoError(err)
	result, err := f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target, MaxBytes: 100})
	requirements.NoError(err)
	assertions.Equal("before\x00after", result.Text)
	assertions.False(result.Truncated)
}

func TestInboxContextOtherMailBindingsAndIMAPEpoch(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "msmail", "imap"} {
		t.Run("provider-"+kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			identifier := "owner@example.test"
			if kind == "imap" {
				identifier = "imap://owner%40example.test@mail.example.test:143"
			}
			source, err := st.GetOrCreateSource(kind, identifier)
			requirements.NoError(err)
			normalized := kind
			if normalized == "" {
				normalized = "gmail"
			}
			conv, err := st.EnsureConversation(source.ID, "synthetic-thread", "Synthetic thread")
			requirements.NoError(err)
			mid, err := st.UpsertMessage(&store.Message{SourceID: source.ID, ConversationID: conv, SourceMessageID: "native-message", MessageType: "email"})
			requirements.NoError(err)
			_, err = st.DB().Exec(st.Rebind(`INSERT INTO message_bodies(message_id,body_text) VALUES (?,?)`), mid, "Synthetic context")
			requirements.NoError(err)
			target := inboxcontrol.Target{SourceID: source.ID, SourceType: normalized, SourceIdentifier: identifier, AccountID: "owner@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "native-message"}
			if kind == "imap" {
				target.Mailbox = "INBOX"
				target.UIDValidity = 77
				target.UID = 1
				requirements.NoError(st.ApplyIMAPMailboxDeltas(source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 2}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 77, UID: 1, SourceMessageID: target.ProviderID, Flags: []string{}}}}}))
			}
			result, err := st.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
			requirements.NoError(err)
			assertions.Equal("Synthetic context", result.Text)
			if kind == "imap" {
				target.UIDValidity = 78
				_, err = st.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
				assertions.ErrorIs(err, inboxcontrol.ErrInvalid)
			}
		})
	}
}

func TestInboxContextDeniesAccountBeforeBodyLookup(t *testing.T) {
	t.Parallel()
	f := storetest.New(t)
	mid := f.CreateMessage("context-message")
	_, err := f.Store.DB().Exec(`DROP TABLE message_bodies`)
	require.NoError(t, err)
	target := inboxcontrol.Target{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: "foreign@example.test", Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: "context-message"}
	_, err = f.Store.InboxContext(t.Context(), inboxcontrol.ContextRequest{Target: target})
	assert.ErrorIs(t, err, inboxcontrol.ErrDenied)
}
