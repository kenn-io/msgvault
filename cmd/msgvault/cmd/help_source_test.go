package cmd

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
)

func TestSourceImportRequiredValuesBeforeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command func() *cobra.Command
		args    []string
		want    []string
	}{
		{"eml", newImportEMLCommand, []string{"./mail"}, []string{"--identifier", "you@example.com"}},
		{"slackdump", newImportSlackdumpCmd, []string{"./slackdump.zip"}, []string{"--me", "you@example.com"}},
		{"imazing", newImportIMazingCSVCmd, []string{"./messages"}, []string{"--me", "+15551234567"}},
		{"synctech", newImportSynctechSMSCmd, []string{"./sms.xml"}, []string{"--owner-phone", "+15551234567"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command()
			require.NotNil(t, command.Args)
			err := command.Args(command, tc.args)
			require.Error(t, err)
			for _, want := range tc.want {
				assert.Contains(t, err.Error(), want)
			}
		})
	}
}

func TestSourceMergeSelectorsBeforeConfirmation(t *testing.T) {
	command := newMergeAccountCmd()
	var output bytes.Buffer
	command.SetOut(&output)
	err := command.RunE(command, nil)
	require := require.New(t)
	assert := assert.New(t)
	require.Error(err)
	assert.Contains(err.Error(), "--from")
	assert.Contains(err.Error(), "--into")
	assert.Contains(err.Error(), "msgvault account list")
	assert.Empty(output.String())
}

func TestSourceImportUnknownFormat(t *testing.T) {
	if helpTestSubprocess(t) {
		return
	}
	ensureHelpLayout(rootCmd)
	command := directCommand(rootCmd, "import")
	var diagnostics bytes.Buffer
	command.SetErr(&diagnostics)
	err := command.Args(command, []string{"nope"})
	require := require.New(t)
	assert := assert.New(t)
	require.Error(err)
	assert.Contains(err.Error(), "unknown import format")
	for _, format := range command.Commands() {
		if !format.Hidden {
			assert.Contains(err.Error(), format.Name())
		}
	}
	assert.NotContains(diagnostics.String(), "deprecated")
}

func TestSourceImportArityExplainsSignature(t *testing.T) {
	for _, command := range []*cobra.Command{importMboxCmd, importPstCmd} {
		for _, args := range [][]string{nil, {"you@example.com"}} {
			err := command.Args(command, args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "<identifier>")
		}
	}
}

func TestSourceSyncCursorRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{HomeDir: dir, Data: config.DataConfig{DataDir: dir}}
	state := &invocation{cfg: cfg, logger: slog.Default()}
	err := runIncrementalSync(t.Context(), nil, nil, &store.Source{Identifier: "cursor@example.com"}, state)
	require := require.New(t)
	assert := assert.New(t)
	require.Error(err)
	assert.Equal(5, ExitCode(err))
	assert.Contains(classifyCLIError(nil, err).Hint, "msgvault sync full cursor@example.com")
	message, err := imapSkipReason(&store.Source{Identifier: "imap@example.com"}, cfg, slog.Default())
	require.NoError(err)
	assert.Contains(message, "msgvault source imap add")
	assert.Contains(message, "msgvault source o365 add imap@example.com")
}

func TestSourceGmailAddressIsValidationError(t *testing.T) {
	command := newAddAccountCmd()
	err := command.Args(command, []string{"Display Name <you@example.com>"})
	require.Error(t, err)
	assert.Equal(t, 3, ExitCode(err))
	assert.Contains(t, err.Error(), "--display-name")
}

func TestSourceRemoveEOFExplainsPreview(t *testing.T) {
	var prompt bytes.Buffer
	confirmed, err := confirmRemoveAccount(strings.NewReader(""), &prompt)
	assert := assert.New(t)
	assert.False(confirmed)
	require.Error(t, err)
	assert.Equal(2, ExitCode(err))
	require.ErrorIs(t, err, io.EOF)
	assert.Contains(err.Error(), "--yes")
	assert.Contains(err.Error(), "--dry-run")
	assert.Contains(err.Error(), "msgvault account remove")
}

func TestSourceCardDAVIDsExplainListCommand(t *testing.T) {
	root := newCardDAVCmd()
	assert := assert.New(t)
	require := require.New(t)
	for _, tc := range []struct {
		path []string
		hint string
	}{
		{[]string{"books", "set-role"}, "msgvault carddav books"},
		{[]string{"conflicts", "show"}, "msgvault carddav conflicts list"},
	} {
		command, _, err := root.Find(tc.path)
		require.NoError(err)
		for _, raw := range []string{"0", "-1", "abc", "9223372036854775808"} {
			_, err := cardDAVCLIPositiveID(command, raw)
			require.Error(err)
			assert.Equal(3, ExitCode(err))
			assert.Contains(classifyCLIError(command, err).Hint, tc.hint)
		}
	}
}

func TestSourceMissingProviderSetup(t *testing.T) {
	cfg := &config.Config{}
	for _, tc := range []struct {
		provider string
		run      func() error
	}{
		{"granola", func() error { _, err := granolaSources(cfg).one(nil); return err }},
		{"omi", func() error { _, err := omiSources(cfg).one(nil); return err }},
		{"circleback", func() error { _, err := circlebackSources(cfg).one(nil); return err }},
		{"muesli", func() error { _, err := muesliSources(cfg).one(nil); return err }},
		{"twenty", func() error { _, err := twentySources(cfg).one(nil); return err }},
		{"notion-meetings", func() error { _, err := notionMeetingsSources(cfg).one(nil); return err }},
		{"plaud", func() error { _, err := resolvePlaudSource(nil, cfg); return err }},
		{"twilio", func() error { _, err := resolveTwilioSource(nil, cfg); return err }},
		{"chatwoot", func() error { _, err := resolveChatwootProfiles(nil, cfg, false); return err }},
		{"inline", func() error { _, err := resolveInlineAccounts(cfg, nil); return err }},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			err := tc.run()
			require.Error(t, err)
			assert.Equal(t, 5, ExitCode(err))
			assert.Contains(t, classifyCLIError(nil, err).Hint, "msgvault source "+tc.provider+" add")
		})
	}
}
