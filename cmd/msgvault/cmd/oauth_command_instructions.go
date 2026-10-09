package cmd

import (
	"fmt"
	"io"

	"go.kenn.io/msgvault/internal/oauth"
)

// printCommandHeadlessInstructions explains headless setup with command-backed
// tokens. accountArgs comes from oauth.HeadlessAccountArgs for add-account or
// add-calendar; calendar selects the Calendar-only export.
func printCommandHeadlessInstructions(out io.Writer, email string, accountArgs []string, calendar bool) {
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
