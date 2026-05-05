package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var (
	identityListAccount    string
	identityListCollection string
	identityListJSON       bool
	identityShowJSON       bool
	identityAddSignal      string
)

var identityCmd = &cobra.Command{
	Use:   "identity",
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
	Use:   "list",
	Short: "List confirmed identifiers across one or more accounts",
	RunE:  runIdentityList,
}

func runIdentityList(cmd *cobra.Command, _ []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	var sourceIDs []int64
	switch {
	case identityListAccount != "":
		scope, err := ResolveAccountFlag(st, identityListAccount)
		if err != nil {
			return err
		}
		sourceIDs = scope.SourceIDs()
	case identityListCollection != "":
		scope, err := ResolveCollectionFlag(st, identityListCollection)
		if err != nil {
			return err
		}
		sourceIDs = scope.SourceIDs()
	default:
		sources, err := st.ListSources("")
		if err != nil {
			return fmt.Errorf("list sources: %w", err)
		}
		sourceIDs = make([]int64, len(sources))
		for i, src := range sources {
			sourceIDs[i] = src.ID
		}
	}

	rows, err := collectIdentityRows(st, sourceIDs)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
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

// collectIdentityRows assembles per-source rows for the given source IDs.
// For each source, it emits one row per confirmed identifier; if a source
// has zero confirmed identifiers, it emits a single (none) row so the
// account is still visible.
func collectIdentityRows(st *store.Store, sourceIDs []int64) ([]identityRow, error) {
	var out []identityRow
	for _, sid := range sourceIDs {
		src, err := st.GetSourceByID(sid)
		if err != nil {
			return nil, fmt.Errorf("get source %d: %w", sid, err)
		}
		identifiers, err := st.ListAccountIdentities(sid)
		if err != nil {
			return nil, fmt.Errorf("list identities for source %d: %w", sid, err)
		}
		if len(identifiers) == 0 {
			out = append(out, identityRow{
				Account:    src.Identifier,
				SourceID:   src.ID,
				SourceType: src.SourceType,
				None:       true,
			})
			continue
		}
		for _, ai := range identifiers {
			out = append(out, identityRow{
				Account:     src.Identifier,
				SourceID:    src.ID,
				SourceType:  src.SourceType,
				Identifier:  ai.Address,
				Signals:     splitSignalSet(ai.SourceSignal),
				ConfirmedAt: ai.ConfirmedAt,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Identifier < out[j].Identifier
	})
	return out, nil
}

// splitSignalSet parses a stored source_signal field into a sorted slice.
// Empty input returns an empty slice (so JSON encoding emits [], not null).
// Empty parts (from stray commas in legacy data) are filtered to mirror
// mergeSignalSet's producer-side normalization.
func splitSignalSet(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

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
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	scope, err := ResolveAccountFlag(st, args[0])
	if err != nil {
		return err
	}
	if scope.Source == nil {
		return fmt.Errorf("no account found for %q", args[0])
	}

	rows, err := collectIdentityRows(st, []int64{scope.Source.ID})
	if err != nil {
		return err
	}
	if identityShowJSON {
		return writeIdentityJSON(cmd.OutOrStdout(), rows)
	}
	if err := writeIdentityTable(cmd.OutOrStdout(), rows); err != nil {
		return err
	}
	if len(rows) == 1 && rows[0].None {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nThis account has no confirmed identity. Add one with:\n")
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  msgvault identity add %s <identifier>\n", scope.Source.Identifier)
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
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	accountArg, identifierArg := args[0], args[1]
	identifier := strings.TrimSpace(identifierArg)
	if identifier == "" {
		return fmt.Errorf("identifier cannot be empty")
	}
	if strings.Contains(identityAddSignal, ",") {
		return fmt.Errorf("signal names cannot contain commas: %q", identityAddSignal)
	}

	scope, err := ResolveAccountFlag(st, accountArg)
	if err != nil {
		return err
	}
	if scope.Source == nil {
		return fmt.Errorf("no account found for %q", accountArg)
	}

	existing, err := st.ListAccountIdentities(scope.Source.ID)
	if err != nil {
		return fmt.Errorf("list existing: %w", err)
	}
	// Match the SQL-side LOWER() rule used by AddAccountIdentity so a
	// re-add of "Foo@x.com" against a stored "foo@x.com" hits the
	// "already confirmed" / "additional signal" branches instead of
	// silently looking new at the CLI layer.
	var prevSignals []string
	for _, ai := range existing {
		if store.EqualIdentifier(ai.Address, identifier) {
			prevSignals = splitSignalSet(ai.SourceSignal)
			break
		}
	}

	if err := st.AddAccountIdentity(scope.Source.ID, identifier, identityAddSignal); err != nil {
		return fmt.Errorf("add identity: %w", err)
	}

	switch {
	case len(prevSignals) == 0:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Added %s to %s (signal: %s).\n",
			identifier, scope.Source.Identifier, identityAddSignal)
	case slices.Contains(prevSignals, identityAddSignal):
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s already confirmed for %s with signal %s.\n",
			identifier, scope.Source.Identifier, identityAddSignal)
	default:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Recorded additional signal %s for %s on %s.\n",
			identityAddSignal, identifier, scope.Source.Identifier)
	}
	return nil
}

var identityRemoveCmd = &cobra.Command{
	Use:   "remove <account> <identifier>",
	Short: "Remove a confirmed identifier from an account's identity",
	Args:  cobra.ExactArgs(2),
	RunE:  runIdentityRemove,
}

func runIdentityRemove(cmd *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	identifier := strings.TrimSpace(args[1])
	if identifier == "" {
		return fmt.Errorf("identifier must not be empty")
	}

	scope, err := ResolveAccountFlag(st, args[0])
	if err != nil {
		return err
	}
	if scope.Source == nil {
		return fmt.Errorf("no account found for %q", args[0])
	}

	removed, err := st.RemoveAccountIdentity(scope.Source.ID, identifier)
	if err != nil {
		return fmt.Errorf("remove identity: %w", err)
	}
	if removed == 0 {
		existing, listErr := st.ListAccountIdentities(scope.Source.ID)
		if listErr != nil {
			return fmt.Errorf("%s is not in %s's identity (and looking up the current set failed: %w)",
				identifier, scope.Source.Identifier, listErr)
		}
		var have []string
		for _, ai := range existing {
			have = append(have, ai.Address)
		}
		if len(have) == 0 {
			return fmt.Errorf("%s is not in %s's identity (no confirmed identifiers on this account)",
				identifier, scope.Source.Identifier)
		}
		return fmt.Errorf("%s is not in %s's identity. Currently confirmed: %s",
			identifier, scope.Source.Identifier, strings.Join(have, ", "))
	}
	switch removed {
	case 1:
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s from %s.\n", identifier, scope.Source.Identifier)
	default:
		// >1 means a legacy database held case-variant duplicates of an
		// email-shaped identifier; the case-fold remove cleaned them up
		// in one call. Report the count so the user knows.
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Removed %d entries matching %s from %s.\n",
			removed, identifier, scope.Source.Identifier)
	}

	// Best-effort post-remove warning. If the lookup errors we suppress
	// the warning rather than risk a misleading "no identity left"
	// message — the remove itself already succeeded and was reported.
	rest, listErr := st.ListAccountIdentities(scope.Source.ID)
	if listErr == nil && len(rest) == 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Warning: %s now has no confirmed identity. "+
			"Dedup sent-copy detection for this account will rely on is_from_me "+
			"and SENT label signals only.\n", scope.Source.Identifier)
	}
	return nil
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
		"json", false, "Output as JSON")
	identityShowCmd.Flags().BoolVar(&identityShowJSON,
		"json", false, "Output as JSON")
	identityAddCmd.Flags().StringVar(&identityAddSignal,
		"signal", "manual",
		"Evidence signal name (e.g. manual, account-identifier, phone-e164). "+
			"Cannot contain commas.")
}
