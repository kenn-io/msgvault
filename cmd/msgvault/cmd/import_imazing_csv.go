package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/imazingcsv"
)

var resolveLocalTimezone = ResolveLocalTimezone

// ResolveLocalTimezone returns a concrete IANA timezone for offset-free CSV
// timestamps on Unix hosts, where TZ, /etc/localtime, or /etc/timezone name
// the local zone. Go's opaque "Local" label is not stable enough to persist.
// Windows exposes no dependable IANA name for the system timezone, so it
// refuses there and callers must require an explicit --timezone instead.
func ResolveLocalTimezone() (string, error) {
	if runtime.GOOS == "windows" {
		return "", errors.New("the Windows system timezone has no dependable IANA name")
	}
	candidates := []string{strings.TrimPrefix(strings.TrimSpace(os.Getenv("TZ")), ":")}
	if time.Local != nil {
		candidates = append(candidates, time.Local.String())
	}
	if target, err := filepath.EvalSymlinks("/etc/localtime"); err == nil {
		if _, zone, ok := strings.Cut(filepath.ToSlash(target), "/zoneinfo/"); ok {
			candidates = append(candidates, zone)
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		candidates = append(candidates, strings.TrimSpace(string(data)))
	}
	for _, candidate := range candidates {
		if candidate == "" || candidate == "Local" || filepath.IsAbs(candidate) {
			continue
		}
		if _, err := time.LoadLocation(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("local timezone has no concrete IANA name")
}

func newImportIMazingCSVCmd() *cobra.Command {
	var opts imazingcsv.Options
	command := &cobra.Command{
		Use:   "import-imazing-csv <export-dir>",
		Short: "Import Messages CSV exports created by iMazing",
		Long: `Import iMessage and SMS history from an iMazing Messages CSV export.

The input can be the export root containing csv/ and attachments/, or the csv/
directory itself. --me identifies your phone number or email address. Offset-free
message dates use --timezone. On Unix, an omitted --timezone resolves the local
IANA zone; Windows has no dependable local IANA zone, so --timezone is required
there.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Timezone == "" {
				zone, err := resolveLocalTimezone()
				if err != nil {
					return fmt.Errorf("resolve local timezone; pass --timezone with an IANA name: %w", err)
				}
				opts.Timezone = zone
				if err := cmd.Flags().Set("timezone", zone); err != nil {
					return fmt.Errorf("forward resolved timezone: %w", err)
				}
			}
			if !isDaemonCLISubprocess() {
				// The export directory and --contacts file are caller-local
				// inputs; a configured remote daemon cannot read them.
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(cmd, args, nil)
			}
			return runImportIMazingCSV(cmd, args[0], opts)
		},
	}
	command.Flags().StringVar(&opts.Owner, "me", "", "your phone number or email address")
	command.Flags().StringVar(&opts.ContactsPath, "contacts", "", "vCard file used to fill empty participant names")
	command.Flags().StringVar(&opts.Timezone, "timezone", "",
		"IANA timezone for dates without an offset (required on Windows)")
	_ = command.MarkFlagRequired("me")
	return command
}

func runImportIMazingCSV(cmd *cobra.Command, exportDir string, opts imazingcsv.Options) error {
	info, err := os.Stat(exportDir)
	if err != nil {
		return fmt.Errorf("inspect iMazing export directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("iMazing export path is not a directory: %s", exportDir)
	}
	dbPath := cfg.DatabaseDSN()
	st, cleanup, err := openWritableStoreAndInitForIngest()
	if err != nil {
		return err
	}
	defer cleanup()
	opts.AttachmentsDir = cfg.AttachmentsDir()
	summary, err := imazingcsv.NewImporter(st, opts).ImportPath(cmd.Context(), exportDir)
	if err != nil {
		// Messages commit one by one; committed rows must still reach the
		// analytics cache even though the import as a whole failed.
		return errors.Join(
			fmt.Errorf("import iMazing CSV: %w", err),
			rebuildCacheAfterWrite(dbPath),
		)
	}
	if err := runPostSourceCreateMigrations(st); err != nil {
		return errors.Join(
			fmt.Errorf("post-source-create migrations: %w", err),
			rebuildCacheAfterWrite(dbPath),
		)
	}
	_, _ = fmt.Fprintln(cmd.OutOrStdout(), "Import complete")
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Files:               %d\n", summary.Files)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Conversations:       %d\n", summary.Conversations)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Messages:            %d\n", summary.Messages)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Participants:        %d\n", summary.Participants)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments stored:  %d\n", summary.AttachmentsStored)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments missing: %d\n", summary.AttachmentsMissing)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Attachments skipped: %d\n", summary.AttachmentsSkipped)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Replies linked:      %d\n", summary.RepliesLinked)
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Replies unresolved:  %d\n", summary.RepliesUnresolved)
	if summary.ContactsTotal > 0 {
		_, _ = fmt.Fprintf(cmd.OutOrStdout(), "  Contacts matched:    %d of %d\n",
			summary.ContactsMatched, summary.ContactsTotal)
	}
	return rebuildCacheAfterWrite(dbPath)
}

func init() {
	rootCmd.AddCommand(newImportIMazingCSVCmd())
}
