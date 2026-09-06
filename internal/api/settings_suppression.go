package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/providercredentials"
)

var errSuppressionUnavailable = errors.New("person-enrichment suppression key is unavailable")

func prepareFirstEnrichmentEnable(
	cfg *config.Config,
	updates []SettingUpdate,
	credentials providercredentials.Snapshot,
) ([]config.Edit, providercredentials.Snapshot, string, error) {
	if cfg.People.Enrichment.Enabled || !requestsEnrichmentEnable(updates) {
		return nil, credentials, "", nil
	}
	environmentName := cfg.People.Enrichment.SuppressionKeyEnv
	if environmentName != "" && environmentName != providercredentials.StoredSuppressionEnvironment {
		value, ok := os.LookupEnv(environmentName)
		if !ok {
			return nil, credentials, "", errSuppressionUnavailable
		}
		if _, err := personenrichment.NewSuppressionHasher([]byte(value)); err != nil {
			return nil, credentials, "", errSuppressionUnavailable
		}
		return nil, credentials, "", nil
	}
	stored, ok, err := credentials.ResolveSuppression()
	if err != nil {
		return nil, credentials, "", err
	}
	generated := ""
	if !ok {
		secretBytes := make([]byte, 48)
		if _, err := rand.Read(secretBytes); err != nil {
			return nil, credentials, "", fmt.Errorf("generate suppression key: %w", err)
		}
		stored = base64.RawURLEncoding.EncodeToString(secretBytes)
		clear(secretBytes)
		credentials, err = providercredentials.PutSuppression(cfg.TokensDir(), credentials.ETag, stored)
		if err != nil {
			return nil, credentials, "", err
		}
		generated = stored
	}
	if _, err := personenrichment.NewSuppressionHasher([]byte(stored)); err != nil {
		return nil, credentials, generated, errSuppressionUnavailable
	}
	return []config.Edit{{
		Key: "people.enrichment.suppression_key_env", Value: providercredentials.StoredSuppressionEnvironment,
	}}, credentials, generated, nil
}

func requestsEnrichmentEnable(updates []SettingUpdate) bool {
	for _, update := range updates {
		if update.Key == "people.enrichment.enabled" && update.Value != nil &&
			update.Value.Boolean != nil && *update.Value.Boolean {
			return true
		}
	}
	return false
}
