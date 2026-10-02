package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/fileutil"
	"go.kenn.io/msgvault/internal/store"
)

func requireDocumentProvider(state *invocation, provider string) error {
	if state == nil || state.cfg == nil {
		return errors.New("document operation requires loaded configuration")
	}
	if state.cfg.Attachments.Documents.Provider != provider {
		return fmt.Errorf("document command requires attachments.documents.provider=%s", provider)
	}
	return nil
}

func newConsentDoclingCmd(deps documentsCommandDeps) *cobra.Command {
	var confirmed bool
	command := &cobra.Command{
		Use: "consent-docling", Short: "Record consent for the exact self-hosted Docling document policy", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !isDaemonCLISubprocess() {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runConsentDocling(command, confirmed, deps)
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm uploads to the configured operator-controlled endpoint")
	return command
}

// Docling commands carry no local capability manifest. Mistral commands still
// require the manifest to be readable on the daemon host.
func runDocumentMutationViaDaemon(command *cobra.Command, args []string, env map[string]string) error {
	state := invocationFromCommand(command)
	if state != nil && state.cfg != nil && state.cfg.Attachments.Documents.Provider == documentindex.ProviderDocling {
		return runDaemonCLICommandHTTPFromCobraWithEnv(command, args, env)
	}
	return runDaemonCLICommandHTTPFromCobraWithLocalFiles(command, args, env)
}

func runConsentDocling(command *cobra.Command, confirmed bool, deps documentsCommandDeps) error {
	if err := requireDocumentProvider(invocationFromCommand(command), documentindex.ProviderDocling); err != nil {
		return err
	}
	c, _, input, profile, err := configuredDocumentProfile("", invocationFromCommand(command))
	if err != nil {
		return err
	}
	if !c.Enabled {
		return errors.New("document consent requires attachments.documents.enabled=true")
	}
	printDocumentConsentDisclosure(command.OutOrStdout(), c, profile, input)
	if !confirmed {
		return errors.New("document consent requires --yes after reviewing the operator-controlled upload policy")
	}
	st, cleanup, err := deps.openStore(command.Context())
	if err != nil {
		return err
	}
	defer cleanup()
	if err := recordDocumentConsent(command.Context(), st, profile); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Recorded Docling document consent for profile %s (%d configured format(s), endpoint=%s).\n", profile.ID, len(input.AllowedMediaTypes), profile.Endpoint)
	return nil
}

func recordDocumentConsent(ctx context.Context, st *store.Store, profile store.DocumentExtractionProfile) error {
	if _, err := st.EnsureDocumentExtractionProfile(ctx, profile); err != nil {
		return err
	}
	if err := st.RecordDocumentProviderConsent(ctx, store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}); err != nil {
		return err
	}
	if !profile.IncludeInline {
		return bootstrapDocumentOccurrencesIfConsented(ctx, st)
	}
	// Every confirmed inline consent repeats the full scan. A previous bootstrap
	// or an interrupted consent scan may have omitted historical inline rows.
	reconciler, err := documentindex.NewReconciler(st, documentindex.ReconcilerConfig{AttachmentPageSize: 1000, ChangePageSize: 1000})
	if err != nil {
		return err
	}
	_, err = reconciler.FullReconcile(ctx)
	return err
}

func printDoclingConsentDisclosure(w io.Writer, c *documentindex.DocumentsConfig, profile store.DocumentExtractionProfile, input documentindex.ResolvedInputPolicy) {
	_, _ = fmt.Fprintln(w, "Self-hosted Docling extraction disclosure:")
	roles := "standalone document attachments"
	if profile.IncludeInline {
		roles = "standalone and inline document attachments"
	}
	_, _ = fmt.Fprintf(w, "- Scope includes %s in the configured formats and message sources.\n", roles)
	_, _ = fmt.Fprintf(w, "- Original private document bytes and media types are sent only to %s. Original filenames are withheld.\n", profile.Endpoint)
	if c.APIKeyEnv == "" {
		_, _ = fmt.Fprintln(w, "- No API key is configured for this endpoint.")
	} else {
		_, _ = fmt.Fprintf(w, "- The credential from environment variable %s is sent to this endpoint as X-Api-Key.\n", c.APIKeyEnv)
	}
	_, _ = fmt.Fprintln(w, "- This operator-controlled service is your responsibility: deployment, credentials, model downloads, network egress, resource limits, retention and training behavior.")
	for _, mediaType := range input.AllowedMediaTypes {
		_, _ = fmt.Fprintf(w, "  - original %s bytes are sent.\n", mediaType)
	}
	_, _ = fmt.Fprintf(w, "- The exact policy allows %d format(s), at most %s and %d returned unit(s) per document; requests time out after %s, jobs after %s.\n", len(input.AllowedMediaTypes), formatSize(c.MaxFileBytes), c.MaxPagesPerDocument, c.RequestTimeout, c.TotalTimeout)
	_, _ = fmt.Fprintln(w, "- Source preparation uses bounded memory. Docling processing does not use the Mistral private disk spool.")
	_, _ = fmt.Fprintln(w, "- Complete PDF page evidence retains page locators. Incomplete structured mappings use whole-document Markdown without page claims to preserve tables and text.")
	_, _ = fmt.Fprintln(w, "- Partial results and ambiguous or interrupted jobs require manual retry; resume does not automatically upload them again.")
	_, _ = fmt.Fprintln(w, "- Normalized plaintext units and chunks are stored in the local archive database and may be included in disclosed full backups.")
	_, _ = fmt.Fprintln(w, "- Raw provider JSON and full provider Markdown are transient. This consent does not enable document text embeddings; their provider and consents are separate.")
}

func documentConsentCommand(provider string) string {
	if provider == documentindex.ProviderDocling {
		return "msgvault documents consent-docling --yes"
	}
	return "msgvault documents consent-mistral --capabilities <manifest> --yes"
}

func documentResumeCommand(provider string) string {
	if provider == documentindex.ProviderDocling {
		return "msgvault documents resume"
	}
	return "msgvault documents resume --capabilities <manifest>"
}

func documentRetryCommand(provider string) string {
	if provider == documentindex.ProviderDocling {
		return "msgvault documents retry"
	}
	return "msgvault documents retry --capabilities <manifest>"
}

type documentCandidateProcessor interface {
	ProcessCandidate(ctx context.Context, candidate store.DocumentExtractionCandidate) (documentindex.DocumentExtractionResult, error)
}

func newDocumentBuildWorker(st *store.Store, attachments documentindex.DocumentAttachmentOpener, processor documentindex.MistralProcessor,
	c *documentindex.DocumentsConfig, manifest mistral.CapabilityManifest, profileID, leaseOwner, dataDirectory string,
	rebuild *store.DocumentExtractionRebuild) (documentCandidateProcessor, error) {
	if c.Provider == documentindex.ProviderDocling {
		if c.APIKeyEnv != "" {
			if _, err := c.ResolveAPIKey(); err != nil {
				return nil, err
			}
		}
		provider, err := documentindex.NewDoclingClient(c)
		if err != nil {
			return nil, err
		}
		config := documentindex.DoclingWorkerConfig{Documents: *c, ProfileID: profileID, LeaseOwner: leaseOwner,
			LeaseDuration: min(c.TotalTimeout+time.Minute, time.Hour), RetryDelay: 15 * time.Minute}
		if rebuild != nil {
			config.RebuildID, config.ReplaceCurrent = rebuild.ID, true
		}
		return documentindex.NewDoclingWorker(st, attachments, provider, config)
	}
	if dataDirectory == "" {
		return nil, errors.New("document build requires a data directory")
	}
	spoolDirectory := filepath.Join(dataDirectory, "tmp", "document-index")
	if err := fileutil.SecureMkdirAll(spoolDirectory, 0o700); err != nil {
		return nil, fmt.Errorf("create private document spool directory: %w", err)
	}
	if _, err := mistral.ScavengeSpoolDirectory(spoolDirectory, time.Now().UTC().Add(-2*time.Hour)); err != nil {
		return nil, fmt.Errorf("scavenge Mistral document spool: %w", err)
	}
	policy, err := c.MistralPolicy()
	if err != nil {
		return nil, err
	}
	input, err := documentindex.ResolveInputPolicy(c, manifest)
	if err != nil {
		return nil, err
	}
	config := documentindex.MistralWorkerConfig{ProfileID: profileID, LeaseOwner: leaseOwner,
		LeaseDuration: c.RequestTimeout + time.Minute, RetryDelay: 15 * time.Minute, SpoolDirectory: spoolDirectory,
		MaxSpoolBytes: c.MaxSpoolBytes, MinFreeBytes: c.MinFreeSpaceBytes,
		MessageTypes: c.Scope.MessageTypes, CapabilityPolicy: manifest, Policy: policy, InputPolicy: input}
	if rebuild != nil {
		config.RebuildID, config.ReplaceCurrent = rebuild.ID, true
	}
	return documentindex.NewMistralWorker(st, attachments, processor, config)
}
