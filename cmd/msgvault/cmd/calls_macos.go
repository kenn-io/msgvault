package cmd

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/macoscalls"
)

func newMacOSCallsCommand(open func(context.Context) (callCommandClient, func(), error), write func(*cobra.Command, any) error) *cobra.Command {
	var path, account string
	command := &cobra.Command{Use: "import-macos", Short: "Import client-side macOS Phone and FaceTime history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(account) == "" || len(account) > 256 || !utf8.ValidString(account) || strings.ContainsRune(account, 0) {
				return errors.New("--account must be a stable nonempty label of at most 256 bytes")
			}
			if path == "" {
				if runtime.GOOS != "darwin" {
					return errors.New("--db is required outside macOS")
				}
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				path = filepath.Join(home, "Library", "Application Support", "CallHistoryDB", "CallHistory.storedata")
			}
			var client callCommandClient
			var cleanup func()
			defer func() {
				if cleanup != nil {
					cleanup()
				}
			}()
			imported := 0
			err := macoscalls.Read(cmd.Context(), path, account, func(o callhistory.Observation) error {
				record, err := json.Marshal(o)
				if err != nil {
					return err
				}
				data, err := json.Marshal(callhistory.ImportRequest{Format: "normalized", Account: account, Record: record})
				if err != nil {
					return err
				}
				if _, err := callhistory.DecodeImport(bytes.NewReader(data)); err != nil {
					return err
				}
				if client == nil {
					client, cleanup, err = open(cmd.Context())
					if err != nil {
						return err
					}
				}
				if _, err := client.ImportCallObservation(cmd.Context(), data); err != nil {
					return err
				}
				imported++
				return nil
			})
			if err != nil {
				return fmt.Errorf("import macOS calls after %d imported; rerun safely to update the same records: %w", imported, err)
			}
			return write(cmd, struct {
				Imported int `json:"imported"`
			}{imported})
		}}
	command.Flags().StringVar(&path, "db", "", "Path to CallHistory.storedata (default: macOS call history)")
	command.Flags().StringVar(&account, "account", "", "Stable namespace for this call history; reuse for later imports")
	_ = command.MarkFlagRequired("account")
	return command
}
