package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personenrichment"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

// legacyConsentSchema is the SQLite consent DDL an older release created.
const legacyConsentSchema = `
DROP TABLE provider_consents;
CREATE TABLE person_inference_consents (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    profile_fingerprint  TEXT NOT NULL REFERENCES person_inference_profiles(fingerprint),
    granted_by           TEXT NOT NULL,
    granted_at           DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_by           TEXT,
    revoked_at           DATETIME,
    CHECK ((revoked_by IS NULL) = (revoked_at IS NULL))
);
CREATE UNIQUE INDEX idx_person_inference_consents_active
    ON person_inference_consents(profile_fingerprint)
    WHERE revoked_at IS NULL;
CREATE TABLE person_enrichment_consents (
    id                  INTEGER PRIMARY KEY,
    profile_fingerprint TEXT NOT NULL REFERENCES person_enrichment_profiles(fingerprint),
    granted_by          TEXT NOT NULL,
    granted_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    revoked_by          TEXT,
    revoked_at          DATETIME,
    CHECK ((revoked_by IS NULL) = (revoked_at IS NULL))
);
CREATE UNIQUE INDEX person_enrichment_consents_active
    ON person_enrichment_consents(profile_fingerprint)
    WHERE revoked_at IS NULL;
DELETE FROM applied_migrations WHERE name = 'provider_consents_v1';
`

func TestPersonProviderUpgradeImportsLegacyKeyAndConsent(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	authorizations := make(chan string, 1)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizations <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model":"test-model",
			"choices":[{"message":{"content":"{\"ok\":true}"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1}
		}`)
	}))
	t.Cleanup(provider.Close)

	peopleConfig := personProviderTestConfig()
	remote := configuredPersonProvider(peopleConfig)
	remote.Endpoint = provider.URL + "/v1"
	remote.Credential = peoplesweep.CredentialStored
	remote.CredentialEnv = ""
	peopleConfig.Provider = peoplesweep.ProviderSelection{Name: "remote"}
	peopleConfig.Providers = map[string]peoplesweep.ProviderConfig{"remote": remote}
	profile, err := peopleConfig.Profile()
	require.NoError(err)

	fixture := &storetest.Fixture{T: t, Store: testutil.NewSQLiteTestStore(t)}
	st := fixture.Store
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	enrichmentProvider, enrichmentProfile, _ := scheduleWorkerProfile(t, fixture, "legacy-enrichment", "TEST_LEGACY_ENRICHMENT_KEY")

	_, err = st.DB().Exec(legacyConsentSchema)
	require.NoError(err)
	_, err = st.DB().Exec(`
		INSERT INTO person_inference_consents (id, profile_fingerprint, granted_by, granted_at, revoked_by, revoked_at)
		VALUES (1, ?, 'cli', '2025-01-02 03:04:05', 'web', '2025-01-03 04:05:06'),
		       (2, ?, 'cli', '2025-02-01 09:00:00', NULL, NULL)`, profile.Fingerprint, profile.Fingerprint)
	require.NoError(err)
	_, err = st.DB().Exec(`
		INSERT INTO person_enrichment_consents (id, profile_fingerprint, granted_by, granted_at)
		VALUES (7, ?, 'cli', '2025-03-04 05:06:07')`, enrichmentProfile.Fingerprint)
	require.NoError(err)
	tokensDir := t.TempDir()
	legacyPath := writeLegacyPeopleCredentialForTest(t, tokensDir, "remote", `{"scheme":"bearer","value":"legacy-upgrade-key"}`)

	require.NoError(st.InitSchema())

	credentials := peoplesweep.NewStoredCredentials(tokensDir)
	deps := localPersonProviderDeps(peopleConfig, st, nil)
	deps.setup.credentials = credentials
	deps.newChecker = func(
		config peoplesweep.Config, consent personProviderStore, _ personProviderSetupDeps,
	) (personProviderChecker, error) {
		registry, err := peoplesweep.NewDriverRegistry(provider.Client(), nil, nil)
		if err != nil {
			return nil, err
		}
		return peoplesweep.NewRunner(config, consent, registry, peoplesweep.NewCredentialResolver(credentials, nil))
	}
	statusJSON, err := executePersonProviderCommand(t, deps, "status", "remote", "--json")
	require.NoError(err)
	var status struct {
		Consent json.RawMessage `json:"consent"`
	}
	require.NoError(json.Unmarshal([]byte(statusJSON), &status))
	assert.JSONEq(`{
		"fingerprint":"`+profile.Fingerprint+`","profile_exists":true,"active":true,
		"consent":{"id":2,"profile_fingerprint":"`+profile.Fingerprint+`","granted_by":"cli","granted_at":"2025-02-01T09:00:00Z"},
		"last_revoked":{"id":1,"profile_fingerprint":"`+profile.Fingerprint+`","granted_by":"cli","granted_at":"2025-01-02T03:04:05Z","revoked_by":"web","revoked_at":"2025-01-03T04:05:06Z"}
	}`, string(status.Consent))

	enrichmentDeps := testPersonEnrichmentCommandDeps(t, personenrichment.Config{
		Enabled: true, SuppressionKeyEnv: "TEST_SUPPRESSION_KEY",
		Providers: []personenrichment.ProviderConfig{enrichmentProvider},
	}, st)
	enrichmentJSON, _, err := executePersonEnrichmentCommand(t, enrichmentDeps, "", "status", "--json")
	require.NoError(err)
	var enrichment struct {
		Consents []json.RawMessage `json:"consents"`
	}
	require.NoError(json.Unmarshal([]byte(enrichmentJSON), &enrichment))
	require.Len(enrichment.Consents, 1)
	assert.JSONEq(`{
		"fingerprint":"`+enrichmentProfile.Fingerprint+`","profile_exists":true,"active":true,
		"consent":{"id":7,"profile_fingerprint":"`+enrichmentProfile.Fingerprint+`","granted_by":"cli","granted_at":"2025-03-04T05:06:07Z"}
	}`, string(enrichment.Consents[0]))

	checkJSON, err := executePersonProviderCommand(t, deps, "check", "remote", "--json")
	require.NoError(err, checkJSON)
	assert.Equal("Bearer legacy-upgrade-key", <-authorizations)
	assert.NoFileExists(legacyPath)
	assert.NotContains(checkJSON, "legacy-upgrade-key")
	assert.NotContains(statusJSON, "legacy-upgrade-key")
}
