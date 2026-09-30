package store_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func newLegacyIMAPMembershipFixture(t *testing.T) imapMembershipFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("imap", "legacy-membership@example.test")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(source.ID, "legacy", "Legacy membership")
	require.NoError(t, err)
	return imapMembershipFixture{store: st, source: source, convID: conversation}
}

func TestApplyIMAPMailboxDeltas_LegacyMessageID(t *testing.T) {
	for _, test := range []struct {
		name, stored, observed string
	}{
		{"missing closing bracket", "<legacy@example.test", "legacy@example.test"},
		{"trailing parameters", `<ABCDEF@example.test> type="multipart/alternative"`, "ABCDEF@example.test"},
		{"domain case", `<ABCDEF@EXAMPLE.TEST> type="multipart/alternative"`, "ABCDEF@example.test"},
		{"bare first token", "legacy-token type=alternative", "legacy-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newLegacyIMAPMembershipFixture(t)
			id := f.createMessage(t, "Archive|1", test.stored)
			deltas := []store.IMAPMailboxDelta{
				{Mailbox: "Archive", Reset: true, State: store.IMAPFolderState{Mailbox: "Archive", UIDValidity: 77, UIDNext: 3}, Memberships: []store.IMAPMembershipObservation{
					{UID: 1, SourceMessageID: "Archive|1", RFC822MessageID: test.observed},
					{UID: 2, SourceMessageID: "Archive|2", RFC822MessageID: test.observed},
				}},
				{Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 78, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{
					{UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: test.observed},
				}},
			}
			for range 2 {
				requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, deltas))
				known, err := f.store.GetIMAPKnownUIDs(f.source.ID)
				requirements.NoError(err)
				assertions.Equal(map[string][]uint32{"Archive": {1, 2}, "INBOX": {1}}, known)
				got, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 78, 1)
				assertions.Equal(id, got)
				states, err := f.store.GetIMAPFolderStates(f.source.ID)
				requirements.NoError(err)
				assertions.ElementsMatch([]store.IMAPFolderState{deltas[0].State, deltas[1].State}, states)
			}
			var stored string
			requirements.NoError(f.store.DB().QueryRow(f.store.Rebind(`SELECT rfc822_message_id FROM messages WHERE id = ?`), id).Scan(&stored))
			assertions.Equal(test.stored, stored)
			assertions.Equal(3, membershipCount(t, f.store, f.source.ID))
		})
	}
}

func TestApplyIMAPMailboxDeltas_LegacyMessageIDSafety(t *testing.T) {
	for _, mode := range []string{"ambiguous", "live-deleted collision", "local case", "other source", "nested", "empty", "source precedence", "exact precedence"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newLegacyIMAPMembershipFixture(t)
			stored := `<Local@EXAMPLE.TEST> type="multipart/alternative"`
			id := f.createMessage(t, "Archive|1", stored)
			observation := store.IMAPMembershipObservation{UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: "Local@example.test"}
			success := false
			switch mode {
			case "ambiguous", "live-deleted collision":
				f.createMessage(t, "Archive|2", "<Local@example.test")
				if mode == "live-deleted collision" {
					requirements.NoError(f.store.MarkMessageDeleted(f.source.ID, "Archive|1"))
				}
			case "local case":
				observation.RFC822MessageID = "local@example.test"
			case "other source":
				other, err := f.store.GetOrCreateSource("imap", "other-legacy@example.test")
				requirements.NoError(err)
				f.source = other
			case "nested":
				observation.RFC822MessageID = "<<Local@example.test>>"
			case "empty":
				observation.RFC822MessageID = ""
			case "source precedence":
				f.createMessage(t, "Archive|2", "<Local@example.test")
				observation.SourceMessageID = "Archive|1"
				success = true
			case "exact precedence":
				f.createMessage(t, "Archive|2", "<Local@example.test")
				id = f.createMessage(t, "exact", "Local@example.test")
				success = true
			}
			err := f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
				Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 2},
				Memberships: []store.IMAPMembershipObservation{observation},
			}})
			if success {
				requirements.NoError(err)
				got, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 1, 1)
				assertions.Equal(id, got)
			} else {
				requirements.ErrorIs(err, sql.ErrNoRows)
				assertions.Zero(membershipCount(t, f.store, f.source.ID))
				states, err := f.store.GetIMAPFolderStates(f.source.ID)
				requirements.NoError(err)
				assertions.Empty(states)
			}
		})
	}
}

func TestApplyIMAPMailboxDeltas_LegacyMessageIDIndependent(t *testing.T) {
	for _, mode := range []string{"unique", "reappearance", "normalized ambiguity", "exact ambiguity", "live incumbent"} {
		t.Run(mode, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)
			f := newLegacyIMAPMembershipFixture(t)
			oldID := f.createMessage(t, "INBOX|1", "<old@example.test")
			if mode != "live incumbent" {
				requirements.NoError(f.store.MarkMessageDeleted(f.source.ID, "INBOX|1"))
			}
			requirements.NoError(f.store.UpsertIMAPFolderStates(f.source.ID, []store.IMAPFolderState{{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 2}}))
			id := f.createMessage(t, "new", `<new@EXAMPLE.TEST> type="multipart/alternative"`)
			observed := "new@example.test"
			switch mode {
			case "reappearance":
				observed, id = "old@example.test", oldID
			case "normalized ambiguity", "live incumbent":
				f.createMessage(t, "duplicate", "<new@example.test")
				if mode == "live incumbent" {
					id = oldID
				}
			case "exact ambiguity":
				f.createMessage(t, "exact-one", "new@example.test")
				f.createMessage(t, "exact-two", "<new@example.test>")
			}
			err := f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{
				Mailbox: "INBOX", Reset: true, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 20, UIDNext: 2},
				Memberships: []store.IMAPMembershipObservation{{UID: 1, SourceMessageID: "INBOX|1", RFC822MessageID: observed}},
			}})
			if mode == "normalized ambiguity" || mode == "exact ambiguity" {
				requirements.ErrorIs(err, sql.ErrNoRows)
				assertions.True(messageTombstoned(t, f.store, oldID))
				states, err := f.store.GetIMAPFolderStates(f.source.ID)
				requirements.NoError(err)
				assertions.Equal([]store.IMAPFolderState{{Mailbox: "INBOX", UIDValidity: 10, UIDNext: 2}}, states)
				key, err := f.store.GetMessageSourceID(oldID)
				requirements.NoError(err)
				assertions.Equal("INBOX|1", key)
				return
			}
			requirements.NoError(err)
			got, _ := membershipMessageAndFlags(t, f.store, f.source.ID, 20, 1)
			assertions.Equal(id, got)
			assertions.False(messageTombstoned(t, f.store, id))
		})
	}
}
