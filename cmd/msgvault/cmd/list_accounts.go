package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/remote"
	"github.com/wesm/msgvault/internal/store"
)

var listAccountsJSON bool

var listAccountsCmd = &cobra.Command{
	Use:   "list-accounts",
	Short: "List synced email accounts",
	Long: `List all email accounts that have been added to msgvault.

Uses remote server if [remote].url is configured, otherwise uses local database.
Use --local to force local database.

Shows account email, message count, and last sync time.

Examples:
  msgvault list-accounts
  msgvault list-accounts --json`,
	RunE: func(cmd *cobra.Command, args []string) error {
		// Use remote if configured
		if IsRemoteMode() {
			return listRemoteAccounts()
		}

		return listLocalAccounts()
	},
}

// listRemoteAccounts fetches and displays accounts from the remote server.
func listRemoteAccounts() error {
	s, err := OpenRemoteStore()
	if err != nil {
		return fmt.Errorf("connect to remote: %w", err)
	}
	defer func() { _ = s.Close() }()

	accounts, err := s.ListAccounts()
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	if len(accounts) == 0 {
		fmt.Println("No accounts found on remote server.")
		return nil
	}

	if listAccountsJSON {
		return outputRemoteAccountsJSON(accounts)
	}
	outputRemoteAccountsTable(accounts)
	return nil
}

// listLocalAccounts fetches and displays accounts from the local database.
func listLocalAccounts() error {
	dbPath := cfg.DatabaseDSN()
	s, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = s.Close() }()

	if err := s.InitSchema(); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	if err := runStartupMigrations(s); err != nil {
		return fmt.Errorf("startup migrations: %w", err)
	}

	sources, err := s.ListSources("")
	if err != nil {
		return fmt.Errorf("list accounts: %w", err)
	}

	if len(sources) == 0 {
		fmt.Println("No accounts found. Use 'msgvault add-account <email>' to add one.")
		return nil
	}

	// Gather stats for each account
	stats := make([]accountStats, len(sources))
	for i, src := range sources {
		count, err := s.CountMessagesForSource(src.ID)
		if err != nil {
			return fmt.Errorf("count messages for %s: %w", src.Identifier, err)
		}

		var lastSync *time.Time
		if src.LastSyncAt.Valid {
			lastSync = &src.LastSyncAt.Time
		}

		displayName := ""
		if src.DisplayName.Valid {
			displayName = src.DisplayName.String
		}

		stats[i] = accountStats{
			ID:           src.ID,
			Email:        src.Identifier,
			Type:         src.SourceType,
			DisplayName:  displayName,
			MessageCount: count,
			LastSync:     lastSync,
		}
	}

	logger.Info("list-accounts",
		"sources", len(stats),
	)

	if listAccountsJSON {
		return outputAccountsJSON(stats)
	}
	outputAccountsTable(stats)
	return nil
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
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Email, s.Type, displayName, formatCount(s.MessageCount), lastSync)
	}

	_ = w.Flush()
}

func outputAccountsJSON(stats []accountStats) error {
	output := make([]map[string]interface{}, len(stats))
	for i, s := range stats {
		entry := map[string]interface{}{
			"id":            s.ID,
			"email":         s.Email,
			"type":          s.Type,
			"display_name":  s.DisplayName,
			"message_count": s.MessageCount,
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
		return fmt.Sprintf("%d", n)
	}

	// Format with commas
	s := fmt.Sprintf("%d", n)
	result := make([]byte, 0, len(s)+(len(s)-1)/3)
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

type accountStats struct {
	ID           int64
	Email        string
	Type         string
	DisplayName  string
	MessageCount int64
	LastSync     *time.Time
}

func outputRemoteAccountsTable(accounts []remote.AccountInfo) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "EMAIL\tSCHEDULE\tENABLED\tLAST SYNC\tNEXT SYNC")

	for _, a := range accounts {
		enabled := "no"
		if a.Enabled {
			enabled = "yes"
		}
		lastSync := "-"
		if a.LastSyncAt != "" {
			if t, err := time.Parse(time.RFC3339, a.LastSyncAt); err == nil {
				lastSync = t.Format("2006-01-02 15:04")
			}
		}
		nextSync := "-"
		if a.NextSyncAt != "" {
			if t, err := time.Parse(time.RFC3339, a.NextSyncAt); err == nil {
				nextSync = t.Format("2006-01-02 15:04")
			}
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.Email, a.Schedule, enabled, lastSync, nextSync)
	}

	_ = w.Flush()
}

func outputRemoteAccountsJSON(accounts []remote.AccountInfo) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(accounts)
}

func init() {
	rootCmd.AddCommand(listAccountsCmd)
	listAccountsCmd.Flags().BoolVar(&listAccountsJSON, "json", false, "Output as JSON")
}
