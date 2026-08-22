package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
)

const (
	personProviderCommandName  = "provider"
	personProviderConsentActor = "cli"
)

type personProviderStore interface {
	EnsurePersonInferenceProfile(ctx context.Context, profile peoplesweep.ProviderProfile) (bool, error)
	ListPersonInferenceProfiles(ctx context.Context) ([]peoplesweep.ProviderProfile, error)
	GrantPersonInferenceConsent(ctx context.Context, fingerprint, actor string) (*store.PersonInferenceConsent, bool, error)
	RevokePersonInferenceConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	RevokeAllPersonInferenceConsents(ctx context.Context, actor string) (int64, error)
	GetPersonInferenceConsentStatus(ctx context.Context, fingerprint string) (*store.PersonInferenceConsentStatus, error)
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
	EnsurePersonSemanticEmbeddingProfile(ctx context.Context, profile vector.SemanticPersonEmbeddingProfile) (bool, error)
	ListPersonSemanticEmbeddingProfiles(ctx context.Context) ([]vector.SemanticPersonEmbeddingProfile, error)
	GrantPersonSemanticEmbeddingConsent(ctx context.Context, fingerprint, actor string) (*store.PersonSemanticEmbeddingConsent, bool, error)
	RevokePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	RevokeAllPersonSemanticEmbeddingConsents(ctx context.Context, actor string) (int64, error)
	GetPersonSemanticEmbeddingConsentStatus(ctx context.Context, fingerprint string) (*store.PersonSemanticEmbeddingConsentStatus, error)
	HasActivePersonSemanticEmbeddingConsent(ctx context.Context, fingerprint string) (bool, error)
}

type personProviderChecker interface {
	Check(ctx context.Context) (peoplesweep.StructuredResponse, error)
}

type personProviderCommandDeps struct {
	config             func() peoplesweep.Config
	vectorConfig       func() vector.Config
	openStore          func() (personProviderStore, func(), error)
	newChecker         func(peoplesweep.Config, personProviderStore) (personProviderChecker, error)
	isDaemonSubprocess func() bool
	lookupEnv          peoplesweep.CredentialLookup
	proxy              func(*cobra.Command, []string, map[string]string) error
}

type personProviderStatusOutput struct {
	Profile peoplesweep.ProviderProfile        `json:"profile"`
	Consent store.PersonInferenceConsentStatus `json:"consent"`
}

type personProviderCheckOutput struct {
	OK                bool                   `json:"ok"`
	ProviderRequestID string                 `json:"provider_request_id,omitempty"`
	Model             string                 `json:"model"`
	Usage             peoplesweep.TokenUsage `json:"usage"`
}

type personProviderStatusesOutput struct {
	Profiles []personProviderStatusOutput `json:"profiles"`
}

type personProviderRevokeAllOutput struct {
	Revoked  int64                        `json:"revoked"`
	Profiles []personProviderStatusOutput `json:"profiles"`
}

type personSemanticProviderStatusOutput struct {
	Profile vector.SemanticPersonEmbeddingProfile      `json:"profile"`
	Consent store.PersonSemanticEmbeddingConsentStatus `json:"consent"`
}

type personSemanticProviderStatusesOutput struct {
	Profiles []personSemanticProviderStatusOutput `json:"profiles"`
}

type personSemanticProviderRevokeAllOutput struct {
	Revoked  int64                                `json:"revoked"`
	Profiles []personSemanticProviderStatusOutput `json:"profiles"`
}

func defaultPersonProviderCommandDeps() personProviderCommandDeps {
	return personProviderCommandDeps{
		config: func() peoplesweep.Config {
			if cfg == nil {
				return peoplesweep.Config{}
			}
			return cfg.People.Sweep
		},
		vectorConfig: func() vector.Config {
			if cfg == nil {
				return vector.Config{}
			}
			return cfg.Vector
		},
		openStore: func() (personProviderStore, func(), error) {
			return openWritableStoreAndInit()
		},
		newChecker: func(config peoplesweep.Config, st personProviderStore) (personProviderChecker, error) {
			return peoplesweep.NewRunner(
				config,
				st,
				peoplesweep.NewOpenAICompatibleTransport(http.DefaultClient),
				os.LookupEnv,
			)
		},
		isDaemonSubprocess: isDaemonCLISubprocess,
		lookupEnv:          os.LookupEnv,
		proxy: func(command *cobra.Command, args []string, env map[string]string) error {
			if len(env) == 0 {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(command, args, env)
		},
	}
}

func newPersonProviderCommand(deps personProviderCommandDeps) *cobra.Command {
	provider := &cobra.Command{
		Use:   personProviderCommandName,
		Short: "Manage people-sweep inference",
	}
	provider.AddCommand(
		newPersonProviderStatusCommand(deps),
		newPersonProviderConsentCommand(deps),
		newPersonProviderRevokeCommand(deps),
		newPersonProviderCheckCommand(deps),
	)
	return provider
}

func newPersonProviderStatusCommand(deps personProviderCommandDeps) *cobra.Command {
	var all bool
	var jsonOutput bool
	var semanticEmbeddings bool
	command := &cobra.Command{
		Use:   statusValue,
		Short: "Show the exact people inference policy and consent state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderStatus(command, deps, all, jsonOutput, semanticEmbeddings)
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Show every stored provider policy and consent state")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().BoolVar(&semanticEmbeddings, "semantic-embeddings", false,
		"Select the curated-person semantic embedding policy")
	return command
}

func newPersonProviderConsentCommand(deps personProviderCommandDeps) *cobra.Command {
	var confirmed bool
	var jsonOutput bool
	var semanticEmbeddings bool
	command := &cobra.Command{
		Use:   "consent",
		Short: "Consent to the exact people inference policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderConsent(command, deps, confirmed, jsonOutput, semanticEmbeddings)
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the disclosed provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().BoolVar(&semanticEmbeddings, "semantic-embeddings", false,
		"Select the curated-person semantic embedding policy")
	return command
}

func newPersonProviderRevokeCommand(deps personProviderCommandDeps) *cobra.Command {
	var all bool
	var jsonOutput bool
	var semanticEmbeddings bool
	command := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke consent for the exact people inference policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderRevoke(command, deps, all, jsonOutput, semanticEmbeddings)
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Revoke consent for every stored provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().BoolVar(&semanticEmbeddings, "semantic-embeddings", false,
		"Select the curated-person semantic embedding policy")
	return command
}

func newPersonProviderCheckCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "check",
		Short: "Run a fixed synthetic request through the people inference provider",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				config := deps.config()
				return deps.proxy(command, args, personProviderForwardEnv(config, deps.lookupEnv))
			}
			return runPersonProviderCheck(command, deps, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func runPersonProviderStatus(
	command *cobra.Command,
	deps personProviderCommandDeps,
	all bool,
	jsonOutput bool,
	semanticEmbeddings bool,
) error {
	if semanticEmbeddings {
		return runPersonSemanticProviderStatus(command, deps, all, jsonOutput)
	}
	if all {
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		defer cleanup()
		profiles, err := st.ListPersonInferenceProfiles(command.Context())
		if err != nil {
			return err
		}
		statuses, err := personProviderStatuses(command.Context(), st, profiles)
		if err != nil {
			return err
		}
		return writePersonProviderStatuses(command.OutOrStdout(), statuses, jsonOutput)
	}
	profile, st, cleanup, err := openPersonProviderProfile(deps)
	if err != nil {
		return err
	}
	defer cleanup()
	status, err := st.GetPersonInferenceConsentStatus(command.Context(), profile.Fingerprint)
	if err != nil {
		return err
	}
	return writePersonProviderStatus(command.OutOrStdout(), profile, status, jsonOutput)
}

func runPersonProviderConsent(
	command *cobra.Command,
	deps personProviderCommandDeps,
	confirmed bool,
	jsonOutput bool,
	semanticEmbeddings bool,
) error {
	if semanticEmbeddings {
		return runPersonSemanticProviderConsent(command, deps, confirmed, jsonOutput)
	}
	profile, err := deps.config().Profile()
	if err != nil {
		return err
	}
	if !confirmed {
		printPersonProviderDisclosure(command.OutOrStdout(), profile)
		return errors.New("people inference consent requires --yes after reviewing the provider disclosure")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.EnsurePersonInferenceProfile(command.Context(), profile); err != nil {
		return err
	}
	if _, _, err := st.GrantPersonInferenceConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return err
	}
	status, err := st.GetPersonInferenceConsentStatus(command.Context(), profile.Fingerprint)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writePersonProviderStatus(command.OutOrStdout(), profile, status, true)
	}
	printPersonProviderDisclosure(command.OutOrStdout(), profile)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent: active (%s)\n", profile.Fingerprint)
	return nil
}

func runPersonProviderRevoke(
	command *cobra.Command,
	deps personProviderCommandDeps,
	all bool,
	jsonOutput bool,
	semanticEmbeddings bool,
) error {
	if semanticEmbeddings {
		return runPersonSemanticProviderRevoke(command, deps, all, jsonOutput)
	}
	if all {
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		defer cleanup()
		revoked, err := st.RevokeAllPersonInferenceConsents(
			command.Context(), personProviderConsentActor,
		)
		if err != nil {
			return err
		}
		profiles, err := st.ListPersonInferenceProfiles(command.Context())
		if err != nil {
			return err
		}
		statuses, err := personProviderStatuses(command.Context(), st, profiles)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(personProviderRevokeAllOutput{
				Revoked: revoked, Profiles: statuses,
			})
		}
		_, _ = fmt.Fprintf(command.OutOrStdout(),
			"Consent revoked for %d active people inference profile(s).\n", revoked)
		return nil
	}
	profile, st, cleanup, err := openPersonProviderProfile(deps)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.RevokePersonInferenceConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return err
	}
	status, err := st.GetPersonInferenceConsentStatus(command.Context(), profile.Fingerprint)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writePersonProviderStatus(command.OutOrStdout(), profile, status, true)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent revoked for %s\n", profile.Fingerprint)
	return nil
}

func runPersonSemanticProviderStatus(
	command *cobra.Command,
	deps personProviderCommandDeps,
	all bool,
	jsonOutput bool,
) error {
	if all {
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		defer cleanup()
		profiles, err := st.ListPersonSemanticEmbeddingProfiles(command.Context())
		if err != nil {
			return err
		}
		statuses, err := personSemanticProviderStatuses(command.Context(), st, profiles)
		if err != nil {
			return err
		}
		return writePersonSemanticProviderStatuses(command.OutOrStdout(), statuses, jsonOutput)
	}
	profile, st, cleanup, err := openPersonSemanticProviderProfile(deps)
	if err != nil {
		return err
	}
	defer cleanup()
	status, err := st.GetPersonSemanticEmbeddingConsentStatus(
		command.Context(), profile.Fingerprint,
	)
	if err != nil {
		return err
	}
	return writePersonSemanticProviderStatus(command.OutOrStdout(), profile, status, jsonOutput)
}

func runPersonSemanticProviderConsent(
	command *cobra.Command,
	deps personProviderCommandDeps,
	confirmed bool,
	jsonOutput bool,
) error {
	profile, err := currentPersonSemanticProviderProfile(deps)
	if err != nil {
		return err
	}
	if !confirmed {
		printPersonSemanticProviderDisclosure(command.OutOrStdout(), profile)
		return errors.New("semantic person embedding consent requires --yes after reviewing the provider disclosure")
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.EnsurePersonSemanticEmbeddingProfile(command.Context(), profile); err != nil {
		return err
	}
	if _, _, err := st.GrantPersonSemanticEmbeddingConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return err
	}
	status, err := st.GetPersonSemanticEmbeddingConsentStatus(
		command.Context(), profile.Fingerprint,
	)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writePersonSemanticProviderStatus(command.OutOrStdout(), profile, status, true)
	}
	printPersonSemanticProviderDisclosure(command.OutOrStdout(), profile)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent: active (%s)\n", profile.Fingerprint)
	return nil
}

func runPersonSemanticProviderRevoke(
	command *cobra.Command,
	deps personProviderCommandDeps,
	all bool,
	jsonOutput bool,
) error {
	if all {
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		defer cleanup()
		revoked, err := st.RevokeAllPersonSemanticEmbeddingConsents(
			command.Context(), personProviderConsentActor,
		)
		if err != nil {
			return err
		}
		profiles, err := st.ListPersonSemanticEmbeddingProfiles(command.Context())
		if err != nil {
			return err
		}
		statuses, err := personSemanticProviderStatuses(command.Context(), st, profiles)
		if err != nil {
			return err
		}
		if jsonOutput {
			return json.NewEncoder(command.OutOrStdout()).Encode(personSemanticProviderRevokeAllOutput{
				Revoked: revoked, Profiles: statuses,
			})
		}
		_, _ = fmt.Fprintf(command.OutOrStdout(),
			"Consent revoked for %d active semantic person embedding profile(s).\n", revoked)
		return nil
	}
	profile, st, cleanup, err := openPersonSemanticProviderProfile(deps)
	if err != nil {
		return err
	}
	defer cleanup()
	if _, err := st.RevokePersonSemanticEmbeddingConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return err
	}
	status, err := st.GetPersonSemanticEmbeddingConsentStatus(
		command.Context(), profile.Fingerprint,
	)
	if err != nil {
		return err
	}
	if jsonOutput {
		return writePersonSemanticProviderStatus(command.OutOrStdout(), profile, status, true)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent revoked for %s\n", profile.Fingerprint)
	return nil
}

func runPersonProviderCheck(
	command *cobra.Command,
	deps personProviderCommandDeps,
	jsonOutput bool,
) error {
	config := deps.config()
	profile, err := config.Profile()
	if err != nil {
		return err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	checker, err := deps.newChecker(config, st)
	if err != nil {
		return err
	}
	response, err := checker.Check(command.Context())
	if err != nil {
		return err
	}
	output := personProviderCheckOutput{
		OK: true, ProviderRequestID: response.ProviderRequestID,
		Model: profile.Model, Usage: response.Usage,
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(output)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"People inference provider check succeeded (model=%s, request_id=%s, input_tokens=%d, output_tokens=%d).\n",
		output.Model, output.ProviderRequestID, output.Usage.InputTokens, output.Usage.OutputTokens)
	return nil
}

func openPersonProviderProfile(
	deps personProviderCommandDeps,
) (peoplesweep.ProviderProfile, personProviderStore, func(), error) {
	profile, err := deps.config().Profile()
	if err != nil {
		return peoplesweep.ProviderProfile{}, nil, nil, err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return peoplesweep.ProviderProfile{}, nil, nil, err
	}
	return profile, st, cleanup, nil
}

func currentPersonSemanticProviderProfile(
	deps personProviderCommandDeps,
) (vector.SemanticPersonEmbeddingProfile, error) {
	if deps.vectorConfig == nil {
		return vector.SemanticPersonEmbeddingProfile{}, errors.New(
			"semantic person embedding configuration is unavailable",
		)
	}
	config := deps.vectorConfig()
	if !config.Enabled || !config.People.Enabled {
		return vector.SemanticPersonEmbeddingProfile{}, vector.ErrSemanticPersonEmbeddingsDisabled
	}
	return config.SemanticPersonEmbeddingProfile()
}

func configuredPersonSemanticProviderProfile(
	deps personProviderCommandDeps,
) (vector.SemanticPersonEmbeddingProfile, error) {
	if deps.vectorConfig == nil {
		return vector.SemanticPersonEmbeddingProfile{}, errors.New(
			"semantic person embedding configuration is unavailable",
		)
	}
	config := deps.vectorConfig()
	return config.SemanticPersonEmbeddingProfile()
}

func openPersonSemanticProviderProfile(
	deps personProviderCommandDeps,
) (vector.SemanticPersonEmbeddingProfile, personProviderStore, func(), error) {
	profile, err := configuredPersonSemanticProviderProfile(deps)
	if err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, nil, nil, err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return vector.SemanticPersonEmbeddingProfile{}, nil, nil, err
	}
	return profile, st, cleanup, nil
}

func writePersonProviderStatus(
	w io.Writer,
	profile peoplesweep.ProviderProfile,
	status *store.PersonInferenceConsentStatus,
	jsonOutput bool,
) error {
	if status == nil {
		return errors.New("people inference consent status is empty")
	}
	if jsonOutput {
		return json.NewEncoder(w).Encode(personProviderStatusOutput{
			Profile: profile, Consent: *status,
		})
	}
	printPersonProviderDisclosure(w, profile)
	state := "inactive"
	if status.Active {
		state = "active"
	} else if status.LastRevoked != nil {
		state = "revoked"
	}
	_, _ = fmt.Fprintf(w, "Consent: %s\n", state)
	return nil
}

func writePersonSemanticProviderStatus(
	w io.Writer,
	profile vector.SemanticPersonEmbeddingProfile,
	status *store.PersonSemanticEmbeddingConsentStatus,
	jsonOutput bool,
) error {
	if status == nil {
		return errors.New("semantic person embedding consent status is empty")
	}
	if jsonOutput {
		return json.NewEncoder(w).Encode(personSemanticProviderStatusOutput{
			Profile: profile, Consent: *status,
		})
	}
	printPersonSemanticProviderDisclosure(w, profile)
	state := "inactive"
	if status.Active {
		state = "active"
	} else if status.LastRevoked != nil {
		state = "revoked"
	}
	_, _ = fmt.Fprintf(w, "Consent: %s\n", state)
	return nil
}

func personProviderStatuses(
	ctx context.Context,
	st personProviderStore,
	profiles []peoplesweep.ProviderProfile,
) ([]personProviderStatusOutput, error) {
	statuses := make([]personProviderStatusOutput, 0, len(profiles))
	for _, profile := range profiles {
		status, err := st.GetPersonInferenceConsentStatus(ctx, profile.Fingerprint)
		if err != nil {
			return nil, err
		}
		if status == nil {
			return nil, errors.New("people inference consent status is empty")
		}
		statuses = append(statuses, personProviderStatusOutput{Profile: profile, Consent: *status})
	}
	return statuses, nil
}

func personSemanticProviderStatuses(
	ctx context.Context,
	st personProviderStore,
	profiles []vector.SemanticPersonEmbeddingProfile,
) ([]personSemanticProviderStatusOutput, error) {
	statuses := make([]personSemanticProviderStatusOutput, 0, len(profiles))
	for _, profile := range profiles {
		status, err := st.GetPersonSemanticEmbeddingConsentStatus(ctx, profile.Fingerprint)
		if err != nil {
			return nil, err
		}
		if status == nil {
			return nil, errors.New("semantic person embedding consent status is empty")
		}
		statuses = append(statuses, personSemanticProviderStatusOutput{
			Profile: profile, Consent: *status,
		})
	}
	return statuses, nil
}

func writePersonProviderStatuses(
	w io.Writer,
	statuses []personProviderStatusOutput,
	jsonOutput bool,
) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(personProviderStatusesOutput{Profiles: statuses})
	}
	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(w, "No stored people inference provider profiles.")
		return nil
	}
	for i, status := range statuses {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := writePersonProviderStatus(w, status.Profile, &status.Consent, false); err != nil {
			return err
		}
	}
	return nil
}

func writePersonSemanticProviderStatuses(
	w io.Writer,
	statuses []personSemanticProviderStatusOutput,
	jsonOutput bool,
) error {
	if jsonOutput {
		return json.NewEncoder(w).Encode(personSemanticProviderStatusesOutput{Profiles: statuses})
	}
	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(w, "No stored semantic person embedding profiles.")
		return nil
	}
	for i, status := range statuses {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
			_, _ = fmt.Fprintln(w)
		}
		if err := writePersonSemanticProviderStatus(w, status.Profile, &status.Consent, false); err != nil {
			return err
		}
	}
	return nil
}

func printPersonProviderDisclosure(w io.Writer, profile peoplesweep.ProviderProfile) {
	dateRange := profile.SourceSince + " through " + profile.SourceUntil
	if profile.SourceUntil == "" {
		dateRange = profile.SourceSince + " onward"
	}
	authentication := "anonymous loopback"
	if profile.APIKeyEnv != "" {
		authentication = "environment variable " + profile.APIKeyEnv
	}
	sensitive := "denied"
	if profile.AllowSensitive {
		sensitive = "allowed"
	}
	sources := make([]string, len(profile.AllowedSources))
	for i, source := range profile.AllowedSources {
		sources[i] = string(source)
	}
	_, _ = fmt.Fprintln(w, "People inference provider disclosure:")
	_, _ = fmt.Fprintf(w, "Fingerprint: %s\n", profile.Fingerprint)
	_, _ = fmt.Fprintf(w, "Destination: %s\n", profile.Endpoint)
	_, _ = fmt.Fprintf(w, "Model: %s\n", profile.Model)
	_, _ = fmt.Fprintf(w, "Authentication: %s\n", authentication)
	_, _ = fmt.Fprintf(w, "Provider assertions: retention=%s, training=%s\n",
		profile.RetentionPosture, profile.TrainingPosture)
	_, _ = fmt.Fprintf(w, "Allowed sources: %s\n", strings.Join(sources, ", "))
	_, _ = fmt.Fprintf(w, "Source dates: %s\n", dateRange)
	_, _ = fmt.Fprintf(w, "Sensitive content: %s\n", sensitive)
}

func printPersonSemanticProviderDisclosure(
	w io.Writer,
	profile vector.SemanticPersonEmbeddingProfile,
) {
	authentication := "none configured"
	if profile.APIKeyEnv != "" {
		authentication = "environment variable " + profile.APIKeyEnv
	}
	_, _ = fmt.Fprintln(w, "Semantic person embedding provider disclosure:")
	_, _ = fmt.Fprintf(w, "Purpose: %s\n", profile.Purpose)
	_, _ = fmt.Fprintf(w, "Fingerprint: %s\n", profile.Fingerprint)
	_, _ = fmt.Fprintf(w, "Destination: %s\n", profile.Destination)
	_, _ = fmt.Fprintf(w, "API format: %s\n", profile.APIFormat)
	_, _ = fmt.Fprintf(w, "Model: %s\n", profile.Model)
	_, _ = fmt.Fprintf(w, "Authentication: %s\n", authentication)
	_, _ = fmt.Fprintf(w, "Provider assertions: retention=%s, training=%s\n",
		profile.RetentionPosture, profile.TrainingPosture)
	_, _ = fmt.Fprintf(w, "Renderer policy: %s\n", profile.RendererPolicy)
	_, _ = fmt.Fprintln(w, "Disclosed curated document field classes:")
	for _, field := range profile.DisclosedFieldClasses {
		if field == vector.SemanticPersonSearchQueryDisclosedFieldClass {
			continue
		}
		_, _ = fmt.Fprintf(w, "- %s\n", field)
	}
	if slices.Contains(
		profile.DisclosedFieldClasses,
		vector.SemanticPersonSearchQueryDisclosedFieldClass,
	) {
		_, _ = fmt.Fprintln(w,
			"Caller-supplied query egress: free-text semantic person search queries are sent to the embedding provider.")
	}
	_, _ = fmt.Fprintf(w, "Corpus scope: %s\n", profile.CorpusScope)
	_, _ = fmt.Fprintln(w,
		"Scope note: [vector.embed.scope] does not filter curated people; this policy covers every durable person.")
}

func personProviderForwardEnv(
	config peoplesweep.Config,
	lookup peoplesweep.CredentialLookup,
) map[string]string {
	name := config.Provider.APIKeyEnv
	if name == "" || lookup == nil {
		return nil
	}
	value, ok := lookup(name)
	if !ok || value == "" {
		return nil
	}
	return map[string]string{name: value}
}
