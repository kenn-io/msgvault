package store_test

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/personscope"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
	"testing"
)

func TestFilenameSuggestionPersonDirections(t *testing.T) {
	f := storetest.New(t)
	member, err := f.Store.EnsureParticipant("member@example.test", "Member Example", "example.test")
	require.NoError(t, err)
	other, err := f.Store.EnsureParticipant("other@example.test", "Other Example", "example.test")
	require.NoError(t, err)
	type example struct {
		name, kind, recipient         string
		sender                        any
		roster                        bool
		from, to, group, defaultGroup bool
		role                          personscope.Role
	}
	cases := []example{
		{name: "sender", sender: member, from: true, role: personscope.RoleFrom},
		{name: "envelope-from", recipient: "from", from: true, role: personscope.RoleFrom},
		{name: "to", recipient: "to", to: true, role: personscope.RoleTo},
		{name: "cc", recipient: "cc", to: true, role: personscope.RoleCC},
		{name: "bcc", recipient: "bcc", to: true, role: personscope.RoleBCC},
		{name: "direct-roster", kind: "direct_chat", sender: other, roster: true, to: true, role: personscope.RoleTo},
		{name: "direct-self", kind: "direct_chat", sender: member, roster: true, from: true, role: personscope.RoleFrom},
		{name: "group-roster", kind: "group_chat", roster: true, group: true, defaultGroup: true, role: personscope.RoleConversationMember},
		{name: "channel-roster", kind: "channel", roster: true, group: true, defaultGroup: true, role: personscope.RoleConversationMember},
		{name: "unclassified-roster", kind: "email_thread", roster: true, defaultGroup: true, role: personscope.RoleConversationMember},
		{name: "direct-unknown", kind: "direct_chat", roster: true, defaultGroup: true, role: personscope.RoleConversationMember},
		{name: "direct-envelope-self", kind: "direct_chat", recipient: "from", roster: true, from: true, role: personscope.RoleFrom},
	}
	var ids, messages []int64
	for _, tc := range cases {
		require := require.New(t)
		var conv, message, id int64
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,?,?) RETURNING id`), f.Source.ID, tc.name, tc.kind).Scan(&conv))
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sender_id) VALUES(?,?,?,'email',?) RETURNING id`), f.Source.ID, conv, tc.name, tc.sender).Scan(&message))
		require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'report.pdf','') RETURNING id`), message).Scan(&id))
		if tc.recipient != "" {
			require.NoError(f.Store.ReplaceMessageRecipients(message, tc.recipient, []int64{member}, []string{"Member Example"}))
		}
		if tc.roster {
			require.NoError(f.Store.EnsureConversationParticipant(conv, member, "member"))
		}
		ids = append(ids, id)
		messages = append(messages, message)
	}
	for _, selection := range []struct {
		name         string
		direction    personscope.Direction
		unclassified bool
	}{
		{"from", personscope.FromPerson, false}, {"to", personscope.ToPerson, false}, {"group", personscope.Group, false}, {"default-group", personscope.Group, true},
	} {
		t.Run(selection.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			scope := personscope.Scope{ParticipantIDs: []int64{member}, Directions: []personscope.Direction{selection.direction}, IncludeUnclassifiedRosterRows: selection.unclassified}
			rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), ids, store.FilenameSuggestionScope{Person: &scope})
			require.NoError(err)
			var want, got []int64
			var selectedMessages []int64
			for i, tc := range cases {
				match := tc.from && selection.direction == personscope.FromPerson || tc.to && selection.direction == personscope.ToPerson || tc.group && selection.direction == personscope.Group || tc.defaultGroup && selection.unclassified
				if match {
					want = append(want, ids[i])
					selectedMessages = append(selectedMessages, messages[i])
				}
			}
			// The untouched archive provenance implementation is an independent
			// parity oracle in this small fixture, never the request implementation.
			original, err := f.Store.PersonProvenanceForMessages(t.Context(), selectedMessages, scope)
			require.NoError(err)
			for _, row := range rows {
				got = append(got, row.File.ID)
				require.NotNil(row.PersonProvenance)
				assert.Equal(original[row.File.MessageID], row.PersonProvenance)
				for i, id := range ids {
					if id == row.File.ID {
						assert.Equal([]personscope.Role{cases[i].role}, row.PersonProvenance.Roles)
					}
				}
			}
			assert.ElementsMatch(want, got)
		})
	}
}

// A person's second binding must not turn their own direct message into a
// received attachment. All roles on a selected message remain in provenance.
func TestFilenameSuggestionAllRolesAndSenderSuppression(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	f, _, members := filenamePersonFixture(t, 2)
	var conv, message, id int64
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO conversations(source_id,source_conversation_id,conversation_type) VALUES(?,'multi-role','direct_chat') RETURNING id`), f.Source.ID).Scan(&conv))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO messages(source_id,conversation_id,source_message_id,message_type,sender_id) VALUES(?,?,'multi-role','email',?) RETURNING id`), f.Source.ID, conv, members[0]).Scan(&message))
	require.NoError(f.Store.DB().QueryRow(f.Store.Rebind(`INSERT INTO attachments(message_id,filename,storage_path) VALUES(?,'report.pdf','') RETURNING id`), message).Scan(&id))
	require.NoError(f.Store.EnsureConversationParticipant(conv, members[1], "member"))
	to := personscope.Scope{ParticipantIDs: members, Directions: []personscope.Direction{personscope.ToPerson}}
	rows, err := f.Store.FilenameSuggestionRowsContext(t.Context(), []int64{id}, store.FilenameSuggestionScope{Person: &to})
	require.NoError(err)
	assert.Empty(rows)
	_, err = f.Store.DB().Exec(f.Store.Rebind(`UPDATE conversations SET conversation_type='group_chat' WHERE id=?`), conv)
	require.NoError(err)
	for _, role := range []string{"from", "to", "cc", "bcc"} {
		require.NoError(f.Store.ReplaceMessageRecipients(message, role, members, []string{"First Example", "Second Example"}))
	}
	rows, err = f.Store.FilenameSuggestionRowsContext(t.Context(), []int64{id}, store.FilenameSuggestionScope{Person: &to})
	require.NoError(err)
	require.Len(rows, 1)
	require.NotNil(rows[0].PersonProvenance)
	assert.Equal(members, rows[0].PersonProvenance.ParticipantIDs)
	assert.Equal([]personscope.Role{personscope.RoleFrom, personscope.RoleTo, personscope.RoleCC, personscope.RoleBCC, personscope.RoleConversationMember}, rows[0].PersonProvenance.Roles)
	assert.Equal([]personscope.Direction{personscope.FromPerson, personscope.ToPerson, personscope.Group}, rows[0].PersonProvenance.Directions)
}
