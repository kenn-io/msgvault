package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/identityops"
)

var (
	identityListAccount      string
	identityListCollection   string
	identityListSourceID     int64
	identityListJSON         bool
	identityShowSourceID     int64
	identityShowJSON         bool
	identityAddSourceID      int64
	identityAddSignal        string
	identityRemoveSourceID   int64
	identityDiscoverSourceID int64
	identityDiscoverApply    bool
	identityDiscoverProvider bool
	identityDiscoverConfirm  []string
	identityDiscoverJSON     bool
	identityImportSourceID   int64
	identityImportFile       string
	identityImportStdin      bool
	identityImportSignal     string
	identityImportApply      bool
	identityImportJSON       bool
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
			Account:     identityListAccount,
			Collection:  identityListCollection,
			SourceID:    identityListSourceID,
			SourceIDSet: cmd.Flags().Changed("source-id"),
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
	Use:   "show [account]",
	Short: "Show one account's identity in detail",
	Args:  identityShowArgs,
	RunE:  runIdentityShow,
}

func runIdentityShow(cmd *cobra.Command, args []string) error {
	account := ""
	if len(args) == 1 {
		account = args[0]
	}
	selector, err := identitySourceSelector(cmd, account, identityShowSourceID)
	if err != nil {
		return err
	}
	rows, err := fetchHTTPIdentityRows(cmd, daemonclient.CLIIdentitiesRequest{
		Account:     selector.Account,
		SourceID:    selector.SourceID,
		SourceIDSet: cmd.Flags().Changed("source-id"),
		PrimaryOnly: selector.SourceID == 0,
	})
	if err != nil {
		return err
	}
	return renderIdentityShow(cmd.OutOrStdout(), rows, account)
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
	Use:   "add [account] <identifier>",
	Short: "Add a confirmed identifier to an account's identity",
	Args:  identityAddArgs,
	RunE:  runIdentityAdd,
}

func runIdentityAdd(cmd *cobra.Command, args []string) error {
	accountArg, identifierArg := identityMutationArguments(cmd, args)
	identifier := strings.TrimSpace(identifierArg)
	if identifier == "" {
		return usageErr(cmd, errors.New("identifier cannot be empty"))
	}
	if strings.Contains(identityAddSignal, ",") {
		return usageErr(cmd, fmt.Errorf("signal names cannot contain commas: %q", identityAddSignal))
	}
	selector, err := identitySourceSelector(cmd, accountArg, identityAddSourceID)
	if err != nil {
		return err
	}
	return runHTTPIdentityAdd(cmd, selector, identifier)
}

func runHTTPIdentityAdd(cmd *cobra.Command, selector daemonclient.CLIIdentitySourceSelector, identifier string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.AddCLIIdentity(cmd.Context(), daemonclient.CLIIdentityAddRequest{
		SourceSelector: selector,
		Identifier:     identifier,
		Signal:         identityAddSignal,
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
	Use:   "remove [account] <identifier>",
	Short: "Remove a confirmed identifier from an account's identity",
	Args:  identityRemoveArgs,
	RunE:  runIdentityRemove,
}

var identityDiscoverCmd = &cobra.Command{
	Use:   "discover [account]",
	Short: "Preview or apply source-scoped identity evidence",
	Long: `Scan one ingestion source's archived message metadata for identity
evidence. The command is read-only unless --apply is present. Strong sent
evidence is applied automatically with --apply; weak candidates require a
repeatable --confirm address flag.`,
	Args: identityDiscoverArgs,
	RunE: runIdentityDiscover,
}

var identityImportCmd = &cobra.Command{
	Use:   "import [account]",
	Short: "Preview or apply source-scoped identities from text or JSON",
	Args:  identityImportArgs,
	RunE:  runIdentityImport,
}

func runIdentityImport(cmd *cobra.Command, args []string) error {
	account := ""
	if len(args) == 1 {
		account = args[0]
	}
	selector, err := identitySourceSelector(cmd, account, identityImportSourceID)
	if err != nil {
		return err
	}
	if err := cmd.Context().Err(); err != nil {
		return err
	}

	var input io.Reader
	format := ""
	var file *os.File
	if identityImportStdin {
		input = cmd.InOrStdin()
	} else {
		file, err = os.Open(identityImportFile)
		if err != nil {
			return fmt.Errorf("open identity import file: %w", err)
		}
		defer func() { _ = file.Close() }()
		input = file
		format = filepath.Ext(identityImportFile)
	}
	entries, err := identityops.ParseImport(input, format)
	if err != nil {
		return fmt.Errorf("parse identity import: %w", err)
	}

	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = client.Close() }()
	result, err := client.ImportCLIIdentities(cmd.Context(), identityops.ImportRequest{
		SourceSelector: selector,
		Entries:        entries,
		Signal:         identityImportSignal,
		Apply:          identityImportApply,
	})
	if err != nil {
		return fmt.Errorf("import identities: %w", err)
	}
	if result == nil {
		return errors.New("identity import ended without a result")
	}
	return renderIdentityImport(cmd.OutOrStdout(), *result, identityImportJSON)
}

func renderIdentityImport(w io.Writer, result identityops.ImportResult, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if _, err := fmt.Fprintf(
		w,
		"Identity import for %s (source %d)\nSignal: %s\n\nCANDIDATES (%d)\n",
		result.Account,
		result.SourceID,
		result.Signal,
		len(result.Candidates),
	); err != nil {
		return fmt.Errorf("render identity import heading: %w", err)
	}
	for _, candidate := range result.Candidates {
		states := strings.Join(candidate.ProviderStates, ",")
		if states == "" {
			states = "-"
		}
		if _, err := fmt.Fprintf(
			w,
			"  %s\n    classification: %s | states: %s\n",
			candidate.Identifier,
			candidate.Classification,
			states,
		); err != nil {
			return fmt.Errorf("render imported identity candidate: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "\nApplied %d identity confirmation(s).\n", len(result.Applied)); err != nil {
		return fmt.Errorf("render imported identity outcomes: %w", err)
	}
	return nil
}

func runIdentityDiscover(cmd *cobra.Command, args []string) error {
	account := ""
	if len(args) == 1 {
		account = args[0]
	}
	selector, err := identitySourceSelector(cmd, account, identityDiscoverSourceID)
	if err != nil {
		return err
	}
	client, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = client.Close() }()

	var result *identityops.DiscoverResult
	err = client.DiscoverCLIIdentities(cmd.Context(), identityops.DiscoverRequest{
		SourceSelector: selector,
		Apply:          identityDiscoverApply,
		Provider:       identityDiscoverProvider,
		Confirm:        append([]string(nil), identityDiscoverConfirm...),
	}, func(event identityops.DiscoverEvent) error {
		switch event.Type {
		case "progress":
			if !identityDiscoverJSON && event.Progress != nil {
				return renderIdentityDiscoverProgress(cmd.ErrOrStderr(), *event.Progress)
			}
		case "result":
			if event.Result != nil {
				copyResult := *event.Result
				result = &copyResult
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("discover identities: %w", err)
	}
	if result == nil {
		return errors.New("identity discovery ended without a result")
	}
	return renderIdentityDiscover(cmd.OutOrStdout(), *result, identityDiscoverJSON)
}

func renderIdentityDiscoverProgress(w io.Writer, progress identityops.DiscoverProgress) error {
	_, err := fmt.Fprintf(
		w,
		"Scanning identity evidence: %d/%d messages, %d candidate(s)\n",
		progress.Done,
		progress.Total,
		progress.Candidates,
	)
	if err != nil {
		return fmt.Errorf("render identity discovery progress: %w", err)
	}
	return nil
}

func renderIdentityDiscover(w io.Writer, result identityops.DiscoverResult, jsonOutput bool) error {
	if jsonOutput {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}

	if _, err := fmt.Fprintf(
		w,
		"Identity discovery for %s (source %d, %s)\nScanned %d message(s).\n",
		result.Account,
		result.SourceID,
		result.SourceType,
		result.ScannedMessages,
	); err != nil {
		return fmt.Errorf("render identity discovery heading: %w", err)
	}
	for _, classification := range []string{"confirmed", "strong", "weak"} {
		if err := renderIdentityDiscoverCandidateGroup(w, classification, result.Candidates); err != nil {
			return fmt.Errorf("render identity discovery candidates: %w", err)
		}
	}
	if _, err := fmt.Fprintf(w, "\nREJECTED (%d)\n", len(result.Rejected)); err != nil {
		return fmt.Errorf("render rejected identity candidates: %w", err)
	}
	for _, candidate := range result.Rejected {
		if _, err := fmt.Fprintf(w, "  %s\n    reason: %s\n", candidate.Identifier, candidate.Reason); err != nil {
			return fmt.Errorf("render rejected identity candidate: %w", err)
		}
	}
	if result.Applied != nil {
		if _, err := fmt.Fprintf(w, "\nApplied %d identity confirmation(s).\n", len(result.Applied)); err != nil {
			return fmt.Errorf("render applied identity confirmations: %w", err)
		}
	}
	return nil
}

func renderIdentityDiscoverCandidateGroup(
	w io.Writer,
	classification string,
	candidates []identityops.Candidate,
) error {
	count := 0
	for _, candidate := range candidates {
		if candidate.Classification == classification {
			count++
		}
	}
	if _, err := fmt.Fprintf(w, "\n%s (%d)\n", strings.ToUpper(classification), count); err != nil {
		return fmt.Errorf("render identity candidate group: %w", err)
	}
	for _, candidate := range candidates {
		if candidate.Classification != classification {
			continue
		}
		signals := strings.Join(candidate.Signals, ",")
		if signals == "" {
			signals = "-"
		}
		providerStates := strings.Join(candidate.ProviderStates, ",")
		if providerStates == "" {
			providerStates = "-"
		}
		if _, err := fmt.Fprintf(
			w,
			"  %s\n    signals: %s | provider states: %s | sent: %d | received: %d\n",
			candidate.Identifier,
			signals,
			providerStates,
			candidate.SentMessageCount,
			candidate.ReceivedMessageCount,
		); err != nil {
			return fmt.Errorf("render identity candidate: %w", err)
		}
	}
	return nil
}

func runIdentityRemove(cmd *cobra.Command, args []string) error {
	accountArg, identifierArg := identityMutationArguments(cmd, args)
	identifier := strings.TrimSpace(identifierArg)
	if identifier == "" {
		return usageErr(cmd, errors.New("identifier must not be empty"))
	}
	selector, err := identitySourceSelector(cmd, accountArg, identityRemoveSourceID)
	if err != nil {
		return err
	}
	return runHTTPIdentityRemove(cmd, selector, identifier)
}

func runHTTPIdentityRemove(cmd *cobra.Command, selector daemonclient.CLIIdentitySourceSelector, identifier string) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	result, err := s.RemoveCLIIdentity(cmd.Context(), daemonclient.CLIIdentityRemoveRequest{
		SourceSelector: selector,
		Identifier:     identifier,
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

func identityShowArgs(cmd *cobra.Command, args []string) error {
	return identitySelectorArgs(cmd, args, 0, 1)
}

func identityAddArgs(cmd *cobra.Command, args []string) error {
	return identitySelectorArgs(cmd, args, 1, 2)
}

func identityRemoveArgs(cmd *cobra.Command, args []string) error {
	return identitySelectorArgs(cmd, args, 1, 2)
}

func identityDiscoverArgs(cmd *cobra.Command, args []string) error {
	return identitySelectorArgs(cmd, args, 0, 1)
}

func identityImportArgs(cmd *cobra.Command, args []string) error {
	if err := identitySelectorArgs(cmd, args, 0, 1); err != nil {
		return err
	}
	fileSet := cmd.Flags().Changed("file")
	if fileSet == identityImportStdin {
		return errors.New("exactly one of --file or --stdin is required")
	}
	if fileSet && strings.TrimSpace(identityImportFile) == "" {
		return errors.New("identity import file path must not be empty")
	}
	if fileSet && identityImportFile == "-" {
		return errors.New(`--file "-" is not supported; use --stdin to read standard input`)
	}
	return nil
}

func identitySelectorArgs(cmd *cobra.Command, args []string, sourceIDCount, accountCount int) error {
	if cmd.Flags().Changed("source-id") {
		if len(args) == sourceIDCount {
			return nil
		}
		if len(args) == accountCount {
			return errors.New("account and source ID are mutually exclusive")
		}
		return cobra.ExactArgs(sourceIDCount)(cmd, args)
	}
	return cobra.ExactArgs(accountCount)(cmd, args)
}

func identityMutationArguments(cmd *cobra.Command, args []string) (account, identifier string) {
	if cmd.Flags().Changed("source-id") {
		return "", args[0]
	}
	return args[0], args[1]
}

func identitySourceSelector(
	cmd *cobra.Command,
	account string,
	sourceID int64,
) (daemonclient.CLIIdentitySourceSelector, error) {
	if cmd.Flags().Changed("source-id") {
		if sourceID <= 0 {
			return daemonclient.CLIIdentitySourceSelector{}, errors.New("source ID must be positive")
		}
		return daemonclient.CLIIdentitySourceSelector{SourceID: sourceID}, nil
	}
	return daemonclient.CLIIdentitySourceSelector{Account: account}, nil
}

func init() {
	rootCmd.AddCommand(identityCmd)
	identityCmd.AddCommand(identityListCmd)
	identityCmd.AddCommand(identityShowCmd)
	identityCmd.AddCommand(identityAddCmd)
	identityCmd.AddCommand(identityRemoveCmd)
	identityCmd.AddCommand(identityDiscoverCmd)
	identityCmd.AddCommand(identityImportCmd)

	identityListCmd.Flags().StringVar(&identityListAccount,
		"account", "", "Restrict to a single account")
	identityListCmd.Flags().StringVar(&identityListCollection,
		"collection", "", "Restrict to all member accounts of one collection")
	identityListCmd.Flags().Int64Var(&identityListSourceID,
		"source-id", 0, "Restrict to one source by numeric ID")
	identityListCmd.MarkFlagsMutuallyExclusive("account", "collection", "source-id")
	identityListCmd.Flags().BoolVar(&identityListJSON,
		flagJSON, false, "Output as JSON")
	identityShowCmd.Flags().BoolVar(&identityShowJSON,
		flagJSON, false, "Output as JSON")
	identityShowCmd.Flags().Int64Var(&identityShowSourceID,
		"source-id", 0, "Select one source by numeric ID")
	identityAddCmd.Flags().Int64Var(&identityAddSourceID,
		"source-id", 0, "Select one source by numeric ID")
	identityAddCmd.Flags().StringVar(&identityAddSignal,
		"signal", "manual",
		"Evidence signal name (e.g. manual, account-identifier, phone-e164). "+
			"Cannot contain commas.")
	identityRemoveCmd.Flags().Int64Var(&identityRemoveSourceID,
		"source-id", 0, "Select one source by numeric ID")
	identityDiscoverCmd.Flags().Int64Var(&identityDiscoverSourceID,
		"source-id", 0, "Select one source by numeric ID")
	identityDiscoverCmd.Flags().BoolVar(&identityDiscoverApply,
		"apply", false, "Confirm strong identity evidence after the preview scan completes")
	identityDiscoverCmd.Flags().BoolVar(&identityDiscoverProvider,
		"provider", false, "Include configured provider alias inventory")
	identityDiscoverCmd.Flags().StringArrayVar(&identityDiscoverConfirm,
		"confirm", nil, "Explicitly confirm a weak candidate (repeatable; requires --apply)")
	identityDiscoverCmd.Flags().BoolVar(&identityDiscoverJSON,
		flagJSON, false, "Output the final result as JSON and suppress progress")
	identityImportCmd.Flags().Int64Var(&identityImportSourceID,
		"source-id", 0, "Select one source by numeric ID")
	identityImportCmd.Flags().StringVar(&identityImportFile,
		"file", "", "Read identities from a text or JSON file")
	identityImportCmd.Flags().BoolVar(&identityImportStdin,
		"stdin", false, "Read identities from standard input")
	identityImportCmd.Flags().StringVar(&identityImportSignal,
		"signal", "manual", "Evidence signal name (cannot contain commas)")
	identityImportCmd.Flags().BoolVar(&identityImportApply,
		"apply", false, "Confirm all validated imported identities")
	identityImportCmd.Flags().BoolVar(&identityImportJSON,
		flagJSON, false, "Output the result as JSON")
}
