package importer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	pstlib "github.com/mooijtech/go-pst/v6/pkg"
	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep the package constructor available beside local helpers.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep the package constructor available beside local helpers.
	pstreader "go.kenn.io/msgvault/internal/pst"
	"go.kenn.io/msgvault/internal/testutil"
)

var syntheticPSTFixture = testutil.PSTFixture()
var syntheticPSTMediaFixture = testutil.PSTMediaFixture()
var syntheticPSTBrokenFixture = testutil.PSTBrokenMediaFixture()

func TestImportPSTSyntheticFixtureUsesNativeParser(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := filepath.Join(t.TempDir(), "synthetic-events.pst")
	require.NoError(os.WriteFile(path, syntheticPSTFixture, 0o600))
	file, err := pstreader.Open(path)
	require.NoError(err)
	defer func() { Assert.NoError(t, file.Close()) }()
	var messages []*pstreader.MessageEntry
	require.NoError(file.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		if entry.MsgCount == 0 {
			return nil
		}
		iterator, err := folder.GetMessageIterator()
		if err != nil {
			return fmt.Errorf("read synthetic PST folder messages: %w", err)
		}
		for iterator.Next() {
			message := pstreader.ExtractMessage(iterator.Value(), entry.Path)
			Require.NotNil(t, message)
			messages = append(messages, message)
		}
		return iterator.Err()
	}))
	require.Len(messages, 1)
	assert.Equal("Synthetic PST message", messages[0].Subject)
	assert.Equal("sender@example.net", messages[0].SenderEmail)
	assert.Equal("Synthetic body split across receipt-bound pages.", messages[0].BodyText)
	assert.Equal("<synthetic-pst@example.net>", messages[0].MessageID)
}

func TestImportPSTSyntheticAttachmentUsesNativeParser(t *testing.T) {
	assert := Assert.New(t)

	require := Require.New(t)

	path := filepath.Join(t.TempDir(), "synthetic-media.pst")
	require.NoError(os.WriteFile(path, syntheticPSTMediaFixture, 0o600))
	file, err := pstreader.Open(path)
	require.NoError(err)
	defer func() { Assert.NoError(t, file.Close()) }()
	var attachments []pstreader.AttachmentEntry
	require.NoError(file.WalkFolders(func(entry pstreader.FolderEntry, folder *pstlib.Folder) error {
		if entry.MsgCount == 0 {
			return nil
		}
		iterator, err := folder.GetMessageIterator()
		if err != nil {
			return fmt.Errorf("read synthetic PST folder messages: %w", err)
		}
		for iterator.Next() {
			found, err := pstreader.ReadAttachments(iterator.Value(), 1<<20)
			Require.NoError(t, err)
			attachments = append(attachments, found...)
		}
		return iterator.Err()
	}))
	require.Len(attachments, 1)
	assert.Equal("synthetic.bin", attachments[0].Filename)
	assert.Equal("application/octet-stream", attachments[0].MIMEType)
	assert.Equal([]byte("Synthetic PST attachment"), attachments[0].Content)
}
