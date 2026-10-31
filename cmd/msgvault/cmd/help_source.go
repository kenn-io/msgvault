package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// applySourceHelp supplies presentation for groups synthesized by the grammar.
// Leaf help belongs to the original constructors so detached commands share it.
func applySourceHelp(root *cobra.Command) {
	source := directCommand(root, "source")
	if source == nil {
		return
	}
	source.Long = `Add a provider source, then sync it to archive its messages or contacts.
Start by choosing the provider below. Gmail, IMAP and Microsoft mail use
'msgvault sync'; chat, calendar and meeting providers use their own sync action.

Local exports use 'msgvault import <format>'. List registered sources and
their IDs with 'msgvault account list --json'.`
	source.Example = `  msgvault source gmail add you@gmail.com
  msgvault sync full you@gmail.com
  msgvault account list --json`
	providers := map[string]struct{ short, credentials string }{
		sourceTypeGmail:   {"Archive Gmail mail with Google OAuth", "Authorize a Google account in the browser or use --headless for instructions."},
		"imap":            {"Archive mail from an IMAP server", "Supply --host and --username; read the password from a masked prompt, stdin or MSGVAULT_IMAP_PASSWORD."},
		"o365":            {"Archive Microsoft 365 or Outlook.com mail", "Configure [microsoft] in config.toml, then authorize using a browser or --headless. --graph selects the Graph mail API."},
		"slack":           {"Archive Slack workspaces with a user token", "Read a Slack user token from --token-file, stdin, MSGVAULT_SLACK_TOKEN or the masked prompt."},
		"discord":         {"Archive Discord guilds with a bot token", "Read the bot token from stdin or the masked prompt; --guild selects guild IDs."},
		"teams":           {"Archive Microsoft Teams chats with Microsoft OAuth", "Configure [microsoft] in config.toml, then authorize delegated Graph access."},
		"matrix":          {"Archive Matrix rooms with a read-only device", "Provide the homeserver, user ID and --password-file or --login-token-file."},
		"beeper":          {"Archive chats exposed by Beeper Desktop", "Keep Beeper Desktop running on the daemon host and provide its developer token."},
		"inline":          {"Archive Inline chats with read-only authorization", "Authorize browser OAuth, or choose --transport cli to use an authenticated Inline CLI on the daemon host."},
		"calendar":        {"Archive Google Calendar events with OAuth", "Authorize Google Calendar access. --write additionally requests event write access; source permissions remain explicit opt-ins."},
		"carddav":         {"Sync CardDAV, Google and Microsoft contacts", "Provide a CardDAV URL, username and password on stdin, or authorize with --google or --microsoft."},
		"granola":         {"Archive Granola meeting notes and transcripts", "Configure [[granola]] in config.toml with an identifier and API key."},
		"omi":             {"Archive Omi meeting conversations and transcripts", "Configure [[omi]] in config.toml with an identifier and API key."},
		"plaud":           {"Archive Plaud cloud transcripts and notes", "Configure [[plaud]] in config.toml, then authorize browser OAuth on the daemon host."},
		"twenty":          {"Archive Twenty Call Recorder meetings", "Configure [[twenty]] in config.toml with the API endpoint and read-only key."},
		"twilio":          {"Archive Twilio calls, recordings and transcripts", "Configure [[twilio]] in config.toml with account credentials and account_email."},
		"circleback":      {"Archive Circleback meetings and action items", "Configure [[circleback]] in config.toml, then authorize browser OAuth."},
		"muesli":          {"Archive meetings from a local Muesli database", "Configure [[muesli]] in config.toml with the database path and account_email."},
		"notion-meetings": {"Archive Notion AI Meeting Notes", "Configure [[notion_meetings]] in config.toml with an integration token and account_email."},
		"chatwoot":        {"Archive Chatwoot inbox conversations and calls", "Configure [[chatwoot]] profiles in config.toml; set each api_key_env token on the daemon host."},
		"synctech-sms":    {"Archive SMS Backup & Restore files from Google Drive", "Provide a Drive folder ID, Google OAuth account and your E.164 owner phone number."},
	}
	for _, provider := range source.Commands() {
		definition, ok := providers[provider.Name()]
		if !ok {
			continue
		}
		provider.Short = definition.short
		provider.Long = definition.short + ". " + definition.credentials + "\n\nStart with 'msgvault source " + provider.Name() + " add'. "
		add := directCommand(provider, "add")
		if add != nil {
			provider.Example = firstSourceExample(add.Example)
		}
		if sync := directCommand(provider, "sync"); sync != nil {
			provider.Long += "Then run 'msgvault source " + provider.Name() + " sync'; use the add and sync help for selectors and options."
			provider.Example += "\n" + firstSourceExample(sync.Example)
		} else {
			provider.Long += "Use 'msgvault sync full <account>' for the first Gmail or IMAP download. Microsoft Graph mail uses 'msgvault sync <account>' from the first run."
			provider.Example += "\n  msgvault account list --json"
		}
	}
	account := directCommand(root, "account")
	account.Long = `Accounts are archive sources: email, chat, meetings and imported files.
Start with account list to find identifiers, types and numeric IDs. Commands
accept an identifier or alias, or --source-id for one exact row when supported.
Use update for display names and aliases; remove deletes an account's archive
data; merge moves a historical account into a compatible account.`
	account.Example = `  msgvault account list --json
  msgvault account update --source-id 3 --alias work`
	if list := directCommand(account, "list"); list != nil {
		list.Aliases = append(list.Aliases, "ls")
	}
	if remove := directCommand(account, "remove"); remove != nil {
		remove.Aliases = append(remove.Aliases, "rm", "delete")
	}
	if gmail := directCommand(source, "gmail"); gmail != nil {
		gmail.SuggestFor = append(gmail.SuggestFor, "google")
	}
	if microsoft := directCommand(source, "o365"); microsoft != nil {
		microsoft.SuggestFor = append(microsoft.SuggestFor, "outlook", "microsoft", "office365")
	}
	imports := directCommand(root, "import")
	imports.Long = `Import messages from a local export. Start with the matching format:

  mbox, pst         <identifier> <export-file>
  eml, maildir      <directory> --identifier <email-or-name>
  emlx              [Apple-Mail-directory] --identifier <email-or-name>
  whatsapp          <database> --phone <your-E.164-number>
  imessage          --db-path <chat.db> (defaults to macOS Messages)
  gvoice            <Takeout-Voice-directory>
  messenger         <export-directory> --me <your-Messenger-identifier>
  slackdump         <directory-or-zip> --me <your-Slack-ID-or-email>
  imazing-csv        <export-directory> --me <your-phone-or-email>
  synctech-sms       <XML-or-ZIP> --owner-phone <your-E.164-number>

eml expects MailMate-style .mailbox directories; maildir reads cur/new folders.
Imported sources appear in 'msgvault account list' and are not synced. Re-run
the import command for another export; see each format's help for checkpoints
and duplicate handling. Files must be readable on the host named by that
format's help.`
	imports.Example = `  msgvault import mbox you@example.com ./all-mail.mbox
  msgvault import maildir ./Maildir --identifier you@example.com
  msgvault import whatsapp ./msgstore.db --phone +15551234567`
	if slackdump := directCommand(imports, "slackdump"); slackdump != nil {
		slackdump.SuggestFor = append(slackdump.SuggestFor, "slack")
	}
	groupArgs := imports.Args
	imports.Args = func(cmd *cobra.Command, args []string) error {
		if cmd.Flags().Changed("type") && groupArgs != nil {
			return groupArgs(cmd, args)
		}
		if len(args) > 0 {
			formats := make([]string, 0, len(cmd.Commands()))
			for _, child := range cmd.Commands() {
				if !child.Hidden {
					formats = append(formats, child.Name())
				}
			}
			return usageErr(cmd, fmt.Errorf("unknown import format %q; choose: %s", args[0], strings.Join(formats, ", ")))
		}
		if groupArgs != nil {
			return groupArgs(cmd, args)
		}
		return nil
	}
}

func firstSourceExample(examples string) string {
	for line := range strings.SplitSeq(examples, "\n") {
		if strings.Contains(line, "msgvault ") {
			return line
		}
	}
	return ""
}

func sourceExactArgs(count int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.ExactArgs(count)(cmd, args); err != nil {
			return usageErr(cmd, fmt.Errorf("%s: %w", canonicalHelpText(cmd.UseLine()), err))
		}
		return nil
	}
}

func sourceImportArgs(name, meaning, example string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := sourceExactArgs(1)(cmd, args); err != nil {
			return err
		}
		return sourceRequiredString(cmd, name, meaning, example)
	}
}

// sourceNotConfigured adds setup metadata without changing the provider's
// existing diagnostic or losing a wrapped cause.
func sourceNotConfigured(provider string, err error) error {
	if err == nil {
		return nil
	}
	setup := "msgvault source " + provider + " add"
	switch provider {
	case sourceTypeGmail, "calendar":
		setup += " you@gmail.com"
	case "o365", "teams":
		setup += " you@example.com"
	case "imap":
		setup += " --host imap.example.com --username you@example.com"
	case "matrix":
		setup += " --homeserver https://matrix.example.org --user-id @archive:example.org --password-file ./matrix-password"
	case "synctech-sms":
		setup += " phone-backups --folder-id example-drive-folder --google-account you@gmail.com --owner-phone +15551234567"
	}
	return &cliError{
		Kind: "uninitialized", Code: "source_not_configured", ExitCode: 5,
		Message: err.Error(), Err: err,
		Hint: "configure this provider as shown by 'msgvault source " + provider + " add --help', then run '" + setup + "' with your account details",
	}
}
