package cmd

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftLifecycleVerificationContent(t *testing.T) {
	for _, tc := range []struct {
		name, subject, body string
		candidate           bool
	}{
		{"candidate code", "Question", "Thanks", true},
		{"ordinary reply", "Question", "Thanks", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requirements := require.New(t)
			fixture := newDraftReplyFixture(t)
			adapter := fixture.grantedAdapter()
			raw, err := fixture.store.GetMessageRaw(fixture.parentID)
			requirements.NoError(err)
			raw = bytes.Replace(raw, []byte("Subject: Question\r\n"), []byte("Subject: "+tc.subject+"\r\n"), 1)
			requirements.NoError(fixture.store.UpsertMessageRaw(fixture.parentID, raw))
			ref := draftSourceRef(fixture.source)
			ref.SenderKeys = []string{testutil.IMAPTestUsername}
			grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate}, Sources: []agentgrant.SourceRef{ref}}
			var created draftReplyOutput
			err = adapter.runCLIReplyDraft(t.Context(), api.CLIRunRequest{
				Args:  []string{api.CLIRunDraftReplyCommand, strconv.FormatInt(fixture.parentID, 10), "--from", testutil.IMAPTestUsername, "--body", tc.body, "--json"},
				Grant: grant,
			}, func(event api.CLIRunEvent) error {
				return json.Unmarshal([]byte(event.Data), &created)
			})
			requirements.NoError(err)
			requirements.NotEmpty(created.DraftID)
			if tc.candidate {
				draft, err := fixture.store.GetIMAPDraftContext(t.Context(), created.DraftID)
				requirements.NoError(err)
				raw, err := fixture.store.GetMessageRaw(draft.CurrentMessageID)
				requirements.NoError(err)
				raw = bytes.Replace(raw, []byte("Thanks"), []byte("Your verification code is 482913"), 1)
				_, err = fixture.store.ClaimIMAPDraftContext(t.Context(), created.DraftID, created.Revision, store.IMAPDraftOperationEdit, raw)
				requirements.NoError(err)
			}
			reveal := *grant
			reveal.Permissions = []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionVerificationRead}
			for _, reader := range []struct {
				name  string
				grant *agentgrant.Grant
			}{
				{"verification reader", &reveal},
				{"owner", nil},
			} {
				t.Run(reader.name, func(t *testing.T) {
					requirements, assertions := require.New(t), assert.New(t)
					for _, asJSON := range []bool{true, false} {
						args := []string{api.CLIRunDraftGetCommand, created.DraftID}
						if asJSON {
							args = append(args, "--json")
						}
						var out strings.Builder
						err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: args, Grant: reader.grant}, func(event api.CLIRunEvent) error {
							out.WriteString(event.Data)
							return nil
						})
						requirements.NoError(err)
						assertions.Contains(out.String(), tc.body)
						if asJSON {
							var output draftLifecycleOutput
							requirements.NoError(json.Unmarshal([]byte(out.String()), &output))
							assertions.Equal(created.DraftID, output.DraftID)
							assertions.Equal(created.Revision, output.Revision)
							assertions.Contains(output.RawMIME, "Re: "+tc.subject)
							assertions.Equal(tc.body, output.Content)
							if tc.candidate {
								assertions.Contains(output.CandidateContent, "Your verification code is 482913")
							}
						}
					}
				})
			}
		})
	}
}

func TestDraftLifecycleBeeperVerificationMetadata(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	f := newBeeperDraftFixture(t)
	created, err := f.create(t, nil, "Your verification code is 482913")
	requirements.NoError(err)
	_, err = f.store.ClaimBeeperDraftContext(t.Context(), created.DraftID, created.Revision, store.BeeperDraftOperationEdit, "Use 482913 to verify your account")
	requirements.NoError(err)
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{Type: "beeper", Identifier: f.source.Identifier}}}
	for _, reader := range []*agentgrant.Grant{grant, nil, {Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit, agentgrant.PermissionVerificationRead}, Sources: grant.Sources}} {
		output, err := f.run(t, reader, api.CLIRunDraftGetCommand, created.DraftID)
		requirements.NoError(err)
		assertions.Equal(created.DraftID, output.DraftID)
		assertions.Equal(store.BeeperDraftOperationEdit, output.PendingOperation)
		if reader == grant {
			assertions.Nil(output.Content)
			assertions.Empty(output.CandidateContent)
		} else {
			requirements.NotNil(output.Content)
			assertions.Contains(*output.Content, "482913")
			assertions.Contains(output.CandidateContent, "482913")
		}
	}
}

func TestDraftLifecycleChatVerificationMetadata(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "workspace")
	requirements.NoError(err)
	conversation, err := st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	requirements.NoError(err)
	draft, err := st.CreateChatDraftContext(t.Context(), conversation, 0, "Your verification code is 482913", func(string, string) error { return nil })
	requirements.NoError(err)
	adapter := &storeAPIAdapter{store: st}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{Type: source.SourceType, Identifier: source.Identifier}}}
	for _, reader := range []*agentgrant.Grant{grant, nil, {Permissions: []agentgrant.Permission{agentgrant.PermissionDraftEdit, agentgrant.PermissionVerificationRead}, Sources: grant.Sources}} {
		var output chatDraftOutput
		err := adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: []string{api.CLIRunDraftGetCommand, draft.DraftID, "--json"}, Grant: reader}, func(event api.CLIRunEvent) error { return json.Unmarshal([]byte(event.Data), &output) })
		requirements.NoError(err)
		assertions.Equal(draft.DraftID, output.DraftID)
		if reader == grant {
			assertions.Empty(output.Body)
		} else {
			assertions.Equal("Your verification code is 482913", output.Body)
		}
	}
}

func TestDraftLifecycleGmailVerificationMetadata(t *testing.T) {
	requirements, assertions := require.New(t), assert.New(t)
	fixture := newGmailDraftTestFixture(t)
	draft := fixture.seedDraft(t)
	grant := delegatedGmailDraftGrant(fixture, []agentgrant.Permission{agentgrant.PermissionDraftEdit}, fixture.source.SourceType, fixture.source.Identifier)
	reveal := *grant
	reveal.Permissions = append([]agentgrant.Permission{agentgrant.PermissionVerificationRead}, grant.Permissions...)
	for _, reader := range []*agentgrant.Grant{nil, &reveal} {
		var output gmailDraftLifecycleOutput
		err := fixture.adapter.runCLIDraftLifecycle(t.Context(), api.CLIRunRequest{Args: []string{api.CLIRunDraftGetCommand, draft.DraftID, "--json"}, Grant: reader}, func(event api.CLIRunEvent) error { return json.Unmarshal([]byte(event.Data), &output) })
		requirements.NoError(err)
		assertions.Equal(draft.DraftID, output.DraftID)
		assertions.NotEmpty(output.Content)
		assertions.NotEmpty(output.RawMIME)
	}
}
