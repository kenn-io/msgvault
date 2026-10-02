package cmd

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/gmail"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func setParentRecipients(t *testing.T, st *store.Store, parentID int64, recipientType string, addresses ...string) {
	t.Helper()
	ids := make([]int64, 0, len(addresses))
	for _, address := range addresses {
		id, err := st.EnsureParticipant(address, "", "")
		require.NoError(t, err)
		ids = append(ids, id)
	}
	require.NoError(t, st.ReplaceMessageRecipients(parentID, recipientType, ids, make([]string, len(ids))))
}

func TestGmailReplyPicksAddressedSender(t *testing.T) {
	const work, masked = "work@workspace.example", "masked@fastmail.example"
	for _, tc := range []struct {
		name, explicit, want, code string
		to, cc, grantSenders       []string
		unconfirmed                bool
	}{
		{name: "To alias", to: []string{work}, want: work},
		{name: "Cc alias", to: []string{"recipient@example.test"}, cc: []string{masked}, want: masked},
		{name: "same alias twice", to: []string{work}, cc: []string{"WORK@workspace.example"}, want: work},
		{name: "two aliases", to: []string{work}, cc: []string{masked}, code: "from_ambiguous"},
		{name: "no alias", to: []string{"recipient@example.test"}, code: "from_ambiguous"},
		{name: "explicit wins", to: []string{work}, explicit: "owner@example.test", want: "owner@example.test"},
		{name: "unconfirmed alias", to: []string{work}, unconfirmed: true, want: "owner@example.test"},
		{name: "grant keeps its one sender", to: []string{work}, grantSenders: []string{"owner@example.test"}, want: "owner@example.test"},
		{name: "grant filters addressed aliases", to: []string{work, masked}, grantSenders: []string{"owner@example.test", work}, want: work},
		{name: "grant allows no addressed alias", to: []string{masked}, grantSenders: []string{"owner@example.test", work}, code: "from_ambiguous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newGmailDraftTestFixture(t)
			if !tc.unconfirmed {
				for _, address := range []string{work, masked} {
					require.NoError(f.store.AddAccountIdentity(f.source.ID, address, "manual"))
				}
			}
			setParentRecipients(t, f.store, f.parentID, "to", tc.to...)
			setParentRecipients(t, f.store, f.parentID, "cc", tc.cc...)
			for _, address := range []string{work, masked} {
				f.client.sendAs = append(f.client.sendAs, gmail.SendAs{Email: address, VerificationStatus: "accepted"})
			}
			args := []string{"draft-reply", strconv.FormatInt(f.parentID, 10), "--body", "reply", "--json"}
			if tc.explicit != "" {
				args = append(args, "--from", tc.explicit)
			}
			req := api.CLIRunRequest{Args: args}
			if tc.grantSenders != nil {
				req.Grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{{ID: f.source.ID, Type: "gmail", Identifier: f.source.Identifier, SenderKeys: tc.grantSenders}}}
			}
			var events []api.CLIRunEvent
			err := f.adapter.runCLIReplyDraft(t.Context(), req, func(e api.CLIRunEvent) error { events = append(events, e); return nil })
			if tc.code != "" {
				require.EqualError(err, tc.code)
				assert.Zero(f.client.createCalls)
				return
			}
			require.NoError(err)
			require.Len(events, 1)
			var out gmailDraftReplyOutput
			require.NoError(json.Unmarshal([]byte(events[0].Data), &out))
			m, err := f.store.GetMessage(out.MessageID)
			require.NoError(err)
			assert.Equal(tc.want, m.From)
			assert.Equal("gmail-thread-1", out.ThreadID)
		})
	}
}

func TestIMAPReplyPicksAddressedSender(t *testing.T) {
	require := require.New(t)
	f := newDraftReplyFixture(t)
	require.NoError(f.store.AddAccountIdentity(f.source.ID, "alias@example.com", "manual"))
	setParentRecipients(t, f.store, f.parentID, "to", "alias@example.com")
	_, from, _, err := f.grantedAdapter().resolveDraftTarget(t.Context(), &f.parentID, "", 0, false, "", nil)
	require.NoError(err)
	assert.Equal(t, "<alias@example.com>", from)

	setParentRecipients(t, f.store, f.parentID, "to", testutil.IMAPTestUsername, "alias@example.com")
	_, from, _, err = f.grantedAdapter().resolveDraftTarget(t.Context(), &f.parentID, "", 0, false, "", nil)
	require.EqualError(err, "from_ambiguous")
	assert.Empty(t, from)
}
