package cmd

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var collectionCmd = &cobra.Command{
	Use:   "collection",
	Short: "Manage named groups of accounts",
	Long: `Collections are named groupings of accounts that let you view and
deduplicate across multiple sources as one unified archive.

A default "All" collection is created automatically and includes
every account.`,
}

var collectionCreateCmd = &cobra.Command{
	Use:   "create <name> --accounts <email1,email2,...>",
	Short: "Create a new collection",
	Args:  cobra.ExactArgs(1),
	RunE:  runCollectionCreate,
}

var collectionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all collections",
	RunE:  runCollectionList,
}

var collectionShowCmd = &cobra.Command{
	Use:   "show <name>",
	Short: "Show collection details",
	Args:  cobra.ExactArgs(1),
	RunE:  runCollectionShow,
}

var collectionAddCmd = &cobra.Command{
	Use:   "add <name> --accounts <email1,email2,...>",
	Short: "Add accounts to a collection",
	Args:  cobra.ExactArgs(1),
	RunE:  runCollectionAdd,
}

var collectionRemoveCmd = &cobra.Command{
	Use:   "remove <name> --accounts <email1,email2,...>",
	Short: "Remove accounts from a collection",
	Args:  cobra.ExactArgs(1),
	RunE:  runCollectionRemove,
}

var collectionDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a collection (sources and messages are untouched)",
	Args:  cobra.ExactArgs(1),
	RunE:  runCollectionDelete,
}

var (
	collectionCreateAccounts string
	collectionAddAccounts    string
	collectionRemoveAccounts string
)

func runCollectionCreate(_ *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	name := args[0]
	sourceIDs, err := resolveAccountList(st, collectionCreateAccounts)
	if err != nil {
		return err
	}

	coll, err := st.CreateCollection(name, "", sourceIDs)
	if err != nil {
		return err
	}
	fmt.Printf("Created collection %q with %d source(s).\n",
		coll.Name, len(sourceIDs))
	return nil
}

func runCollectionList(_ *cobra.Command, _ []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	collections, err := st.ListCollections()
	if err != nil {
		return err
	}
	if len(collections) == 0 {
		fmt.Println("No collections.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSOURCES\tMESSAGES")
	for _, c := range collections {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n",
			c.Name, len(c.SourceIDs),
			formatCount(c.MessageCount))
	}
	_ = w.Flush()
	return nil
}

func runCollectionShow(_ *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	coll, err := st.GetCollectionByName(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("Collection: %s\n", coll.Name)
	if coll.Description != "" {
		fmt.Printf("Description: %s\n", coll.Description)
	}
	fmt.Printf("Sources: %d\n", len(coll.SourceIDs))
	fmt.Printf("Messages: %s\n", formatCount(coll.MessageCount))
	fmt.Printf("Created: %s\n", coll.CreatedAt.Format("2006-01-02 15:04"))

	if len(coll.SourceIDs) > 0 {
		fmt.Println("\nMember sources:")
		for _, sid := range coll.SourceIDs {
			src, err := st.GetSourceByID(sid)
			if err != nil {
				return fmt.Errorf("get source %d: %w", sid, err)
			}
			label := src.Identifier
			if src.DisplayName.Valid && src.DisplayName.String != "" {
				label = src.DisplayName.String
			}
			fmt.Printf("- %s (id %d)\n", label, src.ID)
		}
	}
	return nil
}

func runCollectionAdd(_ *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	sourceIDs, err := resolveAccountList(st, collectionAddAccounts)
	if err != nil {
		return err
	}

	if err := st.AddSourcesToCollection(args[0], sourceIDs); err != nil {
		return err
	}
	fmt.Printf("Added %d source(s) to %q.\n", len(sourceIDs), args[0])
	return nil
}

func runCollectionRemove(_ *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	sourceIDs, err := resolveAccountList(st, collectionRemoveAccounts)
	if err != nil {
		return err
	}

	if err := st.RemoveSourcesFromCollection(args[0], sourceIDs); err != nil {
		return err
	}
	fmt.Printf("Removed %d source(s) from %q.\n", len(sourceIDs), args[0])
	return nil
}

func runCollectionDelete(_ *cobra.Command, args []string) error {
	st, err := openStoreAndInit()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	if err := st.DeleteCollection(args[0]); err != nil {
		return err
	}
	fmt.Printf("Deleted collection %q.\n", args[0])
	return nil
}

func resolveAccountList(st *store.Store, accounts string) ([]int64, error) {
	if accounts == "" {
		return nil, fmt.Errorf("--accounts is required")
	}
	parts := strings.Split(accounts, ",")
	var ids []int64
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Try as numeric ID first, but only for plain digit tokens.
		// strconv.ParseInt accepts a leading '+' or '-' sign, so an
		// E.164 phone identifier like "+15551234567" would parse as
		// the integer 15551234567 and be treated as a source ID,
		// silently breaking WhatsApp/Google Voice accounts that key
		// on phone numbers. Restrict the numeric branch to tokens
		// whose first byte is a decimal digit so signed inputs fall
		// through to identifier resolution. If the numeric lookup
		// returns ErrSourceNotFound, fall through to ResolveAccountFlag
		// — the digit string may be a numeric identifier (e.g.
		// unprefixed phone number, account name) rather than a source
		// ID. Surface any other error (real DB failure) so it isn't
		// masked as a "not found".
		if p[0] >= '0' && p[0] <= '9' {
			if id, err := strconv.ParseInt(p, 10, 64); err == nil {
				_, lookupErr := st.GetSourceByID(id)
				switch {
				case lookupErr == nil:
					ids = append(ids, id)
					continue
				case errors.Is(lookupErr, store.ErrSourceNotFound):
					// fall through to identifier resolution
				default:
					return nil, fmt.Errorf("get source %d: %w", id, lookupErr)
				}
			}
		}
		// Resolve by identifier
		scope, err := ResolveAccountFlag(st, p)
		if err != nil {
			return nil, err
		}
		ids = append(ids, scope.SourceIDs()...)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no valid accounts in --accounts")
	}
	return ids, nil
}

func init() {
	rootCmd.AddCommand(collectionCmd)
	collectionCmd.AddCommand(collectionCreateCmd)
	collectionCmd.AddCommand(collectionListCmd)
	collectionCmd.AddCommand(collectionShowCmd)
	collectionCmd.AddCommand(collectionAddCmd)
	collectionCmd.AddCommand(collectionRemoveCmd)
	collectionCmd.AddCommand(collectionDeleteCmd)

	collectionCreateCmd.Flags().StringVar(&collectionCreateAccounts,
		"accounts", "", "Comma-separated account emails or source IDs")
	collectionAddCmd.Flags().StringVar(&collectionAddAccounts,
		"accounts", "", "Comma-separated account emails or source IDs")
	collectionRemoveCmd.Flags().StringVar(&collectionRemoveAccounts,
		"accounts", "", "Comma-separated account emails or source IDs")
}
