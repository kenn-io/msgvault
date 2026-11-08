package cmd

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/importantdate"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/taskclient"
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
		st, cleanup, err := openReadOnlyStoreForInvocation(cmd.Context(), state)
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
	lineage := &cobra.Command{Use: "lineage <person-id>", Short: "Inspect retained important-date identity history", Long: "Inspect connected merge and split identities and retained occurrences. This does not reconcile or release a quarantined identity.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		st, cleanup, err := openReadOnlyStoreForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		result, err := st.ImportantDateLineageContext(cmd.Context(), id)
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, result)
	}}
	var reconcileDigest, reconcileKey, reconcileDecisions string
	var reconcileRevision int64
	var inspectedSnapshots bool
	reconcile := &cobra.Command{Use: "reconcile <person-id>", Short: "Apply reviewed important-date identity decisions", Long: "After reviewing lineage and required merge snapshots, provide explicit decisions for every listed historical person and owned subject. Existing consent is revoked. Ambiguous mappings remain quarantined.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		id, err := positivePersonCLIArg(cmd, args[0], personValue)
		if err != nil {
			return err
		}
		var decisions []store.ImportantDateIdentityDecision
		if err := json.Unmarshal([]byte(reconcileDecisions), &decisions); err != nil {
			return errors.New("invalid important-date decisions JSON")
		}
		if reconcileRevision < 0 {
			return errors.New("identity revision is required")
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
		uid, err := st.ReconcileImportantDateIdentityContext(cmd.Context(), store.ImportantDateReconcileRequest{PersonID: id, IdentityRevision: reconcileRevision, LineageDigest: reconcileDigest, IdempotencyKey: reconcileKey, Actor: "owner", InspectedSnapshots: inspectedSnapshots, Decisions: decisions})
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, map[string]string{"reconciliation_uid": uid})
	}}
	reconcile.Flags().StringVar(&reconcileDigest, "digest", "", "Digest from the inspected lineage view")
	reconcile.Flags().Int64Var(&reconcileRevision, "identity-revision", -1, "Identity revision from the inspected lineage view")
	reconcile.Flags().StringVar(&reconcileKey, "key", "", "Unique idempotency key for this decision")
	reconcile.Flags().StringVar(&reconcileDecisions, "decisions-json", "", "Explicit subject and meaning mappings as a JSON array")
	reconcile.Flags().BoolVar(&inspectedSnapshots, "inspected-snapshots", false, "Confirm that required merge snapshots were inspected")
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
		if grant && (!cfg.PrivateDestination || strings.TrimSpace(cfg.Project) == "" || strings.TrimSpace(cfg.Recipient) == "") {
			return errors.New("configure an explicit private destination first")
		}
		st, cleanup, err := openWritableStoreAndInitForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		if !grant {
			if err := st.RevokeImportantDateConsentContext(cmd.Context(), args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Consent revoked for all destinations")
			if err != nil {
				return fmt.Errorf("write consent status: %w", err)
			}
			return nil
		}
		if !state.cfg.Integrations.Kata.Enabled {
			return errors.New("enable the Kata integration first")
		}
		client, err := taskclient.ConnectKata(cmd.Context(), taskclient.IntegrationConfig{Enabled: true, Endpoint: state.cfg.Integrations.Kata.Endpoint, APIKey: state.cfg.Integrations.Kata.APIKey, DefaultProject: cfg.Project})
		if err != nil {
			return fmt.Errorf("connect important-date destination: %w", err)
		}
		projectUID, err := client.ProjectUID(cmd.Context(), cfg.Project)
		if err != nil {
			return fmt.Errorf("resolve important-date project: %w", err)
		}
		if err := st.SetImportantDateConsentContext(cmd.Context(), args[0], importantdate.DestinationScope(state.cfg.Integrations.Kata.Endpoint, cfg.Project, projectUID), cfg.Recipient, grant); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Consent updated")
		if err != nil {
			return fmt.Errorf("write important-date status: %w", err)
		}
		return nil
	}}
	consent.Flags().BoolVar(&grant, "grant", false, "Grant recipient-specific consent; omit to revoke")
	var statusLimit, statusOffset int
	status := &cobra.Command{Use: "status", Short: "Show retained delivery states", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openReadOnlyStoreForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		rows, err := st.ListImportantDateDeliveriesPageContext(cmd.Context(), statusLimit, statusOffset)
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, rows)
	}}
	status.Flags().IntVar(&statusLimit, "limit", 100, "Maximum occurrences to show (1-500)")
	status.Flags().IntVar(&statusOffset, "offset", 0, "Number of occurrences to skip")
	preview := &cobra.Command{Use: "preview", Short: "Preview currently due eligible dates", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		state := invocationFromCommand(cmd)
		if state == nil || state.cfg == nil {
			return errors.New("configuration unavailable")
		}
		st, cleanup, err := openReadOnlyStoreForInvocation(cmd.Context(), state)
		if err != nil {
			return err
		}
		defer cleanup()
		rows, err := importantdate.PreviewCandidates(cmd.Context(), st, state.cfg.People.ImportantDates, time.Now())
		if err != nil {
			return err
		}
		return writeJSONOutput(cmd, rows)
	}}
	var updateName, updateDate string
	var revision int64
	update := &cobra.Command{Use: "update <subject-uid>", Short: "Correct a dependent or pet birthday", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		date, err := store.ParsePartialDate(updateDate)
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
		changed, err := st.UpdateImportantDateSubjectContext(cmd.Context(), args[0], revision, updateName, date, false)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("subject revision changed or subject is unavailable")
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Subject updated")
		if err != nil {
			return fmt.Errorf("write important-date status: %w", err)
		}
		return nil
	}}
	update.Flags().StringVar(&updateName, "name", "", "Subject name")
	update.Flags().StringVar(&updateDate, "date", "", "Birthday as YYYY-MM-DD or --MM-DD")
	update.Flags().Int64Var(&revision, "revision", 0, "Current revision from list")
	var withdrawRevision int64
	withdraw := &cobra.Command{Use: "withdraw <subject-uid>", Short: "Withdraw an explicit dependent or pet", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		changed, err := st.UpdateImportantDateSubjectContext(cmd.Context(), args[0], withdrawRevision, "", store.PartialDate{}, true)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("subject revision changed or subject is unavailable")
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Subject withdrawn")
		if err != nil {
			return fmt.Errorf("write important-date status: %w", err)
		}
		return nil
	}}
	withdraw.Flags().Int64Var(&withdrawRevision, "revision", 0, "Current revision from list")
	var resolution, issueUID string
	resolve := &cobra.Command{Use: "resolve <occurrence-key>", Short: "Record the result of an inspected uncertain delivery", Long: "After inspecting the issue and inbox, mark an unresolved occurrence delivered or cancelled. This never resends a request.", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
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
		changed, err := st.ResolveImportantDateDeliveryContext(cmd.Context(), args[0], store.ImportantDateDeliveryState(resolution), issueUID)
		if err != nil {
			return err
		}
		if !changed {
			return errors.New("occurrence is not unresolved or issue identity differs")
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Delivery resolved")
		if err != nil {
			return fmt.Errorf("write important-date status: %w", err)
		}
		return nil
	}}
	resolve.Flags().StringVar(&resolution, "resolution", "", "delivered or cancelled")
	resolve.Flags().StringVar(&issueUID, "issue", "", "Inspected Kata issue UID (required for delivered)")
	var correctionDate string
	acceptCorrection := &cobra.Command{Use: "accept-correction <subject-uid> <birthday|anniversary>", Short: "Approve a corrected date after inspecting prior issues", Long: "After inspecting prior issues, explicitly approve the current month and day for future occurrences. This does not send or recreate an issue.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if !isDaemonCLISubprocess() {
			return runDaemonCLICommandHTTPFromCobra(cmd, args)
		}
		if (!strings.HasPrefix(args[0], "person/") && !strings.HasPrefix(args[0], "owned/")) || (args[1] != "birthday" && args[1] != "anniversary") {
			return errors.New("invalid subject or date kind")
		}
		date, err := store.ParsePartialDate(correctionDate)
		if err != nil {
			return errors.New("invalid date")
		}
		fingerprint := importantdate.DateFingerprint(date)
		if fingerprint == "" {
			return errors.New("month and day are required")
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
		count, err := st.AcceptImportantDateCorrectionContext(cmd.Context(), args[0]+":"+args[1]+":", fingerprint)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Accepted correction for %d retained record(s)\n", count)
		if err != nil {
			return fmt.Errorf("write correction status: %w", err)
		}
		return nil
	}}
	acceptCorrection.Flags().StringVar(&correctionDate, "date", "", "Current date as YYYY-MM-DD or --MM-DD")
	group.AddCommand(add, list, lineage, reconcile, consent, status, preview, update, withdraw, resolve, acceptCorrection)
	return group
}
