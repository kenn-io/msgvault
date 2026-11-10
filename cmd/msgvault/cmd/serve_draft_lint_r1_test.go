package cmd

import (
	"bytes"
	"encoding/json"
	"net/mail"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/agentgrant"
	"go.kenn.io/msgvault/internal/draftlint"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestR1DraftLintExplicitFalse(t *testing.T) {
	for _, command := range []string{"draft-compose", "draft-reply", "draft-forward", "draft-edit"} {
		for _, flag := range []string{"strict", "allow-placeholders", "validate-only"} {
			_, opts, err := parseDraftLintFlags([]string{command, "--" + flag + "=false"})
			require.NoError(t, err)
			assert.Equal(t, draftLintOptions{}, opts)
		}
	}
}
func TestR1QuotedMailboxValidation(t *testing.T) {
	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	args := []string{"draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--to", `"alex smith"@example.com`, "--cc", `"pat smith"@example.com`, "--bcc", `"sam smith"@example.com`, "--subject", "Hello", "--body", "Thanks", "--strict", "--json"}
	events, err := lintRun(t, a, nil, append(args, "--validate-only")...)
	require.NoError(t, err)
	got := lintObject(t, events)
	assert.Equal(t, true, got["valid"])
	recipients := got["recipients"].(map[string]any)
	for _, field := range []string{"to", "cc", "bcc"} {
		values := recipients[field].([]any)
		require.Len(t, values, 1)
		_, err := mail.ParseAddress(values[0].(string))
		assert.NoError(t, err)
	}
	_, err = lintRun(t, a, nil, args...)
	require.NoError(t, err)
}
func TestR1InheritedSubjectStrict(t *testing.T) {
	for _, command := range []string{"draft-reply", "draft-forward"} {
		t.Run(command, func(t *testing.T) {
			f := newDraftReplyFixture(t)
			raw, err := f.store.GetMessageRawContext(t.Context(), f.parentID)
			require.NoError(t, err)
			require.NoError(t, f.store.UpsertMessageRaw(f.parentID, bytes.Replace(raw, []byte("Subject: Question"), []byte("Subject: TODO: x"), 1)))
			args := []string{command, strconv.FormatInt(f.parentID, 10), "--body", "Thanks", "--strict", "--json"}
			if command == "draft-forward" {
				args = append(args, "--source-id", strconv.FormatInt(f.source.ID, 10), "--to", "alex@example.com")
			}
			_, err = lintRun(t, f.grantedAdapter(), nil, args...)
			require.NoError(t, err)
		})
	}
}
func TestR1HumanValidationRecipients(t *testing.T) {
	f := newDraftReplyFixture(t)
	events, err := lintRun(t, f.grantedAdapter(), nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--to", "alex@example.com,pat@example.com", "--body", "Thanks", "--validate-only")
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Contains(t, events[0].Data, "to: alex@example.com, pat@example.com\n")
	assert.NotContains(t, events[0].Data, "[alex")
}
func TestR1ChatGreetingContext(t *testing.T) {
	for _, provider := range []string{"chat", "beeper"} {
		for _, kind := range []string{"direct_chat", "group_chat", "channel"} {
			t.Run(provider+"/"+kind, func(t *testing.T) {
				var st *store.Store
				var source *store.Source
				var a *storeAPIAdapter
				native := "C1"
				if provider == "beeper" {
					f := newBeeperDraftFixture(t)
					st, source, a = f.store, f.source, f.adapter
					native = beeperDraftTestChat
				} else {
					st = testutil.NewTestStore(t)
					var err error
					source, err = st.GetOrCreateSource("slack", "synthetic")
					require.NoError(t, err)
					a = &storeAPIAdapter{store: st}
				}
				conversation, err := st.EnsureConversationWithType(source.ID, native, kind, "Synthetic")
				require.NoError(t, err)
				participant, err := st.EnsureParticipantByIdentifierContext(t.Context(), "slack", "U-ALEX", "Alex Rivera")
				require.NoError(t, err)
				person, _, err := st.CreatePersonFromParticipantContext(t.Context(), participant)
				require.NoError(t, err)
				given := "Alex"
				_, err = st.AddPersonNameContext(t.Context(), person.ID, store.PersonNameInput{NameKind: store.PersonNameStructured, Envelope: store.ValueEnvelopeInput{Source: store.ProvenanceUser}, GivenName: &given})
				require.NoError(t, err)
				require.NoError(t, st.EnsureConversationParticipantContext(t.Context(), conversation, participant, "member"))
				create := []string{"draft-compose", "--conversation", strconv.FormatInt(conversation, 10)}
				var draftID string
				if provider == "beeper" {
					create = []string{"draft-compose", "--source-id", strconv.FormatInt(source.ID, 10), "--to", native}
					draft, err := st.CreateBeeperDraftContext(t.Context(), source.ID, native, "original")
					require.NoError(t, err)
					draft, err = st.FinishBeeperDraftContext(t.Context(), draft.DraftID, draft.Revision, "original")
					require.NoError(t, err)
					draftID = draft.DraftID
				} else {
					draft, err := st.CreateChatDraftContext(t.Context(), conversation, 0, "original", func(string, string) error { return nil })
					require.NoError(t, err)
					draftID = draft.DraftID
				}
				for _, args := range [][]string{create, {"draft-edit", draftID, "--revision", "1"}} {
					for _, access := range []string{"owner", "read", "write-only"} {
						var grant *agentgrant.Grant
						if access != "owner" {
							grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}}
							if access == "read" {
								grant.Permissions = append(grant.Permissions, agentgrant.PermissionMessageRead)
							}
						}
						events, err := lintRun(t, a, grant, append(append([]string{}, args...), "--body", "Hi Sam,", "--validate-only", "--json")...)
						require.NoError(t, err, access)
						got := lintObject(t, events)
						assert.Nil(t, got["recipients"])
						if kind == "direct_chat" && access != "write-only" {
							assert.Contains(t, events[0].Data, `"code":"greeting_name_mismatch"`, access)
							assert.Empty(t, got["lint_skipped"], access)
						} else {
							assert.Empty(t, got["warnings"], access)
							assert.Equal(t, []any{"greeting_name_mismatch"}, got["lint_skipped"], access)
						}
					}
				}
			})
		}
	}
}

func TestR1CleanReceiptGolden(t *testing.T) {
	f := newDraftReplyFixture(t)
	participant, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	require.NoError(t, err)
	_, _, err = f.store.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	events, err := f.run(t, f.grantedAdapter(), "--body", "Thanks", "--json")
	require.NoError(t, err)
	got := lintObject(t, events)
	// Replace only generated values; compare every remaining byte to the base receipt.
	for _, key := range []string{"draft_id", "operation_ref", "rfc822_message_id"} {
		got[key] = "generated"
	}
	for _, key := range []string{"message_id", "source_id", "uid", "uidvalidity"} {
		got[key] = float64(1)
	}
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	assert.Equal(t, `{"draft_id":"generated","mailbox":"Drafts","message_id":1,"operation_ref":"generated","recipient_provenance":[{"address":"sender@example.com","basis":"curated_contact","flag":"ok","relationship":"contact","seen_in_archive":true}],"recipient_provenance_status":"checked","revision":1,"rfc822_message_id":"generated","source_id":1,"status":"created","uid":1,"uidvalidity":1}`, string(encoded))
}

func TestR1ChatGreetingReadScope(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic")
	require.NoError(t, err)
	a := &storeAPIAdapter{store: st}
	grant := &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionMessageRead}, Sources: []agentgrant.SourceRef{{ID: source.ID + 100, Type: source.SourceType, Identifier: "other"}}}
	got := a.lintDraft(t.Context(), draftlint.Input{Channel: "chat", Body: "Hi Sam,", SourceConversationID: "C1"}, source.ID, grant)
	assert.Empty(t, got.Warnings)
	assert.Equal(t, []string{draftlint.CodeGreetingNameMismatch}, got.Skipped)
	scope, status := a.draftReadableSources(t.Context(), source.ID, grant)
	assert.Nil(t, scope)
	assert.Equal(t, "unchecked", status)
}

func TestR1ChatWithoutContextReportsSkipped(t *testing.T) {
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "synthetic")
	require.NoError(t, err)
	_, err = st.EnsureConversationWithType(source.ID, "C1", "channel", "General")
	require.NoError(t, err)
	a := &storeAPIAdapter{store: st}
	result := a.lintDraft(t.Context(), draftlint.Input{Channel: "chat", Body: "Thanks", SourceConversationID: "C1"}, source.ID, nil)
	assert.Empty(t, result.Warnings)
	assert.Equal(t, []string{draftlint.CodeGreetingNameMismatch}, result.Skipped)
}
