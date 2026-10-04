package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/msgvault/internal/attachmentpolicy"
	imaplib "go.kenn.io/msgvault/internal/imap"
	"go.kenn.io/msgvault/internal/store"
)

func TestDraftForwardArgs(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	intent, err := parseDraftForwardArgs([]string{
		"draft-forward", "42", "--source-id", "7", "--from", "sender@example.test",
		"--to", "to@example.test", "--cc=cc@example.test", "--bcc", "bcc@example.test",
	})
	requirements.NoError(err)
	assertions.Equal(int64(42), intent.MessageID)
	assertions.Equal(int64(7), intent.SourceID)
	assertions.Equal([]string{"to@example.test"}, intent.To)
	assertions.Equal([]string{"cc@example.test"}, intent.Cc)
	assertions.Equal([]string{"bcc@example.test"}, intent.Bcc)
}

func TestDraftForwardArgsRequireExplicitDestinationAndRecipient(t *testing.T) {
	assertions := assert.New(t)
	for _, args := range [][]string{
		{"draft-forward", "42", "--to", "to@example.test"},
		{"draft-forward", "42", "--source-id", "7"},
		{"draft-forward", "42", "--source-id", "7", "--account", "account@example.test", "--to", "to@example.test"},
		{"draft-forward", "42", "--source-id", "7", "--to", "to@example.test", "--person-id", "3"},
		{"draft-forward", "42", "--source-id", "7", "--to", "to@example.test", "--person-id=3"},
		{"draft-forward", "42", "--person-id=3"},
		{"draft-forward", "42", "--person-id", "3"},
	} {
		_, err := parseDraftForwardArgs(args)
		assertions.Error(err, args)
	}
}

func TestDraftForwardArgsAllowEmptyNote(t *testing.T) {
	intent, err := parseDraftForwardArgs([]string{
		"draft-forward", "42", "--account=account@example.test", "--to=to@example.test", "--body=",
	})
	require.NoError(t, err)
	assert.Empty(t, intent.Body)
}

func TestPrepareIMAPDraftAttachmentWritesKeepsOccurrences(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	content := []byte("same bytes")
	digest := sha256.Sum256(content)
	hash := hex.EncodeToString(digest[:])
	draft, err := imaplib.BuildForward(imaplib.ForwardOptions{
		From: "sender@example.test", To: []string{"recipient@example.test"}, Subject: "Original",
		Attachments: []imaplib.ForwardAttachment{
			{Filename: "one.txt", ContentType: "text/plain", Content: content},
			{Filename: "two.txt", ContentType: "text/plain", Content: content},
		},
	}, time.Now(), "forward@example.test")
	requirements.NoError(err)
	refs := []store.AttachmentRef{
		{Filename: "one.txt", ContentHash: hash, State: attachmentpolicy.StateStored},
		{Filename: "two.txt", ContentHash: hash, State: attachmentpolicy.StateStored},
	}
	writes, err := prepareIMAPDraftAttachmentWrites(context.Background(), draft.Parsed, refs)
	requirements.NoError(err)
	requirements.Len(writes, 2)
	assertions.NotEmpty(writes[0].SourcePartKey)
	assertions.NotEqual(writes[0].SourcePartKey, writes[1].SourcePartKey)
	assertions.Equal(hash, writes[0].ContentHash)
	assertions.Equal(hash, writes[1].ContentHash)
}
