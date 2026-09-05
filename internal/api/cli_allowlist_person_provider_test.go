package api

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func configureAPIProvider(cfg *config.Config, provider peoplesweep.ProviderConfig) {
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{"default": provider}
}

func completeAPIProvider(keyEnv, model string) peoplesweep.ProviderConfig {
	return peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://provider.example/v1",
		Model: model, Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: keyEnv,
		OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens",
		RetentionPosture:    "zero_data_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceMeetingText},
		SourceSince:    "2025-01-01", RequestTimeout: time.Second,
	}
}

func TestCLIRunCommandAllowedPermitsExactPersonProviderCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "provider", "status"}, want: true},
		{name: "status flags", args: []string{"person", "provider", "status", "--json"}, want: true},
		{name: "named status", args: []string{"person", "provider", "status", "alpha", "--json"}, want: true},
		{name: "list", args: []string{"person", "provider", "list", "--json"}, want: true},
		{name: "consent", args: []string{"person", "provider", "consent", "--yes"}, want: true},
		{name: "revoke", args: []string{"person", "provider", "revoke"}, want: true},
		{name: "guarded revoke", args: []string{
			"person", "provider", "revoke", "alpha", "--if-fingerprint", strings.Repeat("a", 64),
		}, want: true},
		{name: "targeted revoke", args: []string{
			"person", "provider", "revoke", "alpha", "--fingerprint", strings.Repeat("b", 64),
		}, want: true},
		{name: "check", args: []string{"person", "provider", "check", "--json"}, want: true},
		{name: "named check", args: []string{"person", "provider", "check", "alpha", "--json"}, want: true},
		{name: "guarded check", args: []string{
			"person", "provider", "check", "alpha", "--if-fingerprint", strings.Repeat("a", 64),
		}, want: true},
		{name: "history", args: []string{"person", "provider", "history", "alpha", "--limit", "20"}, want: true},
		{name: "add is local", args: []string{"person", "provider", "add", "alpha"}},
		{name: "use is local", args: []string{"person", "provider", "use", "alpha"}},
		{name: "remove is local", args: []string{"person", "provider", "remove", "alpha"}},
		{name: "login is local", args: []string{"person", "provider", "login"}},
		{name: "models are local", args: []string{"person", "provider", "models"}},
		{name: "secret flag smuggling", args: []string{"person", "provider", "check", "--api-key=secret-canary"}},
		{name: "control profile name", args: []string{"person", "provider", "check", "bad\nname"}},
		{name: "oversize profile name", args: []string{"person", "provider", "history", strings.Repeat("x", 65)}},
		{name: "invalid guarded revoke fingerprint", args: []string{
			"person", "provider", "revoke", "alpha", "--if-fingerprint", "not-a-fingerprint",
		}},
		{name: "guarded revoke without profile", args: []string{
			"person", "provider", "revoke", "--if-fingerprint", strings.Repeat("a", 64),
		}},
		{name: "extra positional smuggling", args: []string{"person", "provider", "check", "alpha", "beta"}},
		{name: "list positional smuggling", args: []string{"person", "provider", "list", "alpha"}},
		{name: "missing operation", args: []string{"person", "provider"}},
		{name: "unknown operation", args: []string{"person", "provider", "run"}},
		{name: "ordinary person mutation", args: []string{"person", "delete", "7"}},
		{name: "different nested group", args: []string{"person", "attributes", "list", "7"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cliRunCommandAllowed(test.args))
		})
	}
}

func TestCLIRunEnvAllowedPermitsConfiguredPeopleProviderKeyOnly(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv,
		CredentialEnv: "MSGVAULT_PEOPLE_PROVIDER_KEY",
	})

	checks.True(srv.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
	checks.False(srv.cliRunEnvAllowed("OPENAI_API_KEY"))

	unconfigured := &Server{cfg: &config.Config{}}
	checks.False(unconfigured.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))

	codex := &Server{cfg: &config.Config{}}
	configureAPIProvider(codex.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Credential: peoplesweep.CredentialNone,
	})
	checks.False(codex.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
}

func TestCLIAllowlistPermitsExactPersonSweepCommands(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: []string{"person", "sweep", "run"}, want: true},
		{args: []string{"person", "sweep", "status", "--json"}, want: true},
		{args: []string{"person", "sweep", "history", "--limit", "20"}, want: true},
		{args: []string{"person", "sweep"}},
		{args: []string{"person", "sweep", "delete"}},
		{args: []string{"person", "sweep", "run-everything"}},
	}
	for _, test := range tests {
		assert.Equal(t, test.want, cliRunCommandAllowed(test.args), test.args)
	}
}

func TestCLIAllowlistRejectsProviderCredentialValuesInDaemonRequest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	cfg.People.Sweep.Enabled = true
	cfg.People.Sweep.Provider = peoplesweep.ProviderSelection{Name: "default"}
	cfg.People.Sweep.Providers = map[string]peoplesweep.ProviderConfig{
		"default": completeAPIProvider("STALE_ACTIVE_KEY", "active-model"),
	}
	require.NoError(cfg.Save())

	latest, err := config.Load(cfg.ConfigFilePath(), "")
	require.NoError(err)
	latest.People.Sweep.Providers["named"] = completeAPIProvider("EXACT_NAMED_KEY", "named-model")
	require.NoError(latest.Save())

	srv := &Server{cfg: cfg}
	args := []string{"person", "provider", "check", "named", "--json"}
	assert.False(srv.cliRunEnvAllowedForCommand(args, "EXACT_NAMED_KEY"))
	assert.False(srv.cliRunEnvAllowedForCommand(args, "STALE_ACTIVE_KEY"))
	assert.False(srv.cliRunEnvAllowedForCommand(args, "OPENAI_API_KEY"))
}

func TestCLIAllowlistPersonSweepForwardsExactCredentialOnly(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Credential: peoplesweep.CredentialEnv,
		CredentialEnv: "PEOPLE_SWEEP_KEY",
	})
	srv.cfg.Vector.Embeddings.APIKeyEnv = "EMBEDDINGS_KEY"
	srv.cfg.Attachments.Documents.APIKeyEnv = "DOCUMENT_KEY"

	checks.True(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "EMBEDDINGS_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "run"}, "DOCUMENT_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "status"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"person", "sweep", "history"}, "PEOPLE_SWEEP_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(
		[]string{"add-account"}, "PEOPLE_SWEEP_KEY"))
}

func TestCLIAllowlistCodexProviderOperations(t *testing.T) {
	checks := assert.New(t)
	for _, operation := range []string{"login", "models"} {
		args := []string{"person", "provider", operation}
		checks.False(cliRunCommandAllowed(args), args)
	}
	for _, operation := range []string{"logout", "delete", "exec", "account"} {
		args := []string{"person", "provider", operation}
		checks.False(cliRunCommandAllowed(args), args)
	}

	srv := &Server{cfg: &config.Config{}}
	configureAPIProvider(srv.cfg, peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolCodexAppServer, Credential: peoplesweep.CredentialNone,
	})
	for _, operation := range []string{"login", "models", "status", "check"} {
		checks.False(srv.cliRunEnvAllowedForCommand(
			[]string{"person", "provider", operation}, "MUST_NOT_FORWARD"), operation)
	}
}

func TestCLIRunCommandAllowedPermitsExactPersonEnrichmentCommandsAndRejectsRawShapes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "enrichment", "status", "--limit=20", "--json"}, want: true},
		{name: "profiles", args: []string{"person", "enrichment", "profiles"}, want: true},
		{name: "consent", args: []string{"person", "enrichment", "consent", strings.Repeat("a", 64)}, want: true},
		{name: "revoke", args: []string{"person", "enrichment", "revoke", "--all"}, want: true},
		{name: "run", args: []string{"person", "enrichment", "run", "--person=7", "--provider=exa-default", "--idempotency-key=run-1"}, want: true},
		{name: "safe digest suppression", args: []string{
			"person", "enrichment", "suppress", "--provider-namespace=exa:" + strings.Repeat("a", 64),
			"--identifier-class=email", "--normalization-version=email-v1", "--key-id=" + strings.Repeat("b", 64),
			"--digest=" + strings.Repeat("c", 64), "--reason=opt_out", "--actor=cli",
		}, want: true},
		{name: "person suppression", args: []string{"person", "enrichment", "suppress", "--person=7", "--reason=data_subject_request"}, want: true},
		{name: "raw provider flag rejected", args: []string{"person", "enrichment", "suppress", "--provider=exa-default", "--identifier-class=email", "--reason=opt_out"}},
		{name: "raw positional rejected", args: []string{"person", "enrichment", "suppress", "person@example.test", "--reason=opt_out"}},
		{name: "raw metadata field rejected", args: []string{
			"person", "enrichment", "suppress", "--provider-namespace=exa:" + strings.Repeat("a", 64),
			"--identifier-class=email", "--normalization-version=Opaque-ID", "--key-id=" + strings.Repeat("b", 64),
			"--digest=" + strings.Repeat("c", 64), "--reason=opt_out", "--actor=cli",
		}},
		{name: "unknown flag rejected", args: []string{"person", "enrichment", "status", "--credential=secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, cliRunCommandAllowed(test.args))
		})
	}
}

func TestCLIRunCommandAllowedRejectsPersonSuppressionMetadataSmuggling(t *testing.T) {
	for _, field := range []string{
		"provider-namespace", "identifier-class", "normalization-version", "key-id", "digest", "actor",
	} {
		t.Run(field, func(t *testing.T) {
			args := []string{"person", "enrichment", "suppress", "--person=7", "--reason=opt_out",
				"--" + field + "=RAW-MARKER"}
			assert.False(t, cliRunCommandAllowed(args))
		})
	}
}

func TestCLIAllowlistPersonEnrichmentForwardsOnlyCommandCredentials(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Enrichment.SuppressionKeyEnv = "ENRICHMENT_SUPPRESSION_KEY"
	srv.cfg.People.Enrichment.Providers = []personenrichment.ProviderConfig{
		{Name: "exa-primary", Enabled: true, APIKeyEnv: "EXA_PRIMARY_KEY"},
		{Name: "exa-secondary", Enabled: true, APIKeyEnv: "EXA_SECONDARY_KEY"},
		{Name: "exa-disabled", Enabled: false, APIKeyEnv: "EXA_DISABLED_KEY"},
	}

	run := []string{"person", "enrichment", "run", "--person=7", "--provider=exa-primary", "--idempotency-key=run-1"}
	checks.True(srv.cliRunEnvAllowedForCommand(run, "ENRICHMENT_SUPPRESSION_KEY"))
	checks.True(srv.cliRunEnvAllowedForCommand(run, "EXA_PRIMARY_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(run, "EXA_SECONDARY_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(run, "EXA_DISABLED_KEY"))

	personSuppression := []string{"person", "enrichment", "suppress", "--person=7", "--reason=data_subject_request"}
	checks.True(srv.cliRunEnvAllowedForCommand(personSuppression, "ENRICHMENT_SUPPRESSION_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand(personSuppression, "EXA_PRIMARY_KEY"))

	digestSuppression := []string{"person", "enrichment", "suppress", "--provider-namespace=exa:" + strings.Repeat("a", 64),
		"--identifier-class=email", "--normalization-version=email-v1", "--key-id=" + strings.Repeat("b", 64),
		"--digest=" + strings.Repeat("c", 64), "--reason=opt_out", "--actor=cli"}
	checks.False(srv.cliRunEnvAllowedForCommand(digestSuppression, "ENRICHMENT_SUPPRESSION_KEY"))
	checks.False(srv.cliRunEnvAllowedForCommand([]string{"person", "enrichment", "status"}, "EXA_PRIMARY_KEY"))
}
