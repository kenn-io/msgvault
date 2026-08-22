package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/store"
	vectordocument "go.kenn.io/msgvault/internal/vector/document"
)

const (
	defaultDocumentVectorOperationLimit = 100
	documentVectorsSubcommand           = "vectors"
)

func newDocumentVectorsCmd(deps documentsCommandDeps) *cobra.Command {
	command := &cobra.Command{Use: documentVectorsSubcommand, Short: "Manage document attachment vectors"}
	command.AddCommand(
		newDocumentVectorConsentCmd(deps),
		newDocumentVectorBuildCmd(deps, false),
		newDocumentVectorBuildCmd(deps, true),
		newDocumentVectorRetryCmd(deps),
		newDocumentVectorRebuildCmd(deps),
		newDocumentVectorRetireCmd(deps),
		newDocumentVectorStatusCmd(deps),
	)
	return command
}

func desiredDocumentVectorSpec(ctx context.Context, st *store.Store) (store.DocumentVectorGenerationSpec, error) {
	if cfg == nil || !cfg.Vector.Enabled || !cfg.Attachments.Documents.Index.Embeddings.Enabled {
		return store.DocumentVectorGenerationSpec{}, errors.New("document embeddings are disabled; enable [vector] and [attachments.documents.index.embeddings]")
	}
	return configuredDocumentVectorSpec(ctx, st)
}

func configuredDocumentVectorSpec(ctx context.Context, st *store.Store) (store.DocumentVectorGenerationSpec, error) {
	if cfg == nil || !cfg.Attachments.Documents.Index.Embeddings.Enabled {
		return store.DocumentVectorGenerationSpec{}, errors.New("document embeddings are not configured")
	}
	if err := cfg.Vector.Validate(); err != nil {
		return store.DocumentVectorGenerationSpec{}, fmt.Errorf("vector config: %w", err)
	}
	target, err := st.GetDocumentVectorTargetProfileID(ctx)
	if err != nil {
		return store.DocumentVectorGenerationSpec{}, err
	}
	return store.DocumentVectorGenerationSpec{
		Fingerprint:               vectordocument.Fingerprint(target, cfg.Vector),
		TargetExtractionProfileID: target,
		EmbeddingProfile:          cfg.Attachments.Documents.Index.Embeddings.Profile,
		Model:                     cfg.Vector.Embeddings.Model,
		Dimension:                 cfg.Vector.Embeddings.Dimension,
	}, nil
}

func configuredDocumentVectorConsentSpec(spec store.DocumentVectorGenerationSpec) (store.DocumentVectorConsentSpec, error) {
	egressFingerprint, err := vectordocument.EgressFingerprint(spec.TargetExtractionProfileID, cfg.Vector)
	if err != nil {
		return store.DocumentVectorConsentSpec{}, err
	}
	return store.DocumentVectorConsentSpec{
		DocumentVectorGenerationSpec: spec,
		EgressFingerprint:            egressFingerprint,
	}, nil
}

func withDocumentVectorStore(deps documentsCommandDeps, fn func(*store.Store) error) error {
	if deps.openStore == nil {
		return errors.New("document vector ledger is unavailable")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	return fn(st)
}

func newDocumentVectorConsentCmd(deps documentsCommandDeps) *cobra.Command {
	var yes bool
	command := &cobra.Command{
		Use:   cmdUseConsent,
		Short: "Consent to hosted embedding for the configured document policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				spec, err := configuredDocumentVectorSpec(command.Context(), st)
				if err != nil {
					return err
				}
				consentSpec, err := configuredDocumentVectorConsentSpec(spec)
				if err != nil {
					return err
				}
				printDocumentVectorConsentDisclosure(command.OutOrStdout(), consentSpec)
				if !yes {
					return errors.New("hosted document embedding consent requires --yes after reviewing the provider disclosure")
				}
				consent, _, err := st.RecordDocumentVectorConsent(command.Context(), consentSpec, time.Now())
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(command.OutOrStdout(), "Recorded consent for document vector egress fingerprint %s. Restart the daemon to enable scheduled document vector work.\n", consent.EgressFingerprint)
				return nil
			})
		},
	}
	command.Flags().BoolVar(&yes, "yes", false, "Confirm hosted document embedding consent")
	return command
}

func printDocumentVectorConsentDisclosure(w io.Writer, spec store.DocumentVectorConsentSpec) {
	authentication := "no authentication environment variable configured"
	if cfg.Vector.Embeddings.APIKeyEnv != "" {
		authentication = "environment variable " + cfg.Vector.Embeddings.APIKeyEnv
	}
	_, _ = fmt.Fprintln(w, "Hosted document embedding disclosure:")
	_, _ = fmt.Fprintf(w, "Corpus fingerprint: %s\n", spec.Fingerprint)
	_, _ = fmt.Fprintf(w, "Egress fingerprint: %s\n", spec.EgressFingerprint)
	_, _ = fmt.Fprintf(w, "Destination: %s\n", documentVectorConsentEndpoint(cfg.Vector.Embeddings.Endpoint))
	_, _ = fmt.Fprintf(w, "Authentication: %s\n", authentication)
	_, _ = fmt.Fprintf(w, "API format: %s\n", cfg.Vector.Embeddings.EffectiveAPIFormat())
	_, _ = fmt.Fprintf(w, "Model: %s\n", spec.Model)
	_, _ = fmt.Fprintf(w, "Dimension: %d\n", spec.Dimension)
	_, _ = fmt.Fprintf(w, "Maximum input: %d characters\n", cfg.Vector.Embeddings.MaxInputChars)
	_, _ = fmt.Fprintln(w, "Normalized attachment document chunk text will be sent to the configured destination for embedding.")
}

func documentVectorConsentEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return "[configured endpoint omitted: invalid URL]"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return parsed.String()
}

func newDocumentVectorBuildCmd(deps documentsCommandDeps, resume bool) *cobra.Command {
	name, short := documentBuildSubcommand, "Build the configured document vector generation"
	if resume {
		name, short = cmdUseResume, "Resume a building document vector generation"
	}
	var generationID int64
	var limit int
	command := &cobra.Command{
		Use: name, Short: short, Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				spec, err := desiredDocumentVectorSpec(command.Context(), st)
				if err != nil {
					return err
				}
				if err := requireDocumentVectorConsent(command.Context(), st, spec); err != nil {
					return err
				}
				if resume {
					generation, err := st.GetDocumentVectorGeneration(command.Context(), generationID)
					if err != nil {
						return err
					}
					if generation.State != store.DocumentVectorGenerationBuilding || generation.DocumentVectorGenerationSpec != spec {
						return store.ErrDocumentVectorInvalidGenerationState
					}
				} else {
					generation, _, err := st.EnsureDocumentVectorGeneration(command.Context(), spec)
					if err != nil {
						return err
					}
					if generation.State != store.DocumentVectorGenerationBuilding {
						return errors.New("the configured generation is already active; use documents vectors rebuild for coverage drift")
					}
					generationID = generation.ID
				}
				return runDocumentVectorCommand(command, deps, st, generationID, limit)
			})
		},
	}
	command.Flags().IntVarP(&limit, "limit", "n", defaultDocumentVectorOperationLimit, "Maximum chunks and cleanup tokens to process (1-1000)")
	if resume {
		command.Flags().Int64Var(&generationID, "generation-id", 0, "Building generation to resume")
		_ = command.MarkFlagRequired("generation-id")
	}
	return command
}

func requireDocumentVectorConsent(ctx context.Context, st *store.Store, spec store.DocumentVectorGenerationSpec) error {
	consentSpec, err := configuredDocumentVectorConsentSpec(spec)
	if err != nil {
		return err
	}
	consent, err := st.GetDocumentVectorConsent(ctx, consentSpec.EgressFingerprint)
	if err != nil {
		return err
	}
	if consent == nil || consent.DocumentVectorConsentSpec != consentSpec {
		return errors.New("exact document vector policy is not consented; run `msgvault documents vectors consent --yes`")
	}
	return nil
}

func runDocumentVectorCommand(command *cobra.Command, deps documentsCommandDeps, st *store.Store, generationID int64, limit int) error {
	if limit < 1 || limit > 1000 {
		return errors.New("document vector operation limit must be between 1 and 1000")
	}
	if deps.runDocumentVector == nil {
		return errors.New("document vector backend is unavailable in this binary")
	}
	result, err := deps.runDocumentVector(command.Context(), st, generationID, limit)
	if encodeErr := json.NewEncoder(command.OutOrStdout()).Encode(result); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
}

func newDocumentVectorRetryCmd(deps documentsCommandDeps) *cobra.Command {
	var generationID int64
	var afterToken string
	var limit int
	command := &cobra.Command{Use: "retry", Short: "Reset current failed publications for retry", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				result, err := st.ResetDocumentVectorFailures(command.Context(), generationID, afterToken, limit, time.Now())
				if err != nil {
					return err
				}
				return json.NewEncoder(command.OutOrStdout()).Encode(result)
			})
		}}
	command.Flags().Int64Var(&generationID, "generation-id", 0, "Generation whose failures should be reset")
	command.Flags().StringVar(&afterToken, "after-token", "", "Stable retry cursor token")
	command.Flags().IntVarP(&limit, "limit", "n", defaultDocumentVectorOperationLimit, "Maximum failures to scan (1-1000)")
	_ = command.MarkFlagRequired("generation-id")
	return command
}

func newDocumentVectorRebuildCmd(deps documentsCommandDeps) *cobra.Command {
	var activeID int64
	var limit int
	var yes bool
	command := &cobra.Command{Use: "rebuild", Short: "Build a fresh generation while the active generation remains searchable", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("document vector rebuild requires --yes")
			}
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				spec, err := desiredDocumentVectorSpec(command.Context(), st)
				if err != nil {
					return err
				}
				if err := requireDocumentVectorConsent(command.Context(), st, spec); err != nil {
					return err
				}
				generation, err := st.StartDocumentVectorRebuild(command.Context(), activeID, spec, time.Now())
				if err != nil {
					return err
				}
				return runDocumentVectorCommand(command, deps, st, generation.ID, limit)
			})
		}}
	command.Flags().Int64Var(&activeID, "generation-id", 0, "Active generation being replaced")
	command.Flags().IntVarP(&limit, "limit", "n", defaultDocumentVectorOperationLimit, "Maximum chunks and cleanup tokens to process (1-1000)")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm the rebuild")
	_ = command.MarkFlagRequired("generation-id")
	return command
}

func newDocumentVectorRetireCmd(deps documentsCommandDeps) *cobra.Command {
	var generationID int64
	var yes bool
	command := &cobra.Command{Use: cliEmbeddingsOperationRetire, Short: "Retire a document vector generation without deleting its backend ledger", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if !yes {
				return errors.New("document vector retirement requires --yes")
			}
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				retired, err := st.RetireDocumentVectorGeneration(command.Context(), generationID, time.Now())
				if err != nil {
					return err
				}
				_, _ = fmt.Fprintf(command.OutOrStdout(), "retired=%t generation_id=%d; backend cleanup will resume when vector operations next run\n", retired, generationID)
				return nil
			})
		}}
	command.Flags().Int64Var(&generationID, "generation-id", 0, "Generation to retire")
	command.Flags().BoolVar(&yes, "yes", false, "Confirm retirement")
	_ = command.MarkFlagRequired("generation-id")
	return command
}

func newDocumentVectorStatusCmd(deps documentsCommandDeps) *cobra.Command {
	var generationID int64
	var afterToken string
	var limit int
	var jsonOutput bool
	command := &cobra.Command{Use: statusValue, Short: "Inspect document vector generations, consent, usage, and failures", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			if cfg == nil || !cfg.Vector.Enabled || !cfg.Attachments.Documents.Index.Embeddings.Enabled {
				if jsonOutput {
					return json.NewEncoder(command.OutOrStdout()).Encode(map[string]bool{"enabled": false})
				}
				_, _ = fmt.Fprintln(command.OutOrStdout(), "document_vectors=disabled")
				return nil
			}
			return withDocumentVectorStore(deps, func(st *store.Store) error {
				spec, err := desiredDocumentVectorSpec(command.Context(), st)
				if errors.Is(err, store.ErrDocumentVectorInvalidGenerationState) {
					if jsonOutput {
						return json.NewEncoder(command.OutOrStdout()).Encode(map[string]bool{"enabled": true, "configured": false})
					}
					_, _ = fmt.Fprintln(command.OutOrStdout(), "document_vectors=enabled configured=false")
					return nil
				}
				if err != nil {
					return err
				}
				consentSpec, err := configuredDocumentVectorConsentSpec(spec)
				if err != nil {
					return err
				}
				status, err := st.GetDocumentVectorOperationsStatus(command.Context(), spec, consentSpec.EgressFingerprint, generationID, afterToken, limit)
				if err != nil {
					return err
				}
				if jsonOutput {
					return json.NewEncoder(command.OutOrStdout()).Encode(struct {
						Enabled    bool                                 `json:"enabled"`
						Configured bool                                 `json:"configured"`
						Status     store.DocumentVectorOperationsStatus `json:"status"`
					}{Enabled: true, Configured: true, Status: status})
				}
				_, _ = fmt.Fprintf(command.OutOrStdout(), "configured_fingerprint=%s consented=%t provider_calls=%d provider_documents=%d provider_chunks=%d provider_input_chars=%d\n", status.ConfiguredSpec.Fingerprint, status.Consent != nil, status.Usage.ProviderCalls, status.Usage.ProviderDocuments, status.Usage.ProviderChunks, status.Usage.ProviderInputChars)
				if status.Active != nil {
					_, _ = fmt.Fprintf(command.OutOrStdout(), "active_generation=%d state=%s\n", status.Active.ID, status.Active.State)
				} else {
					_, _ = fmt.Fprintln(command.OutOrStdout(), "active_generation=none")
				}
				if status.Building != nil {
					_, _ = fmt.Fprintf(command.OutOrStdout(), "building_generation=%d state=%s\n", status.Building.ID, status.Building.State)
				} else {
					_, _ = fmt.Fprintln(command.OutOrStdout(), "building_generation=none")
				}
				if status.Selected != nil {
					_, _ = fmt.Fprintf(command.OutOrStdout(), "selected_generation=%d state=%s pending=%d retryable=%d terminal=%d ready_live=%d obsolete=%d cleanup_pending=%d\n",
						status.Selected.GenerationID, status.Selected.State, status.Selected.Pending,
						status.Selected.Retryable, status.Selected.Terminal, status.Selected.ReadyLive,
						status.Selected.Obsolete, status.Selected.CleanupPending)
				}
				if status.Coverage != nil {
					_, _ = fmt.Fprintf(command.OutOrStdout(), "coverage_required=%d coverage_ready=%d\n", status.Coverage.Required, status.Coverage.Ready)
				}
				if status.Consent != nil {
					_, _ = fmt.Fprintln(command.OutOrStdout(), "scheduled_registration=restart-required-after-new-consent")
				}
				return nil
			})
		}}
	command.Flags().Int64Var(&generationID, "generation-id", 0, "Generation whose bounded failures to inspect")
	command.Flags().StringVar(&afterToken, "after-token", "", "Stable failure cursor token")
	command.Flags().IntVarP(&limit, "limit", "n", 20, "Maximum failure diagnostics (1-1000)")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}
