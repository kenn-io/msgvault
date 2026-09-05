//go:build sqlite_vec

package cmd

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/voyage"
	"go.kenn.io/docbank/document/voyage/voyagetest"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/providercredentials"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"go.kenn.io/msgvault/internal/vector/sqlitevec"
	"go.kenn.io/msgvault/internal/vector/visual"
)

type visualCredentialRoundTripFunc func(*http.Request) (*http.Response, error)

func TestSetupVisualConsentMatchesCurrentGenerationAndManifest(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := config.NewDefaultConfig()
	c.HomeDir = t.TempDir()
	c.Vector.Multimodal.Enabled = true
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{
		Model: c.Vector.Multimodal.Model, Dimension: c.Vector.Multimodal.Dimension,
		Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true},
	})
	require.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(err)
	c.Vector.Multimodal.CapabilitiesFile = filepath.Join(c.HomeDir, "capabilities.json")
	require.NoError(writeVisualCapabilityManifest(c.Vector.Multimodal.CapabilitiesFile, manifest))
	fingerprint, err := policy.Fingerprint(manifest)
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: c.Vector.MultimodalGenerationFingerprint(),
		Model:       c.Vector.Multimodal.Model, Dimension: c.Vector.Multimodal.Dimension,
	})
	require.NoError(err)
	require.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, fingerprint))
	assert.True(setupConsentFromStore(t.Context(), c, st).Visual, "matching building generation")
	_, err = st.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	require.NoError(err)
	assert.True(setupConsentFromStore(t.Context(), c, st).Visual, "matching active generation")

	contextChars := c.Vector.Multimodal.MaxContextChars
	c.Vector.Multimodal.MaxContextChars++
	assert.False(setupConsentFromStore(t.Context(), c, st).Visual, "configuration changed")
	c.Vector.Multimodal.MaxContextChars = contextChars
	changed, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText, voyage.CapabilityImagePNG)
	require.NoError(err)
	c.Vector.Multimodal.CapabilitiesFile = filepath.Join(c.HomeDir, "changed-capabilities.json")
	require.NoError(writeVisualCapabilityManifest(c.Vector.Multimodal.CapabilitiesFile, changed))
	env := setupEnvironment{
		lookupEnv: func(string) (string, bool) { return "synthetic-key", true }, fileExists: defaultFileExists,
		consent: setupConsentFromStore(t.Context(), c, st),
	}
	lane := visualSearchLane(c, env)
	assert.Equal(laneStatePending, lane.State)
	assert.Equal([]string{"msgvault multimodal build --yes"}, lane.Next)
	changedFingerprint, err := policy.Fingerprint(changed)
	require.NoError(err)
	vf := &visualFeatures{Archive: st, Generation: generation, PolicyFingerprint: changedFingerprint}
	require.ErrorContains(requireVisualConsent(t.Context(), vf), "different capability manifest")
	require.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, changedFingerprint))
	require.NoError(requireVisualConsent(t.Context(), vf))
	assert.True(setupConsentFromStore(t.Context(), c, st).Visual, "new policy consent")
	c.Vector.Multimodal.CapabilitiesFile = filepath.Join(c.HomeDir, "missing.json")
	assert.False(setupConsentFromStore(t.Context(), c, st).Visual, "manifest cannot be read")
}

func (f visualCredentialRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type unavailableVisualCredentialOpener struct{}

func (unavailableVisualCredentialOpener) OpenStream(
	context.Context, string,
) (io.ReadCloser, int64, error) {
	return nil, 0, errors.New("content unavailable in credential test")
}

func TestNewVisualRuntimeUsesStoredCredentialSnapshotAndRejectsRedirectReplay(t *testing.T) {
	t.Setenv("VOYAGE_API_KEY", "")
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	mainStore, err := store.Open(mainPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mainStore.Close() })
	require.NoError(t, mainStore.InitSchema())
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{
		Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath,
		MainDB: mainStore.DB(), Dimension: 4,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })

	policy, err := voyage.NewPolicy(voyage.PolicyConfig{
		Model: voyage.DefaultModel, Dimension: 1024,
		Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true},
	})
	require.NoError(t, err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(t, err)
	manifestPath := filepath.Join(dir, "voyage-capabilities.json")
	require.NoError(t, writeVisualCapabilityManifest(manifestPath, manifest))

	cfg := config.NewDefaultConfig()
	cfg.Data.DataDir = dir
	cfg.Vector.Multimodal.Enabled = true
	cfg.Vector.Multimodal.CapabilitiesFile = manifestPath
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(t, err)
	stored, err := providercredentials.Put(cfg.TokensDir(), empty.ETag,
		providercredentials.VectorMultimodalID, cfg.Vector.Multimodal.Endpoint, "stored-at-startup")
	require.NoError(t, err)
	startup, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(t, err)
	apiKey, err := resolveProviderCredentialFromSnapshot(startup,
		providercredentials.VectorMultimodalID, cfg.Vector.Multimodal.Endpoint,
		cfg.Vector.Multimodal.APIKeyEnv)
	require.NoError(t, err)
	_, err = providercredentials.Put(cfg.TokensDir(), stored.ETag,
		providercredentials.VectorMultimodalID, cfg.Vector.Multimodal.Endpoint, "stored-after-startup")
	require.NoError(t, err)

	var authorization string
	var redirectedAuthorization string
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		redirectedAuthorization = r.Header.Get("Authorization")
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Location", target.URL+"/replayed")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)
	targetURL, err := url.Parse(origin.URL)
	require.NoError(t, err)
	transport := origin.Client().Transport
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: visualCredentialRoundTripFunc(
		func(request *http.Request) (*http.Response, error) {
			clone := request.Clone(request.Context())
			clone.URL.Scheme = targetURL.Scheme
			clone.URL.Host = targetURL.Host
			return transport.RoundTrip(clone)
		},
	)}

	runtime, err := newVisualRuntime(
		t.Context(), cfg.Vector, mainStore, backend, unavailableVisualCredentialOpener{},
		visualRuntimeCredential{APIKey: apiKey, HTTPClient: httpClient},
	)
	require.NoError(t, err)
	_, _, err = runtime.Provider.EmbedQuery(t.Context(), visual.QueryInput{Text: "private query"})
	require.Error(t, err)
	assert.Equal(t, "Bearer stored-at-startup", authorization)
	assert.Empty(t, redirectedAuthorization)
}
