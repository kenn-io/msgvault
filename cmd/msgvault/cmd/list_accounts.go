package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/daemonclient"
)

var listAccountsJSON bool

var listAccountsCmd = &cobra.Command{
	Use:   "list-accounts",
	Short: "List synced email accounts",
	Long: `List all email accounts that have been added to msgvault.

Uses configured remote server or the local daemon by default.
Use --local to use the local daemon even when a remote is configured.

Shows account email, message count, and last sync time.

Examples:
	msgvault list-accounts
	msgvault list-accounts --json`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return listHTTPAccounts(cmd)
	},
}

func listHTTPAccounts(cmd *cobra.Command) error {
	s, _, err := OpenHTTPStore(cmd.Context())
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() { _ = s.Close() }()

	accounts, err := s.GetCLIAccounts(cmd.Context())
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}
	return outputAccountStats(daemonAccountsToStats(accounts))
}

func outputAccountStats(stats []accountStats) error {
	if len(stats) == 0 {
		fmt.Println("No accounts found. Use 'msgvault add-account <email>' to add one.")
		return nil
	}
	if listAccountsJSON {
		return outputAccountsJSON(stats)
	}
	outputAccountsTable(stats)
	return nil
}

func daemonAccountsToStats(accounts []daemonclient.CLIAccount) []accountStats {
	stats := make([]accountStats, len(accounts))
	for i, account := range accounts {
		stats[i] = accountStats{
			ID:                 account.ID,
			Email:              account.Email,
			Type:               account.Type,
			DisplayName:        account.DisplayName,
			MessageCount:       account.MessageCount,
			SourceDeletedCount: account.SourceDeletedCount,
			LastSync:           account.LastSync,
		}
	}
	return stats
}

func outputAccountsTable(stats []accountStats) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ID\tACCOUNT\tTYPE\tDISPLAY NAME\tMESSAGES\tLAST SYNC")

	for _, s := range stats {
		displayName := s.DisplayName
		if displayName == "" {
			displayName = "-"
		}
		lastSync := "-"
		if s.LastSync != nil && !s.LastSync.IsZero() {
			lastSync = s.LastSync.Format("2006-01-02 15:04")
		}
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Email, s.Type, displayName, formatMessagesCell(s), lastSync)
	}

	_ = w.Flush()
}

// formatMessagesCell renders the active message count and, when the account
// has archived messages that were deleted from the source, appends the
// deleted count so the primary column keeps its active-only meaning while
// still surfacing the retained-but-source-deleted population.
func formatMessagesCell(s accountStats) string {
	if s.SourceDeletedCount > 0 {
		return fmt.Sprintf("%s (+%s deleted from source)",
			formatCount(s.MessageCount), formatCount(s.SourceDeletedCount))
	}
	return formatCount(s.MessageCount)
}

func outputAccountsJSON(stats []accountStats) error {
	output := make([]map[string]any, len(stats))
	for i, s := range stats {
		entry := map[string]any{
			"id":                   s.ID,
			keyEmail:               s.Email,
			"type":                 s.Type,
			"display_name":         s.DisplayName,
			"message_count":        s.MessageCount,
			"source_deleted_count": s.SourceDeletedCount,
		}
		if s.LastSync != nil && !s.LastSync.IsZero() {
			entry["last_sync"] = s.LastSync.Format(time.RFC3339)
		} else {
			entry["last_sync"] = nil
		}
		output[i] = entry
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(output)
}

// formatCount formats a number with thousand separators.
func formatCount(n int64) string {
	if n < 1000 {
		return strconv.FormatInt(n, 10)
	}

	// Format with commas
	s := strconv.FormatInt(n, 10)
	result := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i := range len(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, s[i]) // s is ASCII decimal digits
	}
	return string(result)
}

type accountStats struct {
	ID                 int64
	Email              string
	Type               string
	DisplayName        string
	MessageCount       int64
	SourceDeletedCount int64
	LastSync           *time.Time
}

func init() {
	rootCmd.AddCommand(listAccountsCmd)
	listAccountsCmd.Flags().BoolVar(&listAccountsJSON, flagJSON, false, "Output as JSON")
}
