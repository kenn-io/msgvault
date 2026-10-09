package cmd

import (
	"fmt"
	"io"

	"go.kenn.io/msgvault/internal/oauth"
)

func printCommandHeadlessInstructions(out io.Writer, email, app string, calendar, readonly bool, write ...bool) {
	command := "add-account"
	if calendar {
		command = "add-calendar"
	}
	accountArgs := oauth.HeadlessAccountArgs(command, email, app, readonly, len(write) > 0 && write[0])
	browserArgs := append([]string(nil), accountArgs...)
	if !calendar {
		browserArgs = append(browserArgs, "--force")
	}
	exportArgs := []string{"msgvault", "export-token", email}
	if calendar {
		exportArgs = append(exportArgs, "--upload-only")
	}
	_, _ = fmt.Fprintln(out, "Authorize on a machine with a browser using the same Google OAuth client.")
	_, _ = fmt.Fprintln(out, "Make any existing grant available in that machine's secret store first so re-consent preserves its permissions.")
	_, _ = fmt.Fprintln(out, "Run:")
	oauth.PrintHeadlessCommand(out, browserArgs...)
	_, _ = fmt.Fprintln(out, "Configure the server's credential commands and secret store separately, then upload the token:")
	oauth.PrintHeadlessCommand(out, exportArgs...)
	_, _ = fmt.Fprintln(out, "On the server, register the account with:")
	oauth.PrintHeadlessCommand(out, accountArgs...)
}
