package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
)

const maxProviderCredentialBytes = 16 << 10

type personProviderSetupDeps struct {
	catalog             func(context.Context) ([]peoplesweep.ProviderSuggestion, error)
	negotiate           func(context.Context, peoplesweep.ProviderConfig, peoplesweep.Credential) (peoplesweep.NegotiatedCapabilities, error)
	credentials         peoplesweep.CredentialStore
	openCredentialStore func() (peoplesweep.CredentialStore, error)
	lookupEnv           peoplesweep.CredentialLookup
	isTerminal          func(uintptr) bool
	readMasked          func(*os.File, int) ([]byte, error)
}

type personProviderCreateCredentialStore interface {
	SaveNew(
		profileName string,
		credential peoplesweep.Credential,
	) (peoplesweep.CredentialCleanupGuard, bool, error)
	CleanupNew(profileName string, guard peoplesweep.CredentialCleanupGuard) error
}

type personProviderAddOptions struct {
	custom              bool
	protocol            string
	endpoint            string
	model               string
	auth                string
	credentialEnv       string
	apiKeyStdin         bool
	retentionPosture    string
	trainingPosture     string
	allowedSources      []string
	sourceSince         string
	sourceUntil         string
	allowSensitive      bool
	reasoningEffort     string
	reasoningMode       string
	requestTimeout      time.Duration
	acceptCatalogPrices bool
	confirmed           bool
	jsonOutput          bool
}

type personProviderSetOptions struct {
	model            string
	retentionPosture string
	trainingPosture  string
	allowedSources   []string
	sourceSince      string
	sourceUntil      string
	allowSensitive   bool
	reasoningEffort  string
	reasoningMode    string
	requestTimeout   time.Duration
	confirmed        bool
	jsonOutput       bool
}

type personProviderAddOutput struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	Checked     bool   `json:"checked"`
}

// explicitTransport reports whether the operator supplied every transport
// field the catalog could otherwise fill.
func (o personProviderAddOptions) explicitTransport() bool {
	return o.protocol != "" && o.endpoint != "" && o.model != "" && o.auth != ""
}

// needsCatalog reports whether add must fetch the models.dev catalog: only
// when a transport field is missing or a catalog price hint was requested.
func (o personProviderAddOptions) needsCatalog() bool {
	return !o.custom && (!o.explicitTransport() || o.acceptCatalogPrices)
}

func defaultPersonProviderSetupDeps() personProviderSetupDeps {
	return personProviderSetupDeps{
		catalog: func(ctx context.Context) ([]peoplesweep.ProviderSuggestion, error) {
			return peoplesweep.NewModelsDevClient().Fetch(ctx)
		},
		negotiate: func(
			ctx context.Context,
			candidate peoplesweep.ProviderConfig,
			credential peoplesweep.Credential,
		) (peoplesweep.NegotiatedCapabilities, error) {
			registry, err := peoplesweep.NewDriverRegistry(
				http.DefaultClient,
				peoplesweep.NewCodexCommandStarter(),
				peoplesweep.NewReleasedCodexIsolationGate(),
			)
			if err != nil {
				return peoplesweep.NegotiatedCapabilities{}, err
			}
			return peoplesweep.NewCapabilityChecker(registry).Negotiate(ctx, candidate, credential)
		},
		lookupEnv:  os.LookupEnv,
		isTerminal: term.IsTerminal,
		readMasked: readBoundedMaskedCredential,
	}
}

func (deps personProviderSetupDeps) resolveCredentialStore() (peoplesweep.CredentialStore, error) {
	if deps.credentials != nil {
		return deps.credentials, nil
	}
	if deps.openCredentialStore == nil {
		return nil, errors.New("people provider credential store is unavailable")
	}
	credentials, err := deps.openCredentialStore()
	if err != nil {
		return nil, fmt.Errorf("open people provider credential store: %w", err)
	}
	if credentials == nil {
		return nil, errors.New("people provider credential store is unavailable")
	}
	return credentials, nil
}

func newPersonProviderAddCommand(deps personProviderCommandDeps) *cobra.Command {
	var options personProviderAddOptions
	command := &cobra.Command{
		Use:   "add <name>",
		Short: "Add and check a named people inference provider profile",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return runPersonProviderAdd(command, deps, args[0], options)
		},
	}
	flags := command.Flags()
	flags.BoolVar(&options.custom, "custom", false, "Skip public catalog suggestions")
	flags.StringVar(&options.protocol, "protocol", "", "Explicit protocol identifier")
	flags.StringVar(&options.endpoint, "endpoint", "", "Explicit provider endpoint")
	flags.StringVar(&options.model, "model", "", "Explicit provider model identifier")
	flags.StringVar(&options.auth, "auth", "", "Explicit auth scheme")
	flags.StringVar(&options.credentialEnv, "credential-env", "", "Read only this environment variable")
	flags.BoolVar(&options.apiKeyStdin, "api-key-stdin", false, "Read the API key locally from standard input")
	flags.StringVar(&options.retentionPosture, "retention-posture", "", "Provider retention assertion")
	flags.StringVar(&options.trainingPosture, "training-posture", "", "Provider training assertion")
	flags.StringSliceVar(&options.allowedSources, "source", nil, "Allowed source class (repeatable)")
	flags.StringVar(&options.sourceSince, "source-since", "", "Earliest disclosed source date")
	flags.StringVar(&options.sourceUntil, "source-until", "", "Latest disclosed source date")
	flags.BoolVar(&options.allowSensitive, "allow-sensitive", false, "Allow sensitive text in provider packets")
	flags.StringVar(&options.reasoningEffort, "reasoning-effort", "", "Explicit reasoning effort")
	flags.StringVar(&options.reasoningMode, "reasoning-mode", "", "Explicit reasoning mode")
	flags.DurationVar(&options.requestTimeout, "request-timeout", time.Minute, "Provider request timeout")
	flags.BoolVar(&options.acceptCatalogPrices, "accept-catalog-prices", false,
		"Explicitly copy the exact matching catalog price hint into sweep budgets")
	flags.BoolVar(&options.confirmed, "yes", false, "Confirm the final provider and privacy values")
	flags.BoolVar(&options.jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonProviderSetCommand(deps personProviderCommandDeps) *cobra.Command {
	var options personProviderSetOptions
	command := &cobra.Command{
		Use:   "set <name>",
		Short: "Update and check a named people inference provider profile",
		Args:  exactPersonProviderNameArgs,
		RunE: func(command *cobra.Command, args []string) error {
			return runPersonProviderSet(command, deps, args[0], options)
		},
	}
	flags := command.Flags()
	flags.StringVar(&options.model, "model", "", "Provider model identifier")
	flags.StringVar(&options.retentionPosture, "retention-posture", "", "Provider retention assertion")
	flags.StringVar(&options.trainingPosture, "training-posture", "", "Provider training assertion")
	flags.StringSliceVar(&options.allowedSources, "source", nil, "Allowed source class (repeatable)")
	flags.StringVar(&options.sourceSince, "source-since", "", "Earliest disclosed source date")
	flags.StringVar(&options.sourceUntil, "source-until", "", "Latest disclosed source date")
	flags.BoolVar(&options.allowSensitive, "allow-sensitive", false, "Allow sensitive text in provider packets")
	flags.StringVar(&options.reasoningEffort, "reasoning-effort", "", "Explicit reasoning effort")
	flags.StringVar(&options.reasoningMode, "reasoning-mode", "", "Explicit reasoning mode")
	flags.DurationVar(&options.requestTimeout, "request-timeout", time.Minute, "Provider request timeout")
	flags.BoolVar(&options.confirmed, "yes", false, "Confirm the final provider and privacy values")
	flags.BoolVar(&options.jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

var personProviderSetMutableFlags = []string{
	"model", "retention-posture", "training-posture", "source", "source-since", "source-until",
	"allow-sensitive", "reasoning-effort", "reasoning-mode", "request-timeout",
}

func runPersonProviderAdd(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	options personProviderAddOptions,
) error {
	// A configured remote daemon owns its own config file, credential store,
	// and checks database on the remote host. Publishing those records here
	// would strand them where the daemon cannot read them and the proxied
	// validation would then roll the whole setup back, so refuse up front
	// rather than transport credentials or half-publish state. There is
	// deliberately no remote setup proxy: setup must run on the daemon host
	// (or against a local daemon via --local).
	if deps.remoteConfigured != nil && deps.remoteConfigured() {
		return errors.New(
			"people provider add cannot run against a configured remote daemon: the provider profile, credential, and check must be published on the daemon host; run this command there, or pass --local to configure a daemon on this machine")
	}
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	if err := validatePersonProviderAddOptions(options); err != nil {
		return err
	}
	if deps.readConfigFile == nil || deps.editConfigTables == nil || deps.restoreConfigFile == nil {
		return errors.New("people provider config editing is unavailable")
	}

	var suggestions []peoplesweep.ProviderSuggestion
	if options.needsCatalog() {
		if deps.setup.catalog == nil {
			return errors.New("models.dev suggestions are unavailable; use --custom with explicit transport fields")
		}
		var catalogErr error
		suggestions, catalogErr = deps.setup.catalog(command.Context())
		if catalogErr != nil {
			return errors.New("models.dev suggestions are unavailable; use --custom with explicit transport fields")
		}
		if !options.jsonOutput {
			printPersonProviderSuggestions(command.OutOrStdout(), suggestions)
		}
	}
	candidate, err := resolvePersonProviderAddCandidate(options, suggestions)
	if err != nil {
		return err
	}
	if candidate.Credential == peoplesweep.CredentialStored && !peoplesweep.StoredCredentialsSupported() {
		return errors.New(
			"stored people provider credentials are unsupported on this platform; pass --credential-env NAME to reference an environment variable instead")
	}
	configured := deps.config()
	if _, exists := configured.Providers[name]; exists {
		return fmt.Errorf("people provider profile %q already exists", name)
	}
	if err := validatePersonProviderCandidate(configured, name, candidate); err != nil {
		return err
	}
	if !options.jsonOutput {
		printPersonProviderCandidate(command.OutOrStdout(), name, candidate)
	}

	before, err := deps.readConfigFile()
	if err != nil {
		return err
	}
	configured, err = personProviderConfigFromSnapshot(deps, before)
	if err != nil {
		return err
	}
	if _, exists := configured.Providers[name]; exists {
		return fmt.Errorf("people provider profile %q already exists", name)
	}
	if err := validatePersonProviderCandidate(configured, name, candidate); err != nil {
		return err
	}
	var catalogPrices *peoplesweep.BudgetConfig
	if options.acceptCatalogPrices {
		catalogPrices, err = acceptedPersonProviderCatalogPrices(configured.Budgets, suggestions, candidate)
		if err != nil {
			return err
		}
	}
	if err := validatePersonProviderProposal(configured, name, candidate, catalogPrices); err != nil {
		return err
	}
	_, err = planPersonProviderAddEdits(before, name, candidate, catalogPrices)
	if err != nil {
		return err
	}
	var credentialStore peoplesweep.CredentialStore
	if candidate.Credential == peoplesweep.CredentialStored {
		credentialStore, err = deps.setup.resolveCredentialStore()
		if err != nil {
			return err
		}
	}

	credential, stored, err := readPersonProviderCredential(command, deps.setup, name, candidate, options)
	if err != nil {
		return err
	}
	if deps.setup.negotiate == nil {
		return errors.New("people provider capability negotiation is unavailable")
	}
	capabilities, err := deps.setup.negotiate(command.Context(), candidate, credential)
	if err != nil {
		return err
	}
	candidate.OutputMode = capabilities.OutputMode
	candidate.TokenLimitParameter = capabilities.TokenLimitParameter
	candidate.ReasoningEffort = capabilities.ReasoningEffort
	candidate.ReasoningMode = capabilities.ReasoningMode
	candidate.DriverVersion = capabilities.DriverVersion
	if err := validatePersonProviderProposal(configured, name, candidate, catalogPrices); err != nil {
		return err
	}
	plannedEdits, err := planPersonProviderAddEdits(before, name, candidate, catalogPrices)
	if err != nil {
		return err
	}

	var credentialCleanup peoplesweep.CredentialCleanupGuard
	if stored {
		createStore, ok := credentialStore.(personProviderCreateCredentialStore)
		if !ok {
			return errors.New("people provider credential store does not support create-only publication")
		}
		var createdCredential bool
		credentialCleanup, createdCredential, err = createStore.SaveNew(name, credential)
		if err != nil {
			return err
		}
		if !createdCredential {
			return fmt.Errorf("people provider credential %q already exists", name)
		}
	}

	after, err := deps.editConfigTables(before.ETag, plannedEdits)
	if err != nil {
		if errors.Is(err, config.ErrConfigChanged) {
			return rollbackUncertainPersonProviderAdd(err, deps, before, after, credentialStore, name, credentialCleanup)
		}
		return rollbackNewPersonProviderCredential(err, credentialStore, name, credentialCleanup)
	}
	checkOutput := command.OutOrStdout()
	if options.jsonOutput {
		checkOutput = io.Discard
	}
	checkedConfig, err := personProviderConfigFromSnapshot(deps, after)
	if err == nil {
		checkedDeps := deps
		checkedDeps.config = func() peoplesweep.Config { return checkedConfig }
		err = executeSavedPersonProviderCheck(command, checkedDeps, name, "", checkOutput)
	}
	if err != nil {
		rollbackErr := rollbackPersonProviderAdd(deps, before, after, credentialStore, name, credentialCleanup)
		return errors.Join(err, rollbackErr)
	}
	if credentialCleanup != nil {
		if err := credentialCleanup.Close(); err != nil {
			return fmt.Errorf("close newly created people provider credential cleanup guard: %w", err)
		}
	}
	if options.jsonOutput {
		selected, err := selectPersonProviderConfig(checkedConfig, name)
		if err != nil {
			return err
		}
		selected.Enabled = true
		profile, err := selected.Profile()
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(personProviderAddOutput{
			Name: name, Fingerprint: profile.Fingerprint, Checked: true,
		})
	}
	_, _ = fmt.Fprintf(command.OutOrStdout(),
		"Added and checked people provider profile %q; run `msgvault person provider consent %q --yes` to grant consent, then `msgvault person provider use %q` to select and enable it.\n",
		name, name, name)
	return nil
}

func personProviderCandidate(options personProviderAddOptions) (peoplesweep.ProviderConfig, error) {
	if options.protocol == "" || options.endpoint == "" || options.model == "" || options.auth == "" ||
		options.retentionPosture == "" || options.trainingPosture == "" ||
		len(options.allowedSources) == 0 || options.sourceSince == "" {
		return peoplesweep.ProviderConfig{}, errors.New("protocol, endpoint, model, auth, retention, training, source, and source-since are required")
	}
	candidate := peoplesweep.ProviderConfig{
		Protocol: peoplesweep.Protocol(options.protocol), Endpoint: options.endpoint,
		Model: options.model, Auth: peoplesweep.AuthScheme(options.auth),
		RetentionPosture: options.retentionPosture, TrainingPosture: options.trainingPosture,
		SourceSince: options.sourceSince, SourceUntil: options.sourceUntil,
		AllowSensitive: options.allowSensitive, ReasoningEffort: options.reasoningEffort,
		ReasoningMode: options.reasoningMode, RequestTimeout: options.requestTimeout,
	}
	for _, source := range options.allowedSources {
		candidate.AllowedSources = append(candidate.AllowedSources, peoplesweep.SourceClass(source))
	}
	if candidate.Auth == peoplesweep.AuthNone {
		candidate.Credential = peoplesweep.CredentialNone
	} else if options.credentialEnv != "" {
		candidate.Credential = peoplesweep.CredentialEnv
		candidate.CredentialEnv = options.credentialEnv
	} else {
		candidate.Credential = peoplesweep.CredentialStored
	}
	if candidate.Protocol == peoplesweep.ProtocolOpenAIChat {
		candidate.OutputMode = peoplesweep.OutputModeNativeJSONSchema
		candidate.TokenLimitParameter = "max_completion_tokens"
	} else {
		candidate.OutputMode = peoplesweep.OutputModeNativeJSONSchema
	}
	return candidate, nil
}

func validatePersonProviderAddOptions(options personProviderAddOptions) error {
	if options.protocol == string(peoplesweep.ProtocolCodexAppServer) {
		return errors.New("codex_app_server profiles are not created by person provider add: " +
			"generic onboarding requires an HTTP endpoint that codex_app_server forbids, and capability negotiation " +
			"does not drive the Codex process transport. Configure the profile manually under " +
			"[people.sweep.providers.<name>] in config.toml (protocol = \"codex_app_server\" with model and " +
			"reasoning_effort, auth = \"none\", credential = \"none\", and no endpoint field), then authorize with " +
			"`msgvault person provider login`; see docs/configuration.md")
	}
	if !options.confirmed {
		return errors.New("people provider add requires --yes after reviewing the final values")
	}
	if options.custom && options.acceptCatalogPrices {
		return errors.New("--custom cannot be combined with --accept-catalog-prices")
	}
	if options.apiKeyStdin && options.credentialEnv != "" {
		return errors.New("--api-key-stdin and --credential-env are mutually exclusive")
	}
	if options.auth == string(peoplesweep.AuthNone) &&
		(options.apiKeyStdin || options.credentialEnv != "") {
		return errors.New("auth=none cannot accept a credential")
	}
	return nil
}

type personProviderCatalogSelection struct {
	endpoint string
	model    string
	protocol peoplesweep.Protocol
}

func resolvePersonProviderAddCandidate(
	options personProviderAddOptions,
	suggestions []peoplesweep.ProviderSuggestion,
) (peoplesweep.ProviderConfig, error) {
	if options.custom || options.explicitTransport() {
		return personProviderCandidate(options)
	}
	selection, err := selectPersonProviderCatalogSuggestion(options, suggestions)
	if err != nil {
		return peoplesweep.ProviderConfig{}, err
	}
	explicitEndpoint := options.endpoint != ""
	if options.protocol == "" {
		options.protocol = string(selection.protocol)
	}
	if options.endpoint == "" {
		options.endpoint = selection.endpoint
	}
	if options.model == "" {
		options.model = selection.model
	}
	if options.auth == "" {
		auth, authErr := personProviderCatalogAuth(selection.protocol, "")
		if authErr != nil {
			return peoplesweep.ProviderConfig{}, authErr
		}
		options.auth = string(auth)
	} else if _, authErr := personProviderCatalogAuth(selection.protocol, peoplesweep.AuthScheme(options.auth)); authErr != nil {
		return peoplesweep.ProviderConfig{}, authErr
	}
	candidate, err := personProviderCandidate(options)
	if err != nil {
		return peoplesweep.ProviderConfig{}, err
	}
	if err := requireCredentialEndpointChoice(explicitEndpoint, candidate); err != nil {
		return peoplesweep.ProviderConfig{}, err
	}
	return candidate, nil
}

// requireCredentialEndpointChoice enforces the onboarding credential
// boundary: a catalog suggestion never chooses the destination of a
// credential. Onboarding may transmit a credential only to an endpoint the
// operator explicitly supplied or to a first-party API host compiled into
// this binary, so a compromised models.dev catalog cannot redirect keys to
// itself. The check runs before any credential is read or contacted.
func requireCredentialEndpointChoice(
	explicitEndpoint bool,
	candidate peoplesweep.ProviderConfig,
) error {
	if explicitEndpoint || candidate.Credential == peoplesweep.CredentialNone {
		return nil
	}
	if peoplesweep.IndependentlyTrustedEndpoint(candidate.Protocol, candidate.Endpoint) {
		return nil
	}
	return fmt.Errorf(
		"people provider add cannot send a credential to the catalog-selected endpoint %q: "+
			"pass --endpoint to confirm the exact endpoint, or pick a first-party default endpoint",
		candidate.Endpoint)
}

func selectPersonProviderCatalogSuggestion(
	options personProviderAddOptions,
	suggestions []peoplesweep.ProviderSuggestion,
) (personProviderCatalogSelection, error) {
	var matches []personProviderCatalogSelection
	for _, provider := range suggestions {
		if provider.Endpoint == "" || (options.endpoint != "" && provider.Endpoint != options.endpoint) {
			continue
		}
		for _, model := range provider.Models {
			if model.ID == "" || (options.model != "" && model.ID != options.model) {
				continue
			}
			for _, protocol := range provider.ProtocolCandidates {
				if options.protocol != "" && protocol != peoplesweep.Protocol(options.protocol) {
					continue
				}
				matches = append(matches, personProviderCatalogSelection{
					endpoint: provider.Endpoint, model: model.ID, protocol: protocol,
				})
			}
		}
	}
	if len(matches) != 1 {
		return personProviderCatalogSelection{}, errors.New(
			"models.dev catalog must select exactly one provider, model, and protocol; use --custom with explicit transport fields",
		)
	}
	return matches[0], nil
}

func personProviderCatalogAuth(
	protocol peoplesweep.Protocol,
	explicit peoplesweep.AuthScheme,
) (peoplesweep.AuthScheme, error) {
	var defaultAuth peoplesweep.AuthScheme
	switch protocol {
	case peoplesweep.ProtocolOpenAIChat, peoplesweep.ProtocolOpenAIResponses:
		defaultAuth = peoplesweep.AuthBearer
		if explicit == "" || explicit == peoplesweep.AuthBearer || explicit == peoplesweep.AuthXAPIKey {
			if explicit != "" {
				return explicit, nil
			}
			return defaultAuth, nil
		}
	case peoplesweep.ProtocolAnthropicMessages:
		defaultAuth = peoplesweep.AuthXAPIKey
	case peoplesweep.ProtocolGoogleGenerateContent:
		defaultAuth = peoplesweep.AuthGoogleAPIKey
	default:
		return "", fmt.Errorf("models.dev catalog selected unsupported protocol %q", protocol)
	}
	if explicit == "" || explicit == defaultAuth {
		return defaultAuth, nil
	}
	return "", fmt.Errorf("models.dev catalog protocol %q does not support auth %q", protocol, explicit)
}

func personProviderConfigFromSnapshot(
	deps personProviderCommandDeps,
	snapshot config.ConfigFile,
) (peoplesweep.Config, error) {
	homeDir := ""
	if deps.configHomeDir != nil {
		homeDir = deps.configHomeDir()
	}
	loaded, err := config.LoadConfigFile(snapshot, homeDir)
	if err != nil {
		return peoplesweep.Config{}, err
	}
	return loaded.People.Sweep, nil
}

func applyPersonProviderSetOptions(
	command *cobra.Command,
	provider *peoplesweep.ProviderConfig,
	options personProviderSetOptions,
) {
	flags := command.Flags()
	if flags.Changed("model") {
		provider.Model = options.model
	}
	if flags.Changed("retention-posture") {
		provider.RetentionPosture = options.retentionPosture
	}
	if flags.Changed("training-posture") {
		provider.TrainingPosture = options.trainingPosture
	}
	if flags.Changed("source") {
		provider.AllowedSources = make([]peoplesweep.SourceClass, len(options.allowedSources))
		for index, source := range options.allowedSources {
			provider.AllowedSources[index] = peoplesweep.SourceClass(source)
		}
	}
	if flags.Changed("source-since") {
		provider.SourceSince = options.sourceSince
	}
	if flags.Changed("source-until") {
		provider.SourceUntil = options.sourceUntil
	}
	if flags.Changed("allow-sensitive") {
		provider.AllowSensitive = options.allowSensitive
	}
	if flags.Changed("reasoning-effort") {
		provider.ReasoningEffort = options.reasoningEffort
	}
	if flags.Changed("reasoning-mode") {
		provider.ReasoningMode = options.reasoningMode
	}
	if flags.Changed("request-timeout") {
		provider.RequestTimeout = options.requestTimeout
	}
}

func readExistingPersonProviderCredential(
	setup personProviderSetupDeps,
	name string,
	profile peoplesweep.ProviderProfile,
) (peoplesweep.Credential, error) {
	var credentials peoplesweep.CredentialStore
	if profile.Credential == peoplesweep.CredentialStored {
		var err error
		credentials, err = setup.resolveCredentialStore()
		if err != nil {
			return peoplesweep.Credential{}, err
		}
	}
	credential, err := peoplesweep.NewCredentialResolver(credentials, setup.lookupEnv).Resolve(name, profile)
	if err != nil {
		return peoplesweep.Credential{}, fmt.Errorf("resolve existing people provider credential: %w", err)
	}
	return credential, nil
}

func readPersonProviderCredential(
	command *cobra.Command,
	setup personProviderSetupDeps,
	name string,
	candidate peoplesweep.ProviderConfig,
	options personProviderAddOptions,
) (peoplesweep.Credential, bool, error) {
	if candidate.Credential == peoplesweep.CredentialNone {
		if options.apiKeyStdin || options.credentialEnv != "" {
			return peoplesweep.Credential{}, false, errors.New("auth=none cannot accept a credential")
		}
		return peoplesweep.NewCredential(peoplesweep.AuthNone, ""), false, nil
	}
	if options.apiKeyStdin && options.credentialEnv != "" {
		return peoplesweep.Credential{}, false, errors.New("--api-key-stdin and --credential-env are mutually exclusive")
	}
	if candidate.Credential == peoplesweep.CredentialEnv {
		if setup.lookupEnv == nil {
			return peoplesweep.Credential{}, false, errors.New("people provider environment lookup is unavailable")
		}
		value, ok := setup.lookupEnv(options.credentialEnv)
		if !ok || value == "" {
			return peoplesweep.Credential{}, false,
				fmt.Errorf("people provider credential environment variable %s is not set", options.credentialEnv)
		}
		return peoplesweep.NewCredential(candidate.Auth, value), false, nil
	}
	var raw []byte
	var err error
	if options.apiKeyStdin {
		raw, err = readProviderCredentialLine(command.InOrStdin())
	} else {
		file, ok := command.InOrStdin().(*os.File)
		if !ok || setup.isTerminal == nil || !setup.isTerminal(file.Fd()) || setup.readMasked == nil {
			return peoplesweep.Credential{}, false,
				errors.New("a masked terminal is required; use --api-key-stdin for non-interactive setup")
		}
		_, _ = fmt.Fprintf(command.ErrOrStderr(), "API key for %s: ", name)
		raw, err = setup.readMasked(file, maxProviderCredentialBytes)
		_, _ = fmt.Fprintln(command.ErrOrStderr())
	}
	if err != nil {
		return peoplesweep.Credential{}, false, errors.New("read people provider credential")
	}
	if len(raw) == 0 || len(raw) > maxProviderCredentialBytes {
		return peoplesweep.Credential{}, false, errors.New("people provider credential is empty or too large")
	}
	return peoplesweep.NewCredential(candidate.Auth, string(raw)), true, nil
}

func readBoundedMaskedCredential(file *os.File, limit int) ([]byte, error) {
	return readBoundedMaskedCredentialWithTerminal(
		file, file.Fd(), limit, term.MakeRaw, term.Restore,
	)
}

func readBoundedMaskedCredentialWithTerminal(
	reader io.Reader,
	fd uintptr,
	limit int,
	makeRaw func(uintptr) (*term.State, error),
	restore func(uintptr, *term.State) error,
) (credential []byte, resultErr error) {
	state, err := makeRaw(fd)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := restore(fd, state); err != nil {
			clear(credential)
			credential = nil
			resultErr = errors.Join(resultErr, fmt.Errorf("restore credential terminal: %w", err))
		}
	}()
	return readBoundedMaskedCredentialInput(reader, limit)
}

func readBoundedMaskedCredentialInput(reader io.Reader, limit int) ([]byte, error) {
	if limit <= 0 {
		return nil, errors.New("credential input limit is invalid")
	}
	credential := make([]byte, 0, min(limit, 128))
	tooLarge := false
	var input [1]byte
	for {
		read, err := reader.Read(input[:])
		if read > 0 {
			switch input[0] {
			case '\r', '\n', 0x04:
				if tooLarge {
					clear(credential)
					return nil, errors.New("credential is too large")
				}
				return credential, nil
			case 0x03:
				clear(credential)
				return nil, errors.New("credential entry canceled")
			case 0x08, 0x7f:
				if !tooLarge && len(credential) > 0 {
					credential = credential[:len(credential)-1]
				}
			default:
				if len(credential) == limit {
					tooLarge = true
				} else if !tooLarge {
					credential = append(credential, input[0])
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && !tooLarge {
				return credential, nil
			}
			clear(credential)
			if tooLarge {
				return nil, errors.New("credential is too large")
			}
			return nil, err
		}
		if read == 0 {
			clear(credential)
			return nil, io.ErrNoProgress
		}
	}
}

func readProviderCredentialLine(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxProviderCredentialBytes+2))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxProviderCredentialBytes+1 {
		return nil, errors.New("credential is too large")
	}
	if len(raw) > 0 && raw[len(raw)-1] == '\n' {
		raw = raw[:len(raw)-1]
		if len(raw) > 0 && raw[len(raw)-1] == '\r' {
			raw = raw[:len(raw)-1]
		}
	}
	if len(raw) > maxProviderCredentialBytes {
		return nil, errors.New("credential is too large")
	}
	for _, value := range raw {
		if value == '\r' || value == '\n' {
			return nil, errors.New("credential input contains trailing data")
		}
	}
	return raw, nil
}

func validatePersonProviderCandidate(
	configured peoplesweep.Config,
	name string,
	candidate peoplesweep.ProviderConfig,
) error {
	configured = personProviderProposedConfig(configured, name, candidate)
	_, err := configured.Profile()
	return err
}

func personProviderProposedConfig(
	configured peoplesweep.Config,
	name string,
	candidate peoplesweep.ProviderConfig,
) peoplesweep.Config {
	configured.Enabled = true
	configured.Provider = peoplesweep.ProviderSelection{Name: name}
	providers := make(map[string]peoplesweep.ProviderConfig, len(configured.Providers)+1)
	maps.Copy(providers, configured.Providers)
	providers[name] = candidate
	configured.Providers = providers
	configured.ApplyDefaults()
	return configured
}

func validatePersonProviderProposal(
	configured peoplesweep.Config,
	name string,
	candidate peoplesweep.ProviderConfig,
	prices *peoplesweep.BudgetConfig,
) error {
	proposed := personProviderProposedConfig(configured, name, candidate)
	if prices != nil {
		proposed.Budgets = *prices
	}
	if err := peoplesweep.ValidateBudgetConfig(proposed.Budgets); err != nil {
		return err
	}
	reservations := []peoplesweep.TokenUsage{
		{InputTokens: proposed.Budgets.MaxInputTokensPerPerson, OutputTokens: proposed.Budgets.MaxOutputTokensPerPerson},
		{InputTokens: proposed.Budgets.MaxInputTokensPerRun, OutputTokens: proposed.Budgets.MaxOutputTokensPerRun},
		{InputTokens: proposed.Budgets.MaxInputTokensPerDay, OutputTokens: proposed.Budgets.MaxOutputTokensPerDay},
	}
	for _, reservation := range reservations {
		if _, err := peoplesweep.EstimateCostMicroUSD(reservation, proposed.Budgets); err != nil {
			return fmt.Errorf("people provider catalog prices overflow a configured token reservation: %w", err)
		}
	}
	_, err := proposed.Profile()
	return err
}

// planPersonProviderAddEdits publishes the named profile without disturbing
// the operator's active provider selection or enabled state: selection and
// enablement belong to `person provider use`, which requires an exact
// successful check, and an enabled scheduled sweep must never be switched
// to a newly added profile before anyone consents to it.
func planPersonProviderAddEdits(
	before config.ConfigFile,
	name string,
	candidate peoplesweep.ProviderConfig,
	prices *peoplesweep.BudgetConfig,
) ([]config.TableEdit, error) {
	edits := []config.TableEdit{personProviderProfileEdit(name, candidate)}
	if prices != nil {
		edits = append(edits, personProviderBudgetEdit(prices))
	}
	if err := config.ValidateConfigTableEdits(before, edits); err != nil {
		return nil, err
	}
	return edits, nil
}

func personProviderProfileEdit(name string, provider peoplesweep.ProviderConfig) config.TableEdit {
	return config.TableEdit{
		Path:   []string{"people", "sweep", "providers", name},
		Values: personProviderTableValues(provider), InsertOnly: true,
	}
}

func personProviderProfileUpdateEdit(name string, provider peoplesweep.ProviderConfig) config.TableEdit {
	return config.TableEdit{
		Path:   []string{"people", "sweep", "providers", name},
		Values: personProviderTableUpdateValues(provider),
	}
}

func personProviderBudgetEdit(prices *peoplesweep.BudgetConfig) config.TableEdit {
	return config.TableEdit{
		Path: []string{"people", "sweep", "budgets"},
		Values: map[string]any{
			"input_cost_microusd_per_million_tokens":  prices.InputCostMicroUSDPerMillionTokens,
			"output_cost_microusd_per_million_tokens": prices.OutputCostMicroUSDPerMillionTokens,
		},
	}
}

func personProviderTableValues(provider peoplesweep.ProviderConfig) map[string]any {
	sources := make([]string, len(provider.AllowedSources))
	for index, source := range provider.AllowedSources {
		sources[index] = string(source)
	}
	values := map[string]any{
		"protocol": provider.Protocol, "model": provider.Model,
		"auth": provider.Auth, "credential": provider.Credential,
		"output_mode":       provider.OutputMode,
		"retention_posture": provider.RetentionPosture, "training_posture": provider.TrainingPosture,
		"allowed_sources": sources, "source_since": provider.SourceSince,
		"allow_sensitive": provider.AllowSensitive, "request_timeout": provider.RequestTimeout,
	}
	// Protocol-optional fields stay absent when unconfigured: endpoint and the
	// Codex isolation fields keep codex_app_server profiles in their
	// documented endpoint-free shape, and token_limit_parameter belongs to
	// openai_chat profiles only.
	if provider.Endpoint != "" {
		values["endpoint"] = provider.Endpoint
	}
	if provider.TokenLimitParameter != "" {
		values["token_limit_parameter"] = provider.TokenLimitParameter
	}
	if provider.Executable != "" {
		values["executable"] = provider.Executable
	}
	if provider.ExecutionBoundary != "" {
		values["execution_boundary"] = provider.ExecutionBoundary
	}
	if provider.CredentialEnv != "" {
		values["credential_env"] = provider.CredentialEnv
	}
	if provider.SourceUntil != "" {
		values["source_until"] = provider.SourceUntil
	}
	if provider.ReasoningEffort != "" {
		values["reasoning_effort"] = provider.ReasoningEffort
	}
	if provider.ReasoningMode != "" {
		values["reasoning_mode"] = provider.ReasoningMode
	}
	return values
}

func personProviderTableUpdateValues(provider peoplesweep.ProviderConfig) map[string]any {
	values := personProviderTableValues(provider)
	values["source_until"] = provider.SourceUntil
	values["reasoning_effort"] = provider.ReasoningEffort
	values["reasoning_mode"] = provider.ReasoningMode
	if provider.Protocol == peoplesweep.ProtocolOpenAIChat {
		values["token_limit_parameter"] = provider.TokenLimitParameter
	}
	return values
}

func acceptedPersonProviderCatalogPrices(
	current peoplesweep.BudgetConfig,
	suggestions []peoplesweep.ProviderSuggestion,
	candidate peoplesweep.ProviderConfig,
) (*peoplesweep.BudgetConfig, error) {
	type pair struct{ input, output *int64 }
	var matches []pair
	for _, provider := range suggestions {
		if provider.Endpoint != candidate.Endpoint {
			continue
		}
		for _, model := range provider.Models {
			if model.ID == candidate.Model {
				matches = append(matches, pair{model.InputCostMicroUSDPerMillionTokens,
					model.OutputCostMicroUSDPerMillionTokens})
			}
		}
	}
	if len(matches) != 1 || matches[0].input == nil || matches[0].output == nil {
		return nil, errors.New("exactly one complete catalog price suggestion must match the explicit endpoint and model")
	}
	current.InputCostMicroUSDPerMillionTokens = *matches[0].input
	current.OutputCostMicroUSDPerMillionTokens = *matches[0].output
	return &current, nil
}

func printPersonProviderSuggestions(w io.Writer, suggestions []peoplesweep.ProviderSuggestion) {
	for _, provider := range suggestions {
		_, _ = fmt.Fprintf(w, "Suggestion: %s (%s) endpoint=%s\n", provider.Name, provider.ID, provider.Endpoint)
		for _, model := range provider.Models {
			_, _ = fmt.Fprintf(w, "  model hint: %s (%s)\n", model.Name, model.ID)
		}
	}
}

func printPersonProviderCandidate(w io.Writer, name string, candidate peoplesweep.ProviderConfig) {
	_, _ = fmt.Fprintf(w,
		"Final provider %q: endpoint=%s protocol=%s model=%s auth=%s credential=%s retention=%s training=%s sources=%s sensitive=%t\n",
		name, candidate.Endpoint, candidate.Protocol, candidate.Model, candidate.Auth,
		candidate.Credential, candidate.RetentionPosture, candidate.TrainingPosture,
		strings.Join(sourceStrings(candidate.AllowedSources), ","), candidate.AllowSensitive)
}

func sourceStrings(sources []peoplesweep.SourceClass) []string {
	result := make([]string, len(sources))
	for index, source := range sources {
		result[index] = string(source)
	}
	return result
}

func executeSavedPersonProviderCheck(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	ifFingerprint string,
	out io.Writer,
) error {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	directStore := deps.isDaemonSubprocess != nil && deps.isDaemonSubprocess()
	if !directStore && deps.providerStoreOwnedByDaemon != nil {
		owned, err := deps.providerStoreOwnedByDaemon(command.Context())
		if err != nil {
			return err
		}
		directStore = !owned
	}
	if directStore {
		if err := verifyPersonProviderFingerprint(deps, name, ifFingerprint); err != nil {
			return err
		}
		output, err := checkPersonProvider(command, deps, name)
		if err != nil {
			return err
		}
		return writePersonProviderCheckOutput(out, output, false)
	}
	selected, err := selectPersonProviderConfig(deps.config(), name)
	if err != nil {
		return err
	}
	selected.Enabled = true
	profile, err := selected.Profile()
	if err != nil {
		return err
	}
	if ifFingerprint != "" && ifFingerprint != profile.Fingerprint {
		return errors.New("people provider profile changed before checking")
	}
	var env map[string]string
	if profile.Credential == peoplesweep.CredentialEnv && profile.Auth != peoplesweep.AuthNone && deps.setup.lookupEnv != nil {
		if value, ok := deps.setup.lookupEnv(profile.CredentialRef); ok && strings.TrimSpace(value) != "" {
			env = map[string]string{profile.CredentialRef: value}
		}
	}
	return proxySavedPersonProviderOperationWithFlag(command, deps, "check", name,
		personProviderIfFingerprintFlag, profile.Fingerprint, out, env)
}

func verifyPersonProviderFingerprint(
	deps personProviderCommandDeps,
	name, expected string,
) error {
	if expected == "" {
		return nil
	}
	if err := validatePersonProviderFingerprint(expected); err != nil {
		return err
	}
	current, err := selectPersonProviderConfig(deps.config(), name)
	if err != nil {
		return err
	}
	current.Enabled = true
	profile, err := current.Profile()
	if err != nil {
		return err
	}
	if profile.Fingerprint != expected {
		return errors.New("people provider profile changed before checking")
	}
	return nil
}

func proxySavedPersonProviderRevoke(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	fingerprint string,
) error {
	return proxySavedPersonProviderOperation(command, deps, "revoke", name, fingerprint, command.OutOrStdout())
}

func proxySavedPersonProviderRevokeFingerprint(
	command *cobra.Command,
	deps personProviderCommandDeps,
	name string,
	fingerprint string,
) error {
	return proxySavedPersonProviderOperationWithFlag(
		command, deps, "revoke", name, "fingerprint", fingerprint, io.Discard, nil,
	)
}

func proxySavedPersonProviderOperation(
	command *cobra.Command,
	deps personProviderCommandDeps,
	operation string,
	name string,
	fingerprint string,
	out io.Writer,
) error {
	return proxySavedPersonProviderOperationWithFlag(
		command, deps, operation, name, personProviderIfFingerprintFlag, fingerprint, out, nil,
	)
}

func proxySavedPersonProviderOperationWithFlag(
	command *cobra.Command,
	deps personProviderCommandDeps,
	operation string,
	name string,
	flag string,
	fingerprint string,
	out io.Writer,
	env map[string]string,
) error {
	if err := peoplesweep.ValidateProviderProfileName(name); err != nil {
		return err
	}
	if deps.proxy == nil {
		return errors.New("people provider daemon proxy is unavailable")
	}
	root := &cobra.Command{Use: "msgvault"}
	person := &cobra.Command{Use: "person"}
	provider := &cobra.Command{Use: "provider"}
	leaf := &cobra.Command{Use: operation}
	if fingerprint != "" {
		if flag != "fingerprint" && operation != "revoke" && operation != "check" {
			return errors.New("people provider fingerprint guard is unavailable for this operation")
		}
		if err := validatePersonProviderFingerprint(fingerprint); err != nil {
			return err
		}
		leaf.Flags().String(flag, "", "")
		if err := leaf.Flags().Set(flag, fingerprint); err != nil {
			return fmt.Errorf("set person provider fingerprint guard: %w", err)
		}
	}
	provider.AddCommand(leaf)
	person.AddCommand(provider)
	root.AddCommand(person)
	leaf.SetOut(out)
	leaf.SetErr(command.ErrOrStderr())
	leaf.SetContext(command.Context())
	return deps.proxy(leaf, []string{name}, env)
}

func rollbackNewPersonProviderCredential(
	cause error,
	credentials peoplesweep.CredentialStore,
	name string,
	cleanup peoplesweep.CredentialCleanupGuard,
) error {
	if cleanup == nil || credentials == nil {
		return cause
	}
	if err := cleanupNewPersonProviderCredential(credentials, name, cleanup); err != nil {
		return errors.Join(cause, err)
	}
	return cause
}

func rollbackUncertainPersonProviderAdd(
	cause error,
	deps personProviderCommandDeps,
	before, expected config.ConfigFile,
	credentials peoplesweep.CredentialStore,
	name string,
	cleanup peoplesweep.CredentialCleanupGuard,
) error {
	return errors.Join(cause,
		rollbackPersonProviderAdd(deps, before, expected, credentials, name, cleanup))
}

func rollbackPersonProviderAdd(
	deps personProviderCommandDeps,
	before, after config.ConfigFile,
	credentials peoplesweep.CredentialStore,
	name string,
	cleanup peoplesweep.CredentialCleanupGuard,
) error {
	var rollbackErr error
	if _, err := deps.restoreConfigFile(after, before); err != nil {
		rollbackErr = fmt.Errorf("restore people provider config: %w", err)
	}
	if cleanup != nil && credentials != nil {
		if err := cleanupNewPersonProviderCredential(credentials, name, cleanup); err != nil {
			rollbackErr = errors.Join(rollbackErr, err)
		}
	}
	return rollbackErr
}

func cleanupNewPersonProviderCredential(
	credentials peoplesweep.CredentialStore,
	name string,
	guard peoplesweep.CredentialCleanupGuard,
) (retErr error) {
	defer func() {
		if closeErr := guard.Close(); closeErr != nil {
			retErr = errors.Join(retErr,
				fmt.Errorf("close new people provider credential cleanup guard: %w", closeErr))
		}
	}()
	createStore, ok := credentials.(personProviderCreateCredentialStore)
	if !ok {
		return errors.New("new people provider credential cleanup conflict: store does not support bound cleanup")
	}
	if err := createStore.CleanupNew(name, guard); err != nil {
		return fmt.Errorf("new people provider credential cleanup conflict: %w", err)
	}
	return nil
}
