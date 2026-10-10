package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/inboxcontrol"
)

func newInboxCandidatesCmd() *cobra.Command {
	var source inboxcontrol.SourceIdentity
	var scope string
	var limit int
	var cursor string
	command := &cobra.Command{Use: "candidates", Short: "List bounded committed Inbox metadata for one exact source", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		nativeScope := inboxcontrol.Scope(scope)
		if source.Validate() != nil || limit < 1 || limit > 100 || len(cursor) > 16384 || (source.SourceType == "beeper" && nativeScope != inboxcontrol.ScopeChat) || (source.SourceType != "beeper" && nativeScope != inboxcontrol.ScopeMessage) {
			return usageErr(cmd, inboxcontrol.ErrInvalid)
		}
		client, _, err := OpenHTTPStore(cmd.Context())
		if err != nil {
			return err
		}
		defer func() { _ = client.Close() }()
		page, err := client.InboxCandidates(cmd.Context(), source, nativeScope, limit, cursor)
		if err != nil {
			return err
		}
		output, err := json.Marshal(page, jsontext.WithIndent("  "))
		if err != nil {
			return fmt.Errorf("encode inbox candidates: %w", err)
		}
		if _, err = fmt.Fprintln(cmd.OutOrStdout(), string(output)); err != nil {
			return fmt.Errorf("write inbox candidates: %w", err)
		}
		return nil
	}}
	flags := command.Flags()
	flags.Int64Var(&source.SourceID, "source-id", 0, "Exact archive source ID")
	flags.StringVar(&source.SourceType, "source-type", "", "Exact provider type: gmail, imap, msmail, or beeper")
	flags.StringVar(&source.SourceIdentifier, "source-identifier", "", "Exact archive source identifier")
	flags.StringVar(&source.AccountID, "account-id", "", "Exact provider account ID")
	flags.StringVar(&scope, "scope", "", "Native scope: message for mail, chat for Beeper")
	flags.IntVar(&limit, "limit", 25, "Maximum candidates per page (1–100)")
	flags.StringVar(&cursor, "cursor", "", "Opaque next_cursor from a prior page for this source and scope")
	for _, name := range []string{"source-id", "source-type", "source-identifier", "account-id", "scope"} {
		_ = command.MarkFlagRequired(name)
	}
	return command
}
