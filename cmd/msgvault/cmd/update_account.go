package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wesm/msgvault/internal/store"
)

var updateDisplayName string

var updateAccountCmd = &cobra.Command{
	Use:   "update-account <email>",
	Short: "Update account settings",
	Long: `Update settings for an existing account.

Currently supports updating the display name for an account.

Examples:
  msgvault update-account you@gmail.com --display-name "Work"
  msgvault update-account you@gmail.com --display-name "Personal Email"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		email := args[0]

		if updateDisplayName == "" {
			return fmt.Errorf("nothing to update: use --display-name to set a display name")
		}

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

		source, err := s.GetSourceByIdentifier(email)
		if err != nil {
			return fmt.Errorf("get account: %w", err)
		}
		if source == nil {
			return fmt.Errorf("account not found: %s", email)
		}

		if err := s.UpdateSourceDisplayName(source.ID, updateDisplayName); err != nil {
			return fmt.Errorf("update display name: %w", err)
		}

		fmt.Printf("Updated account %s: display name set to %q\n", email, updateDisplayName)
		return nil
	},
}

func init() {
	rootCmd.AddCommand(updateAccountCmd)
	updateAccountCmd.Flags().StringVar(&updateDisplayName, "display-name", "", "Set the display name for the account")
}
