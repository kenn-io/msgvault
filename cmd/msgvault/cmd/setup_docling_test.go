package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestSetupDoclingLaneAndConsentWithoutManifest(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)

	cfg := config.NewDefaultConfig()
	cfg.HomeDir = t.TempDir()
	c := &cfg.Attachments.Documents
	c.Provider, c.Endpoint, c.Enabled = documentindex.ProviderDocling, "http://127.0.0.1:5001", true
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	st := testutil.NewSQLiteTestStore(t)
	_, profile, err := documentProfileForConfig(c, mistral.CapabilityManifest{})
	requirements.NoError(err)
	_, err = st.EnsureDocumentExtractionProfile(t.Context(), profile)
	requirements.NoError(err)
	requirements.NoError(st.RecordDocumentProviderConsent(t.Context(), store.DocumentProviderConsent{
		ProfileID: profile.ID, ProfileFingerprint: profile.Fingerprint,
		RetentionPosture: profile.RetentionPosture, TrainingPosture: profile.TrainingPosture,
	}))
	consent := setupConsentFromStore(t.Context(), cfg, st)
	assertions.True(consent.Documents)
	env := setupEnvironment{consent: consent, lookupEnv: func(string) (string, bool) { return "", false }}
	lane := documentsLane(cfg, env)
	assertions.Equal(laneStateOn, lane.State)
	assertions.Equal(documentindex.ProviderDocling, lane.Provider)
	assertions.Contains(lane.Reason, c.Endpoint)
	assertions.NotContains(lane.Reason, "MISTRAL_API_KEY")

	c.Endpoint = "http://127.0.0.1:5002"
	consent = setupConsentFromStore(t.Context(), cfg, st)
	assertions.False(consent.Documents)
	env.consent = consent
	lane = documentsLane(cfg, env)
	assertions.Equal(laneStatePending, lane.State)
	assertions.Equal([]string{"msgvault documents consent-docling --yes", "msgvault documents build --yes"}, lane.Next)
	c.APIKeyEnv = "SYNTHETIC_DOCLING_KEY"
	lane = documentsLane(cfg, env)
	assertions.Contains(lane.Reason, c.APIKeyEnv)
}

func TestSetupDoclingPlanPreservesOperatorConfiguration(t *testing.T) {
	assertions := assert.New(t)

	cfg := config.NewDefaultConfig()
	c := &cfg.Attachments.Documents
	c.Provider, c.Endpoint = documentindex.ProviderDocling, "http://127.0.0.1:5001"
	c.ApplyConfiguredProviderDefaults(func(string) bool { return false })
	for _, enabled := range []bool{false, true} {
		c.Enabled = enabled
		lane := planDocuments(cfg, setupDetection{mistralKey: true, mistralKeyEnv: "SYNTHETIC_DOCLING_KEY"}, setupProvidersOptions{})
		assertions.Equal(documentindex.ProviderDocling, lane.Provider)
		assertions.Empty(lane.edits)
		assertions.Contains(lane.Reason, c.Endpoint)
		assertions.NotContains(lane.Reason, "EU endpoint")
		if enabled {
			assertions.Equal(planActionKeep, lane.Action)
		} else {
			assertions.Equal(planActionSkip, lane.Action)
			assertions.Contains(lane.Reason, "enabled = true")
		}
	}
}
