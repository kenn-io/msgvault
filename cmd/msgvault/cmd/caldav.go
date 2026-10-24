package cmd

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/spf13/cobra"

	"go.kenn.io/msgvault/internal/caldav"
	"go.kenn.io/msgvault/internal/carddav"
	"go.kenn.io/msgvault/internal/providercredentials"
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
		password, err := providercredentials.ReadSecretFile(source.PasswordFile)
		if err != nil {
			return fmt.Errorf("CalDAV password_file: %w", err)
		}
		origin, err := url.Parse(source.URL)
		if err != nil {
			return fmt.Errorf("parse CalDAV source URL: %w", err)
		}
		trusted, addresses, err := source.TrustedDestination()
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
		result, syncErr := client.Sync(cmd.Context(), st, caldav.Options{Name: source.Name, BaseURL: source.URL, Username: source.Username, TimeZone: source.TimeZone, Calendars: source.Calendars, MaxEvents: source.MaxEvents, Full: full})
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "CalDAV sync: %d calendars, %d events, %d removed, %d skipped resources\n", result.Calendars, result.Events, result.Removed, result.Skipped)
		if err != nil {
			return errors.Join(syncErr, fmt.Errorf("write CalDAV sync summary: %w", err))
		}
		return syncErr
	}
	cmd.Flags().BoolVar(&full, "full", false, "reconcile a complete calendar snapshot")
	return addManualSyncCacheFlags(cmd)
}
