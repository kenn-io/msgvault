package cmd

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/textutil"
)

func newFamilyCmd() *cobra.Command {
	parent := &cobra.Command{Use: "family", Short: "Read an email's related copies"}
	parent.AddCommand(newFamilyShowCmd())
	return parent
}

func newFamilyShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "show <id>", Short: "Show an email's copies, replies and forwards",
		Long: `Show the other archived copies of an email, its replies and its forwards in
other accounts and conversations. Each row names the account, whether that
account sent or received the copy, and the header evidence that connects it to
the email: the same Message-ID, a reply, a forward, shared references, a
relation through another copy, or only the same provider thread.

Relations come from Message-ID, In-Reply-To, References and attached-original
headers, plus the Gmail or Google Groups threads of the email and its exact
copies. A copy that hasn't been indexed yet may have relations this output
cannot show; sync, or run repair-derived, to index it. A copy whose original
message isn't stored, or is stored in a format that can't be read, says so, and
storing the message again clears it. A copy whose headers couldn't be read stays
marked, and indexing again won't change that. While any
email in the archive is still waiting to be indexed, a note on stderr says so,
because that email could be a related copy the lookup cannot see yet. Rows are in ascending message ID order, and the
row marked "this message" is the email you gave. The lookup stops at 500
emails or 5,000 identifiers and says which on stderr.
Use --json for the full response.

Examples:
  msgvault family show 123
  msgvault family show 123 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil || id < 1 {
				return usageErr(cmd, errors.New("message ID must be a positive archive ID"))
			}
			client, _, err := OpenHTTPStore(cmd.Context(), emailFamilyMinAPISchemaVersion)
			if err != nil {
				return fmt.Errorf("open store: %w", err)
			}
			defer func() { _ = client.Close() }()
			family, err := client.EmailFamilyContext(cmd.Context(), id)
			if err != nil {
				return fmt.Errorf("get related copies: %w", err)
			}
			if asJSON {
				enc := jsontext.NewEncoder(cmd.OutOrStdout(), jsontext.WithIndent("  "))
				return json.MarshalEncode(enc, family, json.Deterministic(true))
			}
			return writeRelatedCopies(cmd, family)
		},
	}
	cmd.Flags().BoolVar(&asJSON, flagJSON, false, "Output the related copies as JSON")
	return cmd
}

func init() { rootCmd.AddCommand(newFamilyCmd()) }

// relatedUnreadableLabel names why a copy's identifiers are incomplete.
func relatedUnreadableLabel(outcome string) string {
	switch outcome {
	case "missing_raw":
		return "original message not stored"
	case "unsupported_raw":
		return "stored format not readable"
	}
	return "headers not fully read"
}

var relatedEvidenceLabels = map[string]string{
	store.EmailFamilyEvidenceAnchor:            "this message",
	store.EmailFamilyEvidenceCopy:              "same message",
	store.EmailFamilyEvidenceReply:             "reply",
	store.EmailFamilyEvidenceRepliedTo:         "replied to",
	store.EmailFamilyEvidenceForward:           "forwarded",
	store.EmailFamilyEvidenceForwardedOriginal: "forwarded original",
	store.EmailFamilyEvidenceReferences:        "shared references",
	store.EmailFamilyEvidenceIndirect:          "through another copy",
	store.EmailFamilyEvidenceThread:            "same conversation",
}

func relatedDirection(member store.EmailFamilyMember) string {
	switch {
	case member.Draft:
		return "draft"
	case member.AccountPath == "sent":
		return "sent"
	case member.AccountPath == "inbound":
		return "received"
	}
	return "-"
}

func writeRelatedCopies(cmd *cobra.Command, family *store.EmailFamily) error {
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "Message %d: %d related copies\n", family.AnchorMessageID, len(family.Members)-1); err != nil {
		return fmt.Errorf("write related copies: %w", err)
	}
	pending, unreadable := 0, 0
	for _, member := range family.Members {
		account := member.AccountAddress
		if account == "" {
			account = member.SourceIdentifier
		}
		date := "undated"
		if member.SentAt != nil {
			date = member.SentAt.Format("2006-01-02 15:04:05Z07:00")
		}
		label := relatedEvidenceLabels[member.Evidence]
		if label == "" {
			label = member.Evidence
		}
		switch member.LinkageOutcome {
		case "complete":
		case "pending":
			pending++
			label += " (not indexed yet)"
		default:
			unreadable++
			label += " (" + relatedUnreadableLabel(member.LinkageOutcome) + ")"
		}
		if _, err := fmt.Fprintf(out, "%d\t%s\t%s\t%s\t%s\t%s\n", member.MessageID,
			textutil.SanitizeTerminal(account), relatedDirection(member), date, label, textutil.SanitizeTerminal(member.Subject)); err != nil {
			return fmt.Errorf("write related copies: %w", err)
		}
	}
	if pending > 0 {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%d copies are not indexed yet; sync or run repair-derived, then retry\n", pending); err != nil {
			return fmt.Errorf("write related copies: %w", err)
		}
	}
	if unreadable > 0 {
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%d copies had headers or raw content that couldn't be fully read, so they may have relations this output cannot show\n", unreadable); err != nil {
			return fmt.Errorf("write related copies: %w", err)
		}
	}
	if family.PendingLinkage {
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), "Some archived emails haven't been indexed yet, so related emails may be missing; sync or run repair-derived, then retry"); err != nil {
			return fmt.Errorf("write related copies: %w", err)
		}
	}
	if family.Truncated {
		limit := "Stopped at the 500-email limit; more related emails exist"
		if family.TruncatedReason == store.EmailFamilyLimitIdentifiers {
			limit = "Stopped at the 5,000-identifier limit; related emails may be missing"
		}
		if _, err := fmt.Fprintln(cmd.ErrOrStderr(), limit); err != nil {
			return fmt.Errorf("write related copies: %w", err)
		}
	}
	return nil
}
