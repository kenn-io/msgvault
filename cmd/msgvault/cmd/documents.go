package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/msgvault/internal/attachmentstore"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/operations"
	"go.kenn.io/msgvault/internal/personscope"
	personresolver "go.kenn.io/msgvault/internal/personscope/resolver"
	"go.kenn.io/msgvault/internal/store"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

const (
	documentsCommandName            = "documents"
	documentBuildSubcommand         = "build"
	commandOperationRecorderTimeout = 5 * time.Second
)

type commandOperationPass struct {
	recorder operations.Recorder
	id       operations.StableID
	kind     operations.Kind
}

func newOperationPassScope(prefix string, trigger operations.Trigger) operations.PassScope {
	return operations.PassScope{
		Key: prefix + ":" + uuid.NewString(), Trigger: trigger, StartedAt: time.Now().UTC(),
	}
}

func beginCommandOperationPass(
	ctx context.Context, recorder operations.Recorder, kind operations.Kind, scope operations.PassScope,
) (*commandOperationPass, *operations.Run, error) {
	spec := scope.InvocationSpec(kind)
	if err := spec.Validate(); err != nil {
		return nil, nil, fmt.Errorf("%s operation pass scope: %w", kind, err)
	}
	if operationRecorderIsNil(recorder) {
		return nil, nil, fmt.Errorf("begin %s operation pass: operation recorder is required", kind)
	}
	begun, err := recorder.Begin(ctx, spec)
	if err != nil {
		return nil, nil, fmt.Errorf("begin %s operation pass: %w", kind, err)
	}
	switch begun.Disposition {
	case operations.BeginCreated:
		return &commandOperationPass{recorder: recorder, id: begun.ID, kind: kind}, nil, nil
	case operations.BeginTerminal:
		if begun.Terminal == nil {
			return nil, nil, fmt.Errorf("begin %s operation pass returned terminal without outcome", kind)
		}
		return nil, begun.Terminal, nil
	case operations.BeginActive:
		return nil, nil, fmt.Errorf("begin %s operation pass found an active invocation", kind)
	default:
		return nil, nil, fmt.Errorf("begin %s operation pass returned invalid disposition %q", kind, begun.Disposition)
	}
}

func operationRecorderIsNil(recorder operations.Recorder) bool {
	if recorder == nil {
		return true
	}
	value := reflect.ValueOf(recorder)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (p *commandOperationPass) checkpoint(ctx context.Context, counters operations.InvocationCounters) {
	if p == nil {
		return
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandOperationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Checkpoint(checkpointCtx, p.id, counters); err != nil {
		logger.Error("operation recorder checkpoint failed", "kind", p.kind, "error", err)
	}
}

func (p *commandOperationPass) finish(
	ctx context.Context, counters operations.InvocationCounters, runErr error,
) {
	if p == nil {
		return
	}
	publicError := commandOperationPublicError(ctx, runErr)
	state, err := operations.DeriveInvocationState(p.kind, counters, publicError)
	if err != nil {
		logger.Error("operation recorder finish state failed", "kind", p.kind, "error", err)
		return
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandOperationRecorderTimeout)
	defer cancel()
	if err := p.recorder.Finish(finishCtx, p.id, counters, state, publicError); err != nil {
		logger.Error("operation recorder finish failed", "kind", p.kind, "error", err)
	}
}

func commandOperationPublicError(ctx context.Context, runErr error) *operations.PublicError {
	if errors.Is(runErr, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return operations.FixedPublicError(operations.PublicErrorInvocationCancelled)
	}
	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return operations.FixedPublicError(operations.PublicErrorInvocationTimeout)
	}
	if runErr != nil {
		return operations.FixedPublicError(operations.PublicErrorInvocationUpstreamFailed)
	}
	return nil
}

type documentBuildMode int

const (
	documentBuildIncremental documentBuildMode = iota
	documentBuildStartRebuild
	documentBuildResume
)

type documentStatusOutput struct {
	ProfileID                  string                    `json:"profile_id"`
	AuthenticatedFormats       int                       `json:"authenticated_formats"`
	Provider                   string                    `json:"provider"`
	Endpoint                   string                    `json:"endpoint"`
	Region                     string                    `json:"region"`
	Model                      string                    `json:"model"`
	RetentionPosture           string                    `json:"retention_posture"`
	TrainingPosture            string                    `json:"training_posture"`
	StoresPlaintext            bool                      `json:"stores_normalized_plaintext"`
	BackupsMayContainText      bool                      `json:"backups_may_contain_normalized_plaintext"`
	HostedTextEmbeddings       bool                      `json:"hosted_text_embeddings_enabled"`
	PricingAssumptionOn        string                    `json:"pricing_assumption_on,omitempty"`
	EstimatedSuccessfulCostUSD *float64                  `json:"estimated_successful_cost_usd,omitempty"`
	SpoolQuotaBytes            int64                     `json:"spool_quota_bytes"`
	MinFreeSpaceBytes          int64                     `json:"min_free_space_bytes"`
	ActiveRebuild              *documentRebuildStatus    `json:"active_rebuild,omitempty"`
	Status                     store.DocumentIndexStatus `json:"status"`
}

type documentRebuildStatus struct {
	SnapshotOwners  int64 `json:"snapshot_owners"`
	RemainingOwners int64 `json:"remaining_owners"`
}

type documentBuildResult struct {
	Reconciled      int
	Changes         int
	Processed       int
	Units           int
	Skipped         int
	Failed          int
	CleanupFailures int
	RebuildID       string
	Remaining       int64
	Completed       bool
	Failures        []documentBuildFailure
}

type documentBuildFailure struct {
	CanonicalBlobHash string
	ReasonCode        string
}

type documentsCommandDeps struct {
	newMistralClient      func(*documentindex.DocumentsConfig) (*mistral.Client, error)
	newMistralProcessor   func(*documentindex.DocumentsConfig) (documentindex.MistralProcessor, error)
	validateProbeFixtures func(context.Context, mistral.Policy, mistral.ProbeFixtureConfig) error
	runCapabilityProbe    func(context.Context, *mistral.Client, mistral.ProbeConfig) (mistral.CapabilityManifest, error)
	openStore             func() (*store.Store, func(), error)
	openAttachments       func(*store.Store) (documentindex.DocumentAttachmentOpener, func() error, error)
	openReadClient        func(context.Context) (documentReadClient, func(), error)
	runDocumentVector     func(context.Context, *store.Store, int64, int) (vectordocument.ReconcileResult, error)
}

type documentReadClient interface {
	SearchDocuments(
		ctx context.Context,
		request store.DocumentSearchRequest,
	) (store.DocumentSearchResponse, error)
	GetDocumentIndexStatus(
		ctx context.Context,
		request store.DocumentIndexStatusRequest,
	) (store.DocumentIndexStatusResponse, error)
}

func defaultDocumentsCommandDeps() documentsCommandDeps {
	return documentsCommandDeps{
		newMistralClient:      newConfiguredMistralClient,
		newMistralProcessor:   newConfiguredMistralProcessor,
		validateProbeFixtures: mistral.ValidateProbeFixtures,
		runCapabilityProbe:    mistral.RunCapabilityProbe,
		openStore:             openWritableStoreAndInit,
		runDocumentVector:     runConfiguredDocumentVectorGeneration,
		openAttachments:       openDocumentAttachments,
		openReadClient: func(ctx context.Context) (documentReadClient, func(), error) {
			client, _, err := OpenHTTPStore(ctx)
			if err != nil {
				return nil, func() {}, err
			}
			return client, func() { _ = client.Close() }, nil
		},
	}
}

func newDocumentsCmd(deps documentsCommandDeps) *cobra.Command {
	parent := &cobra.Command{
		Use:   documentsCommandName,
		Short: "Manage document attachment indexing",
	}
	parent.AddCommand(newProbeMistralCmd(deps))
	parent.AddCommand(newConsentMistralCmd(deps))
	parent.AddCommand(newBuildDocumentsCmd(deps))
	parent.AddCommand(newResumeDocumentsCmd(deps))
	parent.AddCommand(newSearchDocumentsCmd(deps))
	parent.AddCommand(newDocumentStatusCmd(deps))
	parent.AddCommand(newRetryDocumentCmd(deps))
	parent.AddCommand(newRetireDocumentProfileCmd(deps))
	parent.AddCommand(newPurgeDocumentDerivedCmd(deps))
	parent.AddCommand(newDocumentVectorsCmd(deps))
	return parent
}

func newSearchDocumentsCmd(deps documentsCommandDeps) *cobra.Command {
	var request store.DocumentSearchRequest
	var jsonOutput bool
	var rawDirections []string
	var afterValue, beforeValue string
	command := &cobra.Command{
		Use:   "search <query>",
		Short: "Search extracted document attachments",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			request.Query = strings.Join(args, " ")
			if command.Flags().Changed("person") && request.PersonID <= 0 {
				return errors.New("--person must be a positive person ID")
			}
			if command.Flags().Changed("participant") && request.ParticipantID <= 0 {
				return errors.New("--participant must be a positive participant ID")
			}
			if request.PersonID > 0 && request.ParticipantID > 0 {
				return errors.New("--person and --participant are mutually exclusive")
			}
			if len(rawDirections) > 0 && request.PersonID == 0 && request.ParticipantID == 0 {
				return errors.New("--direction requires --person or --participant")
			}
			request.Directions = make([]personscope.Direction, len(rawDirections))
			for i, direction := range rawDirections {
				request.Directions[i] = personscope.Direction(direction)
			}
			if len(request.Directions) > 0 {
				if _, _, err := personresolver.NormalizeDirections(request.Directions); err != nil {
					return err
				}
			}
			var err error
			if request.After, err = parseDocumentSearchDate(afterValue); err != nil {
				return fmt.Errorf("invalid --after: %w", err)
			}
			if request.Before, err = parseDocumentSearchDate(beforeValue); err != nil {
				return fmt.Errorf("invalid --before: %w", err)
			}
			if request.After != nil && request.Before != nil && !request.After.Before(*request.Before) {
				return errors.New("--after must be before --before")
			}
			return runSearchDocuments(command, request, jsonOutput, deps)
		},
	}
	command.Flags().Int64SliceVar(&request.SourceIDs, "source-id", nil, "Limit results to source IDs")
	command.Flags().StringSliceVar(&request.MessageTypes, "message-type", nil, "Limit results to message types")
	command.Flags().Int64Var(&request.AttachmentID, "attachment-id", 0, "Limit results to one attachment occurrence")
	command.Flags().Int64Var(&request.MessageID, "message-id", 0, "Limit results to one containing message")
	command.Flags().Int64Var(&request.PersonID, "person", 0, "Limit results to one durable person")
	command.Flags().Int64Var(&request.ParticipantID, "participant", 0, "Limit results to one observed participant, translated through its durable person when bound")
	command.Flags().StringSliceVar(&rawDirections, "direction", nil, "Person relation: from_person, to_person, or group")
	command.Flags().StringVar(&afterValue, "after", "", "Only messages on or after YYYY-MM-DD or RFC3339")
	command.Flags().StringVar(&beforeValue, "before", "", "Only messages before YYYY-MM-DD or RFC3339")
	command.Flags().IntVarP(&request.PageSize, "limit", "n", 20, "Maximum results to return")
	command.Flags().StringVar(&request.Cursor, "cursor", "", "Opaque cursor from the previous page")
	command.Flags().StringVar(&request.SearchMode, "mode", "lexical", "Search mode: lexical (default and auto); semantic/hybrid send the query to the embedding provider")
	command.Flags().IntVar(&request.CandidateLimit, "candidate-limit", 0, "Maximum candidates (default/max: lexical 10000; semantic/hybrid 100/1000)")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func parseDocumentSearchDate(value string) (*time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil //nolint:nilnil // An omitted optional date has no value and no error.
	}
	for _, layout := range []string{"2006-01-02", time.RFC3339} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			parsed = parsed.UTC()
			return &parsed, nil
		}
	}
	return nil, errors.New("must be YYYY-MM-DD or RFC3339")
}

func newProbeMistralCmd(deps documentsCommandDeps) *cobra.Command {
	var fixtureDirectory string
	var validateOnly bool
	command := &cobra.Command{
		Use:   "probe-mistral",
		Short: "Probe stateless Mistral OCR document format support",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runProbeMistral(command, fixtureDirectory, validateOnly, deps)
		},
	}
	command.Flags().StringVar(&fixtureDirectory, "fixtures", "", "Directory containing the complete synthetic fixture matrix")
	command.Flags().BoolVar(&validateOnly, "validate-only", false, "Validate private fixtures locally without a provider request")
	_ = command.MarkFlagRequired("fixtures")
	return command
}

func newConsentMistralCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	var confirmed bool
	command := &cobra.Command{
		Use:   "consent-mistral",
		Short: "Record consent for the exact Mistral document policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, nil)
			}
			return runConsentMistral(command, capabilityPath, confirmed, deps)
		},
	}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the configured upload and provider privacy policy")
	_ = command.MarkFlagRequired("capabilities")
	return command
}

func newBuildDocumentsCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	var limit int
	var fullRebuild bool
	var confirmed bool
	command := &cobra.Command{
		Use:   documentBuildSubcommand,
		Short: "Extract and index eligible document attachments",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			mode := documentBuildIncremental
			if fullRebuild {
				mode = documentBuildStartRebuild
			}
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, documentProviderForwardEnv())
			}
			return runBuildDocuments(command, capabilityPath, limit, mode, confirmed, deps)
		},
	}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().IntVar(&limit, "limit", 100, "Maximum canonical documents to process")
	command.Flags().BoolVar(&fullRebuild, "full-rebuild", false, "Replace current extractions for all eligible documents")
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the disclosed provider uploads for this build")
	_ = command.MarkFlagRequired("capabilities")
	return command
}

func newResumeDocumentsCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	var limit int
	var confirmed bool
	command := &cobra.Command{
		Use:   cmdUseResume,
		Short: "Resume pending and retry-ready document extraction",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, documentProviderForwardEnv())
			}
			return runBuildDocuments(command, capabilityPath, limit, documentBuildResume, confirmed, deps)
		},
	}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().IntVar(&limit, "limit", 100, "Maximum canonical documents to process")
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the disclosed provider uploads for this resume pass")
	_ = command.MarkFlagRequired("capabilities")
	return command
}

func newDocumentStatusCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	var jsonOutput bool
	command := &cobra.Command{
		Use:   statusValue,
		Short: "Show document attachment indexing status",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return runDocumentStatus(command, capabilityPath, jsonOutput, deps)
		},
	}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	_ = command.MarkFlagRequired("capabilities")
	return command
}

func newRetryDocumentCmd(deps documentsCommandDeps) *cobra.Command {
	var capabilityPath string
	var canonicalBlobHash string
	command := &cobra.Command{
		Use:   "retry",
		Short: "Retry one terminal document extraction",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, nil)
			}
			return runRetryDocument(command, capabilityPath, canonicalBlobHash, deps)
		},
	}
	command.Flags().StringVar(&capabilityPath, "capabilities", "", "Authenticated Mistral capability manifest")
	command.Flags().StringVar(&canonicalBlobHash, "hash", "", "Exact lowercase attachment SHA-256")
	_ = command.MarkFlagRequired("capabilities")
	_ = command.MarkFlagRequired("hash")
	return command
}

func newRetireDocumentProfileCmd(deps documentsCommandDeps) *cobra.Command {
	var confirmed bool
	command := &cobra.Command{
		Use:   "retire <profile-id>",
		Short: "Retire one exact document extraction profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runRetireDocumentProfile(command, args[0], confirmed, deps)
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the profile retirement")
	return command
}

func newPurgeDocumentDerivedCmd(deps documentsCommandDeps) *cobra.Command {
	var canonicalBlobHash string
	var confirmed bool
	command := &cobra.Command{
		Use:   "purge-derived",
		Short: "Delete local document derivatives for one exact attachment hash",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runPurgeDocumentDerived(command, canonicalBlobHash, confirmed, deps)
		},
	}
	command.Flags().StringVar(&canonicalBlobHash, "hash", "", "Exact lowercase attachment SHA-256")
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm permanent deletion of local document derivatives")
	_ = command.MarkFlagRequired("hash")
	return command
}

// documentProviderForwardEnv carries the caller's configured provider key to
// the daemon-owned subprocess that performs an explicitly requested build.
func documentProviderForwardEnv() map[string]string {
	if cfg == nil {
		return nil
	}
	name := cfg.Attachments.Documents.APIKeyEnv
	if name == "" {
		return nil
	}
	value := os.Getenv(name)
	if value == "" {
		return nil
	}
	return map[string]string{name: value}
}

func runProbeMistral(
	command *cobra.Command,
	fixtureDirectory string,
	validateOnly bool,
	deps documentsCommandDeps,
) error {
	if cfg == nil {
		return errors.New("document probe requires loaded configuration")
	}
	documentsConfig := &cfg.Attachments.Documents
	if !validateOnly {
		if !documentsConfig.Enabled {
			return errors.New("document probe requires attachments.documents.enabled=true")
		}
		if documentsConfig.RetentionPosture == documentindex.RetentionUnknown ||
			documentsConfig.TrainingPosture == documentindex.TrainingUnknown {
			return errors.New("document probe requires explicit retention_posture and training_posture")
		}
	}
	policyConfig := *documentsConfig
	if validateOnly {
		if policyConfig.RetentionPosture == documentindex.RetentionUnknown {
			policyConfig.RetentionPosture = documentindex.RetentionStandard
		}
		if policyConfig.TrainingPosture == documentindex.TrainingUnknown {
			policyConfig.TrainingPosture = documentindex.TrainingDefaultOptOut
		}
	}
	policy, err := policyConfig.MistralPolicy()
	if err != nil {
		return err
	}
	spoolDirectory := filepath.Join(cfg.Data.DataDir, "tmp", "document-probe")
	if err := fileutil.SecureMkdirAll(spoolDirectory, 0o700); err != nil {
		return fmt.Errorf("create private document probe spool directory: %w", err)
	}
	fixtureConfig := mistral.ProbeFixtureConfig{
		FixtureDirectory: fixtureDirectory, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: documentsConfig.MaxSpoolBytes, MinFreeBytes: documentsConfig.MinFreeSpaceBytes,
	}
	if validateOnly {
		if err := deps.validateProbeFixtures(command.Context(), policy, fixtureConfig); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(command.OutOrStdout(),
			"Validated %d private Mistral fixture(s) locally; no provider requests were made.\n",
			len(mistral.CandidateFormats()))
		return nil
	}
	client, err := deps.newMistralClient(documentsConfig)
	if err != nil {
		return err
	}
	manifest, err := deps.runCapabilityProbe(command.Context(), client, mistral.ProbeConfig{
		Fixtures: fixtureConfig,
	})
	if err != nil {
		return err
	}
	if err := mistral.EncodeCapabilityManifest(command.OutOrStdout(), manifest); err != nil {
		return fmt.Errorf("write Mistral capability manifest: %w", err)
	}
	return nil
}

func runConsentMistral(
	command *cobra.Command,
	capabilityPath string,
	confirmed bool,
	deps documentsCommandDeps,
) error {
	documentsConfig, manifest, allowedMediaTypes, profile, err := configuredDocumentProfile(capabilityPath)
	if err != nil {
		return err
	}
	if !documentsConfig.Enabled {
		return errors.New("document consent requires attachments.documents.enabled=true")
	}
	printDocumentConsentDisclosure(
		command.OutOrStdout(), documentsConfig, profile, len(allowedMediaTypes),
	)
	if !confirmed {
		return errors.New("document consent requires --yes after reviewing the configured retention and training postures")
	}
	if manifest.MaxUnits < documentsConfig.MaxPagesPerDocument || len(allowedMediaTypes) == 0 {
		return errors.New("document capability manifest does not authorize the configured policy")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.EnsureDocumentExtractionProfile(command.Context(), profile); err != nil {
		return err
	}
	if err := st.RecordDocumentProviderConsent(command.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}); err != nil {
		return err
	}
	if err := bootstrapDocumentOccurrencesIfConsented(command.Context(), st); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Recorded Mistral document consent for profile %s (%d authenticated format(s), retention=%s, training=%s).\n",
		profile.ID, len(allowedMediaTypes), profile.RetentionPosture, profile.TrainingPosture)
	return nil
}

func repairHistoricalAttachmentRoles(ctx context.Context, st *store.Store) error {
	const batchSize = 1000
	for {
		progress, err := st.RepairHistoricalAttachmentRolesBatch(ctx, batchSize)
		if err != nil {
			return fmt.Errorf("repair historical attachment roles: %w", err)
		}
		if progress.Completed {
			return nil
		}
	}
}

func printDocumentConsentDisclosure(
	w io.Writer,
	documentsConfig *documentindex.DocumentsConfig,
	profile store.DocumentExtractionProfile,
	authenticatedFormats int,
) {
	_, _ = fmt.Fprintln(w, "Hosted document extraction disclosure:")
	_, _ = fmt.Fprintf(w,
		"- Complete original document bytes and their media type will be sent to %s (%s).\n",
		profile.Endpoint, profile.Region)
	_, _ = fmt.Fprintf(w,
		"- The exact authenticated policy allows %d format(s), at most %s and %d provider unit(s) per document.\n",
		authenticatedFormats, formatSize(documentsConfig.MaxFileBytes), documentsConfig.MaxPagesPerDocument)
	_, _ = fmt.Fprintf(w, "- Private temporary spools are capped at %s and preserve %s of free disk space.\n",
		formatSize(documentsConfig.MaxSpoolBytes), formatSize(documentsConfig.MinFreeSpaceBytes))
	_, _ = fmt.Fprintf(w, "- Provider assertions: retention=%s, training=%s.\n",
		profile.RetentionPosture, profile.TrainingPosture)
	_, _ = fmt.Fprintln(w,
		"- Normalized plaintext units and chunks will be stored in the local archive database and may be included in disclosed full backups.")
	_, _ = fmt.Fprintln(w,
		"- Raw provider JSON and full provider Markdown are transient; this consent does not enable hosted document text embeddings.")
	if documentsConfig.EstimatedCostUSDPerKUnits > 0 {
		_, _ = fmt.Fprintf(w,
			"- Cost planning uses %.6g USD per 1,000 provider unit(s), assumed on %s, with a %.2f USD run cap.\n",
			documentsConfig.EstimatedCostUSDPerKUnits, documentsConfig.PricingAssumptionOn,
			documentsConfig.MaxEstimatedCostUSDPerRun)
	} else {
		_, _ = fmt.Fprintln(w,
			"- No current provider-unit price assumption is configured; manual requests may still incur provider charges.")
	}
}

func runBuildDocuments(
	command *cobra.Command,
	capabilityPath string,
	limit int,
	mode documentBuildMode,
	confirmed bool,
	deps documentsCommandDeps,
) (runErr error) {
	if limit <= 0 || limit > 10_000 {
		return errors.New("document build limit must be between 1 and 10000")
	}
	documentsConfig, manifest, allowedMediaTypes, profile, err := configuredDocumentProfile(capabilityPath)
	if err != nil {
		return err
	}
	if !documentsConfig.Enabled {
		return errors.New("document build requires attachments.documents.enabled=true")
	}
	limit, err = documentsConfig.MaxDocumentsWithinRunBudget(limit)
	if err != nil {
		return err
	}
	if !confirmed {
		_, _ = fmt.Fprintln(command.OutOrStdout(), "Document build upload preflight:")
		printDocumentConsentDisclosure(
			command.OutOrStdout(), documentsConfig, profile, len(allowedMediaTypes),
		)
		return errors.New("document build requires --yes after reviewing the provider upload preflight")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.EnsureDocumentExtractionProfile(command.Context(), profile); err != nil {
		return err
	}
	consentStatus, err := st.GetDocumentIndexStatus(command.Context(), profile.ID)
	if err != nil {
		return err
	}
	if !consentStatus.ProfileEnabled || !consentStatus.ExactConsent {
		return errors.New("document build requires exact consent; run `msgvault documents consent-mistral --capabilities <manifest> --yes`")
	}
	if err := repairHistoricalAttachmentRoles(command.Context(), st); err != nil {
		return err
	}
	reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{
		AttachmentPageSize: 1000, ChangePageSize: 1000,
	})
	if err != nil {
		return err
	}
	reconcileResult, err := reconciler.Reconcile(command.Context())
	if err != nil {
		return err
	}
	status, err := st.GetDocumentIndexStatusForScope(
		command.Context(), profile.ID, "original", allowedMediaTypes, documentsConfig.Scope.MessageTypes,
	)
	if err != nil {
		return err
	}
	printDocumentBuildPreflight(
		command.OutOrStdout(), documentsConfig, profile, len(allowedMediaTypes), status, limit, mode,
	)
	attachments, closeAttachments, err := deps.openAttachments(st)
	if err != nil {
		return err
	}
	defer func() { runErr = errors.Join(runErr, closeAttachments()) }()
	processor, err := deps.newMistralProcessor(documentsConfig)
	if err != nil {
		return err
	}
	result, err := executeDocumentBuild(
		command.Context(), st,
		newOperationPassScope("cli:document-extraction", operations.TriggerManual),
		st, attachments, processor, documentsConfig, manifest,
		allowedMediaTypes, profile, limit, "documents-cli", cfg.Data.DataDir, mode, &reconcileResult,
	)
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Reconciled %d attachment(s), consumed %d change(s); indexed %d document(s), %d unit(s), skipped %d, failed %d.\n",
		result.Reconciled, result.Changes, result.Processed, result.Units, result.Skipped, result.Failed)
	if result.CleanupFailures > 0 {
		_, _ = fmt.Fprintf(command.ErrOrStderr(),
			"Warning: %d private document spool file(s) could not be removed; a later cleanup pass will retry.\n",
			result.CleanupFailures)
	}
	if result.RebuildID != "" {
		if result.Completed {
			_, _ = fmt.Fprintln(command.OutOrStdout(), "Full document rebuild completed.")
		} else {
			_, _ = fmt.Fprintf(command.OutOrStdout(),
				"Full document rebuild has %d current owner(s) remaining; run `msgvault documents resume --capabilities <manifest>` with the same capability manifest.\n",
				result.Remaining)
		}
	}
	return err
}

func printDocumentBuildPreflight(
	w io.Writer,
	documentsConfig *documentindex.DocumentsConfig,
	profile store.DocumentExtractionProfile,
	authenticatedFormats int,
	status store.DocumentIndexStatus,
	limit int,
	mode documentBuildMode,
) {
	var modeName string
	switch mode {
	case documentBuildIncremental:
		modeName = "incremental"
	case documentBuildStartRebuild:
		modeName = "full rebuild"
	case documentBuildResume:
		modeName = cmdUseResume
	}
	_, _ = fmt.Fprintln(w, "Document build upload preflight:")
	_, _ = fmt.Fprintf(w,
		"- The current scope contains %d eligible attachment occurrence(s), %d unique canonical document(s), and %s of original bytes.\n",
		status.EligibleOccurrences, status.EligibleOwners, formatSize(status.EligibleBytes))
	_, _ = fmt.Fprintf(w,
		"- This %s pass will process at most %d canonical document(s) and %d provider unit(s).\n",
		modeName, limit, documentsConfig.MaxPagesPerRun)
	printDocumentConsentDisclosure(w, documentsConfig, profile, authenticatedFormats)
}

func executeDocumentBuild(
	ctx context.Context,
	recorder operations.Recorder,
	scope operations.PassScope,
	st *store.Store,
	attachments documentindex.DocumentAttachmentOpener,
	processor documentindex.MistralProcessor,
	documentsConfig *documentindex.DocumentsConfig,
	manifest mistral.CapabilityManifest,
	allowedMediaTypes []string,
	profile store.DocumentExtractionProfile,
	limit int,
	leaseOwner string,
	dataDirectory string,
	mode documentBuildMode,
	preReconciled *documentindex.ReconcileResult,
) (result documentBuildResult, runErr error) {
	pass, terminal, err := beginCommandOperationPass(
		ctx, recorder, operations.KindDocumentExtraction, scope,
	)
	if err != nil {
		return result, err
	}
	if terminal != nil {
		return documentBuildResultFromOperationRun(terminal)
	}
	defer func() {
		pass.finish(ctx, documentExtractionCounters(result), runErr)
	}()
	var reconcileResult documentindex.ReconcileResult
	if preReconciled != nil {
		reconcileResult = *preReconciled
	} else {
		reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{
			AttachmentPageSize: 1000, ChangePageSize: 1000,
		})
		if err != nil {
			return result, err
		}
		reconcileResult, err = reconciler.Reconcile(ctx)
		if err != nil {
			return result, err
		}
	}
	result.Reconciled = reconcileResult.AttachmentsExamined
	result.Changes = reconcileResult.ChangesConsumed
	var rebuild *store.DocumentExtractionRebuild
	switch mode {
	case documentBuildStartRebuild:
		rebuildID, rebuildErr := newDocumentRebuildID()
		if rebuildErr != nil {
			return result, rebuildErr
		}
		started, rebuildErr := st.StartDocumentExtractionRebuild(
			ctx, rebuildID, profile.ID, "original", allowedMediaTypes, documentsConfig.Scope.MessageTypes,
		)
		if rebuildErr != nil {
			return result, rebuildErr
		}
		rebuild = &started
	case documentBuildResume:
		active, rebuildErr := st.GetActiveDocumentExtractionRebuild(ctx, profile.ID, "original")
		if rebuildErr == nil {
			rebuild = &active
		} else if !errors.Is(rebuildErr, store.ErrDocumentExtractionRebuildMissing) {
			return result, rebuildErr
		}
	case documentBuildIncremental:
	default:
		return result, errors.New("document build mode is invalid")
	}
	if dataDirectory == "" {
		return result, errors.New("document build requires a data directory")
	}
	spoolDirectory := filepath.Join(dataDirectory, "tmp", "document-index")
	if err := fileutil.SecureMkdirAll(spoolDirectory, 0o700); err != nil {
		return result, fmt.Errorf("create private document spool directory: %w", err)
	}
	if _, err := mistral.ScavengeSpoolDirectory(
		spoolDirectory, time.Now().UTC().Add(-2*time.Hour),
	); err != nil {
		return result, fmt.Errorf("scavenge Mistral document spool: %w", err)
	}
	policy, err := documentsConfig.MistralPolicy()
	if err != nil {
		return result, err
	}
	workerConfig := documentindex.MistralWorkerConfig{
		ProfileID: profile.ID, LeaseOwner: leaseOwner, LeaseDuration: documentsConfig.RequestTimeout + time.Minute,
		RetryDelay: 15 * time.Minute, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: documentsConfig.MaxSpoolBytes, MinFreeBytes: documentsConfig.MinFreeSpaceBytes,
		MessageTypes:     documentsConfig.Scope.MessageTypes,
		CapabilityPolicy: manifest, Policy: policy,
	}
	if rebuild != nil {
		workerConfig.RebuildID = rebuild.ID
		workerConfig.ReplaceCurrent = true
		result.RebuildID = rebuild.ID
	}
	worker, err := documentindex.NewMistralWorker(st, attachments, processor, workerConfig)
	if err != nil {
		return result, err
	}
	candidates, err := st.ListDocumentExtractionCandidates(
		ctx, profile.ID, "original", allowedMediaTypes, documentsConfig.Scope.MessageTypes, rebuild, limit,
	)
	if err != nil {
		return result, err
	}
	for _, candidate := range candidates {
		if len(documentsConfig.Scope.MessageTypes) > 0 &&
			!slices.Contains(documentsConfig.Scope.MessageTypes, candidate.MessageType) {
			result.Skipped++
			continue
		}
		extraction, processErr := worker.ProcessCandidate(ctx, candidate)
		if processErr != nil && ctx.Err() != nil {
			return result, ctx.Err()
		}
		if errors.Is(processErr, store.ErrDocumentExtractionClaimed) ||
			errors.Is(processErr, store.ErrDocumentExtractionCurrent) {
			result.Skipped++
			continue
		}
		if processErr != nil {
			result.Failed++
			result.Failures = append(result.Failures, documentBuildFailure{
				CanonicalBlobHash: extraction.CanonicalBlobHash,
				ReasonCode:        extraction.FailureReasonCode,
			})
			pass.checkpoint(ctx, documentExtractionCounters(result))
			continue
		}
		result.Processed++
		result.Units += extraction.Units
		if extraction.CleanupError != nil {
			result.CleanupFailures++
		}
		pass.checkpoint(ctx, documentExtractionCounters(result))
		if result.Units > documentsConfig.MaxPagesPerRun {
			return result, errors.New("document provider output exceeded max_pages_per_run")
		}
	}
	if rebuild != nil {
		result.Remaining, err = st.CountIncompleteDocumentExtractionRebuild(
			ctx, *rebuild, allowedMediaTypes, documentsConfig.Scope.MessageTypes,
		)
		if err != nil {
			return result, err
		}
		if result.Remaining == 0 && result.Failed == 0 {
			if err := st.CompleteDocumentExtractionRebuild(ctx, rebuild.ID); err != nil {
				return result, err
			}
			result.Completed = true
		}
	}
	if result.Failed > 0 {
		var details strings.Builder
		for _, failure := range result.Failures {
			fmt.Fprintf(&details, "\n%s: %s", failure.CanonicalBlobHash, failure.ReasonCode)
		}
		return result, fmt.Errorf(
			"document build completed with %d extraction failure(s):%s\nretry one with `msgvault documents retry --capabilities <manifest> --hash <sha256>`",
			result.Failed, details.String(),
		)
	}
	return result, nil
}

func documentExtractionCounters(result documentBuildResult) operations.InvocationCounters {
	succeeded := int64(result.Processed)
	failed := int64(result.Failed)
	return operations.InvocationCounters{
		Attempted: succeeded + failed, Succeeded: succeeded, Failed: failed,
	}
}

func documentBuildResultFromOperationRun(run *operations.Run) (documentBuildResult, error) {
	if run == nil {
		return documentBuildResult{}, errors.New("document extraction operation outcome is required")
	}
	counters, err := operations.InvocationCountersFromPublic(run.ID.Kind(), run.Counters)
	if err != nil {
		return documentBuildResult{}, err
	}
	return documentBuildResult{
		Processed: int(counters.Succeeded), Failed: int(counters.Failed),
	}, operations.TerminalReplayOutcome(run)
}

func newDocumentRebuildID() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("create document rebuild ID: %w", err)
	}
	return "document-rebuild-" + hex.EncodeToString(entropy[:]), nil
}

func runDocumentStatus(
	command *cobra.Command,
	capabilityPath string,
	jsonOutput bool,
	deps documentsCommandDeps,
) error {
	documentsConfig, _, allowedMediaTypes, profile, err := configuredDocumentProfile(capabilityPath)
	if err != nil {
		return err
	}
	response, cleanup, err := readDocumentStatus(command.Context(), store.DocumentIndexStatusRequest{
		ProfileID: profile.ID, ExtractionInputKey: "original",
		AllowedMediaTypes: allowedMediaTypes, AllowedMessageTypes: documentsConfig.Scope.MessageTypes,
	}, deps)
	if err != nil {
		return err
	}
	defer cleanup()
	status := response.Status
	var rebuildStatus *documentRebuildStatus
	if response.ActiveRebuild != nil {
		rebuildStatus = &documentRebuildStatus{
			SnapshotOwners:  response.ActiveRebuild.SnapshotOwners,
			RemainingOwners: response.ActiveRebuild.RemainingOwners,
		}
	}
	storesPlaintext := status.StoredPlaintextChunks > 0
	var estimatedSuccessfulCost *float64
	if documentsConfig.EstimatedCostUSDPerKUnits > 0 {
		cost := float64(status.ProcessedProviderUnits) / 1000 * documentsConfig.EstimatedCostUSDPerKUnits
		estimatedSuccessfulCost = &cost
	}
	output := documentStatusOutput{
		ProfileID: profile.ID, AuthenticatedFormats: len(allowedMediaTypes),
		Provider: profile.Provider, Endpoint: profile.Endpoint, Region: profile.Region, Model: profile.Model,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
		StoresPlaintext: storesPlaintext, BackupsMayContainText: storesPlaintext,
		HostedTextEmbeddings:       documentsConfig.Index.Embeddings.Enabled,
		PricingAssumptionOn:        documentsConfig.PricingAssumptionOn,
		EstimatedSuccessfulCostUSD: estimatedSuccessfulCost,
		SpoolQuotaBytes:            documentsConfig.MaxSpoolBytes, MinFreeSpaceBytes: documentsConfig.MinFreeSpaceBytes,
		ActiveRebuild: rebuildStatus, Status: status,
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(output)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Profile: %s\nProvider: %s %s (%s, %s)\nFormats: %d authenticated\nRetention: %s\nTraining: %s\nPrivate spool: %s quota, %s free-space reserve\nEnabled: %t\nExact consent: %t\nEligible: %d occurrence(s), %d unique document(s), %s\nExcluded roles: %d unknown, %d ineligible\nCoverage: %d ready, %d staging, %d retrying, %d terminal, %d missing\nExtraction accounting: %d attempt(s), %d successful, %d failed, %s verified upload bytes\nProvider accounting: %d request(s), %d internal retry(s), %d ms total latency (%.1f ms average), %d processed unit(s), %s reported bytes, %d successful response(s) without provider bytes\nNormalized plaintext stored: %t\nBackups may contain normalized plaintext: %t\nHosted document text embeddings: %t\n",
		profile.ID, profile.Provider, profile.Model, profile.Region, profile.Endpoint,
		len(allowedMediaTypes), profile.RetentionPosture, profile.TrainingPosture,
		formatSize(documentsConfig.MaxSpoolBytes), formatSize(documentsConfig.MinFreeSpaceBytes),
		status.ProfileEnabled, status.ExactConsent, status.EligibleOccurrences,
		status.EligibleOwners, formatSize(status.EligibleBytes), status.UnknownRoleOccurrences,
		status.IneligibleRoleOccurrences, status.ReadyOwners, status.StagingOwners,
		status.RetryOwners, status.TerminalOwners, status.MissingOwners,
		status.ExtractionAttempts, status.SuccessfulAttempts, status.FailedAttempts,
		formatSize(status.VerifiedUploadBytes), status.ProviderRequests, status.ProviderRetries,
		status.ProviderLatencyMillis, status.AverageProviderLatencyMS, status.ProcessedProviderUnits,
		formatSize(status.ReportedProviderBytes), status.MissingProviderByteReports,
		output.StoresPlaintext, output.BackupsMayContainText, output.HostedTextEmbeddings)
	if estimatedSuccessfulCost != nil {
		_, _ = fmt.Fprintf(command.OutOrStdout(),
			"Minimum estimated successful cost: %.6g USD (pricing assumption %s; excludes failed and provider-internal retry charges)\n",
			*estimatedSuccessfulCost, documentsConfig.PricingAssumptionOn)
	}
	if rebuildStatus != nil {
		_, _ = fmt.Fprintf(command.OutOrStdout(), "Active full rebuild: %d of %d owner(s) remaining\n",
			rebuildStatus.RemainingOwners, rebuildStatus.SnapshotOwners)
	}
	return nil
}

func runRetryDocument(
	command *cobra.Command,
	capabilityPath string,
	canonicalBlobHash string,
	deps documentsCommandDeps,
) error {
	profile, err := configuredDocumentProfileOnly(capabilityPath)
	if err != nil {
		return err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	changed, err := st.RetryDocumentExtraction(command.Context(), profile.ID, canonicalBlobHash)
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("no terminal or retryable document extraction matched the exact profile and hash")
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Scheduled document %s for retry under profile %s.\n",
		canonicalBlobHash, profile.ID)
	return nil
}

func configuredDocumentProfileOnly(capabilityPath string) (store.DocumentExtractionProfile, error) {
	documentsConfig, manifest, allowedMediaTypes, profile, err := configuredDocumentProfile(capabilityPath)
	_ = documentsConfig
	_ = manifest
	_ = allowedMediaTypes
	return profile, err
}

func runRetireDocumentProfile(
	command *cobra.Command,
	profileID string,
	confirmed bool,
	deps documentsCommandDeps,
) error {
	if !confirmed {
		return errors.New("document profile retirement requires --yes")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	changed, err := st.RetireDocumentExtractionProfile(command.Context(), profileID)
	if err != nil {
		return err
	}
	if !changed {
		return errors.New("document extraction profile was not found or is already retired")
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Retired document extraction profile %s.\n", profileID)
	return nil
}

func runPurgeDocumentDerived(
	command *cobra.Command,
	canonicalBlobHash string,
	confirmed bool,
	deps documentsCommandDeps,
) error {
	if !confirmed {
		return errors.New("document derivative purge requires --yes")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	result, err := st.PurgeDocumentDerivedByHash(command.Context(), canonicalBlobHash)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Purged %d extraction(s) and %d current head(s) for document %s.\n",
		result.ExtractionsRemoved, result.HeadsRemoved, canonicalBlobHash)
	return nil
}

func runSearchDocuments(
	command *cobra.Command,
	request store.DocumentSearchRequest,
	jsonOutput bool,
	deps documentsCommandDeps,
) error {
	reader, cleanup, err := openDocumentReadClient(command.Context(), deps)
	if err != nil {
		return err
	}
	defer cleanup()
	response, err := reader.SearchDocuments(command.Context(), request)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(response)
	}
	writer := tabwriter.NewWriter(command.OutOrStdout(), 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(writer, "RANK\tATTACHMENT\tMESSAGE\tFILE\tMATCH\tEXCERPT")
	for _, result := range response.Results {
		_, _ = fmt.Fprintf(writer, "%d\t%d\t%d\t%s\t%s\t%s\n",
			result.Rank, result.AttachmentID, result.MessageID, result.Filename,
			strings.Join(result.MatchedSignals, "+"), strings.Join(strings.Fields(result.Excerpt), " "))
	}
	if err := writer.Flush(); err != nil {
		return fmt.Errorf("write document search results: %w", err)
	}
	if response.NextCursor != "" {
		_, _ = fmt.Fprintf(command.OutOrStdout(), "Next cursor: %s\n", response.NextCursor)
	}
	return nil
}

func openDocumentReadClient(
	ctx context.Context,
	deps documentsCommandDeps,
) (documentReadClient, func(), error) {
	if deps.openReadClient != nil {
		return deps.openReadClient(ctx)
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return nil, func() {}, err
	}
	return localDocumentReadClient{store: st}, cleanup, nil
}

func readDocumentStatus(
	ctx context.Context,
	request store.DocumentIndexStatusRequest,
	deps documentsCommandDeps,
) (store.DocumentIndexStatusResponse, func(), error) {
	reader, cleanup, err := openDocumentReadClient(ctx, deps)
	if err != nil {
		return store.DocumentIndexStatusResponse{}, func() {}, err
	}
	response, err := reader.GetDocumentIndexStatus(ctx, request)
	if err != nil {
		cleanup()
		return store.DocumentIndexStatusResponse{}, func() {}, err
	}
	return response, cleanup, nil
}

type localDocumentReadClient struct{ store *store.Store }

func (c localDocumentReadClient) SearchDocuments(
	ctx context.Context,
	request store.DocumentSearchRequest,
) (store.DocumentSearchResponse, error) {
	if request.PersonID > 0 || request.ParticipantID > 0 {
		reference := personresolver.Reference{Kind: personresolver.ReferencePerson, ID: request.PersonID}
		if request.ParticipantID > 0 {
			reference = personresolver.Reference{Kind: personresolver.ReferenceParticipant, ID: request.ParticipantID}
		}
		resolved, err := personresolver.Resolve(ctx, c.store, reference, request.Directions)
		if err != nil {
			if errors.Is(err, personresolver.ErrEmptyPopulation) {
				return store.DocumentSearchResponse{}, fmt.Errorf(
					"person %d has no linked identities; link participants first: %w",
					reference.ID, err,
				)
			}
			return store.DocumentSearchResponse{}, err
		}
		request.Person = &resolved.Scope
	}
	mode, err := vectordocument.ParseSearchMode(request.SearchMode)
	if err != nil {
		return store.DocumentSearchResponse{}, fmt.Errorf("%w: %w", store.ErrDocumentSearchInvalidRequest, err)
	}
	if mode == vectordocument.SearchModeSemantic || mode == vectordocument.SearchModeHybrid {
		return store.DocumentSearchResponse{}, vectordocument.ErrSemanticSearchUnavailable
	}
	if err := reconcileDocumentOccurrencesForSearch(ctx, c.store); err != nil {
		return store.DocumentSearchResponse{}, err
	}
	return c.store.SearchDocuments(ctx, request)
}

func (c localDocumentReadClient) GetDocumentIndexStatus(
	ctx context.Context,
	request store.DocumentIndexStatusRequest,
) (store.DocumentIndexStatusResponse, error) {
	if err := reconcileDocumentOccurrencesForSearch(ctx, c.store); err != nil {
		return store.DocumentIndexStatusResponse{}, err
	}
	status, err := c.store.GetDocumentIndexStatusForScope(
		ctx, request.ProfileID, request.ExtractionInputKey,
		request.AllowedMediaTypes, request.AllowedMessageTypes,
	)
	if err != nil {
		return store.DocumentIndexStatusResponse{}, err
	}
	response := store.DocumentIndexStatusResponse{Status: status}
	active, err := c.store.GetActiveDocumentExtractionRebuild(
		ctx, request.ProfileID, request.ExtractionInputKey,
	)
	if errors.Is(err, store.ErrDocumentExtractionRebuildMissing) {
		return response, nil
	}
	if err != nil {
		return store.DocumentIndexStatusResponse{}, err
	}
	remaining, err := c.store.CountIncompleteDocumentExtractionRebuild(
		ctx, active, request.AllowedMediaTypes, request.AllowedMessageTypes,
	)
	if err != nil {
		return store.DocumentIndexStatusResponse{}, err
	}
	response.ActiveRebuild = &store.DocumentIndexRebuildStatus{
		SnapshotOwners: active.SnapshotOwners, RemainingOwners: remaining,
	}
	return response, nil
}

func reconcileDocumentOccurrencesForSearch(ctx context.Context, st *store.Store) error {
	if _, err := st.GetAttachmentChangeConsumer(
		ctx, documentindex.DocumentAttachmentConsumerKey,
	); errors.Is(err, store.ErrAttachmentChangeConsumerMissing) {
		return bootstrapDocumentOccurrencesIfConsented(ctx, st)
	} else if err != nil {
		return err
	}
	reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{
		AttachmentPageSize: 1000,
		ChangePageSize:     1000,
	})
	if err != nil {
		return err
	}
	_, err = reconciler.Reconcile(ctx)
	return err
}

func bootstrapDocumentOccurrencesIfConsented(ctx context.Context, st *store.Store) error {
	consented, err := st.HasActiveDocumentProviderConsent(ctx)
	if err != nil || !consented {
		return err
	}
	reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{
		AttachmentPageSize: 1000,
		ChangePageSize:     1000,
	})
	if err != nil {
		return err
	}
	_, err = reconciler.Reconcile(ctx)
	return err
}

func configuredDocumentProfile(
	capabilityPath string,
) (*documentindex.DocumentsConfig, mistral.CapabilityManifest, []string, store.DocumentExtractionProfile, error) {
	if cfg == nil {
		return nil, mistral.CapabilityManifest{}, nil, store.DocumentExtractionProfile{},
			errors.New("document operation requires loaded configuration")
	}
	documentsConfig := &cfg.Attachments.Documents
	if documentsConfig.RetentionPosture == documentindex.RetentionUnknown ||
		documentsConfig.TrainingPosture == documentindex.TrainingUnknown {
		return nil, mistral.CapabilityManifest{}, nil, store.DocumentExtractionProfile{},
			errors.New("document operation requires explicit retention_posture and training_posture")
	}
	manifest, err := loadDocumentCapabilityManifest(capabilityPath)
	if err != nil {
		return nil, mistral.CapabilityManifest{}, nil, store.DocumentExtractionProfile{}, err
	}
	allowedMediaTypes, profile, err := documentProfileForConfig(documentsConfig, manifest)
	if err != nil {
		return nil, mistral.CapabilityManifest{}, nil, store.DocumentExtractionProfile{}, err
	}
	return documentsConfig, manifest, allowedMediaTypes, profile, nil
}

func loadDocumentCapabilityManifest(capabilityPath string) (mistral.CapabilityManifest, error) {
	file, err := os.Open(capabilityPath)
	if err != nil {
		return mistral.CapabilityManifest{}, errors.New("open configured Mistral capability manifest")
	}
	manifest, decodeErr := mistral.DecodeCapabilityManifest(file)
	closeErr := file.Close()
	if decodeErr != nil || closeErr != nil {
		return mistral.CapabilityManifest{}, errors.Join(decodeErr, closeErr)
	}
	return manifest, nil
}

func documentProfileForConfig(
	documentsConfig *documentindex.DocumentsConfig,
	manifest mistral.CapabilityManifest,
) ([]string, store.DocumentExtractionProfile, error) {
	policy, err := documentsConfig.MistralPolicy()
	if err != nil {
		return nil, store.DocumentExtractionProfile{}, err
	}
	allowedMediaTypes := make([]string, 0, len(mistral.CandidateFormats()))
	for _, format := range mistral.CandidateFormats() {
		if _, authorizeErr := policy.Authorize(manifest, format.ID); authorizeErr == nil {
			allowedMediaTypes = append(allowedMediaTypes, format.MediaType)
		}
	}
	if len(allowedMediaTypes) == 0 {
		return nil, store.DocumentExtractionProfile{}, errors.New(
			"no format has authorized upload authority; run the authenticated capability probe and supply its manifest",
		)
	}
	fingerprint, err := documentsConfig.ProfileFingerprint(manifest, allowedMediaTypes)
	if err != nil {
		return nil, store.DocumentExtractionProfile{}, err
	}
	policyJSON, err := documentsConfig.ProfilePolicyJSON(manifest, allowedMediaTypes)
	if err != nil {
		return nil, store.DocumentExtractionProfile{}, err
	}
	values := policy.Values()
	profile := store.DocumentExtractionProfile{
		ID: "documents-v1:" + fingerprint, Fingerprint: fingerprint,
		Provider: values.Provider, Endpoint: values.Endpoint, Region: values.Region,
		Model: values.Model, RetentionPosture: values.Retention,
		TrainingPosture:   values.Training,
		AllowedMediaTypes: allowedMediaTypes, PolicyJSON: policyJSON,
	}
	return allowedMediaTypes, profile, nil
}

func openDocumentAttachments(
	st *store.Store,
) (documentindex.DocumentAttachmentOpener, func() error, error) {
	attachments, err := attachmentstore.New(store.NewPackCatalog(st), cfg.AttachmentsDir())
	if err != nil {
		return nil, nil, err
	}
	return attachments, attachments.Close, nil
}

func newConfiguredMistralClient(
	documentsConfig *documentindex.DocumentsConfig,
) (*mistral.Client, error) {
	apiKey, err := documentsConfig.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	policy, err := documentsConfig.MistralPolicy()
	if err != nil {
		return nil, err
	}
	client, err := mistral.NewClient(policy, mistral.ClientConfig{
		APIKey: apiKey, Timeout: documentsConfig.RequestTimeout,
		MaxRetries: documentsConfig.MaxRetries,
	})
	if err != nil {
		return nil, fmt.Errorf("configure mistral document probe: %w", err)
	}
	return client, nil
}

func newConfiguredMistralProcessor(
	documentsConfig *documentindex.DocumentsConfig,
) (documentindex.MistralProcessor, error) {
	return newConfiguredMistralClient(documentsConfig)
}

func init() {
	rootCmd.AddCommand(newDocumentsCmd(defaultDocumentsCommandDeps()))
}
