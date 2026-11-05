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
		masked              bool
		candidate           bool
	}{
		{"subject code", "482913 is your verification code", "Thanks", true, false},
		{"body code", "Question", "Your verification code is 482913", true, false},
		{"candidate code", "Question", "Thanks", true, true},
		{"ordinary reply", "Question", "Thanks", false, false},
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
				{"delegated", grant},
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
						if tc.masked && reader.grant == grant {
							assertions.NotContains(out.String(), "482913")
							assertions.Contains(out.String(), "[verification content masked]")
						} else {
							assertions.Contains(out.String(), tc.body)
						}
						if asJSON {
							var output draftLifecycleOutput
							requirements.NoError(json.Unmarshal([]byte(out.String()), &output))
							assertions.Equal(created.DraftID, output.DraftID)
							assertions.Equal(created.Revision, output.Revision)
							if tc.masked && reader.grant == grant {
								assertions.Equal("[verification content masked]", output.Content)
								assertions.Equal("[verification content masked]", output.RawMIME)
								assertions.Equal("[verification content masked]", output.CandidateContent)
							} else {
								assertions.Contains(output.RawMIME, "Re: "+tc.subject)
								assertions.Equal(tc.body, output.Content)
								if tc.candidate {
									assertions.Contains(output.CandidateContent, "Your verification code is 482913")
								}
							}
						}
					}
				})
			}
		})
	}
}
