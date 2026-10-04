package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/daemonclient"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/vector"
	"go.kenn.io/msgvault/pkg/client/generated"
)

const (
	personProviderCommandName       = "provider"
	personProviderConsentActor      = "cli"
	personProviderIfFingerprintFlag = "if-fingerprint"
)

type personProviderStore interface {
	EnsurePersonInferenceProfile(ctx context.Context, profile peoplesweep.ProviderProfile) (bool, error)
	ListPersonInferenceProfiles(ctx context.Context) ([]peoplesweep.ProviderProfile, error)
	GrantPersonInferenceConsent(ctx context.Context, fingerprint, actor string) (*store.PersonInferenceConsent, bool, error)
	RevokePersonInferenceConsent(ctx context.Context, fingerprint, actor string) (bool, error)
	RevokeAllPersonInferenceConsents(ctx context.Context, actor string) (int64, error)
	GetPersonInferenceConsentStatus(ctx context.Context, fingerprint string) (*store.PersonInferenceConsentStatus, error)
	HasSuccessfulPersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
	InvalidatePersonInferenceCheck(ctx context.Context, fingerprint string) (bool, error)
	RecordPersonInferenceCheck(ctx context.Context, check store.PersonInferenceCheck) error
	GetPersonInferenceCheck(ctx context.Context, fingerprint string) (*store.PersonInferenceCheck, error)
	HasActivePersonInferenceConsent(ctx context.Context, fingerprint string) (bool, error)
	ListPersonSweepRuns(ctx context.Context, filter peoplesweep.RunFilter) ([]peoplesweep.RunSummary, error)
	ListPersonSweepAttempts(ctx context.Context, filter peoplesweep.AttemptFilter) ([]peoplesweep.AttemptSummary, error)
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

type personProviderCodexClient interface {
	StartDeviceLogin(ctx context.Context, present func(peoplesweep.DeviceLogin) error) error
	ListModels(ctx context.Context) ([]peoplesweep.CodexModel, error)
}

type personProviderCommandDeps struct {
	bind                       func(personProviderCommandDeps, context.Context) personProviderCommandDeps
	config                     func() peoplesweep.Config
	vectorConfig               func() vector.Config
	openStore                  func() (personProviderStore, func(), error)
	openReadStore              func() (personProviderStore, func(), error)
	newChecker                 func(peoplesweep.Config, personProviderStore, personProviderSetupDeps) (personProviderChecker, error)
	newCodexClient             func(peoplesweep.Config, personProviderSetupDeps) (personProviderCodexClient, error)
	isDaemonSubprocess         func() bool
	providerStoreOwnedByDaemon func(context.Context) (bool, error)
	// daemonAliveForRestartNotice reports whether any daemon process in
	// this machine's data dir is alive, regardless of API compatibility.
	// Local config mutations use it for restart guidance; removal refuses a
	// daemon left running across a CLI upgrade that fails the compatibility
	// check because its scheduled sweeps still keep the startup config.
	daemonAliveForRestartNotice func(context.Context) (bool, error)
	remoteConfigured            func() bool
	lookupEnv                   peoplesweep.CredentialLookup
	proxy                       func(*cobra.Command, []string, map[string]string) error
	removeWithDaemon            func(context.Context, string, string) error
	readConfigFile              func() (config.ConfigFile, error)
	configHomeDir               func() string
	editConfigTables            func(string, []config.TableEdit) (config.ConfigFile, error)
	restoreConfigFile           func(config.ConfigFile, config.ConfigFile) (config.ConfigFile, error)
	setup                       personProviderSetupDeps
}

type personProviderStatusOutput struct {
	Name                string                              `json:"name,omitempty"`
	Profile             peoplesweep.ProviderProfile         `json:"profile"`
	Check               *store.PersonInferenceCheck         `json:"check,omitzero"`
	Consent             store.PersonInferenceConsentStatus  `json:"consent"`
	StaleProgramCheck   bool                                `json:"stale_program_check,omitzero"`
	StaleProgramConsent bool                                `json:"stale_program_consent,omitzero"`
	CodexIsolation      *personProviderCodexIsolationStatus `json:"codex_isolation,omitzero"`
}

type personProviderUseOutput struct {
	Name                  string `json:"name"`
	Fingerprint           string `json:"fingerprint"`
	Enabled               bool   `json:"enabled"`
	DaemonRestartRequired bool   `json:"daemon_restart_required"`
}

type personProviderRemoveOutput struct {
	Name                  string `json:"name"`
	Removed               bool   `json:"removed"`
	DaemonRestartRequired bool   `json:"daemon_restart_required"`
}

type personProviderSetOutput struct {
	Name                  string `json:"name"`
	Fingerprint           string `json:"fingerprint"`
	Checked               bool   `json:"checked"`
	DaemonRestartRequired bool   `json:"daemon_restart_required"`
}

type personProviderCodexIsolationStatus struct {
	Available         bool   `json:"available"`
	ExecutionBoundary string `json:"execution_boundary"`
	Reason            string `json:"reason,omitempty"`
}

type personProviderCheckOutput struct {
	OK                bool                   `json:"ok"`
	ProviderRequestID string                 `json:"provider_request_id,omitempty"`
	Model             string                 `json:"model"`
	Usage             peoplesweep.TokenUsage `json:"usage"`
}

type personProviderModelsOutput struct {
	Models []peoplesweep.CodexModel `json:"models"`
}

type personProviderListItem struct {
	Name          string                       `json:"name"`
	Active        bool                         `json:"active"`
	Protocol      peoplesweep.Protocol         `json:"protocol"`
	Endpoint      string                       `json:"endpoint,omitempty"`
	Model         string                       `json:"model"`
	Auth          peoplesweep.AuthScheme       `json:"auth"`
	Credential    peoplesweep.CredentialSource `json:"credential"`
	CredentialEnv string                       `json:"credential_env,omitempty"`
}

type personProviderListOutput struct {
	Profiles []personProviderListItem `json:"profiles"`
}

type personProviderStatusesOutput struct {
	Profiles []personProviderStatusOutput `json:"profiles"`
}

type personProviderRevokeAllOutput struct {
	Revoked  int64                        `json:"revoked"`
	Profiles []personProviderStatusOutput `json:"profiles"`
}

type personProviderRevokeFingerprintOutput struct {
	Fingerprint string `json:"fingerprint"`
	Revoked     bool   `json:"revoked"`
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

func defaultPersonProviderCommandDeps(contexts ...context.Context) personProviderCommandDeps {
	setup := defaultPersonProviderSetupDeps()
	setup.openCredentialStore = func() (peoplesweep.CredentialStore, error) {
		return nil, errors.New("configuration is unavailable")
	}
	deps := personProviderCommandDeps{
		bind: func(deps personProviderCommandDeps, ctx context.Context) personProviderCommandDeps {
			state := invocationFromContext(ctx)
			var currentCfg *config.Config
			if state != nil && state.cfg != nil {
				currentCfg = state.cfg
			}
			deps.setup.codexAuthHome = ""
			if currentCfg != nil {
				deps.setup.codexAuthHome = filepath.Join(currentCfg.TokensDir(), "people-codex")
			}
			deps.config = func() peoplesweep.Config {
				if currentCfg == nil {
					return peoplesweep.Config{}
				}
				return currentCfg.People.Sweep
			}
			deps.vectorConfig = func() vector.Config {
				if currentCfg == nil {
					return vector.Config{}
				}
				return currentCfg.Vector
			}
			deps.openReadStore = func() (personProviderStore, func(), error) {
				if currentCfg == nil {
					return nil, nil, errors.New("configuration is unavailable")
				}
				st, err := store.OpenReadOnly(currentCfg.DatabaseDSN())
				if err != nil {
					return nil, nil, err
				}
				return st, func() { _ = st.Close() }, nil
			}
			deps.openStore = func() (personProviderStore, func(), error) {
				return openWritableStoreAndInitForInvocation(state)
			}
			deps.setup.openCredentialStore = func() (peoplesweep.CredentialStore, error) {
				if currentCfg == nil {
					return nil, errors.New("configuration is unavailable")
				}
				return peoplesweep.NewStoredCredentials(currentCfg.TokensDir()), nil
			}
			deps.remoteConfigured = func() bool { return IsRemoteMode(state) }
			deps.readConfigFile = func() (config.ConfigFile, error) {
				if currentCfg == nil {
					return config.ConfigFile{}, errors.New("configuration is unavailable")
				}
				return config.ReadConfigFile(currentCfg.ConfigFilePath())
			}
			deps.configHomeDir = func() string {
				if currentCfg == nil {
					return ""
				}
				return currentCfg.HomeDir
			}
			deps.editConfigTables = func(ifMatch string, edits []config.TableEdit) (config.ConfigFile, error) {
				if currentCfg == nil {
					return config.ConfigFile{}, errors.New("configuration is unavailable")
				}
				return config.EditConfigTables(currentCfg.ConfigFilePath(), ifMatch, edits)
			}
			deps.restoreConfigFile = func(published, before config.ConfigFile) (config.ConfigFile, error) {
				if currentCfg == nil {
					return config.ConfigFile{}, errors.New("configuration is unavailable")
				}
				return config.RestoreConfigFile(currentCfg.ConfigFilePath(), published, before)
			}
			return deps
		},
		config: func() peoplesweep.Config {
			return peoplesweep.Config{}
		},
		vectorConfig: func() vector.Config {
			return vector.Config{}
		},
		openStore: func() (personProviderStore, func(), error) {
			return nil, nil, errors.New("configuration is unavailable")
		},
		openReadStore: func() (personProviderStore, func(), error) {
			return nil, nil, errors.New("configuration is unavailable")
		},
		newChecker: func(config peoplesweep.Config, st personProviderStore, setup personProviderSetupDeps) (personProviderChecker, error) {
			registry, err := peoplesweep.NewDriverRegistryWithCodexAuthHome(
				http.DefaultClient,
				peoplesweep.NewCodexCommandStarter(),
				peoplesweep.NewReleasedCodexIsolationGate(),
				setup.codexAuthHome,
			)
			if err != nil {
				return nil, err
			}
			var credentialStore peoplesweep.CredentialStore
			_, provider, err := config.ActiveProviderConfig()
			if err != nil {
				return nil, err
			}
			if provider.Credential == peoplesweep.CredentialStored {
				credentialStore, err = setup.resolveCredentialStore()
				if err != nil {
					return nil, err
				}
			}
			return peoplesweep.NewRunner(
				config,
				st,
				registry,
				peoplesweep.NewCredentialResolver(credentialStore, os.LookupEnv),
			)
		},
		newCodexClient: func(config peoplesweep.Config, setup personProviderSetupDeps) (personProviderCodexClient, error) {
			if !peoplesweep.CodexReleaseAvailable() {
				return nil, peoplesweep.ErrCodexIsolationUnreleased
			}
			_, provider, err := config.ActiveProviderConfig()
			if err != nil {
				return nil, err
			}
			registry, err := peoplesweep.NewDriverRegistryWithCodexAuthHome(
				http.DefaultClient,
				peoplesweep.NewCodexCommandStarter(),
				peoplesweep.NewReleasedCodexIsolationGate(),
				setup.codexAuthHome,
			)
			if err != nil {
				return nil, err
			}
			driver, err := registry.Driver(provider.Protocol, provider)
			if err != nil {
				return nil, err
			}
			codex, ok := driver.(*peoplesweep.CodexAppServerDriver)
			if !ok {
				return nil, errors.New("people inference provider is not codex_app_server")
			}
			return codex, nil
		},
		isDaemonSubprocess: isDaemonCLISubprocess,
		providerStoreOwnedByDaemon: func(ctx context.Context) (bool, error) {
			state := invocationFromContext(ctx)
			if IsRemoteMode(state) {
				return true, nil
			}
			if state == nil || state.cfg == nil {
				return false, errors.New("configuration is unavailable")
			}
			runtime, err := findCompatibleDaemonRuntimeContext(ctx, state.cfg.Data.DataDir)
			return runtime != nil, err
		},
		daemonAliveForRestartNotice: func(ctx context.Context) (bool, error) {
			state := invocationFromContext(ctx)
			if state == nil || state.cfg == nil {
				return false, errors.New("configuration is unavailable")
			}
			return findAnyDaemonRuntimeContext(ctx, state.cfg.Data.DataDir) != nil, nil
		},
		removeWithDaemon: removePersonProviderWithDaemon,
		lookupEnv:        os.LookupEnv,
		remoteConfigured: func() bool { return false },
		proxy: func(command *cobra.Command, args []string, env map[string]string) error {
			if len(env) == 0 {
				return runDaemonCLICommandHTTPFromCobra(command, args)
			}
			return runDaemonCLICommandHTTPFromCobraWithEnv(command, args, env)
		},
		readConfigFile: func() (config.ConfigFile, error) {
			return config.ConfigFile{}, errors.New("configuration is unavailable")
		},
		configHomeDir: func() string {
			return ""
		},
		editConfigTables: func(ifMatch string, edits []config.TableEdit) (config.ConfigFile, error) {
			return config.ConfigFile{}, errors.New("configuration is unavailable")
		},
		restoreConfigFile: func(published, before config.ConfigFile) (config.ConfigFile, error) {
			return config.ConfigFile{}, errors.New("configuration is unavailable")
		},
		setup: setup,
	}
	if len(contexts) > 0 {
		return personProviderDepsForContext(contexts[0], deps)
	}
	return deps
}

func defaultPersonProviderCommandDepsForContext(ctx context.Context) personProviderCommandDeps {
	deps := defaultPersonProviderCommandDeps()
	return personProviderDepsForContext(ctx, deps)
}

func personProviderDepsForContext(ctx context.Context, deps personProviderCommandDeps) personProviderCommandDeps {
	if invocationFromContext(ctx) == nil || deps.bind == nil {
		return deps
	}
	return deps.bind(deps, ctx)
}

func newPersonProviderCommand(deps personProviderCommandDeps) *cobra.Command {
	provider := &cobra.Command{
		Use:   personProviderCommandName,
		Short: "Manage people-sweep inference",
	}
	provider.AddCommand(
		newPersonProviderAddCommand(deps),
		newPersonProviderCodexEnrollCommand(defaultCodexEnrollDeps()),
		newPersonProviderSetCommand(deps),
		newPersonProviderRemoveCommand(deps),
		newPersonProviderListCommand(deps),
		newPersonProviderUseCommand(deps),
		newPersonProviderStatusCommand(deps),
		newPersonProviderConsentCommand(deps),
		newPersonProviderReverifyCommand(deps),
		newPersonProviderRevokeCommand(deps),
		newPersonProviderHistoryCommand(deps),
		newPersonProviderCheckCommand(deps),
		newPersonProviderLoginCommand(deps),
		newPersonProviderModelsCommand(deps),
	)
	return provider
}

func newPersonProviderReverifyCommand(deps personProviderCommandDeps) *cobra.Command {
	var confirmed bool
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "reverify [name]",
		Short: "Re-run the exact provider check and consent",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			runDeps := deps
			if len(args) == 1 {
				var err error
				runDeps, err = personProviderDepsForName(deps, args[0])
				if err != nil {
					return err
				}
			}
			return runPersonProviderReverify(command, runDeps, confirmed, jsonOutput)
		},
	}
	command.Flags().BoolVar(&confirmed, "yes", false, "Confirm the disclosed provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func exactPersonProviderNameArgs(command *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(command, args); err != nil {
		return err
	}
	return peoplesweep.ValidateProviderProfileName(args[0])
}

func optionalPersonProviderNameArgs(command *cobra.Command, args []string) error {
	if err := cobra.MaximumNArgs(1)(command, args); err != nil {
		return err
	}
	if len(args) == 1 {
		return peoplesweep.ValidateProviderProfileName(args[0])
	}
	return nil
}

func newPersonProviderStatusCommand(deps personProviderCommandDeps) *cobra.Command {
	var all bool
	var jsonOutput bool
	var semanticEmbeddings bool
	command := &cobra.Command{
		Use:   "status [name]",
		Short: "Show the exact people inference policy and consent state",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			runDeps := deps
			if len(args) == 1 {
				if all || semanticEmbeddings {
					return errors.New("a named people provider cannot be combined with --all or --semantic-embeddings")
				}
				var err error
				runDeps, err = personProviderDepsForName(deps, args[0])
				if err != nil {
					return err
				}
			}
			return runPersonProviderStatus(command, runDeps, all, jsonOutput, semanticEmbeddings)
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Show every stored provider policy and consent state")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().BoolVar(&semanticEmbeddings, "semantic-embeddings", false,
		"Select the curated-person semantic embedding policy")
	return command
}

func newPersonProviderListCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "list",
		Short: "List named people inference provider profiles",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			return runPersonProviderList(command, deps, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderUseCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "use <name>",
		Short: "Select an exactly checked people inference provider profile",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			return runPersonProviderUse(command, deps, args[0], jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderRemoveCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "remove <name>",
		Short: "Remove a named people inference provider profile",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			return runPersonProviderRemove(command, deps, args[0], jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func runPersonProviderList(
	command *cobra.Command,
	deps personProviderCommandDeps,
	jsonOutput bool,
) error {
	configured := deps.config()
	names := make([]string, 0, len(configured.Providers))
	for name := range configured.Providers {
		names = append(names, name)
	}
	slices.Sort(names)
	output := personProviderListOutput{Profiles: make([]personProviderListItem, 0, len(names))}
	for _, name := range names {
		provider := configured.Providers[name]
		output.Profiles = append(output.Profiles, personProviderListItem{
			Name: name, Active: name == configured.Provider.Name,
			Protocol: provider.Protocol, Endpoint: provider.Endpoint, Model: provider.Model,
			Auth: provider.Auth, Credential: provider.Credential,
			CredentialEnv: provider.CredentialEnv,
		})
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), output, json.Deterministic(true))
	}
	for _, item := range output.Profiles {
		selected := ""
		if item.Active {
			selected = " (active)"
		}
		_, _ = fmt.Fprintf(command.OutOrStdout(), "%s%s\t%s\t%s\t%s\n",
			item.Name, selected, item.Protocol, item.Endpoint, item.Model)
	}
	return nil
}

// rejectRemotePersonProviderMutation refuses people provider config
// mutations that a configured remote daemon can never observe. That daemon
// reads its own host's config file and runs its scheduled sweeps from the
// configuration captured at its startup, so a local edit here would report
// success while every sweep on the remote daemon keeps the previous
// selection. Mirrors the person provider add refusal: run the mutation on
// the daemon host, or target a daemon on this machine with --local.
func rejectRemotePersonProviderMutation(deps personProviderCommandDeps, operation string) error {
	if deps.remoteConfigured == nil || !deps.remoteConfigured() {
		return nil
	}
	return fmt.Errorf(
		"people provider %s cannot run against a configured remote daemon: the change would be written to this machine's config file, which the remote daemon never reads; run this command on the daemon host, or pass --local to configure a daemon on this machine",
		operation)
}

// personProviderMutationScope resolves how a local people provider config
// mutation relates to a running daemon. directStore reports whether this
// process may mutate the store directly; daemonRunning reports whether a
// daemon process keeps serving the people sweep configuration it captured
// at startup and therefore must restart before its scheduled sweeps observe
// the mutation. The two signals are deliberately separate: ownership stays
// compatibility-sensitive because it routes store operations (nothing may
// be proxied to an API-incompatible daemon), while a daemon left live across
// a CLI upgrade fails that compatibility check yet still serves stale
// startup config. Removal must reject directStore && daemonRunning; other
// mutations use restart guidance from the compatibility-
// agnostic liveness probe. With no ownership signal at all, remove's
// existing convention is preserved: assume the store is daemon-owned.
func personProviderMutationScope(
	ctx context.Context,
	deps personProviderCommandDeps,
) (directStore bool, daemonRunning bool, err error) {
	if deps.isDaemonSubprocess != nil && deps.isDaemonSubprocess() {
		return true, true, nil
	}
	if deps.providerStoreOwnedByDaemon == nil {
		return false, false, nil
	}
	owned, ownershipErr := deps.providerStoreOwnedByDaemon(ctx)
	if ownershipErr != nil {
		return false, false, ownershipErr
	}
	// A compatible owning daemon is alive by definition; only a negative
	// ownership result needs the separate liveness check to catch an
	// API-incompatible daemon still holding the startup config.
	if owned {
		return false, true, nil
	}
	if deps.daemonAliveForRestartNotice == nil {
		return true, false, nil
	}
	alive, aliveErr := deps.daemonAliveForRestartNotice(ctx)
	if aliveErr != nil {
		return false, false, aliveErr
	}
	return true, alive, nil
}

// writePersonProviderDaemonRestartNotice explains the daemon-side effect of
// a successful local people provider config mutation: the config file is
// updated immediately and proxied CLI operations re-read it, but a running
// daemon's scheduled sweeps keep the startup snapshot, so the daemon must
// restart before they observe the change. There is deliberately no live
// config reload for scheduled jobs; an explicit restart is the documented
// way to publish the change.
func writePersonProviderDaemonRestartNotice(w io.Writer) {
	_, _ = fmt.Fprintln(w,
		"A running daemon keeps the people sweep config it started with; run `msgvault daemon restart` so scheduled sweeps observe this change.")
}

func runPersonProviderSet(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	options personProviderSetOptions,
) error {
	if err := rejectRemotePersonProviderMutation(deps, "set"); err != nil {
		return err
	}
	if !options.confirmed {
		return errors.New("people provider set requires --yes after reviewing the final values")
	}
	if !personProviderSetHasMutableChanges(command) {
		return errors.New("people provider set requires at least one mutable policy flag")
	}
	if deps.readConfigFile == nil || deps.editConfigTables == nil || deps.restoreConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	configured, err := personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	provider, exists := configured.Providers[name]
	if !exists {
		return fmt.Errorf("people provider profile %q is not configured", name)
	}
	oldConfig, err := selectPersonProviderConfig(configured, name)
	if err != nil {
		return err
	}
	oldConfig.Enabled = true
	oldProfile, err := oldConfig.Profile()
	if err != nil {
		return err
	}

	replacement := provider
	applyPersonProviderSetOptions(command, &replacement, options)
	proposed := personProviderProposedConfig(configured, name, replacement)
	proposedProfile, err := proposed.Profile()
	if err != nil {
		return err
	}
	if err := config.ValidateConfigTableEdits(before, []config.TableEdit{
		personProviderProfileUpdateEdit(name, replacement),
	}); err != nil {
		return err
	}
	directStore, daemonRunning, err := personProviderMutationScope(command.Context(), deps)
	if err != nil {
		return err
	}
	credential, err := readExistingPersonProviderCredential(deps.setup, name, proposedProfile)
	if err != nil {
		return err
	}
	if replacement.Protocol != peoplesweep.ProtocolCodexAppServer {
		if deps.setup.negotiate == nil {
			return errors.New("people provider capability negotiation is unavailable")
		}
		capabilities, negotiateErr := deps.setup.negotiate(command.Context(), replacement, credential)
		if negotiateErr != nil {
			return negotiateErr
		}
		replacement.OutputMode = capabilities.OutputMode
		replacement.TokenLimitParameter = capabilities.TokenLimitParameter
		replacement.ReasoningEffort = capabilities.ReasoningEffort
		replacement.ReasoningMode = capabilities.ReasoningMode
		replacement.DriverVersion = capabilities.DriverVersion
	}
	proposed = personProviderProposedConfig(configured, name, replacement)
	proposedProfile, err = proposed.Profile()
	if err != nil {
		return err
	}
	if err := config.ValidateConfigTableEdits(before, []config.TableEdit{
		personProviderProfileUpdateEdit(name, replacement),
	}); err != nil {
		return err
	}

	if err := revokePersonProviderSetConsent(command, deps, name, oldProfile.Fingerprint, directStore, true); err != nil {
		return err
	}

	after, err := deps.editConfigTables(before.ETag, []config.TableEdit{
		personProviderProfileUpdateEdit(name, replacement),
	})
	if err != nil {
		return errors.Join(err, rollbackPersonProviderSetConfig(deps, after, before),
			errors.New("exact people provider consent remains revoked"))
	}
	if err := revokePersonProviderSetConsent(command, deps, name, oldProfile.Fingerprint, directStore, false); err != nil {
		return errors.Join(err, rollbackPersonProviderSetConfig(deps, after, before),
			errors.New("exact people provider consent remains revoked"))
	}
	checkedConfig, err := personProviderConfigFromSnapshot(deps, after)
	if err == nil {
		checkedDeps := deps
		checkedDeps.config = func() peoplesweep.Config { return checkedConfig }
		checkOutput := command.OutOrStdout()
		if options.jsonOutput {
			checkOutput = io.Discard
		}
		err = executeSavedPersonProviderCheck(command, checkedDeps, name, proposedProfile.Fingerprint, checkOutput)
	}
	if err != nil {
		return errors.Join(err, rollbackPersonProviderSetConfig(deps, after, before),
			errors.New("exact people provider consent remains revoked"))
	}
	if options.jsonOutput {
		checkedProvider, err := selectPersonProviderConfig(checkedConfig, name)
		if err != nil {
			return err
		}
		checkedProvider.Enabled = true
		checkedProfile, err := checkedProvider.Profile()
		if err != nil {
			return err
		}
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderSetOutput{
			Name: name, Fingerprint: checkedProfile.Fingerprint, Checked: true,
			DaemonRestartRequired: daemonRunning,
		}, json.Deterministic(true))
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Updated and checked people provider profile %q; run `msgvault person provider consent %q --yes` to grant consent for the new policy.\n",
		name, name)
	if daemonRunning {
		writePersonProviderDaemonRestartNotice(command.OutOrStdout())
	}
	return nil
}

func personProviderSetHasMutableChanges(command *cobra.Command) bool {
	for _, name := range personProviderSetMutableFlags {
		if command.Flags().Changed(name) {
			return true
		}
	}
	return false
}

func revokePersonProviderSetConsent(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name, fingerprint string,
	directStore bool,
	guard bool,
) error {
	if directStore {
		if deps.openStore == nil {
			return errors.New("people provider consent store is unavailable")
		}
		st, cleanup, err := deps.openStore()
		if err != nil {
			return err
		}
		_, revokeErr := st.RevokePersonInferenceConsent(
			command.Context(), fingerprint, personProviderConsentActor,
		)
		cleanup()
		return revokeErr
	}
	if guard {
		return proxySavedPersonProviderOperation(command, deps, "revoke", name, fingerprint, io.Discard)
	}
	return proxySavedPersonProviderRevokeFingerprint(command, deps, name, fingerprint)
}

func rollbackPersonProviderSetConfig(
	deps personProviderCommandDeps,
	published, before config.ConfigFile,
) error {
	if !published.Exists || published.ETag == "" {
		return errors.New("cannot roll back people provider config without a verified published snapshot")
	}
	if _, err := deps.restoreConfigFile(published, before); err != nil {
		return fmt.Errorf("restore people provider config: %w", err)
	}
	return nil
}

func runPersonProviderUse(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	jsonOutput bool,
) error {
	if err := rejectRemotePersonProviderMutation(deps, "use"); err != nil {
		return err
	}
	if deps.readConfigFile == nil || deps.editConfigTables == nil {
		return errors.New("people provider config editing is unavailable")
	}
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	configured, err := personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	selected, err := selectPersonProviderConfig(configured, name)
	if err != nil {
		return err
	}
	selected.Enabled = true
	profile, err := selected.Profile()
	if err != nil {
		return err
	}
	if deps.openReadStore == nil {
		return errors.New("people provider check store is unavailable")
	}
	st, cleanup, err := deps.openReadStore()
	if err != nil {
		return err
	}
	defer cleanup()
	checked, err := st.HasSuccessfulPersonInferenceCheck(command.Context(), profile.Fingerprint)
	if err != nil {
		return err
	}
	if !checked {
		return fmt.Errorf("people provider profile %q requires an exact successful check before selection", name)
	}
	// Resolve daemon ownership before mutating config so a failed runtime
	// probe cannot strand a published selection without its restart advice.
	_, daemonRunning, err := personProviderMutationScope(command.Context(), deps)
	if err != nil {
		return err
	}
	if _, err := deps.editConfigTables(before.ETag, planPersonProviderUseEdits(name)); err != nil {
		return err
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderUseOutput{
			Name: name, Fingerprint: profile.Fingerprint, Enabled: true,
			DaemonRestartRequired: daemonRunning,
		}, json.Deterministic(true))
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Selected people provider profile %q.\n", name)
	if daemonRunning {
		writePersonProviderDaemonRestartNotice(command.OutOrStdout())
	}
	return nil
}

// planPersonProviderUseEdits records the selection edit for `person provider
// use`: enable the people sweep and select the exact checked profile.
func planPersonProviderUseEdits(name string) []config.TableEdit {
	return []config.TableEdit{{
		Path: []string{"people", "sweep"}, Values: map[string]any{"enabled": true, "provider": name},
	}}
}

func runPersonProviderRemove(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	jsonOutput bool,
) error {
	if err := rejectRemotePersonProviderMutation(deps, "remove"); err != nil {
		return err
	}
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	if deps.readConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}
	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	directStore, daemonRunning, err := personProviderMutationScope(command.Context(), deps)
	if err != nil {
		return err
	}
	if !directStore {
		if deps.removeWithDaemon == nil {
			return errors.New("people provider daemon removal is unavailable")
		}
		if err := deps.removeWithDaemon(command.Context(), name, before.ETag); err != nil {
			return err
		}
		return writePersonProviderRemoved(command, name, true, jsonOutput)
	}
	if daemonRunning {
		return errors.New("cannot identify the running people provider policy; stop the daemon before removing a profile")
	}
	if deps.editConfigTables == nil || deps.restoreConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}
	configured, err := personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	provider, exists := configured.Providers[name]
	if !exists {
		return fmt.Errorf("people provider profile %q is not configured", name)
	}
	active := configured.Provider.Name == name
	if active && configured.Enabled {
		return fmt.Errorf("cannot remove active people provider profile %q while people sweep is enabled", name)
	}
	profileConfig := configured
	profileConfig.Enabled = true
	profileConfig.Provider = peoplesweep.ProviderSelection{Name: name}
	profile, err := profileConfig.Profile()
	if err != nil {
		return err
	}
	edits := make([]config.TableEdit, 0, 2)
	if active {
		names := make([]string, 0, len(configured.Providers)-1)
		for candidate := range configured.Providers {
			if candidate != name {
				names = append(names, candidate)
			}
		}
		slices.Sort(names)
		if len(names) == 0 {
			return errors.New("cannot remove the only configured people provider profile")
		}
		edits = append(edits, config.TableEdit{
			Path: []string{"people", "sweep"}, Values: map[string]any{"provider": names[0]},
		})
	}
	edits = append(edits, config.TableEdit{
		Path: []string{"people", "sweep", "providers", name}, Remove: true,
	})
	if err := config.ValidateConfigTableEdits(before, edits); err != nil {
		return err
	}
	var credentials peoplesweep.CredentialStore
	var credentialRevision string
	if provider.Credential == peoplesweep.CredentialStored {
		credentials, err = deps.setup.resolveCredentialStore()
		if err != nil {
			return err
		}
		var present bool
		credentialRevision, present, err = credentials.Revision(name, provider.Endpoint)
		if err == nil && !present {
			err = fmt.Errorf("%w for profile %q", peoplesweep.ErrCredentialNotFound, name)
		}
		if err != nil {
			return fmt.Errorf("preflight stored people provider credential deletion: %w", err)
		}
	}
	after, err := deps.editConfigTables(before.ETag, edits)
	if err != nil {
		if errors.Is(err, config.ErrConfigChanged) {
			return errors.Join(err, restoreRemovedPersonProviderConfig(deps, after, before))
		}
		return err
	}
	rollback := func(cause error) error {
		return errors.Join(cause, restoreRemovedPersonProviderConfig(deps, after, before))
	}

	st, cleanup, openErr := deps.openStore()
	if openErr != nil {
		return rollback(openErr)
	}
	defer cleanup()
	if _, err := st.RevokePersonInferenceConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return rollback(err)
	}
	if _, err := st.InvalidatePersonInferenceCheck(command.Context(), profile.Fingerprint); err != nil {
		return rollback(err)
	}
	if provider.Credential == peoplesweep.CredentialStored {
		if _, err := credentials.DeleteIfRevision(name, provider.Endpoint, credentialRevision); err != nil {
			restoreErr := restoreRemovedPersonProviderConfig(deps, after, before)
			return errors.Join(err, restoreErr,
				errors.New("exact people provider consent remains revoked"))
		}
	}
	return writePersonProviderRemoved(command, name, false, jsonOutput)
}

func writePersonProviderRemoved(command *cobra.Command, name string, daemonRunning, jsonOutput bool) error {
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderRemoveOutput{
			Name: name, Removed: true, DaemonRestartRequired: daemonRunning,
		}, json.Deterministic(true))
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Removed people provider profile %q; audit history was retained.\n", name)
	if daemonRunning {
		writePersonProviderDaemonRestartNotice(command.OutOrStdout())
	}
	return nil
}

func restoreRemovedPersonProviderConfig(
	deps personProviderCommandDeps,
	after, before config.ConfigFile,
) error {
	if _, err := deps.restoreConfigFile(after, before); err != nil {
		return fmt.Errorf("restore removed people provider config: %w", err)
	}
	return nil
}

// removePersonProviderWithDaemon uses the Settings operation so revocation can
// include the policy captured by the daemon at startup. Never auto-start or
// fall back to a local write if the owning daemon cannot perform the removal.
func removePersonProviderWithDaemon(ctx context.Context, name, ifMatch string) error {
	state := invocationFromContext(ctx)
	if state == nil || state.cfg == nil {
		return errors.New("configuration is unavailable")
	}
	currentCfg := state.cfg
	runtime, err := findCompatibleDaemonRuntimeContext(ctx, currentCfg.Data.DataDir)
	if err != nil {
		return err
	}
	if runtime == nil {
		return errors.New("people provider daemon is unavailable; retry after stopping or restarting it")
	}
	if err := probeLocalDaemonAuth(ctx, runtime, currentCfg); err != nil {
		return err
	}
	client, err := localDaemonAPIClient(urlFromDaemonRuntime(runtime), currentCfg.Server.APIKey)
	if err != nil {
		return err
	}
	response, err := client.DeleteSettingsPeopleInferenceProviderWithResponse(ctx,
		&generated.DeleteSettingsPeopleInferenceProviderRequestOptions{
			PathParams: &generated.DeleteSettingsPeopleInferenceProviderPath{Name: name},
			Header:     &generated.DeleteSettingsPeopleInferenceProviderHeaders{IfMatch: ifMatch},
		})
	if err != nil || response == nil || response.StatusCode != http.StatusOK {
		return fmt.Errorf("remove people provider: %w", daemonclient.APIResponseError(response, err))
	}
	return nil
}

func newPersonProviderConsentCommand(deps personProviderCommandDeps) *cobra.Command {
	var confirmed bool
	var jsonOutput bool
	var semanticEmbeddings bool
	command := &cobra.Command{
		Use:   "consent [name]",
		Short: "Consent to the exact people inference policy",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			runDeps := deps
			if len(args) == 1 {
				if semanticEmbeddings {
					return errors.New("a named people provider cannot be combined with --semantic-embeddings")
				}
				var err error
				runDeps, err = personProviderDepsForName(deps, args[0])
				if err != nil {
					return err
				}
			}
			return runPersonProviderConsent(command, runDeps, confirmed, jsonOutput, semanticEmbeddings)
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
	var ifFingerprint string
	var fingerprint string
	command := &cobra.Command{
		Use:   "revoke [name]",
		Short: "Revoke consent for the exact people inference policy",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if fingerprint != "" {
				if err := validatePersonProviderFingerprint(fingerprint); err != nil {
					return err
				}
				if len(args) != 1 || all || semanticEmbeddings {
					return errors.New("--fingerprint requires one named people provider revoke")
				}
			}
			if ifFingerprint != "" {
				if err := validatePersonProviderFingerprint(ifFingerprint); err != nil {
					return err
				}
				if len(args) != 1 || all || semanticEmbeddings {
					return errors.New("--if-fingerprint requires one named people provider revoke")
				}
			}
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			runDeps := deps
			if len(args) == 1 {
				if all || semanticEmbeddings {
					return errors.New("a named people provider cannot be combined with --all or --semantic-embeddings")
				}
				var err error
				runDeps, err = personProviderDepsForName(deps, args[0])
				if err != nil {
					return err
				}
				if ifFingerprint != "" {
					guarded := runDeps.config()
					guarded.Enabled = true
					profile, profileErr := guarded.Profile()
					if profileErr != nil {
						return profileErr
					}
					if profile.Fingerprint != ifFingerprint {
						return errors.New("people provider profile changed since removal began")
					}
				}
				if fingerprint != "" {
					return runPersonProviderRevokeFingerprint(command, runDeps, fingerprint, jsonOutput)
				}
			}
			if fingerprint != "" {
				return errors.New("--fingerprint requires one named people provider revoke")
			}
			return runPersonProviderRevoke(command, runDeps, all, jsonOutput, semanticEmbeddings, ifFingerprint != "")
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Revoke consent for every stored provider policy")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().BoolVar(&semanticEmbeddings, "semantic-embeddings", false,
		"Select the curated-person semantic embedding policy")
	command.Flags().StringVar(&ifFingerprint, personProviderIfFingerprintFlag, "",
		"Require an exact provider fingerprint")
	_ = command.Flags().MarkHidden(personProviderIfFingerprintFlag)
	command.Flags().StringVar(&fingerprint, "fingerprint", "", "Revoke a specific provider fingerprint")
	_ = command.Flags().MarkHidden("fingerprint")
	return command
}

func validatePersonProviderFingerprint(fingerprint string) error {
	if len(fingerprint) != 64 {
		return errors.New("invalid people provider fingerprint")
	}
	decoded, err := hex.DecodeString(fingerprint)
	if err != nil || len(decoded) != 32 || strings.ToLower(fingerprint) != fingerprint {
		return errors.New("invalid people provider fingerprint")
	}
	return nil
}

func newPersonProviderHistoryCommand(deps personProviderCommandDeps) *cobra.Command {
	var personID int64
	var limit int
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "history [name]",
		Short: "Show redacted sweep history for an optional provider profile",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				return deps.proxy(command, args, nil)
			}
			if limit < 1 || limit > maxPersonSweepHistoryLimit {
				return fmt.Errorf("--limit must be between 1 and %d", maxPersonSweepHistoryLimit)
			}
			fingerprint := ""
			if len(args) == 1 {
				selected, err := selectPersonProviderConfig(deps.config(), args[0])
				if err != nil {
					return err
				}
				selected.Enabled = true
				profile, err := selected.Profile()
				if err != nil {
					return err
				}
				fingerprint = profile.Fingerprint
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			runs, err := st.ListPersonSweepRuns(command.Context(), peoplesweep.RunFilter{
				PersonID: personID, ProviderFingerprint: fingerprint, Limit: limit,
			})
			if err != nil {
				return err
			}
			attempts, err := st.ListPersonSweepAttempts(command.Context(), peoplesweep.AttemptFilter{
				PersonID: personID, ProviderFingerprint: fingerprint, Limit: limit,
			})
			if err != nil {
				return err
			}
			return writePersonSweepHistory(command.OutOrStdout(), safePersonSweepHistory(runs, attempts), jsonOutput)
		},
	}
	command.Flags().Int64Var(&personID, "person", 0, "Filter by durable person ID")
	command.Flags().IntVar(&limit, "limit", 20, "Maximum runs and attempts")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderCheckCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	var ifFingerprint string
	command := &cobra.Command{
		Use:   "check [name]",
		Short: "Run a fixed synthetic request through the people inference provider",
		Args:  optionalPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			if !deps.isDaemonSubprocess() {
				if deps.providerStoreOwnedByDaemon != nil {
					owned, err := deps.providerStoreOwnedByDaemon(command.Context())
					if err != nil {
						return err
					}
					if !owned {
						name := ""
						if len(args) == 1 {
							name = args[0]
						}
						return runPersonProviderCheck(command, deps, name, ifFingerprint, jsonOutput)
					}
				}
				return deps.proxy(command, args, nil)
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			return runPersonProviderCheck(command, deps, name, ifFingerprint, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	command.Flags().StringVar(&ifFingerprint, personProviderIfFingerprintFlag, "",
		"Require an exact provider fingerprint")
	_ = command.Flags().MarkHidden(personProviderIfFingerprintFlag)
	return command
}

func newPersonProviderLoginCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "login",
		Short: "Start Codex ChatGPT device-code login",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			return runPersonProviderLogin(command, deps, jsonOutput)
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderModelsCommand(deps personProviderCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use:   "models",
		Short: "List Codex models and reasoning efforts",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, args []string) error {
			deps := personProviderDepsForContext(command.Context(), deps)
			return runPersonProviderModels(command, deps, jsonOutput)
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
	var codexIsolation *personProviderCodexIsolationStatus
	var profileName string
	if !all {
		name, provider, err := deps.config().ActiveProviderConfig()
		if err != nil {
			return err
		}
		profileName = name
		if provider.Protocol == peoplesweep.ProtocolCodexAppServer {
			boundary := provider.ExecutionBoundary
			_, err := currentPersonProviderCodexClient(deps)
			if err != nil {
				if !errors.Is(err, peoplesweep.ErrCodexIsolationUnreleased) {
					return err
				}
				codexIsolation = &personProviderCodexIsolationStatus{
					ExecutionBoundary: boundary,
					Reason:            peoplesweep.ErrCodexIsolationUnreleased.Error(),
				}
			} else {
				codexIsolation = &personProviderCodexIsolationStatus{
					Available: true, ExecutionBoundary: boundary,
				}
			}
		}
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
	output, err := personProviderStatusFor(command.Context(), st, profile)
	if err != nil {
		return err
	}
	output.Name = profileName
	output.CodexIsolation = codexIsolation
	output.StaleProgramCheck, output.StaleProgramConsent, err = personProviderStaleProgramState(
		command.Context(), st, profile, output,
	)
	if err != nil {
		return err
	}
	return writePersonProviderStatus(command.OutOrStdout(), output, jsonOutput)
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
	if jsonOutput {
		output, err := personProviderStatusFor(command.Context(), st, profile)
		if err != nil {
			return err
		}
		return writePersonProviderStatus(command.OutOrStdout(), output, true)
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
	invalidateCheck bool,
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
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderRevokeAllOutput{
				Revoked: revoked, Profiles: statuses,
			}, json.Deterministic(true))
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
	// Fingerprint-guarded revocation precedes profile replacement.
	// Its capability proof must not survive publication of a new credential.
	if invalidateCheck {
		if _, err := st.InvalidatePersonInferenceCheck(command.Context(), profile.Fingerprint); err != nil {
			return err
		}
	}
	if jsonOutput {
		output, err := personProviderStatusFor(command.Context(), st, profile)
		if err != nil {
			return err
		}
		return writePersonProviderStatus(command.OutOrStdout(), output, true)
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent revoked for %s\n", profile.Fingerprint)
	return nil
}

func runPersonProviderRevokeFingerprint(
	command *cobra.Command,
	deps personProviderCommandDeps,
	fingerprint string,
	jsonOutput bool,
) error {
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	revoked, err := st.RevokePersonInferenceConsent(
		command.Context(), fingerprint, personProviderConsentActor,
	)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderRevokeFingerprintOutput{
			Fingerprint: fingerprint, Revoked: revoked,
		}, json.Deterministic(true))
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(), "Consent revoked for %s\n", fingerprint)
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
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personSemanticProviderRevokeAllOutput{
				Revoked: revoked, Profiles: statuses,
			}, json.Deterministic(true))
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
	name string,
	ifFingerprint string,
	jsonOutput bool,
) error {
	if err := verifyPersonProviderFingerprint(deps, name, ifFingerprint); err != nil {
		return err
	}
	output, err := checkPersonProvider(command, deps, name)
	if err != nil {
		return err
	}
	return writePersonProviderCheckOutput(command.OutOrStdout(), output, jsonOutput)
}

func runPersonProviderReverify(
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
		return errors.New("people inference reverify requires --yes after reviewing the provider disclosure")
	}
	if _, err := checkPersonProvider(command, deps, ""); err != nil {
		return err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	if _, _, err := st.GrantPersonInferenceConsent(
		command.Context(), profile.Fingerprint, personProviderConsentActor,
	); err != nil {
		return err
	}
	if jsonOutput {
		output, err := personProviderStatusFor(command.Context(), st, profile)
		if err != nil {
			return err
		}
		return writePersonProviderStatus(command.OutOrStdout(), output, true)
	}
	printPersonProviderDisclosure(command.OutOrStdout(), profile)
	_, _ = fmt.Fprintf(command.OutOrStdout(), "People inference provider reverified (fingerprint=%s).\n", profile.Fingerprint)
	return nil
}

// checkPersonProvider runs the synthetic capability check for one exact
// profile and records it. It performs no consent grant.
func checkPersonProvider(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
) (personProviderCheckOutput, error) {
	config := deps.config()
	if name != "" {
		var err error
		config, err = selectPersonProviderConfig(config, name)
		if err != nil {
			return personProviderCheckOutput{}, err
		}
		config.Enabled = true
	}
	profile, err := config.Profile()
	if err != nil {
		return personProviderCheckOutput{}, err
	}
	st, cleanup, err := deps.openStore()
	if err != nil {
		return personProviderCheckOutput{}, err
	}
	defer cleanup()
	checker, err := deps.newChecker(config, st, deps.setup)
	if err != nil {
		return personProviderCheckOutput{}, err
	}
	response, err := checker.Check(command.Context())
	if err != nil {
		return personProviderCheckOutput{}, err
	}
	// Codex app-server profiles configure the bare codex driver family while
	// the driver attests "<family>:<attestation-digest>"; eligibility accepts
	// that attested identity for its family and still rejects unsafe values.
	if !peoplesweep.DriverVersionMatches(profile.DriverVersion, response.ProviderVersion) {
		return personProviderCheckOutput{}, errors.New("people inference provider check returned a mismatched driver version")
	}
	if _, err := st.EnsurePersonInferenceProfile(command.Context(), profile); err != nil {
		return personProviderCheckOutput{}, err
	}
	if err := st.RecordPersonInferenceCheck(command.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          time.Now().UTC(),
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ProviderRequestID:  response.ProviderRequestID,
		ModelVersion:       response.ModelVersion,
	}); err != nil {
		return personProviderCheckOutput{}, err
	}
	return personProviderCheckOutput{
		OK: true, ProviderRequestID: response.ProviderRequestID,
		Model: profile.Model, Usage: response.Usage,
	}, nil
}

func writePersonProviderCheckOutput(w io.Writer, output personProviderCheckOutput, jsonOutput bool) error {
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(w), output, json.Deterministic(true))
	}
	_, _ = fmt.Fprintf(w,
		"People inference provider check succeeded (model=%s, request_id=%s, input_tokens=%d, output_tokens=%d).\n",
		output.Model, output.ProviderRequestID, output.Usage.InputTokens, output.Usage.OutputTokens)
	return nil
}

func selectPersonProviderConfig(config peoplesweep.Config, name string) (peoplesweep.Config, error) {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return peoplesweep.Config{}, err
	}
	if _, ok := config.Providers[name]; !ok {
		return peoplesweep.Config{}, fmt.Errorf("people provider profile %q is not configured", name)
	}
	config.Provider = peoplesweep.ProviderSelection{Name: name}
	return config, nil
}

func personProviderDepsForName(
	deps personProviderCommandDeps,
	name string,
) (personProviderCommandDeps, error) {
	selected, err := selectPersonProviderConfig(deps.config(), name)
	if err != nil {
		return personProviderCommandDeps{}, err
	}
	selected.Enabled = true
	deps.config = func() peoplesweep.Config { return selected }
	return deps, nil
}

func runPersonProviderLogin(
	command *cobra.Command,
	deps personProviderCommandDeps,
	jsonOutput bool,
) error {
	client, err := currentPersonProviderCodexClient(deps)
	if err != nil {
		return err
	}
	return client.StartDeviceLogin(command.Context(), func(login peoplesweep.DeviceLogin) error {
		if jsonOutput {
			return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), login, json.Deterministic(true))
		}
		_, _ = fmt.Fprintf(command.OutOrStdout(), "Verification URL: %s\n", login.VerificationURL)
		_, _ = fmt.Fprintf(command.OutOrStdout(), "User code: %s\n", login.UserCode)
		_, _ = fmt.Fprintf(command.OutOrStdout(), "Expires: %s\n", login.ExpiresAt.UTC().Format(time.RFC3339))
		return nil
	})
}

func runPersonProviderModels(
	command *cobra.Command,
	deps personProviderCommandDeps,
	jsonOutput bool,
) error {
	client, err := currentPersonProviderCodexClient(deps)
	if err != nil {
		return err
	}
	models, err := client.ListModels(command.Context())
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(command.OutOrStdout()), personProviderModelsOutput{Models: models}, json.Deterministic(true))
	}
	for _, model := range models {
		_, _ = fmt.Fprintf(command.OutOrStdout(),
			"%s\t%s\tdefault=%s\tsupported=%s\n",
			model.ID,
			model.DisplayName,
			model.DefaultReasoningEffort,
			strings.Join(model.SupportedEfforts, ", "),
		)
	}
	return nil
}

func currentPersonProviderCodexClient(
	deps personProviderCommandDeps,
) (personProviderCodexClient, error) {
	config := deps.config()
	_, provider, err := config.ActiveProviderConfig()
	if err != nil {
		return nil, err
	}
	if provider.Protocol != peoplesweep.ProtocolCodexAppServer {
		return nil, errors.New("person provider login and models require codex_app_server")
	}
	if _, err := config.Profile(); err != nil {
		return nil, err
	}
	if deps.newCodexClient == nil {
		return nil, errors.New("codex app-server operations are unavailable")
	}
	return deps.newCodexClient(config, deps.setup)
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

// personProviderStatusFor assembles the exact policy, its recorded synthetic
// check, and its consent state so operators and agents can see every gate a
// sweep must pass for this profile.
func personProviderStatusFor(
	ctx context.Context,
	st personProviderStore,
	profile peoplesweep.ProviderProfile,
) (personProviderStatusOutput, error) {
	check, err := st.GetPersonInferenceCheck(ctx, profile.Fingerprint)
	if err != nil {
		return personProviderStatusOutput{}, err
	}
	status, err := st.GetPersonInferenceConsentStatus(ctx, profile.Fingerprint)
	if err != nil {
		return personProviderStatusOutput{}, err
	}
	if status == nil {
		return personProviderStatusOutput{}, errors.New("people inference consent status is empty")
	}
	return personProviderStatusOutput{Profile: profile, Check: check, Consent: *status}, nil
}

func personProviderStaleProgramState(
	ctx context.Context,
	st personProviderStore,
	current peoplesweep.ProviderProfile,
	status personProviderStatusOutput,
) (bool, bool, error) {
	profiles, err := st.ListPersonInferenceProfiles(ctx)
	if err != nil {
		return false, false, err
	}
	staleCheck, staleConsent := false, false
	for _, historical := range profiles {
		if historical.ProgramFingerprint == current.ProgramFingerprint ||
			historical.Fingerprint == current.Fingerprint ||
			!samePersonProviderPolicyExceptProgram(current, historical) {
			continue
		}
		historicalCheck, err := st.GetPersonInferenceCheck(ctx, historical.Fingerprint)
		if err != nil {
			return false, false, err
		}
		historicalConsent, err := st.GetPersonInferenceConsentStatus(ctx, historical.Fingerprint)
		if err != nil {
			return false, false, err
		}
		if historicalCheck != nil && status.Check == nil {
			staleCheck = true
		}
		if historicalConsent != nil && historicalConsent.Active && !status.Consent.Active {
			staleConsent = true
		}
	}
	return staleCheck, staleConsent, nil
}

func samePersonProviderPolicyExceptProgram(
	left, right peoplesweep.ProviderProfile,
) bool {
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	typeOfProfile := leftValue.Type()
	for index := range typeOfProfile.NumField() {
		name := typeOfProfile.Field(index).Name
		if name == "Fingerprint" || name == "PolicyJSON" || name == "ProgramFingerprint" {
			continue
		}
		if !reflect.DeepEqual(leftValue.Field(index).Interface(), rightValue.Field(index).Interface()) {
			return false
		}
	}
	return true
}

func writePersonProviderStatus(
	w io.Writer,
	output personProviderStatusOutput,
	jsonOutput bool,
) error {
	if jsonOutput {
		return json.MarshalEncode(jsontext.NewEncoder(w), output, json.Deterministic(true))
	}
	printPersonProviderDisclosure(w, output.Profile)
	if output.Check == nil {
		_, _ = fmt.Fprintln(w, "Check: none")
	} else {
		_, _ = fmt.Fprintf(w, "Check: %s (model_version=%s)\n",
			output.Check.CheckedAt.Format(time.RFC3339), output.Check.ModelVersion)
	}
	if output.StaleProgramCheck {
		_, _ = fmt.Fprintf(w, "Check: a matching record uses a different extraction program; run msgvault person provider reverify %s --yes\n", output.Name)
	}
	state := "inactive"
	if output.Consent.Active {
		state = "active"
	} else if output.Consent.LastRevoked != nil {
		state = "revoked"
	}
	_, _ = fmt.Fprintf(w, "Consent: %s\n", state)
	if output.StaleProgramConsent {
		_, _ = fmt.Fprintf(w, "Consent: a matching grant uses a different extraction program; run msgvault person provider reverify %s --yes\n", output.Name)
	}
	if output.CodexIsolation != nil {
		availability := "unavailable"
		if output.CodexIsolation.Available {
			availability = "available"
		}
		_, _ = fmt.Fprintf(w, "Codex isolation: %s\n", availability)
		_, _ = fmt.Fprintf(w, "Execution boundary: %s\n", output.CodexIsolation.ExecutionBoundary)
		if output.CodexIsolation.Reason != "" {
			_, _ = fmt.Fprintf(w, "Reason: %s\n", output.CodexIsolation.Reason)
		}
	}
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
		return json.MarshalEncode(jsontext.NewEncoder(w), personSemanticProviderStatusOutput{
			Profile: profile, Consent: *status,
		}, json.Deterministic(true))
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
		status, err := personProviderStatusFor(ctx, st, profile)
		if err != nil {
			return nil, err
		}
		statuses = append(statuses, status)
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
		return json.MarshalEncode(jsontext.NewEncoder(w), personProviderStatusesOutput{Profiles: statuses}, json.Deterministic(true))
	}
	if len(statuses) == 0 {
		_, _ = fmt.Fprintln(w, "No stored people inference provider profiles.")
		return nil
	}
	for i, status := range statuses {
		if i > 0 {
			_, _ = fmt.Fprintln(w)
		}
		if err := writePersonProviderStatus(w, status, false); err != nil {
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
		return json.MarshalEncode(jsontext.NewEncoder(w), personSemanticProviderStatusesOutput{Profiles: statuses}, json.Deterministic(true))
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

// personBriefProviderDisclosureLine names the second thing this consent covers:
// the "last time we talked" brief sends recent archive text the person wrote to
// the same provider, under the same profile and the same consent.
const personBriefProviderDisclosureLine = "Person brief: recent verbatim messages the " +
	"person wrote, from the conversation lane only, are sent to the same " +
	"provider under this consent"

func printPersonProviderDisclosure(w io.Writer, profile peoplesweep.ProviderProfile) {
	dateRange := profile.SourceSince + " through " + profile.SourceUntil
	if profile.SourceUntil == "" {
		dateRange = profile.SourceSince + " onward"
	}
	authentication := "anonymous loopback"
	switch profile.Credential {
	case peoplesweep.CredentialEnv:
		authentication = "environment variable " + profile.CredentialRef
	case peoplesweep.CredentialStored:
		authentication = "stored credential (" + string(profile.Auth) + ")"
	case peoplesweep.CredentialNone:
		authentication = "anonymous loopback"
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
	_, _ = fmt.Fprintf(w, "Packet renderer: %s\n", profile.PacketRendererPolicy)
	_, _ = fmt.Fprintf(w, "Extraction program fingerprint: %s\n", profile.ProgramFingerprint)
	_, _ = fmt.Fprintln(w, personBriefProviderDisclosureLine)
	_, _ = fmt.Fprintln(w, "Disclosed packet field classes:")
	for _, field := range profile.DisclosedPacketFields {
		_, _ = fmt.Fprintf(w, "- %s\n", field)
	}
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
