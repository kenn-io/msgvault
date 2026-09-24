package cmd

import (
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/api"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestDraftComposeArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	intent, err := parseDraftComposeArgs([]string{
		"draft-compose", "--source-id", "42", "--from", "owner@example.test",
		"--to", "to@example.test", "--cc=copy@example.test", "--bcc", "hidden@example.test",
		"--subject", "Subject", "--body=body", "--json",
	})
	requirements.NoError(err)
	assertions.Equal(int64(42), intent.SourceID)
	assertions.Equal([]string{"to@example.test"}, intent.To)
	assertions.Equal([]string{"copy@example.test"}, intent.Cc)
	assertions.Equal([]string{"hidden@example.test"}, intent.Bcc)
	assertions.True(intent.JSON)

	for _, args := range [][]string{
		{"draft-compose", "--source-id", "0", "--to", "to@example.test"},
		{"draft-compose", "--to", "to@example.test"},
		{"draft-compose", "--source-id", "42"},
		{"draft-compose", "--source-id", "42", "--account", "owner@example.test", "--to", "to@example.test"},
	} {
		_, err := parseDraftComposeArgs(args)
		assertions.Error(err)
	}
}

func TestDraftComposeEndToEnd(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	fixture := newDraftReplyFixture(t)
	adapter := fixture.grantedAdapter()
	events := make([]api.CLIRunEvent, 0, 1)
	err := adapter.runCLIComposeDraft(t.Context(), api.CLIRunRequest{Args: []string{
		"draft-compose", "--source-id", strconv.FormatInt(fixture.source.ID, 10),
		"--from", testutil.IMAPTestUsername,
		"--to", "to@example.test", "--cc", "copy@example.test", "--bcc", "hidden@example.test",
		"--subject", "Compose subject", "--body", "compose body", "--json",
	}}, func(event api.CLIRunEvent) error {
		events = append(events, event)
		return nil
	})
	requirements.NoError(err)
	requirements.Len(events, 1)
	var result draftReplyOutput
	requirements.NoError(json.Unmarshal([]byte(events[0].Data), &result))
	assertions.Equal(draftReplyStatusCreated, result.Status)
	assertions.Equal(int64(1), result.Revision)

	message, err := fixture.store.GetMessage(result.MessageID)
	requirements.NoError(err)
	assertions.Equal([]string{"to@example.test"}, message.To)
	assertions.Equal([]string{"copy@example.test"}, message.Cc)
	assertions.Equal([]string{"hidden@example.test"}, message.Bcc)
	raw, err := fixture.store.GetMessageRaw(result.MessageID)
	requirements.NoError(err)
	assertions.Contains(string(raw), "Bcc:")
	assertions.Contains(string(raw), "hidden@example.test")

	matches, total, err := fixture.store.SearchMessages("copy@example.test", 0, 10)
	requirements.NoError(err)
	requirements.Equal(int64(1), total)
	requirements.Len(matches, 1)
	assertions.Equal(result.MessageID, matches[0].ID)
}
