package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

const personProviderConsentActor = "cli"

type personProviderStore interface {
	EnsurePersonInferenceProfile(ctx context.Context, profile peoplesweep.ProviderProfile) (bool, error)
	ListPersonInferenceProfiles(ctx context.Context) ([]peoplesweep.ProviderProfile, error)
	GrantPersonInferenceConsent(ctx context.Context, fingerprint, actor string) (*store.PersonInferenceConsent, bool, error)
	RevokePersonInferenceConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	RevokeAllPersonInferenceConsents(ctx context.Context, actor string) (int64, error)
	GetPersonInferenceConsentStatus(ctx context.Context, fingerprint string) (*store.PersonInferenceConsentStatus, error)
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
}

type personProviderChecker interface {
	Check(ctx context.Context) (peoplesweep.StructuredResponse, error)
}

type personProviderCommandDeps struct {
	config             func() peoplesweep.Config
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

func defaultPersonProviderCommandDeps() personProviderCommandDeps {
	return personProviderCommandDeps{
		config: func() peoplesweep.Config {
			if cfg == nil {
				return peoplesweep.Config{}
			}
			return cfg.People.Sweep
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
		Use:   "provider",
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
	command := &cobra.Command{
		Use:   statusValue,
		Short: "Show the exact people inference policy and consent state",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderStatus(command, deps, all, jsonOutput)
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Show every stored provider policy and consent state")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderConsentCommand(deps personProviderCommandDeps) *cobra.Command {
	var confirmed bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "consent",
		Short: "Consent to the exact people inference policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderConsent(command, deps, confirmed, jsonOutput)
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the disclosed provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderRevokeCommand(deps personProviderCommandDeps) *cobra.Command {
	var all bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "revoke",
		Short: "Revoke consent for the exact people inference policy",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderRevoke(command, deps, all, jsonOutput)
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Revoke consent for every stored provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
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
) error {
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
) error {
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
) error {
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
