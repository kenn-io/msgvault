package cmd

import (
	"fmt"
	"io"

	"go.kenn.io/msgvault/internal/oauth"
)

func printCommandHeadlessInstructions(out io.Writer, email, app string, calendar, readonly bool) {
	command := "msgvault add-account " + oauth.ShellQuote(email)
	if calendar {
		command = "msgvault add-calendar " + oauth.ShellQuote(email)
	}
	if app != "" {
		command += " --oauth-app " + oauth.ShellQuote(app)
	}
	if readonly {
		command += " --readonly"
	}
	exportCommand := "msgvault export-token " + oauth.ShellQuote(email)
	if calendar {
		exportCommand += " --upload-only"
	}
	_, _ = fmt.Fprintln(out, "Authorize on a machine with a browser using the same Google OAuth client.")
	_, _ = fmt.Fprintln(out, "Make any existing grant available in that machine's secret store first so re-consent preserves its permissions.")
	_, _ = fmt.Fprintln(out, "Run: "+command)
	_, _ = fmt.Fprintln(out, "Configure the server's credential commands and secret store separately, then use "+exportCommand+" to upload the token.")
	_, _ = fmt.Fprintln(out, "On the server, register the account with: "+command)
}
