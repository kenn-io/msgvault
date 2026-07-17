package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var (
	identityListAccount    string
	identityListCollection string
	identityListJSON       bool
	identityShowJSON       bool
	identityAddSignal      string
)

// identityCmdUse is the usage/name of the identity command.
const identityCmdUse = "identity"

var identityCmd = &cobra.Command{
	Use:   identityCmdUse,
	Short: "Manage the confirmed \"me\" identifiers for each account",
	Long: `Each account has one identity: the set of identifiers (email
addresses, phone numbers, chat handles, synthetic identifiers) that mean
"me" inside that account. Dedup's sent-copy detection compares a message's
From: against the identifiers confirmed for the message's account.

Identifiers are stored verbatim; case is preserved so synthetic identifiers
like Slack member IDs and Matrix MXIDs round-trip correctly. Email-address
case-insensitivity is handled at compare time by consumers, not at the store.`,
}

var identityListCmd = &cobra.Command{
	Use:   cmdUseList,
	Short: "List confirmed identifiers across one or more accounts",
	Args:  cobra.NoArgs,
	RunE:  runIdentityList,
}

func runIdentityList(cmd *cobra.Command, _ []string) error {
	rows, err := fetchHTTPIdentityRows(
		cmd,
		daemonclient.CLIIdentitiesRequest{
			Account:    identityListAccount,
			Collection: identityListCollection,
		},
	)
	if err != nil {
		return err
	}
	return renderIdentityList(cmd.OutOrStdout(), rows)
}

func renderIdentityList(w io.Writer, rows []identityRow) error {
	if identityListJSON {
		return writeIdentityJSON(w, rows)
	}
	return writeIdentityTable(w, rows)
}

// identityRow is the unified view used by both `identity list` and
// `identity show`. (none) rows have empty Identifier and Signal.
type identityRow struct {
	Account     string
	SourceID    int64
	SourceType  string
	Identifier  string
	Signals     []string
	ConfirmedAt time.Time
	None        bool
}

// nil error return mirrors writeIdentityJSON so callers can return either
// uniformly; tabwriter output never fails.
func writeIdentityTable(w io.Writer, rows []identityRow) error {
	if len(rows) == 0 {
		_, _ = fmt.Fprintln(w, "No accounts in scope.")
		return nil
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ACCOUNT\tSOURCE_TYPE\tIDENTIFIER\tSIGNALS\tCONFIRMED")
	confirmedCount := 0
	accountCount := 0
	seenAccounts := make(map[int64]struct{})
	noIdentityCount := 0
	for _, r := range rows {
		if _, seen := seenAccounts[r.SourceID]; !seen {
			accountCount++
			seenAccounts[r.SourceID] = struct{}{}
		}
		if r.None {
			noIdentityCount++
			_, _ = fmt.Fprintf(tw, "%s\t%s\t(none)\t-\t-\n",
				r.Account, r.SourceType)
			continue
		}
		confirmedCount++
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			r.Account, r.SourceType, r.Identifier,
			strings.Join(r.Signals, ","),
			r.ConfirmedAt.Format("2006-01-02 15:04"))
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(w, "---\n%d confirmed identifier(s) across %d account(s); %d account(s) have no identity.\n",
		confirmedCount, accountCount, noIdentityCount)
	return nil
}

func writeIdentityJSON(w io.Writer, rows []identityRow) error {
	type entry struct {
		Account     string    `json:"account"`
		SourceID    int64     `json:"source_id"`
		SourceType  string    `json:"source_type"`
		Identifier  string    `json:"identifier"`
		Signals     []string  `json:"signals"`
		ConfirmedAt time.Time `json:"confirmed_at"`
	}
	out := make([]entry, 0, len(rows))
	for _, r := range rows {
		if r.None {
			continue
		}
		out = append(out, entry{
			Account:     r.Account,
			SourceID:    r.SourceID,
			SourceType:  r.SourceType,
			Identifier:  r.Identifier,
			Signals:     r.Signals,
			ConfirmedAt: r.ConfirmedAt,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

var identityShowCmd = &cobra.Command{
	Use:   "show <account>",
	Short: "Show one account's identity in detail",
	Args:  cobra.ExactArgs(1),
	RunE:  runIdentityShow,
}

func runIdentityShow(cmd *cobra.Command, args []string) error {
	rows, err := fetchHTTPIdentityRows(cmd, daemonclient.CLIIdentitiesRequest{
		Account:     args[0],
		PrimaryOnly: true,
	})
	if err != nil {
		return err
	}
	return renderIdentityShow(cmd.OutOrStdout(), rows, args[0])
}

func fetchHTTPIdentityRows(
	cmd *cobra.Command,
	req daemonclient.CLIIdentitiesRequest,
) ([]identityRow, error) {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	rows, err := s.GetCLIIdentities(cmd.Context(), req)
	if err != nil {
		return nil, fmt.Errorf("list identities: %w", err)
	}
	return identityRowsFromDaemon(rows), nil
}

func identityRowsFromDaemon(rows []daemonclient.CLIIdentityRow) []identityRow {
	out := make([]identityRow, 0, len(rows))
	for _, r := range rows {
		confirmedAt := time.Time{}
		if r.ConfirmedAt != nil {
			confirmedAt = *r.ConfirmedAt
		}
		out = append(out, identityRow{
			Account:     r.Account,
			SourceID:    r.SourceID,
			SourceType:  r.SourceType,
			Identifier:  r.Identifier,
			Signals:     append([]string{}, r.Signals...),
			ConfirmedAt: confirmedAt,
			None:        r.None,
		})
	}
	return out
}

func renderIdentityShow(w io.Writer, rows []identityRow, hintAccount string) error {
	if identityShowJSON {
		return writeIdentityJSON(w, rows)
	}
	if err := writeIdentityTable(w, rows); err != nil {
		return err
	}
	if len(rows) == 1 && rows[0].None {
		if rows[0].Account != "" {
			hintAccount = rows[0].Account
		}
		_, _ = fmt.Fprintf(w, "\nThis account has no confirmed identity. Add one with:\n")
		_, _ = fmt.Fprintf(w, "  msgvault identity add %s <identifier>\n", hintAccount)
	}
	return nil
}

var identityAddCmd = &cobra.Command{
	Use:   "add <account> <identifier>",
	Short: "Add a confirmed identifier to an account's identity",
	Args:  cobra.ExactArgs(2),
	RunE:  runIdentityAdd,
}

func runIdentityAdd(cmd *cobra.Command, args []string) error {
	accountArg, identifierArg := args[0], args[1]
	identifier := strings.TrimSpace(identifierArg)
	if identifier == "" {
		return usageErr(cmd, errors.New("identifier cannot be empty"))
	}
	if strings.Contains(identityAddSignal, ",") {
		return usageErr(cmd, fmt.Errorf("signal names cannot contain commas: %q", identityAddSignal))
	}
	return runHTTPIdentityAdd(cmd, accountArg, identifier)
}

func runHTTPIdentityAdd(cmd *cobra.Command, account string, identifier string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.AddCLIIdentity(cmd.Context(), daemonclient.CLIIdentityAddRequest{
		Account:    account,
		Identifier: identifier,
		Signal:     identityAddSignal,
	})
	if err != nil {
		return err
	}
	renderIdentityAddResult(cmd.OutOrStdout(), *result)
	return nil
}

func renderIdentityAddResult(w io.Writer, result daemonclient.CLIIdentityAddResult) {
	switch result.Outcome {
	case "already_confirmed":
		_, _ = fmt.Fprintf(w, "%s already confirmed for %s with signal %s.\n",
			result.Identifier, result.Account, result.Signal)
	case "additional_signal":
		_, _ = fmt.Fprintf(w, "Recorded additional signal %s for %s on %s.\n",
			result.Signal, result.Identifier, result.Account)
	default:
		_, _ = fmt.Fprintf(w, "Added %s to %s (signal: %s).\n",
			result.Identifier, result.Account, result.Signal)
	}
}

var identityRemoveCmd = &cobra.Command{
	Use:   "remove <account> <identifier>",
	Short: "Remove a confirmed identifier from an account's identity",
	Args:  cobra.ExactArgs(2),
	RunE:  runIdentityRemove,
}

func runIdentityRemove(cmd *cobra.Command, args []string) error {
	identifier := strings.TrimSpace(args[1])
	if identifier == "" {
		return usageErr(cmd, errors.New("identifier must not be empty"))
	}
	return runHTTPIdentityRemove(cmd, args[0], identifier)
}

func runHTTPIdentityRemove(cmd *cobra.Command, account string, identifier string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.RemoveCLIIdentity(cmd.Context(), daemonclient.CLIIdentityRemoveRequest{
		Account:    account,
		Identifier: identifier,
	})
	if err != nil {
		return err
	}
	renderIdentityRemoveResult(cmd.OutOrStdout(), *result)
	if result.NoIdentity {
		renderIdentityNoIdentityWarning(cmd.OutOrStdout(), result.Account)
	}
	return nil
}

func renderIdentityRemoveResult(w io.Writer, result daemonclient.CLIIdentityRemoveResult) {
	switch result.Removed {
	case 1:
		_, _ = fmt.Fprintf(w, "Removed %s from %s.\n", result.Identifier, result.Account)
	default:
		_, _ = fmt.Fprintf(w, "Removed %d entries matching %s from %s.\n",
			result.Removed, result.Identifier, result.Account)
	}
}

func renderIdentityNoIdentityWarning(w io.Writer, account string) {
	_, _ = fmt.Fprintf(w, "Warning: %s now has no confirmed identity. "+
		"Dedup sent-copy detection for this account will rely on is_from_me "+
		"and SENT label signals only.\n", account)
}

func init() {
	rootCmd.AddCommand(identityCmd)
	identityCmd.AddCommand(identityListCmd)
	identityCmd.AddCommand(identityShowCmd)
	identityCmd.AddCommand(identityAddCmd)
	identityCmd.AddCommand(identityRemoveCmd)

	identityListCmd.Flags().StringVar(&identityListAccount,
		"account", "", "Restrict to a single account")
	identityListCmd.Flags().StringVar(&identityListCollection,
		"collection", "", "Restrict to all member accounts of one collection")
	identityListCmd.MarkFlagsMutuallyExclusive("account", "collection")
	identityListCmd.Flags().BoolVar(&identityListJSON,
		flagJSON, false, "Output as JSON")
	identityShowCmd.Flags().BoolVar(&identityShowJSON,
		flagJSON, false, "Output as JSON")
	identityAddCmd.Flags().StringVar(&identityAddSignal,
		"signal", "manual",
		"Evidence signal name (e.g. manual, account-identifier, phone-e164). "+
			"Cannot contain commas.")
}
