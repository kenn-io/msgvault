//go:build sqlite_vec

package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/docbank/document/media"
	"go.kenn.io/docbank/document/media/mediatest"
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

func TestSetupVisualConsentResolvesAccountScope(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	c := config.NewDefaultConfig()
	c.HomeDir = t.TempDir()
	c.Vector.Multimodal.Enabled = true
	c.Vector.Multimodal.Scope.Accounts = []string{"visual@example.com"}
	policy, err := voyage.NewPolicy(voyage.PolicyConfig{
		Model: c.Vector.Multimodal.Model, Dimension: c.Vector.Multimodal.Dimension,
		Media: media.Policy{MaxBytes: 20 << 20, MaxPixels: 16_000_000, AllowStill: true, AllowVideo: true},
	})
	require.NoError(err)
	manifest, err := voyagetest.SyntheticManifest(policy, voyage.CapabilityQueryText)
	require.NoError(err)
	c.Vector.Multimodal.CapabilitiesFile = filepath.Join(c.HomeDir, "capabilities.json")
	require.NoError(writeVisualCapabilityManifest(c.Vector.Multimodal.CapabilitiesFile, manifest))
	policyFingerprint, err := policy.Fingerprint(manifest)
	require.NoError(err)
	st := testutil.NewSQLiteTestStore(t)
	source, err := st.GetOrCreateSource("gmail", "visual@example.com")
	require.NoError(err)
	// Runtime generations bind source IDs, not the account names in TOML.
	runtimeConfig := c.Vector
	runtimeConfig.Multimodal.Scope.SourceIDs = []int64{source.ID}
	generation, err := st.EnsureVisualGeneration(t.Context(), store.VisualGenerationSpec{
		Fingerprint: runtimeConfig.MultimodalGenerationFingerprint(),
		Model:       c.Vector.Multimodal.Model, Dimension: c.Vector.Multimodal.Dimension,
	})
	require.NoError(err)
	require.NoError(st.ConsentVisualGeneration(t.Context(), generation.ID, policyFingerprint))
	assert.True(setupConsentFromStore(t.Context(), c, st).Visual, "matching building account scope")
	_, err = st.ActivateVisualGeneration(t.Context(), generation.ID, 0)
	require.NoError(err)
	assert.True(setupConsentFromStore(t.Context(), c, st).Visual, "matching active account scope")
	assert.Empty(c.Vector.Multimodal.Scope.SourceIDs, "status must not mutate the supplied config")
	_, err = st.GetOrCreateSource("gmail", "other@example.com")
	require.NoError(err)
	for _, account := range []string{"other@example.com", "missing@example.com"} {
		changed := *c
		changed.Vector.Multimodal.Scope.Accounts = []string{account}
		env := setupEnvironment{
			lookupEnv: func(string) (string, bool) { return "synthetic-key", true }, fileExists: defaultFileExists,
			consent: setupConsentFromStore(t.Context(), &changed, st),
		}
		assert.Equal(laneStatePending, visualSearchLane(&changed, env).State, account)
	}
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
	mainStore := testutil.NewSQLiteTestStoreAt(t, mainPath)
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
	testCtx := testInvocationContext(t.Context(), cfg, invocationOptions{})
	_ = testCtx
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
		testCtx, cfg.Vector, mainStore, backend, unavailableVisualCredentialOpener{},
		visualRuntimeCredential{APIKey: apiKey, HTTPClient: httpClient},
	)
	require.NoError(t, err)
	_, _, err = runtime.Provider.EmbedQuery(testCtx, visual.QueryInput{Text: "private query"})
	require.Error(t, err)
	assert.Equal(t, "Bearer stored-at-startup", authorization)
	assert.Empty(t, redirectedAuthorization)
}

type kitTestOpener map[string][]byte

func (o kitTestOpener) OpenStream(_ context.Context, hash string) (io.ReadCloser, int64, error) {
	data := o[hash]
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

type countingVisualOpener struct {
	kitTestOpener
	reads int
}

func (o *countingVisualOpener) OpenStream(ctx context.Context, hash string) (io.ReadCloser, int64, error) {
	o.reads++
	return o.kitTestOpener.OpenStream(ctx, hash)
}

func TestKitVisualRuntimeConsentWithoutManifest(t *testing.T) {
	for _, tc := range []struct{ name, api, key string }{{"kit anonymous", "kit", ""}, {"kit keyed", "kit", "synthetic-kit-key"}, {"vertex", "vertex", ""}} {
		t.Run(tc.name, func(t *testing.T) { testKitVisualRuntime(t, tc.api, tc.key) })
	}
}

func testKitVisualRuntime(t *testing.T, api, key string) {
	t.Helper()
	images := make([][]byte, 2)
	for i, shade := range []uint8{50, 200} {
		width, height := 2, 2
		if i == 0 {
			width, height = 4001, 4000
		}
		img := image.NewRGBA(image.Rect(0, 0, width, height))
		img.Set(0, 0, color.RGBA{R: shade, A: 255})
		var buf bytes.Buffer
		require.NoError(t, png.Encode(&buf, img))
		images[i] = buf.Bytes()
		require.LessOrEqual(t, len(images[i]), 20<<20)
	}
	documentRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key != "" {
			assert.Equal(t, "Bearer "+key, r.Header.Get("Authorization"))
		}
		if api == "vertex" {
			assert.Equal(t, "Bearer synthetic-google-token", r.Header.Get("Authorization"))
		}
		var payload map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
		var data string
		if api == "vertex" {
			assert.Equal(t, "/v1/projects/project-a/locations/us-central1/publishers/google/models/gemini-embedding-2:embedContent", r.URL.Path)
			content := payload["content"].(map[string]any)
			parts := content["parts"].([]any)
			part := parts[0].(map[string]any)
			if inline, ok := part["inlineData"].(map[string]any); ok {
				assert.Equal(t, "image/png", inline["mimeType"])
				data = inline["data"].(string)
			} else {
				assert.Equal(t, "task: search result | query: beach at sunset", part["text"])
			}
		} else {
			assert.Equal(t, "/v1/embed", r.URL.Path)
			inputs := payload["inputs"].([]any)
			input := inputs[0].(map[string]any)
			data, _ = input["data"].(string)
		}
		if data != "" {
			documentRequests++
		}
		vector := make([]float32, 1024)
		vector[0] = 1
		if data == base64.StdEncoding.EncodeToString(images[1]) {
			vector[0], vector[1] = 0, 1
		}
		if api == "vertex" {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"embedding": map[string]any{"values": vector}}))
		} else {
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}}))
		}
	}))
	t.Cleanup(server.Close)
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	st := testutil.NewSQLiteTestStoreAt(t, mainPath)
	backend, err := sqlitevec.Open(t.Context(), sqlitevec.Options{Path: filepath.Join(dir, "vectors.db"), MainPath: mainPath, MainDB: st.DB(), Dimension: 1024})
	require.NoError(t, err)
	t.Cleanup(func() { _ = backend.Close() })
	source, err := st.GetOrCreateSource("gmail", "visual@example.com")
	require.NoError(t, err)
	conversation, err := st.EnsureConversation(t.Context(), source.ID, "thread", "Thread")
	require.NoError(t, err)
	opener := kitTestOpener{}
	var messages []int64
	for i, hash := range []string{strings.Repeat("a1", 32), strings.Repeat("b2", 32)} {
		id, err := st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: source.ID, SourceMessageID: fmt.Sprintf("photo-%d", i), MessageType: "email"})
		require.NoError(t, err)
		messages = append(messages, id)
		opener[hash] = images[i]
		require.NoError(t, st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{Filename: "photo.png", MIMEType: "image/png", StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Size: int64(len(images[i])), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1"}))
	}
	for i, data := range [][]byte{mediatest.GIF(2, 2, 1), mediatest.MP4(640, 360, 120001)} {
		hash := []string{strings.Repeat("c3", 32), strings.Repeat("d4", 32)}[i]
		id, err := st.UpsertMessage(&store.Message{ConversationID: conversation, SourceID: source.ID, SourceMessageID: fmt.Sprintf("excluded-%d", i), MessageType: "email"})
		require.NoError(t, err)
		opener[hash] = data
		require.NoError(t, st.UpsertAttachmentRecord(t.Context(), id, store.AttachmentWrite{Filename: "excluded", StoragePath: hash[:2] + "/" + hash, ContentHash: hash, Size: int64(len(data)), Role: store.AttachmentRoleStandalone, RoleSource: store.AttachmentRoleSourceImporterSemantics, SourcePartKey: "part:1"}))
	}
	cfg := config.NewDefaultConfig()
	cfg.Vector.Multimodal.Enabled = true
	cfg.Vector.Multimodal.Provider = api
	cfg.Vector.Multimodal.Endpoint = server.URL + "/v1"
	cfg.Vector.Multimodal.Model = "model"
	if api == "vertex" {
		cfg.Vector.Multimodal.Endpoint += "/projects/project-a/locations/us-central1/publishers/google/models"
		cfg.Vector.Multimodal.Model = "gemini-embedding-2"
	}
	cfg.Vector.Multimodal.Dimension = 1024
	cfg.Vector.Multimodal.APIKeyEnv = ""
	cfg.Vector.Multimodal.CapabilitiesFile = ""
	client := server.Client()
	if api == "vertex" {
		transport := client.Transport
		if transport == nil {
			transport = http.DefaultTransport
		}
		client.Transport = visualCredentialRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			request := r.Clone(r.Context())
			request.Header.Set("Authorization", "Bearer synthetic-google-token")
			return transport.RoundTrip(request)
		})
	}
	countedOpener := &countingVisualOpener{kitTestOpener: opener}
	runtime, err := newVisualRuntime(t.Context(), cfg.Vector, st, backend, countedOpener, visualRuntimeCredential{HTTPClient: client, APIKey: key})
	require.NoError(t, err)
	require.NoError(t, st.ConsentVisualGeneration(t.Context(), runtime.Generation.ID, runtime.PolicyFingerprint))
	assert.True(t, setupVisualConsent(t.Context(), cfg, st))
	for range 10 {
		_, err = executeVisualPass(t.Context(), runtime)
		require.NoError(t, err)
		if _, err = st.ActiveVisualGeneration(t.Context()); err == nil {
			break
		}
	}
	_, err = st.ActiveVisualGeneration(t.Context())
	require.NoError(t, err)
	search, err := visual.NewSearchService(st, runtime.Provider, runtime.Backend, true)
	require.NoError(t, err)
	readsBeforeSearch := countedOpener.reads
	response, err := search.Search(t.Context(), visual.SearchQuery{Text: "beach at sunset", Limit: 5})
	require.NoError(t, err)
	require.Len(t, response.Results, 2)
	assert.Equal(t, messages[0], response.Results[0].MessageID)
	assert.Equal(t, 2, documentRequests)
	assert.Equal(t, readsBeforeSearch, countedOpener.reads)
	imageQuery, err := search.DecodeQueryImage(images[0])
	require.NoError(t, err)
	imageResponse, err := search.Search(t.Context(), visual.SearchQuery{Image: imageQuery, Limit: 1})
	require.NoError(t, err)
	require.Len(t, imageResponse.Results, 1)
	assert.Equal(t, messages[0], imageResponse.Results[0].MessageID)
	assert.Equal(t, readsBeforeSearch, countedOpener.reads)
	lane := visualSearchLane(cfg, setupEnvironment{consent: &setupConsentState{Visual: true}, googleCredentials: func() error { return nil }})
	assert.Equal(t, laneStateOn, lane.State)
	assert.Equal(t, planActionKeep, planVisualSearch(cfg, setupDetection{}).Action)
}

func TestMismatchedVisualCredentialKeepsTextSearchAvailable(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "msgvault.db")
	cfg := config.NewDefaultConfig()
	cfg.HomeDir, cfg.Data.DataDir = dir, dir
	cfg.Vector.Enabled = true
	cfg.Vector.DBPath = filepath.Join(dir, "vectors.db")
	cfg.Vector.Embeddings.Endpoint = "https://example.com/v1"
	cfg.Vector.Embeddings.Model = "text-model"
	cfg.Vector.Embeddings.Dimension = 4
	cfg.Vector.Multimodal.Enabled = true
	cfg.Vector.Multimodal.Provider = "kit"
	cfg.Vector.Multimodal.Endpoint = "https://embed.example.com/v1"
	cfg.Vector.Multimodal.Model = "model"
	cfg.Vector.Multimodal.Dimension = 1024
	cfg.Vector.Multimodal.APIKeyEnv = ""
	empty, err := providercredentials.Read(cfg.TokensDir())
	require.NoError(t, err)
	_, err = providercredentials.Put(cfg.TokensDir(), empty.ETag, providercredentials.VectorMultimodalID, "https://api.voyageai.com/v1", "old-key")
	require.NoError(t, err)
	ctx := withTestConfig(t, cfg)
	st := testutil.NewSQLiteTestStoreAt(t, mainPath)
	features, err := setupVectorFeatures(ctx, st, mainPath, false, unavailableVisualCredentialOpener{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = features.Close() })
	assert.NotNil(t, features.HybridEngine)
	assert.Nil(t, features.Visual)
}

func TestKitVisualSetupReportsCredentials(t *testing.T) {
	cfg := config.NewDefaultConfig()
	cfg.Vector.Multimodal.Enabled = true
	cfg.Vector.Multimodal.Provider = "kit"
	cfg.Vector.Multimodal.Endpoint = "https://embed.example.com/v1"
	cfg.Vector.Multimodal.APIKeyEnv = "EMBED_KEY"
	env := setupEnvironment{consent: &setupConsentState{Visual: true}, lookupEnv: func(string) (string, bool) { return "", false }}
	assert.Equal(t, laneStatePending, visualSearchLane(cfg, env).State)
	assert.Equal(t, planActionPending, planVisualSearch(cfg, setupDetection{visualUnavailable: "credential unavailable"}).Action)
	env.lookupEnv = func(string) (string, bool) { return "key", true }
	assert.Equal(t, laneStateOn, visualSearchLane(cfg, env).State)
	cfg.Vector.Multimodal.Provider = "vertex"
	env.googleCredentials = func() error { return errors.New("missing") }
	assert.Equal(t, laneStatePending, visualSearchLane(cfg, env).State)
	env.googleCredentials = func() error { return nil }
	assert.Equal(t, laneStateOn, visualSearchLane(cfg, env).State)
}
