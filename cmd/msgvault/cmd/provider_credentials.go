package cmd

import (
	"errors"
	"net/http"
	"os"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

func providerHTTPClientWithoutRedirects(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	isolated := *client
	isolated.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &isolated
}

func resolveProviderCredentialFromSnapshot(
	snapshot providercredentials.Snapshot,
	id, endpoint, environment string,
) (string, error) {
	credential, _, err := snapshot.Resolve(id, endpoint, environment, os.LookupEnv)
	if err != nil {
		return "", err
	}
	return credential, nil
}

func personEnrichmentProviderCredentialLookup(
	cfg *config.Config,
) personenrichment.ProviderCredentialLookup {
	return func(profile personenrichment.ProviderProfile) (string, bool, error) {
		if cfg == nil {
			return "", false, errors.New("provider credential config is unavailable")
		}
		credentialEndpoint, err := (personenrichment.ProviderConfig{
			Kind: profile.Kind, Endpoint: profile.Endpoint, PollEndpoint: profile.PollEndpoint,
		}).CredentialEndpoint()
		if err != nil {
			return "", false, err
		}
		snapshot, err := providercredentials.Read(cfg.TokensDir())
		if err != nil {
			return "", false, err
		}
		credential, state, err := snapshot.Resolve(
			providercredentials.PersonEnrichmentID(profile.Name),
			credentialEndpoint, profile.APIKeyEnv, os.LookupEnv,
		)
		return credential, state.Configured, err
	}
}

func resolvePersonEnrichmentSuppression(cfg *config.Config) (string, error) {
	if cfg == nil {
		return "", errors.New("person enrichment suppression config is unavailable")
	}
	if cfg.People.Enrichment.SuppressionKeyEnv != providercredentials.StoredSuppressionEnvironment {
		value, ok := os.LookupEnv(cfg.People.Enrichment.SuppressionKeyEnv)
		if !ok || value == "" {
			return "", errors.New("person enrichment suppression key is unavailable")
		}
		return value, nil
	}
	snapshot, err := providercredentials.Read(cfg.TokensDir())
	if err != nil {
		return "", err
	}
	value, configured, err := snapshot.ResolveSuppression()
	if err != nil {
		return "", err
	}
	if !configured || value == "" {
		return "", errors.New("stored person enrichment suppression key is unavailable")
	}
	return value, nil
}

func personEnrichmentEnvironmentLookup(cfg *config.Config) personenrichment.CredentialLookup {
	return func(name string) (string, bool) {
		if name != providercredentials.StoredSuppressionEnvironment {
			return os.LookupEnv(name)
		}
		value, err := resolvePersonEnrichmentSuppression(cfg)
		return value, err == nil && value != ""
	}
}
