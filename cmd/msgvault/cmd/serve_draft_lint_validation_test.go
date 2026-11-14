package cmd

import (
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
