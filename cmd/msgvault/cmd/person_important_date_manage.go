package cmd

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importantdate"
	"go.kenn.io/msgvault/internal/store"
)

// These owner-only commands execute in the daemon subprocess so the daemon
// retains archive ownership. They never invoke an inference provider or Kata.
func newPersonImportantDateManageCommand() *cobra.Command {
	group := &cobra.Command{Use: "important-date", Short: "Manage important-date alerts"}
	var kind, name, dateText string
	add := &cobra.Command{Use: "add <person-id>", Short: "Record a dependent or pet birthday", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		date, err := store.ParsePartialDate(dateText)
		if err != nil {
			return errors.New("invalid date")
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		person, err := st.GetPersonContext(cmd.Context(), id)
		if err != nil {
			return err
		}
		subject, err := st.CreateImportantDateSubjectContext(cmd.Context(), store.ImportantDateSubjectInput{OwnerUID: person.VCardUID, Kind: kind, DisplayName: name, Date: date})
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, subject)
	}}
	add.Flags().StringVar(&kind, "kind", "", "Subject kind: dependent or pet")
	add.Flags().StringVar(&name, "name", "", "Subject name")
	add.Flags().StringVar(&dateText, "date", "", "Birthday as YYYY-MM-DD or --MM-DD")
	list := &cobra.Command{Use: "list <person-id>", Short: "List explicit dependents and pets", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		person, err := st.GetPersonContext(cmd.Context(), id)
		if err != nil {
			return err
		}
		subjects, err := st.ListImportantDateSubjectsContext(cmd.Context(), person.VCardUID)
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, subjects)
	}}
	var grant bool
	consent := &cobra.Command{Use: "consent <subject-uid>", Short: "Set consent for the configured private destination", Long: "Grant only when the subject's information may be shared with the exact configured private project and recipient. Without --grant, consent is revoked.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		cfg := state.cfg.People.ImportantDates
		if !cfg.PrivateDestination || strings.TrimSpace(cfg.Project) == "" || strings.TrimSpace(cfg.Recipient) == "" {
			return errors.New("configure an explicit private destination first")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		if err := st.SetImportantDateConsentContext(cmd.Context(), args[0], cfg.Project, cfg.Recipient, grant); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Consent updated")
		return err
	}}
	consent.Flags().BoolVar(&grant, "grant", false, "Grant recipient-specific consent; omit to revoke")
	status := &cobra.Command{Use: "status", Short: "Show retained delivery states", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		rows, err := st.ListImportantDateDeliveriesContext(cmd.Context())
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, rows)
	}}
	preview := &cobra.Command{Use: "preview", Short: "Preview currently due eligible dates", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		rows, err := importantdate.Candidates(cmd.Context(), st, state.cfg.People.ImportantDates, time.Now())
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, rows)
	}}
	group.AddCommand(add, list, consent, status, preview)
	return group
}
