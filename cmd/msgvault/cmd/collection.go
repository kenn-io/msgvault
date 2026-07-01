package cmd

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/collectionops"
	"go.kenn.io/msgvault/internal/daemonclient"
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
	Use:   cmdUseList,
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

func runCollectionCreate(cmd *cobra.Command, args []string) error {
	accounts, err := collectionAccountsFromFlag(cmd, collectionCreateAccounts)
	if err != nil {
		return err
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.CreateCLICollection(cmd.Context(), daemonclient.CLICollectionCreateRequest{
		Name:     args[0],
		Accounts: accounts,
	})
	if err != nil {
		return err
	}
	renderCollectionCreateResult(cmd.OutOrStdout(), *result)
	return nil
}

func renderCollectionCreateResult(out io.Writer, result collectionops.MutationResult) {
	_, _ = fmt.Fprintf(out, "Created collection %q with %d source(s).\n",
		result.Name, result.SourceCount)
}

func runCollectionList(cmd *cobra.Command, _ []string) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	collections, err := st.GetCLICollections(cmd.Context())
	if err != nil {
		return err
	}
	renderCollectionList(cmd.OutOrStdout(), collections)
	return nil
}

func renderCollectionList(out io.Writer, collections []daemonclient.CLICollection) {
	if len(collections) == 0 {
		_, _ = fmt.Fprintln(out, "No collections.")
		return
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NAME\tSOURCES\tMESSAGES")
	for _, c := range collections {
		_, _ = fmt.Fprintf(w, "%s\t%d\t%s\n",
			c.Name, len(c.SourceIDs),
			formatCount(c.MessageCount))
	}
	_ = w.Flush()
}

func runCollectionShow(cmd *cobra.Command, args []string) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	coll, err := st.GetCLICollection(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	renderCollectionShow(cmd.OutOrStdout(), *coll)
	return nil
}

func renderCollectionShow(out io.Writer, coll daemonclient.CLICollection) {
	_, _ = fmt.Fprintf(out, "Collection: %s\n", coll.Name)
	if coll.Description != "" {
		_, _ = fmt.Fprintf(out, "Description: %s\n", coll.Description)
	}
	_, _ = fmt.Fprintf(out, "Sources: %d\n", len(coll.SourceIDs))
	_, _ = fmt.Fprintf(out, "Messages: %s\n", formatCount(coll.MessageCount))
	_, _ = fmt.Fprintf(out, "Created: %s\n", coll.CreatedAt.Format("2006-01-02 15:04"))

	if len(coll.SourceIDs) > 0 {
		_, _ = fmt.Fprintln(out, "\nMember sources:")
		for _, src := range coll.Sources {
			label := src.Identifier
			if src.DisplayName != "" {
				label = src.DisplayName
			}
			_, _ = fmt.Fprintf(out, "- %s (id %d)\n", label, src.ID)
		}
	}
}

func runCollectionAdd(cmd *cobra.Command, args []string) error {
	accounts, err := collectionAccountsFromFlag(cmd, collectionAddAccounts)
	if err != nil {
		return err
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.AddCLICollectionSources(cmd.Context(), args[0], daemonclient.CLICollectionSourcesRequest{
		Accounts: accounts,
	})
	if err != nil {
		return err
	}
	renderCollectionAddResult(cmd.OutOrStdout(), *result)
	return nil
}

func renderCollectionAddResult(out io.Writer, result collectionops.MutationResult) {
	_, _ = fmt.Fprintf(out, "Added %d source(s) to %q.\n",
		result.SourceCount, result.Name)
}

func runCollectionRemove(cmd *cobra.Command, args []string) error {
	accounts, err := collectionAccountsFromFlag(cmd, collectionRemoveAccounts)
	if err != nil {
		return err
	}

	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.RemoveCLICollectionSources(cmd.Context(), args[0], daemonclient.CLICollectionSourcesRequest{
		Accounts: accounts,
	})
	if err != nil {
		return err
	}
	renderCollectionRemoveResult(cmd.OutOrStdout(), *result)
	return nil
}

func renderCollectionRemoveResult(out io.Writer, result collectionops.MutationResult) {
	_, _ = fmt.Fprintf(out, "Removed %d source(s) from %q.\n",
		result.SourceCount, result.Name)
}

func runCollectionDelete(cmd *cobra.Command, args []string) error {
	st, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	result, err := st.DeleteCLICollection(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	renderCollectionDeleteResult(cmd.OutOrStdout(), *result)
	return nil
}

func renderCollectionDeleteResult(out io.Writer, result collectionops.MutationResult) {
	_, _ = fmt.Fprintf(out, "Deleted collection %q.\n", result.Name)
}

func collectionAccountsFromFlag(cmd *cobra.Command, accounts string) ([]string, error) {
	parsed, err := collectionops.ParseAccountsFlag(accounts)
	if err != nil {
		if cmd != nil && errors.Is(err, collectionops.ErrAccountsRequired) {
			return nil, usageErr(cmd, err)
		}
		return nil, err
	}
	return parsed, nil
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
