package store_test

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/inboxcontrol"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func inboxCandidateFixture(t *testing.T) (*storetest.Fixture, inboxcontrol.SourceIdentity, []inboxcontrol.State) {
	t.Helper()
	f := storetest.New(t)
	source := inboxcontrol.SourceIdentity{SourceID: f.Source.ID, SourceType: "gmail", SourceIdentifier: f.Source.Identifier, AccountID: f.Source.Identifier}
	states := make([]inboxcontrol.State, 4)
	for i := range states {
		providerID := fmt.Sprintf("candidate-%d", i)
		mid := f.CreateMessage(providerID)
		states[i] = inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeMessage, ItemID: mid, ProviderID: providerID}, Inbox: new(true), Read: new(false), Tags: []string{"INBOX", "Todo"}, ObservedAt: time.Now().UTC()}
		_, err := f.Store.ObserveInboxState(t.Context(), states[i])
		require.NoError(t, err)
	}
	return f, source, states
}

func TestInboxCandidatesBoundedPagination(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	want := make(map[int64]bool, len(states))
	for _, state := range states {
		want[state.Target.ItemID] = true
	}
	// Exercise the complete accepted limit domain against the real query.
	for limit := 1; limit <= 100; limit++ {
		seen := make(map[int64]bool)
		cursor, revision := "", ""
		for {
			page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, limit, cursor)
			requirements.NoError(err)
			assertions.LessOrEqual(len(page.Candidates), limit)
			assertions.False(page.Unavailable)
			requirements.NotEmpty(page.ArchiveRevision)
			if revision == "" {
				revision = page.ArchiveRevision
			} else {
				assertions.Equal(revision, page.ArchiveRevision)
			}
			for _, candidate := range page.Candidates {
				assertions.False(seen[candidate.State.Target.ItemID], "candidate repeated across pages")
				assertions.True(candidate.Available)
				seen[candidate.State.Target.ItemID] = true
			}
			if page.NextCursor == "" {
				break
			}
			assertions.NotEqual(cursor, page.NextCursor)
			cursor = page.NextCursor
			requirements.LessOrEqual(len(seen), len(want))
		}
		assertions.Equal(want, seen)
	}
	for _, limit := range []int{-1, 0, 101} {
		_, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, limit, "")
		assertions.ErrorIs(err, inboxcontrol.ErrInvalid)
	}
}

func TestInboxCandidatesCursorRejectsStaleAndForeignScope(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, "")
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)
	other, err := f.Store.GetOrCreateSource("gmail", "other@example.com")
	requirements.NoError(err)
	foreign := source
	foreign.SourceID = other.ID
	foreign.SourceIdentifier = other.Identifier
	foreign.AccountID = other.Identifier
	_, err = f.Store.InboxCandidates(t.Context(), foreign, inboxcontrol.ScopeMessage, 1, page.NextCursor)
	requirements.ErrorIs(err, inboxcontrol.ErrConflict)
	foreign = source
	foreign.AccountID = "foreign@example.com"
	_, err = f.Store.InboxCandidates(t.Context(), foreign, inboxcontrol.ScopeMessage, 1, "")
	requirements.ErrorIs(err, inboxcontrol.ErrDenied)
	_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, page.NextCursor)
	requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	for _, cursor := range []string{"!", "e30", "bnVsbA"} {
		_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, cursor)
		requirements.ErrorIs(err, inboxcontrol.ErrInvalid)
	}
	// An ignored old observation must not invalidate a committed page.
	older := states[0]
	older.ObservedAt = older.ObservedAt.Add(-time.Hour)
	changed, err := f.Store.ObserveInboxState(t.Context(), older)
	requirements.NoError(err)
	assertions.False(changed)
	_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, page.NextCursor)
	requirements.NoError(err)
	states[0].Read = new(true)
	states[0].ObservedAt = states[0].ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), states[0])
	requirements.NoError(err)
	_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, page.NextCursor)
	assertions.ErrorIs(err, inboxcontrol.ErrPlanChanged)
}

func TestInboxCandidatesUnknownAndIncomingMetadata(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	initial, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, "")
	requirements.NoError(err)
	f.CreateMessage("incoming-unobserved")
	page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	assertions.True(page.Unavailable)
	assertions.NotEqual(initial.ArchiveRevision, page.ArchiveRevision)
	_, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, initial.NextCursor)
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	states[0].Read = nil
	states[0].ObservedAt = states[0].ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), states[0])
	requirements.NoError(err)
	page, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	var found bool
	for _, candidate := range page.Candidates {
		if candidate.State.Target == states[0].Target {
			found = true
			assertions.False(candidate.Available)
			assertions.Nil(candidate.State.Read)
		}
	}
	assertions.True(found)
	states[1].Inbox = new(false)
	states[1].ObservedAt = states[1].ObservedAt.Add(time.Hour)
	_, err = f.Store.ObserveInboxState(t.Context(), states[1])
	requirements.NoError(err)
	page, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	for _, candidate := range page.Candidates {
		assertions.NotEqual(states[1].Target, candidate.State.Target)
	}
}

func TestInboxCandidatesDoNotAccessBodiesOrUIRead(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	initial, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET is_read=TRUE WHERE id=?`), states[0].Target.ItemID)
	requirements.NoError(err)
	// The real candidate query must work even when the body relation is absent.
	_, err = f.Store.DB().Exec(`DROP TABLE message_bodies`)
	requirements.NoError(err)
	page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	assertions.Equal(initial.ArchiveRevision, page.ArchiveRevision)
	for _, candidate := range page.Candidates {
		requirements.NotNil(candidate.State.Read)
		assertions.False(*candidate.State.Read)
	}
	data, err := json.Marshal(page)
	requirements.NoError(err)
	assertions.NotContains(string(data), "body_text")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.Store.InboxCandidates(ctx, source, inboxcontrol.ScopeMessage, 1, "")
	assertions.ErrorIs(err, context.Canceled)
}

func TestInboxCandidatesAtPageCap(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	f, source, states := inboxCandidateFixture(t)
	for i := 4; i < 105; i++ {
		state := states[0]
		state.Target.ProviderID = fmt.Sprintf("candidate-%d", i)
		state.Target.ItemID = f.CreateMessage(state.Target.ProviderID)
		state.ObservedAt = time.Now().UTC()
		_, err := f.Store.ObserveInboxState(t.Context(), state)
		requirements.NoError(err)
	}
	page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	assertions.Len(page.Candidates, 100)
	requirements.NotEmpty(page.NextCursor)
	next, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, page.NextCursor)
	requirements.NoError(err)
	assertions.Len(next.Candidates, 5)
	assertions.Empty(next.NextCursor)
	seen := map[inboxcontrol.Target]bool{}
	for _, candidate := range append(page.Candidates, next.Candidates...) {
		assertions.False(seen[candidate.State.Target])
		seen[candidate.State.Target] = true
	}
}

func TestInboxCandidatesNativeSourceBindings(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"gmail", "", "msmail", "imap", "beeper"} {
		t.Run("provider-"+kind, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			st := testutil.NewTestStore(t)
			identifier := "owner@example.test"
			if kind == "imap" {
				identifier = "imap://owner%40example.test@mail.example.test:143"
			}
			archived, err := st.GetOrCreateSource(kind, identifier)
			requirements.NoError(err)
			normalized := kind
			if normalized == "" {
				normalized = "gmail"
			}
			source := inboxcontrol.SourceIdentity{SourceID: archived.ID, SourceType: normalized, SourceIdentifier: identifier, AccountID: "owner@example.test"}
			conv, err := st.EnsureConversation(archived.ID, "native-chat", "Synthetic chat")
			requirements.NoError(err)
			scope := inboxcontrol.ScopeMessage
			target := inboxcontrol.Target{SourceID: source.SourceID, SourceType: normalized, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: scope, ProviderID: "native-item"}
			if kind == "beeper" {
				scope = inboxcontrol.ScopeChat
				target.Scope = scope
				target.ItemID = conv
				target.ProviderID = "native-chat"
			} else {
				target.ItemID, err = st.UpsertMessage(&store.Message{SourceID: source.SourceID, ConversationID: conv, SourceMessageID: target.ProviderID, MessageType: "email", Subject: sql.NullString{String: "Synthetic subject", Valid: true}, Snippet: sql.NullString{String: "Synthetic snippet", Valid: true}})
				requirements.NoError(err)
			}
			if kind == "imap" {
				target.Mailbox = "INBOX"
				target.UIDValidity = 77
				target.UID = 1
				requirements.NoError(st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 2}, Reset: true, Memberships: []store.IMAPMembershipObservation{{Mailbox: "INBOX", UIDValidity: 77, UID: 1, SourceMessageID: target.ProviderID, Flags: []string{}}}}}))
			} else {
				state := inboxcontrol.State{Target: target, Inbox: new(true), Read: new(false), MarkedUnread: new(false), ObservedAt: time.Now().UTC()}
				_, err = st.ObserveInboxState(t.Context(), state)
				requirements.NoError(err)
			}
			page, err := st.InboxCandidates(t.Context(), source, scope, 1, "")
			requirements.NoError(err)
			requirements.Len(page.Candidates, 1)
			assertions.Equal(target, page.Candidates[0].State.Target)
			assertions.True(page.Candidates[0].Available)
			assertions.False(page.Unavailable)
			selected, err := st.InboxTriageSnapshot(t.Context(), source, []inboxcontrol.Target{target})
			requirements.NoError(err)
			requirements.Len(selected.Candidates, 1)
			assertions.Equal(target, selected.Candidates[0].State.Target)
			assertions.True(selected.Candidates[0].Available)
		})
	}
}

func TestInboxCandidatesBeeperIncomingAndMissingMarkers(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	st := testutil.NewTestStore(t)
	archived, err := st.GetOrCreateSource("beeper", "synthetic-account")
	requirements.NoError(err)
	source := inboxcontrol.SourceIdentity{SourceID: archived.ID, SourceType: "beeper", SourceIdentifier: archived.Identifier, AccountID: archived.Identifier}
	var missing inboxcontrol.State
	for i := range 2 {
		provider := fmt.Sprintf("chat-%d", i)
		conv, err := st.EnsureConversation(source.SourceID, provider, "Synthetic chat")
		requirements.NoError(err)
		state := inboxcontrol.State{Target: inboxcontrol.Target{SourceID: source.SourceID, SourceType: source.SourceType, SourceIdentifier: source.SourceIdentifier, AccountID: source.AccountID, Scope: inboxcontrol.ScopeChat, ItemID: conv, ProviderID: provider}, Inbox: new(true), Read: new(false), MarkedUnread: new(true), ObservedAt: time.Now().UTC()}
		if i == 0 {
			state.MarkedUnread = nil
			missing = state
		}
		_, err = st.ObserveInboxState(t.Context(), state)
		requirements.NoError(err)
	}
	page, err := st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, "")
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.True(page.Candidates[0].Available)
	assertions.True(page.Unavailable, "a missing marker on a later page must not imply complete source evidence")
	missing.MarkedUnread = new(true)
	missing.ObservedAt = missing.ObservedAt.Add(time.Second)
	_, err = st.ObserveInboxState(t.Context(), missing)
	requirements.NoError(err)
	page, err = st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, "")
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)
	assertions.False(page.Unavailable)
	_, err = st.UpsertMessage(&store.Message{SourceID: source.SourceID, ConversationID: missing.Target.ItemID, SourceMessageID: "new-message-in-existing-chat", MessageType: "beeper"})
	requirements.NoError(err)
	_, err = st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeChat, 1, page.NextCursor)
	assertions.ErrorIs(err, inboxcontrol.ErrPlanChanged)
}

func TestInboxCandidatesIMAPVanishedMembershipInvalidatesPage(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	t.Parallel()
	st := testutil.NewTestStore(t)
	archived, err := st.GetOrCreateSource("imap", "imap://owner%40example.test@mail.example.test:143")
	requirements.NoError(err)
	source := inboxcontrol.SourceIdentity{SourceID: archived.ID, SourceType: "imap", SourceIdentifier: archived.Identifier, AccountID: "owner@example.test"}
	conv, err := st.EnsureConversation(source.SourceID, "native-mailbox", "Synthetic mailbox")
	requirements.NoError(err)
	memberships := make([]store.IMAPMembershipObservation, 2)
	for i := range memberships {
		provider := fmt.Sprintf("INBOX|%d", i+1)
		_, err := st.UpsertMessage(&store.Message{SourceID: source.SourceID, ConversationID: conv, SourceMessageID: provider, MessageType: "email"})
		requirements.NoError(err)
		memberships[i] = store.IMAPMembershipObservation{Mailbox: "INBOX", UIDValidity: 77, UID: uint32(i + 1), SourceMessageID: provider, Flags: []string{}}
	}
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 3}, Reset: true, Memberships: memberships}}))
	page, err := st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, "")
	requirements.NoError(err)
	requirements.NotEmpty(page.NextCursor)
	// Only retirement occurs: no new membership observation can advance revision.
	requirements.NoError(st.ApplyIMAPMailboxDeltas(source.SourceID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 77, UIDNext: 3}, VanishedUIDs: []uint32{1}}}))
	_, err = st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 1, page.NextCursor)
	require.ErrorIs(t, err, inboxcontrol.ErrPlanChanged)
	page, err = st.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
	requirements.NoError(err)
	requirements.Len(page.Candidates, 1)
	assertions.Equal(uint32(2), page.Candidates[0].State.Target.UID)
}

func TestInboxCandidatesDeletedUnknownDoesNotMakeInboxUnavailable(t *testing.T) {
	t.Parallel()
	for _, column := range []string{"deleted_at", "deleted_from_source_at"} {
		t.Run(column, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)

			f, source, states := inboxCandidateFixture(t)
			states[0].Read = nil
			states[0].ObservedAt = states[0].ObservedAt.Add(time.Hour)
			_, err := f.Store.ObserveInboxState(t.Context(), states[0])
			requirements.NoError(err)
			page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
			requirements.NoError(err)
			assertions.True(page.Unavailable)
			_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE messages SET `+column+`=CURRENT_TIMESTAMP WHERE id=?`), states[0].Target.ItemID)
			requirements.NoError(err)
			page, err = f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 100, "")
			requirements.NoError(err)
			assertions.Len(page.Candidates, 3)
			assertions.False(page.Unavailable)
		})
	}
}

func TestInboxCandidatesCommittedMarkersDoNotProveProviderCompleteness(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	f, source, _ := inboxCandidateFixture(t)
	page, err := f.Store.InboxCandidates(t.Context(), source, inboxcontrol.ScopeMessage, 25, "")
	requirements.NoError(err)
	assertions.False(page.Unavailable)
	requirements.NotEmpty(page.ArchiveRevision)
	encoded, err := json.Marshal(page)
	requirements.NoError(err)
	var body struct {
		ProviderIngestion struct {
			Status string `json:"status"`
			Reason string `json:"reason"`
		} `json:"provider_ingestion"`
	}
	requirements.NoError(json.Unmarshal(encoded, &body))
	assertions.Equal("unknown", body.ProviderIngestion.Status)
	assertions.Equal("provider_completeness_unverified", body.ProviderIngestion.Reason)
}
