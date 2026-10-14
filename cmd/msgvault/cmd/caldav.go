package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/caldav"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/config"
)

func init() { rootCmd.AddCommand(newSyncCalDAVCmd()) }
func newSyncCalDAVCmd() *cobra.Command {
	var full bool
	cmd := &cobra.Command{Use: "sync-caldav <name>", Short: "Archive CalDAV events", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration is unavailable")
		}
		source := state.cfg.GetCalDAVSource(args[0])
		if source == nil {
			return errors.New("CalDAV source is not configured")
		}
		password, err := readCalDAVPassword(source.PasswordFile)
		if err != nil {
			return err
		}
		origin, err := url.Parse(source.URL)
		if err != nil {
			return fmt.Errorf("parse CalDAV source URL: %w", err)
		}
		policy := config.CardDAVConfig{TrustedOrigin: source.TrustedOrigin, TrustedAddresses: source.TrustedAddresses}
		trusted, addresses, err := policy.TrustedDestination()
		if err != nil {
			return err
		}
		client, err := caldav.NewClient(carddav.ClientOptions{CredentialOrigin: origin, Username: source.Username, Password: password, TrustedOrigin: trusted, TrustedAddresses: addresses})
		if err != nil {
			return err
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(state)
		if err != nil {
			return err
		}
		defer cleanup()
		result, err := client.Sync(cmd.Context(), st, caldav.Options{Name: source.Name, BaseURL: source.URL, Username: source.Username, TimeZone: source.TimeZone, Calendars: source.Calendars, Full: full})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "CalDAV sync: %d calendars, %d events, %d removed\n", result.Calendars, result.Events, result.Removed)
		if err != nil {
			return fmt.Errorf("write CalDAV sync summary: %w", err)
		}
		return nil
	}
	cmd.Flags().BoolVar(&full, "full", false, "reconcile a complete calendar snapshot")
	return addManualSyncCacheFlags(cmd)
}
func readCalDAVPassword(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open CalDAV password_file: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat CalDAV password_file: %w", err)
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return "", errors.New("CalDAV password_file must be a regular file readable only by its owner")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil {
		return "", fmt.Errorf("read CalDAV password_file: %w", err)
	}
	if len(raw) > 65536 {
		return "", errors.New("CalDAV password_file exceeds 64 KiB")
	}
	password := strings.TrimRight(string(raw), "\r\n")
	if password == "" {
		return "", errors.New("CalDAV password_file is empty")
	}
	return password, nil
}
