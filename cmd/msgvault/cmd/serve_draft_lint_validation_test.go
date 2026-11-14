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
	assert := assert.New(t)
	require := require.New(t)

	f := newDraftReplyFixture(t)
	a := f.grantedAdapter()
	args := []string{"draft-compose", "--source-id", strconv.FormatInt(f.source.ID, 10), "--from", testutil.IMAPTestUsername, "--to", `"alex smith"@example.com`, "--cc", `"pat smith"@example.com`, "--bcc", `"sam smith"@example.com`, "--subject", "Hello", "--body", "Thanks", "--strict", "--validate-only"}
	events, err := lintRun(t, a, nil, append(args, "--json")...)
	require.NoError(err)
	got := lintObject(t, events)
	assert.Equal(true, got["valid"])
	recipients, ok := got["recipients"].(map[string]any)
	require.True(ok)
	for _, field := range []string{"to", "cc", "bcc"} {
		values, ok := recipients[field].([]any)
		require.True(ok)
		require.Len(values, 1)
		address, ok := values[0].(string)
		require.True(ok)
		_, err := mail.ParseAddress(address)
		require.NoError(err)
	}
	events, err = lintRun(t, a, nil, args...)
	require.NoError(err)
	require.NotEmpty(events)
	for _, recipient := range []struct{ field, address string }{
		{"to", `"alex smith"@example.com`},
		{"cc", `"pat smith"@example.com`},
		{"bcc", `"sam smith"@example.com`},
	} {
		assert.Contains(events[0].Data, recipient.field+": "+recipient.address+"\n")
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
