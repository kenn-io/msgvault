package cmd

import (
	"bytes"
	"net/mail"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			var a *storeAPIAdapter
			var create []string
			if provider == "beeper" {
				f := newBeeperDraftFixture(t)
				a = f.adapter
				source := f.source
				create = []string{"draft-compose", "--source-id", strconv.FormatInt(source.ID, 10), "--to", beeperDraftTestChat}
			} else {
				st := testutil.NewTestStore(t)
				source, err := st.GetOrCreateSource("slack", "synthetic")
				require.NoError(t, err)
				conversation, err := st.EnsureConversationWithType(source.ID, "C1", "direct_chat", "Synthetic")
				require.NoError(t, err)
				a = &storeAPIAdapter{store: st}
				create = []string{"draft-compose", "--conversation", strconv.FormatInt(conversation, 10)}
			}
			// Create previews run before any managed row exists, including Beeper.
			events, err := lintRun(t, a, nil, append(append([]string{}, create...), "--body", "Hi Sam,", "--validate-only", "--json")...)
			require.NoError(t, err)
			got := lintObject(t, events)
			assert.Empty(t, got["warnings"])
			assert.Equal(t, []any{"greeting_name_mismatch"}, got["lint_skipped"])
			events, err = lintRun(t, a, nil, append(append([]string{}, create...), "--body", "Thanks", "--json")...)
			require.NoError(t, err)
			got = lintObject(t, events)
			assert.NotContains(t, got, "warnings")
			assert.NotContains(t, got, "lint_skipped")
		})
	}
}

func TestDraftCleanReceiptOmitsLintKeys(t *testing.T) {
	f := newDraftReplyFixture(t)
	participant, err := f.store.EnsureParticipant("sender@example.com", "Sender", "example.com")
	require.NoError(t, err)
	_, _, err = f.store.CreatePersonFromParticipantContext(t.Context(), participant)
	require.NoError(t, err)
	events, err := f.run(t, f.grantedAdapter(), "--body", "Thanks", "--json")
	require.NoError(t, err)
	got := lintObject(t, events)
	assert.NotContains(t, got, "warnings")
	assert.NotContains(t, got, "lint_skipped")
}
