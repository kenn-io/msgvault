package cmd

import (
	"testing"

	Assert "github.com/stretchr/testify/assert"   //nolint:importas // Keep constructors available in nested test scopes.
	Require "github.com/stretchr/testify/require" //nolint:importas // Keep constructors available in nested test scopes.
	"go.kenn.io/msgvault/internal/sourcecatalog"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestNativeSourceInventory(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
	// Use the actual registered Cobra tree, including all native sync/import
	// handlers. A missing registration must prevent a misleading inventory.
	got, err := nativeSourceInventory(rootCmd)
	require.NoError(err)
	var types []string
	for _, descriptor := range got {
		types = append(types, descriptor.Type)
	}
	assert.ElementsMatch([]string{
		"gmail", "imap", "msmail", "beeper", "slack", "teams", "discord",
		"matrix", "gcal", "granola", "circleback", "plaud", "notion_meetings",
		"muesli", "synctech_sms", "carddav", "eml", "maildir", "mbox",
		"apple-mail", "pst", "google_voice", "apple_messages", "imazing_csv",
		"facebook_messenger", "whatsapp", "slackdump", "meeting_import", "twilio", "twenty",
	}, types)

	// Drive the production account scheduler resolver through synthetic Store
	// rows; offline and connector-specific types must not acquire mail routing.
	f := storetest.New(t)
	for _, sourceType := range []string{"gmail", "imap", "msmail", "teams", "mbox", "beeper", "vendor_archive"} {
		_, err := f.Store.GetOrCreateSource(sourceType, "owner@example.test")
		require.NoError(err)
	}
	targets, err := findScheduledSyncSources(f.Store, "owner@example.test")
	require.NoError(err)
	var routed []string
	for _, target := range targets {
		routed = append(routed, target.SourceType)
		_, ok := sourcecatalog.Lookup(target.SourceType)
		assert.True(ok)
	}
	assert.Equal([]string{"gmail", "imap", "msmail", "teams"}, routed)

	matrix, ok := sourcecatalog.Lookup("matrix")
	require.True(ok)
	assert.Contains(matrix.Limitations, "encrypted_messages_unavailable")
}

func TestNativeSourceInventoryRejectsAnUnregisteredImport(t *testing.T) {
	// A real command tree change must be detected at the daemon boundary,
	// rather than advertising a provider omitted from the registration table.
	root := newRootCommand()
	root.AddCommand(newImportEMLCommand())
	command := newImportMaildirCommand()
	command.Use = "import-unregistered <path>"
	root.AddCommand(command)
	_, err := nativeSourceInventory(root)
	Require.Error(t, err)
	Assert.ErrorContains(t, err, "import-unregistered")
}
