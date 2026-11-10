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
	args := []string{"draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--to", `"alex smith"@example.com`, "--cc", `"pat smith"@example.com`, "--bcc", `"sam smith"@example.com`, "--subject", "Hello", "--body", "Thanks", "--strict", "--validate-only"}
	events, err := lintRun(t, a, nil, append(args, "--json")...)
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
	events, err = lintRun(t, a, nil, args...)
	require.NoError(t, err)
	require.NotEmpty(t, events)
	for _, recipient := range []struct{ field, address string }{
		{"to", `"alex smith"@example.com`},
		{"cc", `"pat smith"@example.com`},
		{"bcc", `"sam smith"@example.com`},
	} {
		assert.Contains(t, events[0].Data, recipient.field+": "+recipient.address+"\n")
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
