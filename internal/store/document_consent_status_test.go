package store_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/mistral/mistraltest"

	"go.kenn.io/msgvault/internal/documentindex"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestDocumentConsentTimestampRetainsExactRecordedIdentity(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	fixture := storetest.New(t)
	config := documentindex.DefaultDocumentsConfig()
	config.RetentionPosture = documentindex.RetentionStandard
	config.TrainingPosture = documentindex.TrainingOptedOut
	policy, err := config.MistralPolicy()
	requirements.NoError(err)
	manifest, err := mistraltest.SyntheticManifest(policy, true)
	requirements.NoError(err)
	input, err := documentindex.ResolveInputPolicy(&config, manifest)
	requirements.NoError(err)
	fingerprint, err := config.ProfileFingerprint(manifest, input.AllowedMediaTypes)
	requirements.NoError(err)
	canonical, err := config.ProfilePolicyJSON(manifest, input.AllowedMediaTypes)
	requirements.NoError(err)
	values := policy.Values()
	profile := store.DocumentExtractionProfile{ID: "documents-v1:" + fingerprint, Fingerprint: fingerprint, Provider: values.Provider, Endpoint: values.Endpoint, Region: values.Region, Model: values.Model, RetentionPosture: values.Retention, TrainingPosture: values.Training, AllowedMediaTypes: input.AllowedMediaTypes, PolicyJSON: canonical}
	stamp, err := fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	assertions.Nil(stamp)
	_, err = fixture.Store.EnsureDocumentExtractionProfile(t.Context(), profile)
	requirements.NoError(err)
	stamp, err = fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	assertions.Nil(stamp)
	consent := store.DocumentProviderConsent{ProfileID: profile.ID, ProfileFingerprint: fingerprint, RetentionPosture: values.Retention, TrainingPosture: values.Training}
	requirements.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), consent))
	stamp, err = fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	requirements.NotNil(stamp)
	assertions.False(stamp.IsZero())
	// Backdate the synthetic stored record so timestamp renewal is observable
	// even with SQLite's second-resolution database clock.
	historical := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err = fixture.Store.DB().ExecContext(t.Context(), fixture.Store.Rebind(`UPDATE document_provider_consents SET consented_at = ? WHERE profile_id = ?`), historical, profile.ID)
	requirements.NoError(err)
	stamp, err = fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	requirements.NotNil(stamp)
	assertions.True(historical.Equal(*stamp))
	requirements.NoError(fixture.Store.RecordDocumentProviderConsent(t.Context(), consent))
	again, err := fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	requirements.NotNil(again)
	assertions.True(stamp.Equal(*again), "repeated consent must retain its stored timestamp")
	other, err := fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, strings.Repeat("0", 64))
	requirements.NoError(err)
	assertions.Nil(other)
	_, err = fixture.Store.RetireDocumentExtractionProfile(t.Context(), profile.ID)
	requirements.NoError(err)
	retired, err := fixture.Store.GetDocumentProviderConsentTime(t.Context(), profile.ID, fingerprint)
	requirements.NoError(err)
	requirements.NotNil(retired)
	assertions.True(stamp.Equal(*retired))
	status, err := fixture.Store.GetDocumentIndexStatus(t.Context(), profile.ID)
	requirements.NoError(err)
	assertions.True(status.ExactConsent, "retirement retains the exact recorded consent")
	assertions.False(status.ProfileEnabled)
	active, err := fixture.Store.HasMatchingDocumentProviderConsent(t.Context(), profile)
	requirements.NoError(err)
	assertions.False(active)
}
