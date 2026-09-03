package cmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
)

const personEnrichmentConsentActor = "cli"

type personEnrichmentManualWorkerFunc func(context.Context, int64) (bool, error)

func (f personEnrichmentManualWorkerFunc) RunOnce(ctx context.Context, runID int64) (bool, error) {
	return f(ctx, runID)
}

type personEnrichmentCommandDeps struct {
	config             func() personenrichment.Config
	openStore          func() (*store.Store, func(), error)
	lookupEnv          personenrichment.CredentialLookup
	proxyLookupEnv     personenrichment.CredentialLookup
	isDaemonSubprocess func() bool
	proxyArgs          func(*cobra.Command, []string, map[string]string) error
	newManualWorker    func(context.Context, *store.Store, personenrichment.Config) (personEnrichmentScheduleWorker, error)
	clock              func() time.Time
}

func defaultPersonEnrichmentCommandDeps() personEnrichmentCommandDeps {
	return personEnrichmentCommandDeps{
		config: func() personenrichment.Config {
			if cfg == nil {
				return personenrichment.Config{}
			}
			return cfg.People.Enrichment
		},
		openStore:          openWritableStoreAndInit,
		lookupEnv:          personEnrichmentEnvironmentLookup(cfg),
		proxyLookupEnv:     os.LookupEnv,
		isDaemonSubprocess: isDaemonCLISubprocess,
		proxyArgs: func(command *cobra.Command, args []string, env map[string]string) error {
			return runDaemonCLICommandHTTPWithEnv(command, args, env, false, false)
		},
		newManualWorker: func(
			ctx context.Context, st *store.Store, enrichmentConfig personenrichment.Config,
		) (personEnrichmentScheduleWorker, error) {
			return newPersonEnrichmentCLIWorker(
				ctx, st, enrichmentConfig,
				personEnrichmentEnvironmentLookup(cfg),
				personEnrichmentProviderCredentialLookup(cfg),
			)
		},
		clock: time.Now,
	}
}

func newPersonEnrichmentCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	command := &cobra.Command{Use: "enrichment", Short: "Manage external person enrichment"}
	command.AddCommand(
		newPersonEnrichmentStatusCommand(deps),
		newPersonEnrichmentProfilesCommand(deps),
		newPersonEnrichmentConsentCommand(deps),
		newPersonEnrichmentRevokeCommand(deps),
		newPersonEnrichmentRunCommand(deps),
		newPersonEnrichmentSuppressCommand(deps),
	)
	return command
}

func proxyPersonEnrichmentCommand(command *cobra.Command, args []string, deps personEnrichmentCommandDeps) error {
	proxied, err := daemonCLIArgsFromCobra(command, args)
	if err != nil {
		return err
	}
	return deps.proxyArgs(command, proxied, nil)
}

func proxyPersonEnrichmentCommandWithEnv(
	command *cobra.Command, args []string, deps personEnrichmentCommandDeps, names ...string,
) error {
	proxied, err := daemonCLIArgsFromCobra(command, args)
	if err != nil {
		return err
	}
	env := make(map[string]string, len(names))
	lookup := deps.proxyLookupEnv
	if lookup == nil {
		lookup = deps.lookupEnv
	}
	for _, name := range names {
		if name == "" || name == providercredentials.StoredSuppressionEnvironment {
			continue
		}
		if value, ok := lookup(name); ok && value != "" {
			env[name] = value
		}
	}
	if len(env) == 0 {
		env = nil
	}
	return deps.proxyArgs(command, proxied, env)
}

type personEnrichmentStatusOutput struct {
	Profiles     []personenrichment.ProviderProfile    `json:"profiles"`
	Consents     []store.PersonEnrichmentConsentStatus `json:"consents"`
	Suppressions []store.PersonEnrichmentSuppression   `json:"suppressions"`
}

func newPersonEnrichmentStatusCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var limit int
	var jsonOutput bool
	command := &cobra.Command{
		Use: "status", Args: cobra.NoArgs, Short: "Show bounded enrichment policy and privacy status",
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return proxyPersonEnrichmentCommand(command, args, deps)
			}
			if limit < 1 || limit > 200 {
				return errors.New("status limit must be in [1,200]")
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			profiles, err := st.ListPersonEnrichmentProfilesContext(command.Context())
			if err != nil {
				return err
			}
			output := personEnrichmentStatusOutput{Profiles: profiles}
			for _, profile := range profiles {
				status, statusErr := st.PersonEnrichmentConsentStatus(command.Context(), profile.Fingerprint)
				if statusErr != nil {
					return statusErr
				}
				output.Consents = append(output.Consents, *status)
			}
			output.Suppressions, err = st.ListPersonEnrichmentSuppressionsContext(command.Context(),
				store.PersonEnrichmentSuppressionFilter{Limit: limit})
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(output)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Profiles: %d\nSuppressions shown: %d\n",
				len(output.Profiles), len(output.Suppressions))
			if err != nil {
				return fmt.Errorf("write person enrichment status: %w", err)
			}
			return nil
		},
	}
	command.Flags().IntVar(&limit, "limit", 20, "Maximum suppression history rows (1-200)")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonEnrichmentProfilesCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use: "profiles", Args: cobra.NoArgs, Short: "List immutable enrichment provider profiles",
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return proxyPersonEnrichmentCommand(command, args, deps)
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			profiles, err := st.ListPersonEnrichmentProfilesContext(command.Context())
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(profiles)
			}
			for _, profile := range profiles {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n",
					profile.Fingerprint, profile.Name, profile.ProviderNamespace); err != nil {
					return fmt.Errorf("write person enrichment profile: %w", err)
				}
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonEnrichmentConsentCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var jsonOutput bool
	command := &cobra.Command{
		Use: "consent <fingerprint>", Args: cobra.ExactArgs(1), Short: "Grant exact enrichment policy consent",
		RunE: func(command *cobra.Command, args []string) error {
			if !deps.isDaemonSubprocess() {
				return proxyPersonEnrichmentCommand(command, args, deps)
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			consent, created, err := st.GrantPersonEnrichmentConsent(
				command.Context(), args[0], personEnrichmentConsentActor)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(struct {
					Consent *store.PersonEnrichmentConsent `json:"consent"`
					Created bool                           `json:"created"`
				}{consent, created})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Consent active for %s\n", consent.ProfileFingerprint)
			if err != nil {
				return fmt.Errorf("write person enrichment consent: %w", err)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonEnrichmentRevokeCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var all bool
	var jsonOutput bool
	command := &cobra.Command{
		Use: "revoke [fingerprint]", Args: cobra.MaximumNArgs(1), Short: "Revoke exact enrichment policy consent",
		RunE: func(command *cobra.Command, args []string) error {
			if all == (len(args) == 1) {
				return errors.New("revoke requires exactly one fingerprint or --all")
			}
			if !deps.isDaemonSubprocess() {
				return proxyPersonEnrichmentCommand(command, args, deps)
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			var revoked int64
			if all {
				revoked, err = st.RevokeAllPersonEnrichmentConsents(command.Context(), personEnrichmentConsentActor)
			} else {
				var changed bool
				changed, err = st.RevokePersonEnrichmentConsent(command.Context(), args[0], personEnrichmentConsentActor)
				if changed {
					revoked = 1
				}
			}
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(map[string]int64{"revoked": revoked})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "Revoked: %d\n", revoked)
			if err != nil {
				return fmt.Errorf("write person enrichment revocation: %w", err)
			}
			return nil
		},
	}
	command.Flags().BoolVar(&all, "all", false, "Revoke all active enrichment policies")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func newPersonEnrichmentRunCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var personID int64
	var providerName string
	var idempotencyKey string
	var jsonOutput bool
	command := &cobra.Command{
		Use: "run", Args: cobra.NoArgs, Short: "Run durable enrichment work for one person and provider",
		RunE: func(command *cobra.Command, args []string) error {
			if personID <= 0 || strings.TrimSpace(providerName) == "" || strings.TrimSpace(idempotencyKey) == "" {
				return errors.New("run requires --person, --provider, and --idempotency-key")
			}
			config := deps.config()
			if !config.Enabled {
				return errors.New("person enrichment is disabled")
			}
			if !deps.isDaemonSubprocess() {
				provider, ok := personEnrichmentProviderConfig(config, providerName)
				if !ok {
					return fmt.Errorf("person enrichment provider %q is not enabled", providerName)
				}
				return proxyPersonEnrichmentCommandWithEnv(command, args, deps,
					config.SuppressionKeyEnv, provider.APIKeyEnv)
			}
			return runPersonEnrichmentManual(
				command, deps, config, personID, providerName, idempotencyKey, jsonOutput)
		},
	}
	command.Flags().Int64Var(&personID, "person", 0, "Person ID")
	command.Flags().StringVar(&providerName, "provider", "", "Configured provider name")
	command.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Durable caller idempotency key")
	command.Flags().BoolVar(&jsonOutput, flagJSON, false, "Output structured JSON")
	return command
}

func runPersonEnrichmentManual(
	command *cobra.Command, deps personEnrichmentCommandDeps, config personenrichment.Config,
	personID int64, providerName, idempotencyKey string, jsonOutput bool,
) error {
	st, cleanup, err := deps.openStore()
	if err != nil {
		return err
	}
	defer cleanup()
	provider, ok := personEnrichmentProviderConfig(config, providerName)
	if !ok {
		return fmt.Errorf("person enrichment provider %q is not enabled", providerName)
	}
	catalog, err := st.BuildPersonFactCatalogContext(command.Context(), true)
	if err != nil {
		return err
	}
	profile, err := provider.Profile(catalog)
	if err != nil {
		return err
	}
	if _, err := st.EnsurePersonEnrichmentProfile(command.Context(), profile); err != nil {
		return err
	}
	clockNow := func() time.Time {
		if deps.clock != nil {
			return deps.clock().UTC()
		}
		return time.Now().UTC()
	}
	now := clockNow()
	run, _, err := st.StartManualPersonEnrichmentRunContext(
		command.Context(), personID, profile.Fingerprint, idempotencyKey, now)
	if err != nil {
		return err
	}
	if run.State == "running" {
		worker, err := deps.newManualWorker(command.Context(), st, config)
		if err != nil {
			return err
		}
		for {
			processed, err := worker.RunOnce(command.Context(), run.ID)
			if err != nil {
				return err
			}
			if !processed {
				break
			}
		}
		err = st.CompleteRun(command.Context(), run.ID, personenrichment.RunCompletion{
			State: "", CompletedAt: clockNow(),
		})
		if err != nil && !errors.Is(err, store.ErrRunNotTerminal) {
			return err
		}
	}
	stored, err := st.GetPersonEnrichmentRunContext(command.Context(), run.ID)
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(stored)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Run %d: %s\n", stored.ID, stored.State)
	if err != nil {
		return fmt.Errorf("write person enrichment run: %w", err)
	}
	return nil
}

func newPersonEnrichmentSuppressCommand(deps personEnrichmentCommandDeps) *cobra.Command {
	var personID int64
	var providerName string
	var providerNamespace string
	var class string
	var version string
	var keyID string
	var digestHex string
	var reason string
	var actor string
	command := &cobra.Command{
		Use: "suppress", Args: cobra.NoArgs, Short: "Suppress enrichment by person or stdin identifier",
		RunE: func(command *cobra.Command, _ []string) error {
			personMode := personID > 0
			providerMode := strings.TrimSpace(providerName) != "" || strings.TrimSpace(providerNamespace) != ""
			if personMode == providerMode {
				return errors.New("suppress requires exactly one of --person or --provider with --identifier-class")
			}
			if personMode && (strings.TrimSpace(providerName) != "" ||
				strings.TrimSpace(providerNamespace) != "" || strings.TrimSpace(class) != "" ||
				strings.TrimSpace(version) != "" || strings.TrimSpace(keyID) != "" ||
				strings.TrimSpace(digestHex) != "" || strings.TrimSpace(actor) != "") {
				return errors.New("person suppression does not accept provider digest metadata")
			}
			suppressionReason, err := personEnrichmentCLIReason(reason)
			if err != nil {
				return err
			}
			if !deps.isDaemonSubprocess() {
				if personMode {
					return proxyPersonEnrichmentCommandWithEnv(command, nil, deps,
						deps.config().SuppressionKeyEnv)
				}
				return proxyPersonEnrichmentDigest(command, deps, providerName,
					personenrichment.SuppressionIdentifierClass(class), suppressionReason)
			}
			st, cleanup, err := deps.openStore()
			if err != nil {
				return err
			}
			defer cleanup()
			if personMode {
				return suppressPersonEnrichmentPerson(command, deps, st, personID, suppressionReason)
			}
			return persistPersonEnrichmentDigest(command, deps, st, providerNamespace,
				personenrichment.SuppressionIdentifierClass(class), version, keyID,
				digestHex, suppressionReason, actor)
		},
	}
	command.Flags().Int64Var(&personID, "person", 0, "Suppress every current identifier for this person")
	command.Flags().StringVar(&providerName, "provider", "", "Configured provider name")
	command.Flags().StringVar(&class, "identifier-class", "", "Identifier class")
	command.Flags().StringVar(&reason, "reason", "", "opt_out or data_subject_request")
	command.Flags().StringVar(&providerNamespace, "provider-namespace", "", "Digest-only provider namespace")
	command.Flags().StringVar(&version, "normalization-version", "", "Digest normalization version")
	command.Flags().StringVar(&keyID, "key-id", "", "Suppression key ID")
	command.Flags().StringVar(&digestHex, "digest", "", "Hex suppression digest")
	command.Flags().StringVar(&actor, "actor", "", "Safe audit actor")
	for _, name := range []string{"provider-namespace", "normalization-version", "key-id", "digest", "actor"} {
		_ = command.Flags().MarkHidden(name)
	}
	return command
}

func proxyPersonEnrichmentDigest(
	command *cobra.Command, deps personEnrichmentCommandDeps, providerName string,
	class personenrichment.SuppressionIdentifierClass,
	reason store.PersonEnrichmentSuppressionReason,
) error {
	provider, ok := personEnrichmentProviderConfig(deps.config(), providerName)
	if !ok {
		return fmt.Errorf("person enrichment provider %q is not enabled", providerName)
	}
	namespace, err := provider.ProviderNamespace()
	if err != nil {
		return err
	}
	key, ok := deps.lookupEnv(deps.config().SuppressionKeyEnv)
	if !ok || key == "" {
		return fmt.Errorf("person enrichment suppression key environment %q is not set",
			deps.config().SuppressionKeyEnv)
	}
	keyBytes := []byte(key)
	hasher, err := personenrichment.NewSuppressionHasher(keyBytes)
	clear(keyBytes)
	if err != nil {
		return err
	}
	values, err := readPersonEnrichmentSuppressionInput(command.InOrStdin(), class)
	if err != nil {
		return err
	}
	normalized, err := personenrichment.NormalizeSuppressionIdentifier(class, values)
	clearPersonEnrichmentStrings(values)
	if err != nil {
		return errors.New("suppression identifier input is invalid")
	}
	digest := hasher.Digest(namespace, normalized.Class, normalized.NormalizationVersion, normalized.Value)
	normalized.Value = ""
	args := []string{
		"person", "enrichment", "suppress",
		"--provider-namespace=" + digest.ProviderNamespace,
		"--identifier-class=" + string(digest.IdentifierClass),
		"--normalization-version=" + digest.NormalizationVersion,
		"--key-id=" + digest.KeyID,
		"--digest=" + hex.EncodeToString(digest.Digest),
		"--reason=" + string(reason), "--actor=" + personEnrichmentConsentActor,
	}
	return deps.proxyArgs(command, args, nil)
}

func readPersonEnrichmentSuppressionInput(
	reader io.Reader, class personenrichment.SuppressionIdentifierClass,
) ([]string, error) {
	count := 1
	if class == personenrichment.SuppressionNameCompany {
		count = 2
	}
	buffered := bufio.NewReaderSize(reader, 4096)
	values := make([]string, 0, count)
	for i := range count {
		line, err := buffered.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			clear(line)
			clearPersonEnrichmentStrings(values)
			return nil, fmt.Errorf("read suppression input: %w", err)
		}
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 {
			clear(line)
			clearPersonEnrichmentStrings(values)
			if class == personenrichment.SuppressionNameCompany {
				return nil, errors.New("name_company requires non-empty name and company")
			}
			return nil, fmt.Errorf("%s requires one non-empty stdin line", class)
		}
		values = append(values, string(trimmed))
		clear(line)
		if errors.Is(err, io.EOF) && i+1 < count {
			clearPersonEnrichmentStrings(values)
			return nil, errors.New("name_company requires exactly name and company")
		}
	}
	extra, err := buffered.ReadBytes('\n')
	if len(bytes.TrimSpace(extra)) != 0 || (err == nil && len(extra) != 0) {
		clear(extra)
		clearPersonEnrichmentStrings(values)
		return nil, fmt.Errorf("%s accepts exactly %d stdin line(s)", class, count)
	}
	clear(extra)
	if err != nil && !errors.Is(err, io.EOF) {
		clearPersonEnrichmentStrings(values)
		return nil, fmt.Errorf("read trailing suppression input: %w", err)
	}
	return values, nil
}

func persistPersonEnrichmentDigest(
	command *cobra.Command, deps personEnrichmentCommandDeps, st *store.Store, namespace string,
	class personenrichment.SuppressionIdentifierClass, version, keyID, digestHex string,
	reason store.PersonEnrichmentSuppressionReason, actor string,
) error {
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		return errors.New("suppression digest is invalid")
	}
	if actor != personEnrichmentConsentActor {
		return errors.New("suppression actor is invalid")
	}
	hasher, err := loadPersonEnrichmentSuppressionHasher(
		command.Context(), st, deps.config(), deps.lookupEnv)
	if err != nil {
		return err
	}
	configuredKeyID, err := hasher.KeyID()
	if err != nil {
		return err
	}
	if keyID != configuredKeyID {
		return personenrichment.ErrSuppressionKeyMismatch
	}
	input := store.PersonEnrichmentSuppressionInput{
		ProviderNamespace: namespace, IdentifierClass: class, NormalizationVersion: version,
		KeyID: keyID, Digest: digest, Reason: reason, Actor: actor,
	}
	if err := st.InsertPersonEnrichmentSuppressionsForConfiguredKeyContext(command.Context(), configuredKeyID,
		[]store.PersonEnrichmentSuppressionInput{input}); err != nil {
		return err
	}
	rows, err := st.ListPersonEnrichmentSuppressionsContext(command.Context(),
		store.PersonEnrichmentSuppressionFilter{
			ProviderNamespace: namespace, IdentifierClass: class,
			NormalizationVersion: version, KeyID: keyID, Limit: 1,
		})
	if err != nil {
		return err
	}
	if len(rows) != 1 {
		return errors.New("suppression insert was not durable")
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Suppressed %s digest %s\n", class, rows[0].DigestPrefix)
	if err != nil {
		return fmt.Errorf("write person enrichment suppression: %w", err)
	}
	return nil
}

func suppressPersonEnrichmentPerson(
	command *cobra.Command, deps personEnrichmentCommandDeps, st *store.Store,
	personID int64, reason store.PersonEnrichmentSuppressionReason,
) error {
	hasher, err := loadPersonEnrichmentSuppressionHasher(command.Context(), st, deps.config(), deps.lookupEnv)
	if err != nil {
		return err
	}
	adapter := &storeAPIAdapter{
		store: st, personEnrichmentConfig: deps.config(), lookupEnv: deps.lookupEnv,
	}
	configuredKeyID, err := hasher.KeyID()
	if err != nil {
		return err
	}
	const snapshotAttempts = 3
	var digestCount int
	for range snapshotAttempts {
		digests, revision, snapshotErr := adapter.personEnrichmentDeletionDigests(
			command.Context(), personID, hasher)
		if snapshotErr != nil {
			return snapshotErr
		}
		for i := range digests {
			digests[i].Reason = reason
			digests[i].Actor = personEnrichmentConsentActor
		}
		err = st.InsertPersonEnrichmentSuppressionsForPersonRevisionContext(
			command.Context(), personID, revision, configuredKeyID, digests)
		if !errors.Is(err, store.ErrPersonRevisionConflict) {
			if err != nil {
				return err
			}
			digestCount = len(digests)
			break
		}
	}
	if err != nil {
		return fmt.Errorf("person identity changed during suppression: %w", err)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Suppressed %d digest(s) for person %d\n", digestCount, personID)
	if err != nil {
		return fmt.Errorf("write person enrichment person suppression: %w", err)
	}
	return nil
}

func loadPersonEnrichmentSuppressionHasher(
	ctx context.Context, st *store.Store, config personenrichment.Config,
	lookup personenrichment.CredentialLookup,
) (*personenrichment.SuppressionHasher, error) {
	key, ok := lookup(config.SuppressionKeyEnv)
	if !ok || key == "" {
		return nil, fmt.Errorf("person enrichment suppression key environment %q is not set", config.SuppressionKeyEnv)
	}
	keyBytes := []byte(key)
	hasher, err := personenrichment.NewSuppressionHasher(keyBytes)
	clear(keyBytes)
	if err != nil {
		return nil, err
	}
	configuredKeyID, err := hasher.KeyID()
	if err != nil {
		return nil, err
	}
	keyIDs, err := st.ListPersonEnrichmentSuppressionKeyIDsContext(ctx)
	if err != nil {
		return nil, err
	}
	for _, keyID := range keyIDs {
		if keyID != configuredKeyID {
			return nil, personenrichment.ErrSuppressionKeyMismatch
		}
	}
	return hasher, nil
}

func personEnrichmentCLIReason(value string) (store.PersonEnrichmentSuppressionReason, error) {
	reason := store.PersonEnrichmentSuppressionReason(strings.TrimSpace(value))
	if reason != store.PersonEnrichmentSuppressionOptOut && reason != store.PersonEnrichmentSuppressionDataSubjectRequest {
		return "", errors.New("suppression reason must be opt_out or data_subject_request")
	}
	return reason, nil
}

func personEnrichmentProviderConfig(
	config personenrichment.Config, name string,
) (personenrichment.ProviderConfig, bool) {
	for _, provider := range config.Providers {
		if provider.Enabled && provider.Name == strings.TrimSpace(name) {
			return provider, true
		}
	}
	return personenrichment.ProviderConfig{}, false
}

func newPersonEnrichmentCLIWorker(
	ctx context.Context, st *store.Store, config personenrichment.Config,
	suppressionLookup personenrichment.CredentialLookup,
	providerLookup personenrichment.ProviderCredentialLookup,
) (personEnrichmentScheduleWorker, error) {
	if suppressionLookup == nil || providerLookup == nil {
		return nil, errors.New("person enrichment worker requires suppression and provider credential lookups")
	}
	hasher, err := loadPersonEnrichmentSuppressionHasher(ctx, st, config, suppressionLookup)
	if err != nil {
		return nil, err
	}
	catalog, err := st.BuildPersonFactCatalogContext(ctx, true)
	if err != nil {
		return nil, err
	}
	factories := make(map[string]personenrichment.ProviderFactory)
	providerConfigs := make(map[string]personenrichment.ProviderConfig)
	for _, configured := range config.Providers {
		provider := configured
		if !provider.Enabled {
			continue
		}
		profile, err := provider.Profile(catalog)
		if err != nil {
			return nil, err
		}
		if _, err := st.EnsurePersonEnrichmentProfile(ctx, profile); err != nil {
			return nil, err
		}
		providerConfigs[provider.Name] = provider
		switch provider.Kind {
		case personenrichment.ProviderExa:
			factories[provider.Name] = func(config personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
				return personenrichment.NewExaProvider(config, credential, http.DefaultClient)
			}
		case personenrichment.ProviderSixtyfour:
			factories[provider.Name] = func(config personenrichment.ProviderConfig, credential string) (personenrichment.Provider, error) {
				return personenrichment.NewSixtyfourProvider(config, credential, http.DefaultClient)
			}
		}
	}
	gate, err := personenrichment.NewProviderBoundEgressGate(st, st, hasher, providerLookup)
	if err != nil {
		return nil, err
	}
	return personenrichment.NewWorker(st, st, *gate, factories, personenrichment.WorkerOptions{
		Owner: "daemon-person-enrichment-manual", LeaseDuration: config.LeaseDuration,
		RenewEvery: config.LeaseDuration / 4, Clock: time.Now,
		Jitter: func(delay time.Duration) time.Duration { return delay }, ProviderConfigs: providerConfigs,
	})
}
