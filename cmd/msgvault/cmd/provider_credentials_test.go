package cmd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

type activeEnrichmentConsent struct{ events *[]string }

func (c activeEnrichmentConsent) HasActivePersonEnrichmentConsent(context.Context, string) (bool, error) {
	*c.events = append(*c.events, "consent")
	return true, nil
}

type installingEnrichmentSuppression struct {
	events  *[]string
	keyID   string
	install func() error
}

func (s *installingEnrichmentSuppression) ListPersonEnrichmentSuppressionKeyIDsContext(
	context.Context,
) ([]string, error) {
	*s.events = append(*s.events, "key_ids")
	return []string{s.keyID}, nil
}

func (s *installingEnrichmentSuppression) HasPersonEnrichmentSuppressionContext(
	context.Context, personenrichment.SuppressionLookup,
) (bool, error) {
	*s.events = append(*s.events, "suppression")
	if err := s.install(); err != nil {
		return false, err
	}
	return false, nil
}

func TestVectorProviderCredentialResolutionUsesOneStartupSnapshot(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Setenv("TEXT_EMBEDDING_KEY", "environment-secret")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.Vector.Embeddings.Endpoint = "https://embeddings.example.test/v1"
	cfg.Vector.Embeddings.APIKeyEnv = "TEXT_EMBEDDING_KEY"

	empty, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	firstStore, err := providercredentials.Put(cfg.TokensDir(), empty.ETag,
		providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "stored-first")
	requirements.NoError(err)
	startup, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)

	first, err := resolveProviderCredentialFromSnapshot(startup,
		providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint,
		cfg.Vector.Embeddings.APIKeyEnv)
	requirements.NoError(err)
	assertions.Equal("stored-first", first)

	_, err = providercredentials.Put(cfg.TokensDir(), firstStore.ETag,
		providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint, "stored-second")
	requirements.NoError(err)
	stillFirst, err := resolveProviderCredentialFromSnapshot(startup,
		providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint,
		cfg.Vector.Embeddings.APIKeyEnv)
	requirements.NoError(err)
	assertions.Equal("stored-first", stillFirst)

	restarted, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	second, err := resolveProviderCredentialFromSnapshot(restarted,
		providercredentials.VectorEmbeddingsID, cfg.Vector.Embeddings.Endpoint,
		cfg.Vector.Embeddings.APIKeyEnv)
	requirements.NoError(err)
	assertions.Equal("stored-second", second)
}

func TestPersonEnrichmentProviderCredentialLookupUsesStableNameAndReloadsStore(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Setenv("SHARED_EXA_KEY", "environment-secret")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	profile := personenrichment.ProviderProfile{
		Name: "exa-primary", Kind: personenrichment.ProviderExa,
		Endpoint: "https://api.example.test/search", APIKeyEnv: "SHARED_EXA_KEY",
	}
	empty, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	firstStore, err := providercredentials.Put(cfg.TokensDir(), empty.ETag,
		providercredentials.PersonEnrichmentID(profile.Name), profile.Endpoint, "stored-first")
	requirements.NoError(err)
	lookup := personEnrichmentProviderCredentialLookup(cfg)

	first, configured, err := lookup(profile)
	requirements.NoError(err)
	assertions.True(configured)
	assertions.Equal("stored-first", first)

	_, err = providercredentials.Put(cfg.TokensDir(), firstStore.ETag,
		providercredentials.PersonEnrichmentID(profile.Name), profile.Endpoint, "stored-second")
	requirements.NoError(err)
	second, configured, err := lookup(profile)
	requirements.NoError(err)
	assertions.True(configured)
	assertions.Equal("stored-second", second)
}

func TestStoredSuppressionLookupFeedsReservedRuntimeEnvironmentAndFailsClosed(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.People.Enrichment.SuppressionKeyEnv = providercredentials.StoredSuppressionEnvironment
	empty, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	_, err = providercredentials.PutSuppression(cfg.TokensDir(), empty.ETag, "stored-suppression-key-0123456789012345")
	requirements.NoError(err)
	lookup := personEnrichmentEnvironmentLookup(cfg)

	value, configured := lookup(providercredentials.StoredSuppressionEnvironment)
	assertions.True(configured)
	assertions.Equal("stored-suppression-key-0123456789012345", value)

	path := filepath.Join(cfg.TokensDir(), providercredentials.Filename)
	requirements.NoError(os.WriteFile(path, []byte(`{"version":1,"credentials":`), 0o600))
	value, configured = lookup(providercredentials.StoredSuppressionEnvironment)
	assertions.False(configured)
	assertions.Empty(value)
}

func TestPersonEnrichmentGateLoadsStableStoredCredentialOnlyAfterSuppression(t *testing.T) {
	requirements := require.New(t)
	t.Setenv("SCHEDULE_PROVIDER_KEY", "")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	profile := scheduleTestEnrichmentProfile(t)
	empty, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	hasher, err := personenrichment.NewSuppressionHasher([]byte("0123456789abcdef0123456789abcdef"))
	requirements.NoError(err)
	keyID, err := hasher.KeyID()
	requirements.NoError(err)
	events := []string{}
	suppressions := &installingEnrichmentSuppression{
		events: &events, keyID: keyID,
		install: func() error {
			_, putErr := providercredentials.Put(cfg.TokensDir(), empty.ETag,
				providercredentials.PersonEnrichmentID(profile.Name), profile.Endpoint,
				"stored-after-suppression")
			return putErr
		},
	}
	gate, err := personenrichment.NewProviderBoundEgressGate(
		activeEnrichmentConsent{events: &events}, suppressions, hasher,
		personEnrichmentProviderCredentialLookup(cfg),
	)
	requirements.NoError(err)

	authorization, err := gate.Authorize(t.Context(), personenrichment.EgressInput{
		Request: personenrichment.Request{Identity: personenrichment.Identity{Email: "person@example.com"}},
		Profile: profile,
	})
	requirements.NoError(err)
	assert.Equal(t, "stored-after-suppression", authorization.Credential)
	assert.Equal(t, []string{"consent", "key_ids", "suppression"}, events)
}

func TestDefaultCLIProxyLookupsNeverForwardStoredCredentials(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	t.Setenv("TEST_PROVIDER_KEY", "")
	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = t.TempDir()
	cfg.People.Sweep = personProviderTestConfig()
	cfg.People.Enrichment = personEnrichmentCLIConfig(providercredentials.StoredSuppressionEnvironment)
	sweepName, sweepProvider, err := cfg.People.Sweep.ActiveProviderConfig()
	requirements.NoError(err)
	sweepProvider.Credential = peoplesweep.CredentialStored
	sweepProvider.CredentialEnv = ""
	cfg.People.Sweep.Providers[sweepName] = sweepProvider
	requirements.NoError(peoplesweep.NewFileCredentialStore(cfg.TokensDir()).Save(
		sweepName, peoplesweep.NewCredential(sweepProvider.Auth, "stored-sweep-secret")))
	empty, err := providercredentials.Read(cfg.TokensDir())
	requirements.NoError(err)
	stored, err := providercredentials.PutSuppression(cfg.TokensDir(), empty.ETag, "stored-suppression-secret-0123456789")
	requirements.NoError(err)
	_, err = providercredentials.Put(cfg.TokensDir(), stored.ETag,
		providercredentials.PersonEnrichmentID(cfg.People.Enrichment.Providers[0].Name),
		cfg.People.Enrichment.Providers[0].Endpoint, "stored-enrichment-secret")
	requirements.NoError(err)
	withTestConfig(t, cfg)

	sweepDeps := defaultPersonSweepCommandDeps()
	assertions.Empty(personSweepForwardEnv(cfg.People.Sweep, sweepDeps.lookupEnv))
	enrichmentDeps := defaultPersonEnrichmentCommandDeps()
	value, ok := enrichmentDeps.proxyLookupEnv(providercredentials.StoredSuppressionEnvironment)
	assertions.False(ok)
	assertions.Empty(value)
}
