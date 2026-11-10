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
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftQuotedMailboxValidation(t *testing.T) {
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
func TestDraftInheritedSubjectStrict(t *testing.T) {
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
func TestDraftHumanValidationRecipients(t *testing.T) {
	f := newDraftReplyFixture(t)
	events, err := lintRun(t, f.grantedAdapter(), nil, "draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--to", "alex@example.com,pat@example.com", "--body", "Thanks", "--validate-only")
	require.NoError(t, err)
	require.NotEmpty(t, events)
	assert.Contains(t, events[0].Data, "to: alex@example.com, pat@example.com\n")
}
func TestDraftChatGreetingContext(t *testing.T) {
	for _, provider := range []string{"chat", "beeper"} {
		t.Run(provider, func(t *testing.T) {
			var st *store.Store
			var a *storeAPIAdapter
			var source *store.Source
			var create []string
			var draftID string
			if provider == "beeper" {
				f := newBeeperDraftFixture(t)
				st, a, source = f.store, f.adapter, f.source
				create = []string{"draft-compose", "--source-id", strconv.FormatInt(source.ID, 10), "--to", beeperDraftTestChat}
			} else {
				st = testutil.NewTestStore(t)
				var err error
				source, err = st.GetOrCreateSource("slack", "synthetic")
				require.NoError(t, err)
				conversation, err := st.EnsureConversationWithType(source.ID, "C1", "direct_chat", "Synthetic")
				require.NoError(t, err)
				a = &storeAPIAdapter{store: st}
				create = []string{"draft-compose", "--conversation", strconv.FormatInt(conversation, 10)}
			}
			// Create previews run before any managed row exists, including Beeper.
			for _, body := range []string{"Thanks", "Hi team,", "Hi Sam,"} {
				for _, access := range []string{"owner", "write-only"} {
					var grant *agentgrant.Grant
					if access == "write-only" {
						grant = &agentgrant.Grant{Permissions: []agentgrant.Permission{agentgrant.PermissionDraftCreate, agentgrant.PermissionDraftEdit}, Sources: []agentgrant.SourceRef{{ID: source.ID, Type: source.SourceType, Identifier: source.Identifier}}}
					}
					events, err := lintRun(t, a, grant, append(append([]string{}, create...), "--body", body, "--validate-only", "--json")...)
					require.NoError(t, err)
					got := lintObject(t, events)
					assert.Empty(t, got["warnings"])
					assert.Nil(t, got["recipients"])
					if body == "Hi Sam," {
						assert.Equal(t, []any{"greeting_name_mismatch"}, got["lint_skipped"])
					} else {
						assert.Empty(t, got["lint_skipped"])
					}
				}
			}
			events, err := lintRun(t, a, nil, append(append([]string{}, create...), "--body", "Thanks", "--json")...)
			require.NoError(t, err)
			got := lintObject(t, events)
			assert.NotContains(t, got, "warnings")
			assert.NotContains(t, got, "lint_skipped")
			draftID = got["draft_id"].(string)
			events, err = lintRun(t, a, nil, "draft-edit", draftID, "--revision", "1", "--body", "Hi Sam,", "--validate-only", "--json")
			require.NoError(t, err)
			got = lintObject(t, events)
			assert.Empty(t, got["warnings"])
			assert.Equal(t, []any{"greeting_name_mismatch"}, got["lint_skipped"])
		})
	}
}

func TestDraftCleanReceiptGolden(t *testing.T) {
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
