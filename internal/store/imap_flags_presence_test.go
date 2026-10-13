package store_test

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/store"
)

func TestIMAPMembershipFlagsPresenceRoundTrip(t *testing.T) {
	for _, reset := range []bool{false, true} {
		mode := "delta"
		if reset {
			mode = "reset"
		}
		for _, tc := range []struct {
			name        string
			prior       string
			flags       []string
			changedItem bool
			want        []string
		}{
			{name: "new omitted"},
			{name: "new known empty", flags: []string{}, want: []string{}},
			{name: "omitted preserves cached flags", prior: `["\\Seen","Todo"]`, want: []string{`\Seen`, "Todo"}},
			{name: "omitted preserves cached empty", prior: `[]`, want: []string{}},
			{name: "omitted preserves unknown", prior: `null`},
			{name: "known empty clears cached flags", prior: `["\\Seen"]`, flags: []string{}, want: []string{}},
			{name: "complete replaces unknown", prior: `null`, flags: []string{"Todo"}, want: []string{"Todo"}},
			{name: "changed item cannot inherit flags", prior: `["\\Seen"]`, changedItem: true},
			{name: "malformed cached flags become unknown", prior: `{}`},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				assertions := assert.New(t)
				requirements := require.New(t)
				f, _ := newInboxIMAPSyncFixture(t)
				first := f.createMessage(t, "first", "")
				second := f.createMessage(t, "second", "")
				apply := func(id string, flags []string, reset bool) {
					requirements.NoError(f.store.ApplyIMAPMailboxDeltas(f.source.ID, []store.IMAPMailboxDelta{{Mailbox: "INBOX", Reset: reset, State: store.IMAPFolderState{Mailbox: "INBOX", UIDValidity: 17, UIDNext: 2}, Memberships: []store.IMAPMembershipObservation{{UID: 1, CanonicalSourceMessageID: id, Flags: flags}}}}))
				}
				if tc.prior != "" {
					apply("first", []string{}, false)
					_, err := f.store.DB().Exec(f.store.Rebind(`UPDATE imap_message_memberships SET flags=? WHERE source_id=?`), tc.prior, f.source.ID)
					requirements.NoError(err)
				}
				id, wantID := "first", first
				if tc.changedItem {
					id, wantID = "second", second
				}
				apply(id, tc.flags, reset)
				gotID, encoded := membershipMessageAndFlags(t, f.store, f.source.ID, 17, 1)
				var decoded []string
				requirements.NoError(json.Unmarshal([]byte(encoded), &decoded))
				assertions.Equal(wantID, gotID)
				assertions.Equal(tc.want, decoded)
				assertions.Equal(tc.want == nil, decoded == nil, "unknown and known-empty FLAGS are different evidence")
			})
		}
	}
}
