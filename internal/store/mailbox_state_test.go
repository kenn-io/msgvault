package store_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/store"
)

func TestProviderMailboxGmailSnapshot(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)

	st, src, id := tagFixture(t, "gmail", "provider-1")
	check := func(inbox, read string) {
		t.Helper()
		state, err := st.ProviderMailboxStateContext(t.Context(), id)
		requirements.NoError(err)
		assertions.Equal(inbox, state.InboxState)
		assertions.Equal(read, state.ReadState)
	}
	check("unknown", "unknown")
	inbox, err := st.EnsureLabel(src.ID, "INBOX", "Inbox", "system")
	requirements.NoError(err)
	unread, err := st.EnsureLabel(src.ID, "UNREAD", "Unread", "system")
	requirements.NoError(err)
	_, err = st.ReconcileMessageLabels(id, []int64{inbox, unread}, true)
	requirements.NoError(err)
	check("inbox", "unread")
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET is_read=FALSE WHERE id=?"), id)
	requirements.NoError(err)
	check("inbox", "unread")
	requirements.NoError(st.RemoveMessageLabels(id, []int64{unread}))
	check("inbox", "read")
	_, err = st.ReconcileMessageLabels(id, nil, true)
	requirements.NoError(err)
	check("outside", "read")
	_, err = st.DB().Exec(st.Rebind("UPDATE messages SET source_message_id='replacement' WHERE id=?"), id)
	requirements.NoError(err)
	check("unknown", "unknown")
}

func TestProviderMailboxIMAPCopies(t *testing.T) {
	for _, tc := range []struct {
		name         string
		inboxFlags   []string
		archiveFlags []string
		wantRead     string
	}{
		{"Inbox takes precedence", []string{}, []string{"\\Seen"}, "unread"},
		{"read Inbox takes precedence", []string{"\\Seen"}, []string{}, "read"},
		{"missing FLAGS stays unknown", nil, nil, "unknown"},
		{"known unread other copy", nil, []string{}, "unread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			assertions := assert.New(t)

			st, src, id := tagFixture(t, "imap", "INBOX|7")
			deltas := []store.IMAPMailboxDelta{}
			for i, mailbox := range []string{"INBOX", "Archive"} {
				flags := tc.inboxFlags
				if i == 1 {
					flags = tc.archiveFlags
				}
				deltas = append(deltas, store.IMAPMailboxDelta{Mailbox: mailbox,
					State:       store.IMAPFolderState{Mailbox: mailbox, UIDValidity: uint32(i + 1), UIDNext: 8},
					Memberships: []store.IMAPMembershipObservation{{UID: 7, CanonicalSourceMessageID: "INBOX|7", Flags: flags}}})
			}
			requirements.NoError(st.ApplyIMAPMailboxDeltas(src.ID, deltas))
			state, err := st.ProviderMailboxStateContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal("inbox", state.InboxState)
			assertions.Equal(tc.wantRead, state.ReadState)
			requirements.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: deltas[0].State, VanishedUIDs: []uint32{7}}, {Mailbox: "Archive", State: deltas[1].State}}))
			state, err = st.ProviderMailboxStateContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal("outside", state.InboxState)
			// A stale provider copy must never overrule current durable membership.
			_, err = st.DB().Exec(st.Rebind("UPDATE messages SET deleted_from_source_at=CURRENT_TIMESTAMP WHERE id=?"), id)
			requirements.NoError(err)
			state, err = st.ProviderMailboxStateContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal("unknown", state.InboxState)
		})
	}
}

func FuzzProviderMailboxLocalReadIndependence(f *testing.F) {
	f.Add(true, false, false)
	f.Add(false, true, true)
	f.Fuzz(func(t *testing.T, inInbox, unread, localRead bool) {
		requirements := require.New(t)
		assertions := assert.New(t)

		st, src, id := tagFixture(t, "gmail", "provider-fuzz")
		labels := []int64{}
		for _, pair := range []struct {
			name    string
			enabled bool
		}{{"INBOX", inInbox}, {"UNREAD", unread}} {
			if pair.enabled {
				label, err := st.EnsureLabel(src.ID, pair.name, pair.name, "system")
				requirements.NoError(err)
				labels = append(labels, label)
			}
		}
		_, err := st.ReconcileMessageLabels(id, labels, true)
		requirements.NoError(err)
		before, err := st.ProviderMailboxStateContext(t.Context(), id)
		requirements.NoError(err)
		_, err = st.DB().Exec(st.Rebind("UPDATE messages SET is_read=? WHERE id=?"), localRead, id)
		requirements.NoError(err)
		after, err := st.ProviderMailboxStateContext(t.Context(), id)
		requirements.NoError(err)
		assertions.Equal(before, after, "provider=%t/%t local=%t", inInbox, unread, localRead)
	})
}

func TestProviderMailboxIMAPRequiresKnownInboxCopiesToReportRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{"unknown", nil, "unknown"}, {"unread", []string{}, "unread"}, {"read", []string{"\\Seen"}, "read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements, assertions := require.New(t), assert.New(t)
			st, src, id := tagFixture(t, "imap", "INBOX|7")
			requirements.NoError(st.ApplyIMAPMailboxDeltas(src.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 1, UIDNext: 9}, Memberships: []store.IMAPMembershipObservation{
				{UID: 7, CanonicalSourceMessageID: "INBOX|7", Flags: []string{"\\Seen"}},
				{UID: 8, CanonicalSourceMessageID: "INBOX|7", Flags: tc.flags},
			}}}))
			state, err := st.ProviderMailboxStateContext(t.Context(), id)
			requirements.NoError(err)
			assertions.Equal(tc.want, state.ReadState)
		})
	}
}
