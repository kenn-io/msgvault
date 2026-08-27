package api

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personenrichment"
)

func TestCLIRunCommandAllowedPermitsExactPersonProviderCommands(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "status", args: []string{"person", "provider", "status"}, want: true},
		{name: "status flags", args: []string{"person", "provider", "status", "--json"}, want: true},
		{name: "consent", args: []string{"person", "provider", "consent", "--yes"}, want: true},
		{name: "revoke", args: []string{"person", "provider", "revoke"}, want: true},
		{name: "check", args: []string{"person", "provider", "check", "--json"}, want: true},
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
	srv.cfg.People.Sweep.Provider.Kind = "openai_compatible"
	srv.cfg.People.Sweep.Provider.APIKeyEnv = "MSGVAULT_PEOPLE_PROVIDER_KEY"

	checks.True(srv.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))
	checks.False(srv.cliRunEnvAllowed("OPENAI_API_KEY"))

	unconfigured := &Server{cfg: &config.Config{}}
	checks.False(unconfigured.cliRunEnvAllowed("MSGVAULT_PEOPLE_PROVIDER_KEY"))

	codex := &Server{cfg: &config.Config{}}
	codex.cfg.People.Sweep.Provider.Kind = "codex_app_server"
	codex.cfg.People.Sweep.Provider.APIKeyEnv = "MSGVAULT_PEOPLE_PROVIDER_KEY"
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

func TestCLIAllowlistPersonSweepForwardsExactCredentialOnly(t *testing.T) {
	checks := assert.New(t)
	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Sweep.Provider.Kind = "openai_compatible"
	srv.cfg.People.Sweep.Provider.APIKeyEnv = "PEOPLE_SWEEP_KEY"
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
		checks.True(cliRunCommandAllowed(args), args)
	}
	for _, operation := range []string{"logout", "delete", "exec", "account"} {
		args := []string{"person", "provider", operation}
		checks.False(cliRunCommandAllowed(args), args)
	}

	srv := &Server{cfg: &config.Config{}}
	srv.cfg.People.Sweep.Provider.Kind = "codex_app_server"
	srv.cfg.People.Sweep.Provider.APIKeyEnv = "MUST_NOT_FORWARD"
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
