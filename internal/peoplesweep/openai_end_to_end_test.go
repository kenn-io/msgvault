package peoplesweep_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

const (
	sweepSourceSecret   = "source-secret: Alice explicitly likes Ramen"
	sweepProviderSecret = "provider-secret-response-body"
	sweepResponseModel  = "model-build-2026-08-22"
	sweepExcludedState  = "excluded-sensitive-state"
)

type openAISweepResponse struct {
	Status  int
	Headers map[string]string
	Body    string
	Wait    bool
}

type openAISweepServer struct {
	t           *testing.T
	mu          sync.Mutex
	store       *store.Store
	calls       int
	responses   []openAISweepResponse
	responseFor func([]byte, int) (openAISweepResponse, error)
	wireHashes  []string
}

func (s *openAISweepServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	call := s.calls
	s.calls++
	s.mu.Unlock()

	raw, err := io.ReadAll(r.Body)
	assert.NoError(s.t, err)
	assert.Equal(s.t, http.MethodPost, r.Method)
	assert.Equal(s.t, "/compatible/custom/chat/completions", r.URL.Path)
	assert.Equal(s.t, "application/json", r.Header.Get("Content-Type"))
	assert.Equal(s.t, "Bearer synthetic-api-key", r.Header.Get("Authorization"))

	digest := sha256.Sum256(raw)
	wantHash := hex.EncodeToString(digest[:])
	s.mu.Lock()
	s.wireHashes = append(s.wireHashes, wantHash)
	s.mu.Unlock()
	if s.store != nil {
		var reservedHash string
		err = s.store.DB().QueryRowContext(r.Context(), s.store.Rebind(`
			SELECT input_hash FROM person_sweep_batches
			WHERE status = 'running' ORDER BY batch_ordinal DESC LIMIT 1`)).Scan(&reservedHash)
		assert.NoError(s.t, err)
		assert.Equal(s.t, wantHash, reservedHash,
			"the exact sent HTTP body must be covered by the durable reservation")
	}

	var request capturedChatRequest
	assert.NoError(s.t, json.Unmarshal(raw, &request))
	assert.Equal(s.t, "model-request-2026", request.Model)
	assert.Equal(s.t, peoplesweep.ExtractionSchemaName, request.ResponseFormat.JSONSchema.Name)
	assert.Equal(s.t, "json_schema", request.ResponseFormat.Type)
	assert.True(s.t, request.ResponseFormat.JSONSchema.Strict)
	assert.JSONEq(s.t, string(peoplesweep.ExtractionJSONSchema()),
		string(request.ResponseFormat.JSONSchema.Schema))
	var response openAISweepResponse
	if s.responseFor != nil {
		response, err = s.responseFor(raw, call)
		assert.NoError(s.t, err)
		if err != nil {
			response = openAISweepResponse{Status: http.StatusInternalServerError}
		}
	} else {
		s.mu.Lock()
		response = s.responses[min(call, len(s.responses)-1)]
		s.mu.Unlock()
	}

	if response.Wait {
		<-r.Context().Done()
		return
	}
	for key, value := range response.Headers {
		w.Header().Set(key, value)
	}
	if response.Status != 0 {
		w.WriteHeader(response.Status)
	}
	_, err = io.WriteString(w, response.Body)
	assert.NoError(s.t, err)
}

type openAISweepFixture struct {
	store    *store.Store
	config   peoplesweep.Config
	personID int64
	now      time.Time
	worker   peoplesweep.Worker
}

func newOpenAISweepFixture(
	t *testing.T,
	server *httptest.Server,
	provider *openAISweepServer,
	messageCount int,
	configure func(*peoplesweep.Config),
) openAISweepFixture {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "openai-compatible-sweep")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "openai-compatible-chat", "direct_chat", "Synthetic chat")
	require.NoError(t, err)
	participantID, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(participantID)
	require.NoError(t, err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)

	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderConfig{
		Kind: peoplesweep.ProviderOpenAICompatible, Endpoint: server.URL + "/compatible/custom",
		Model: "model-request-2026", APIKeyEnv: "SYNTHETIC_OPENAI_KEY",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2026-01-01", RequestTimeout: 2 * time.Second, AllowSensitive: true,
	}}
	config.ApplyDefaults()
	if configure != nil {
		configure(&config)
	}
	profile, err := config.Profile()
	require.NoError(t, err)
	_, err = st.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = st.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "synthetic-test")
	require.NoError(t, err)
	// Keep the real catalog path while limiting this provider-boundary fixture
	// to the single supported target it exercises.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE attribute_definitions SET api_mutable = FALSE
		WHERE object_type = 'person' AND slug <> ?`), store.AttributeSlugAskMeAbout)
	require.NoError(t, err)
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
	require.NoError(t, err)
	_, err = st.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{{
		PersonID: person.ID, SourceLane: peoplesweep.SourceConversationText,
		ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
	}})
	require.NoError(t, err)
	for index := range messageCount {
		_, err = st.UpsertMessage(&store.Message{
			SourceID:        source.ID,
			SourceMessageID: "openai-sweep-message-" + string(rune('a'+index)),
			ConversationID:  conversationID,
			MessageType:     "chat",
			SenderID:        sql.NullInt64{Int64: participantID, Valid: true},
			SentAt: sql.NullTime{
				Time: time.Date(2026, 8, 22, 12+index, 0, 0, 0, time.UTC), Valid: true,
			},
			Subject: sql.NullString{String: sweepSourceSecret, Valid: true},
		})
		require.NoError(t, err)
	}

	runner, err := peoplesweep.NewRunner(config, st,
		peoplesweep.NewOpenAICompatibleTransport(server.Client()),
		func(name string) (string, bool) {
			assert.Equal(t, "SYNTHETIC_OPENAI_KEY", name)
			return "synthetic-api-key", true
		})
	require.NoError(t, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	ids := 0
	fixture := openAISweepFixture{store: st, config: config, personID: person.ID, now: now}
	fixture.worker = peoplesweep.Worker{
		Config: config, Store: st, Source: st,
		Context: peoplesweep.NewContextRetriever(st), Sink: st, Runner: runner, Catalog: st,
		Clock: func() time.Time { return now },
		NewID: func() string {
			ids++
			if ids == 1 {
				return "run-openai-compatible"
			}
			return "attempt-openai-compatible-" + string(rune('0'+ids))
		},
		WorkerID: "worker-openai-compatible",
	}
	provider.store = st
	return fixture
}

type capturedSweepPacket struct {
	Catalog struct {
		Targets []struct {
			Key  string `json:"key"`
			Slug string `json:"slug"`
		} `json:"targets"`
	} `json:"catalog"`
	CurrentProjection []peoplesweep.ProjectedValue `json:"current_projection"`
	UnresolvedClaims  []personfacts.Claim          `json:"unresolved_claims"`
	Seeds             []struct {
		ID string `json:"id"`
	} `json:"seeds"`
}

func extractionResponseFromWire(
	raw []byte,
	model string,
	usage peoplesweep.TokenUsage,
	inspect func(capturedSweepPacket) error,
) (string, error) {
	var request capturedChatRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return "", err
	}
	if len(request.Messages) != 2 {
		return "", errors.New("expected two chat messages")
	}
	const marker = "Evidence packet JSON:\n"
	markerIndex := strings.Index(request.Messages[1].Content, marker)
	if markerIndex < 0 {
		return "", errors.New("missing evidence packet marker")
	}
	packetJSON := request.Messages[1].Content[markerIndex+len(marker):]
	var packet capturedSweepPacket
	//nolint:musttag // This fixture intentionally decodes a partial nested wire projection.
	if err := json.Unmarshal([]byte(packetJSON), &packet); err != nil {
		return "", err
	}
	if inspect != nil {
		if err := inspect(packet); err != nil {
			return "", err
		}
	}
	if len(packet.Seeds) == 0 {
		return "", errors.New("expected at least one evidence seed")
	}
	targetKey := ""
	for _, target := range packet.Catalog.Targets {
		if target.Slug == store.AttributeSlugAskMeAbout {
			targetKey = target.Key
			break
		}
	}
	if targetKey == "" {
		return "", fmt.Errorf("missing %s target", store.AttributeSlugAskMeAbout)
	}
	content, err := json.Marshal(map[string]any{"claims": []any{map[string]any{
		"target_key": targetKey, "relation": "support", "value": "Ramen",
		"evidence_ids": []string{packet.Seeds[0].ID}, "valid_from": nil,
		"valid_until": nil, "confidence_basis_points": 900,
	}}})
	if err != nil {
		return "", err
	}
	envelope, err := json.Marshal(map[string]any{
		"model":   model,
		"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}},
		"usage":   map[string]any{"prompt_tokens": usage.InputTokens, "completion_tokens": usage.OutputTokens},
	})
	if err != nil {
		return "", err
	}
	return string(envelope), nil
}

func staticOpenAIEnvelope(t *testing.T, model, content string, input, output int64) string {
	t.Helper()
	envelope, err := json.Marshal(map[string]any{
		"model":   model,
		"choices": []any{map[string]any{"message": map[string]any{"content": content}}},
		"usage":   map[string]any{"prompt_tokens": input, "completion_tokens": output},
	})
	require.NoError(t, err)
	return string(envelope)
}

func runOpenAISweep(t *testing.T, fixture openAISweepFixture) (peoplesweep.RunResult, error) {
	t.Helper()
	return fixture.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: fixture.personID, Limit: 1,
	})
}

func openAISweepAttempt(t *testing.T, fixture openAISweepFixture) peoplesweep.AttemptSummary {
	t.Helper()
	attempts, err := fixture.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		PersonID: fixture.personID, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, attempts, 1)
	return attempts[0]
}

func assertOpenAISweepCursorUnchanged(t *testing.T, fixture openAISweepFixture) {
	t.Helper()
	var optimistic int64
	err := fixture.store.DB().QueryRowContext(t.Context(), fixture.store.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors WHERE person_id = ?`),
		fixture.personID).Scan(&optimistic)
	require.NoError(t, err)
	assert.Zero(t, optimistic)
	var generations int
	require.NoError(t, fixture.store.DB().QueryRowContext(t.Context(), fixture.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_generations WHERE person_id = ?`), fixture.personID).
		Scan(&generations))
	assert.Zero(t, generations)
}

func openAISweepTargetBySlug(
	t *testing.T, fixture openAISweepFixture, slug string,
) personfacts.TargetDescriptor {
	t.Helper()
	catalog, err := fixture.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	for _, target := range catalog.Targets {
		if target.Slug == slug {
			return target
		}
	}
	require.FailNow(t, "person fact target not found", slug)
	return personfacts.TargetDescriptor{}
}

func seedOpenAISweepFactState(
	t *testing.T, fixture openAISweepFixture,
) (peoplesweep.ProjectedValue, personfacts.Claim) {
	t.Helper()
	_, err := fixture.store.DB().ExecContext(t.Context(), fixture.store.Rebind(`
		UPDATE attribute_definitions SET api_mutable = TRUE
		WHERE object_type = 'person' AND slug = ?`), store.AttributeSlugReligion)
	require.NoError(t, err)
	confidence := 0.75
	_, err = fixture.store.SetPersonAttributeValueContext(t.Context(), store.PersonAttributeValueInput{
		PersonID: fixture.personID, DefinitionSlug: store.AttributeSlugReligion,
		Value:      store.AttributeValue{Type: store.AttributeValueText, Text: new(sweepExcludedState)},
		Source:     store.ProvenanceExtraction,
		Confidence: &confidence,
	})
	require.NoError(t, err)
	_, err = fixture.store.DB().ExecContext(t.Context(), fixture.store.Rebind(`
		UPDATE attribute_definitions SET api_mutable = FALSE
		WHERE object_type = 'person' AND slug = ?`), store.AttributeSlugReligion)
	require.NoError(t, err)
	target := openAISweepTargetBySlug(t, fixture, store.AttributeSlugAskMeAbout)
	catalog, err := fixture.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	priorResolvedAt := fixture.now.Add(-2 * time.Hour)
	prior := openAISweepPriorGeneration(fixture, target, catalog.Fingerprint,
		"projected", "Dumplings", priorResolvedAt, nil)
	projected, err := fixture.store.ApplyPersonFactGenerationContext(t.Context(), prior, nil)
	require.NoError(t, err)
	require.Len(t, projected.Decisions, 1)
	require.Equal(t, personfacts.DecisionApplied, projected.Decisions[0].Action)

	validFrom := fixture.now.Add(24 * time.Hour)
	unresolved := openAISweepPriorGeneration(fixture, target, catalog.Fingerprint,
		"unresolved", "Pho", fixture.now.Add(-time.Hour), &validFrom)
	retained, err := fixture.store.ApplyPersonFactGenerationContext(t.Context(), unresolved, nil)
	require.NoError(t, err)
	var retainedKey string
	for _, decision := range retained.Decisions {
		if decision.Action == personfacts.DecisionRetained && decision.Reason == personfacts.ReasonOutsideValidity {
			retainedKey = decision.ClaimKey
		}
	}
	require.NotEmpty(t, retainedKey)
	claims, err := fixture.store.ListPersonFactClaimsContext(t.Context(), fixture.personID,
		personfacts.ClaimFilter{Target: &personfacts.TargetRef{
			Kind: target.Kind, Key: target.Key, Revision: target.Revision,
		}})
	require.NoError(t, err)
	var retainedClaim personfacts.Claim
	for _, claim := range claims {
		if claim.ClaimKey == retainedKey {
			retainedClaim = claim
		}
	}
	require.NotZero(t, retainedClaim.ID)
	require.NotZero(t, retainedClaim.Generation.ID)
	require.NotEmpty(t, retainedClaim.EvidenceIDs)

	canonical := []byte(`"Dumplings"`)
	digest := sha256.Sum256(canonical)
	effectiveAt := priorResolvedAt
	return peoplesweep.ProjectedValue{
		TargetKey: target.Key, Value: canonical,
		ValueFingerprint: "sha256:" + hex.EncodeToString(digest[:]), EffectiveAt: &effectiveAt,
	}, retainedClaim
}

func openAISweepPriorGeneration(
	fixture openAISweepFixture,
	target personfacts.TargetDescriptor,
	catalogFingerprint, suffix, value string,
	resolvedAt time.Time,
	validFrom *time.Time,
) personfacts.GenerationInput {
	subject := fixture.personID
	return personfacts.GenerationInput{
		PersonID: fixture.personID,
		SourceCursors: []personfacts.SourceCursor{{
			Lane: "synthetic-prior", Start: suffix, End: suffix + "-end",
		}},
		ProgramID: "synthetic-prior-state", ProgramVersion: "v1",
		ProgramFingerprint: strings.Repeat("a", 64), CatalogFingerprint: catalogFingerprint,
		Provider: "synthetic", ProviderVersion: "v1", Model: "synthetic", ModelVersion: "v1",
		ResolvedAt: resolvedAt,
		Policy:     personfacts.PolicyContext{ProviderPolicyFingerprint: "synthetic-prior-policy"},
		Claims: []personfacts.ProposedClaim{{
			Target: target, Relation: personfacts.RelationSupport,
			SubmittedValue: json.RawMessage(strconv.Quote(value)), ValidFrom: validFrom,
			Origin:     personfacts.OriginExtraction,
			Confidence: personfacts.ConfidenceInputs{ReportedScore: 900},
			Evidence: []personfacts.EvidenceInput{{
				PersonID: fixture.personID, SubjectPersonID: &subject,
				SourceClass: personfacts.EvidencePublic, Directness: personfacts.DirectSelf,
				Authority:  personfacts.AuthorityAuthoritative,
				SourceURL:  "https://example.test/prior/" + suffix,
				SubjectRef: "synthetic-person", Excerpt: "Synthetic prior state " + suffix,
				EventTime: resolvedAt.Add(-time.Hour), RecordedTime: resolvedAt,
				IdentityScore: 990,
			}},
		}},
	}
}

func inspectOpenAISweepFactState(
	packet capturedSweepPacket, wantProjection peoplesweep.ProjectedValue, wantClaim personfacts.Claim,
) error {
	if len(packet.CurrentProjection) != 1 || packet.CurrentProjection[0].TargetKey != wantProjection.TargetKey ||
		!bytes.Equal(packet.CurrentProjection[0].Value, wantProjection.Value) ||
		packet.CurrentProjection[0].ValueFingerprint != wantProjection.ValueFingerprint ||
		packet.CurrentProjection[0].EffectiveAt == nil || wantProjection.EffectiveAt == nil ||
		!packet.CurrentProjection[0].EffectiveAt.Equal(*wantProjection.EffectiveAt) {
		return errors.New("outbound packet omitted or changed the current projection")
	}
	if len(packet.UnresolvedClaims) != 1 {
		return fmt.Errorf("outbound packet has %d unresolved claims, want 1", len(packet.UnresolvedClaims))
	}
	got := packet.UnresolvedClaims[0]
	if !reflect.DeepEqual(got, wantClaim) || got.ClaimKey != wantClaim.ClaimKey || got.PersonID != wantClaim.PersonID ||
		got.GenerationID != wantClaim.GenerationID || got.Generation.ID != wantClaim.Generation.ID ||
		got.Generation.PersonID != wantClaim.Generation.PersonID || got.Target != wantClaim.Target ||
		!bytes.Equal(got.SubmittedValue, wantClaim.SubmittedValue) ||
		!slices.Equal(got.EvidenceIDs, wantClaim.EvidenceIDs) {
		return errors.New("outbound packet omitted or incompletely hydrated the unresolved claim")
	}
	return nil
}

func TestOpenAICompatibleSweepAppliesSupportedClaimEndToEnd(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	var wantProjection peoplesweep.ProjectedValue
	var wantUnresolved personfacts.Claim
	provider := &openAISweepServer{t: t, responseFor: func(raw []byte, _ int) (openAISweepResponse, error) {
		if bytes.Contains(raw, []byte(sweepExcludedState)) {
			return openAISweepResponse{}, errors.New("outbound packet leaked state outside the exact catalog")
		}
		body, err := extractionResponseFromWire(raw, sweepResponseModel,
			peoplesweep.TokenUsage{InputTokens: 37, OutputTokens: 11},
			func(packet capturedSweepPacket) error {
				return inspectOpenAISweepFactState(packet, wantProjection, wantUnresolved)
			})
		return openAISweepResponse{Headers: map[string]string{"x-request-id": "request-e2e-1"}, Body: body}, err
	}}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	fixture := newOpenAISweepFixture(t, server, provider, 1, nil)
	wantProjection, wantUnresolved = seedOpenAISweepFactState(t, fixture)

	result, err := runOpenAISweep(t, fixture)
	require.NoError(err)
	assert.Equal(1, result.PeopleSucceeded)
	assert.Equal(1, result.ProjectedWrites)
	values, err := fixture.store.ListPersonAttributeValuesContext(t.Context(), fixture.personID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugAskMeAbout})
	require.NoError(err)
	require.Len(values, 2)
	projectedValues := make([]string, 0, len(values))
	for _, value := range values {
		require.NotNil(value.Value.Text)
		projectedValues = append(projectedValues, *value.Value.Text)
	}
	assert.ElementsMatch([]string{"Dumplings", "Ramen"}, projectedValues)
	var providerVersion, modelVersion string
	require.NoError(fixture.store.DB().QueryRowContext(t.Context(), fixture.store.Rebind(`
		SELECT provider_version, model_version FROM person_fact_generations
		WHERE person_id = ? ORDER BY id DESC LIMIT 1`), fixture.personID).
		Scan(&providerVersion, &modelVersion))
	assert.Equal(peoplesweep.OpenAICompatibleProviderVersion, providerVersion)
	assert.Equal(sweepResponseModel, modelVersion)
	var optimistic int64
	require.NoError(fixture.store.DB().QueryRowContext(t.Context(), fixture.store.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors WHERE person_id = ?`),
		fixture.personID).Scan(&optimistic))
	assert.Positive(optimistic)
}

func TestOpenAICompatibleRejectsMissingResponseModel(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	provider := &openAISweepServer{t: t, responses: []openAISweepResponse{{
		Body: staticOpenAIEnvelope(t, "", `{"claims":[]}`, 50_001, 8_001),
	}}}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	fixture := newOpenAISweepFixture(t, server, provider, 1, nil)

	_, err := runOpenAISweep(t, fixture)
	require.Error(err)
	attempt := openAISweepAttempt(t, fixture)
	assert.Equal(peoplesweep.FailureInvalidOutput, attempt.FailureClass)
	assert.Equal(1, attempt.Usage.Requests)
	assert.Equal(int64(50_001), attempt.Usage.InputTokens)
	assert.Equal(int64(8_001), attempt.Usage.OutputTokens)
	assert.Nil(attempt.GenerationID)
	assertOpenAISweepCursorUnchanged(t, fixture)
}

func TestOpenAICompatibleSweepRejectsMixedModelVersions(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	provider := &openAISweepServer{t: t}
	server := httptest.NewTLSServer(provider)
	defer server.Close()
	fixture := newOpenAISweepFixture(t, server, provider, 2, func(config *peoplesweep.Config) {
		config.EvidenceMaxItems = 1
	})
	provider.responses = []openAISweepResponse{
		{Body: staticOpenAIEnvelope(t, "model-build-a", `{"claims":[]}`, 30_000, 5_000)},
		{Body: staticOpenAIEnvelope(t, "model-build-b", `{"claims":[]}`, 30_000, 5_000)},
	}
	_, err := runOpenAISweep(t, fixture)
	provider.mu.Lock()
	calls := provider.calls
	provider.mu.Unlock()
	assert.Equal(2, calls)
	require.Error(err)
	attempt := openAISweepAttempt(t, fixture)
	assert.Equal(peoplesweep.FailureInvalidOutput, attempt.FailureClass)
	assert.Equal(2, attempt.Usage.Requests)
	assert.Equal(int64(60_000), attempt.Usage.InputTokens)
	assert.Equal(int64(10_000), attempt.Usage.OutputTokens)
	assert.Nil(attempt.GenerationID)
	assertOpenAISweepCursorUnchanged(t, fixture)
}

func TestOpenAICompatibleRetryAfterBoundsRetry(t *testing.T) {
	tests := []struct {
		name    string
		header  func() string
		minimum time.Duration
		maximum time.Duration
	}{
		{name: "delta seconds", header: func() string { return "5" }, minimum: 5 * time.Second, maximum: 6 * time.Second},
		{name: "http date", header: func() string { return time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat) }, minimum: 3 * time.Second, maximum: 6 * time.Second},
		{name: "malformed", header: func() string { return sweepProviderSecret }, minimum: time.Second, maximum: 2 * time.Second},
		{name: "negative", header: func() string { return "-7" }, minimum: time.Second, maximum: 2 * time.Second},
		{name: "signed positive", header: func() string { return "+5" }, minimum: time.Second, maximum: 2 * time.Second},
		{name: "above retry max", header: func() string { return "500" }, minimum: 10 * time.Second, maximum: 10*time.Second + time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			provider := &openAISweepServer{t: t, responseFor: func([]byte, int) (openAISweepResponse, error) {
				return openAISweepResponse{Status: http.StatusTooManyRequests,
					Headers: map[string]string{"Retry-After": test.header()}, Body: sweepProviderSecret}, nil
			}}
			server := httptest.NewTLSServer(provider)
			defer server.Close()
			fixture := newOpenAISweepFixture(t, server, provider, 1, func(config *peoplesweep.Config) {
				config.RetryBase = time.Second
				config.RetryMax = 10 * time.Second
			})

			_, err := runOpenAISweep(t, fixture)
			require.Error(err)
			assert.NotContains(err.Error(), sweepProviderSecret)
			var available time.Time
			require.NoError(fixture.store.DB().QueryRowContext(t.Context(), fixture.store.Rebind(`
				SELECT available_at FROM person_sweep_work WHERE person_id = ?`),
				fixture.personID).Scan(&available))
			delay := available.UTC().Sub(fixture.now)
			assert.GreaterOrEqual(delay, test.minimum)
			assert.LessOrEqual(delay, test.maximum)
			attempt := openAISweepAttempt(t, fixture)
			assert.Equal(peoplesweep.FailureRateLimited, attempt.FailureClass)
		})
	}
}

func TestOpenAICompatibleSweepFailureNeverAdvancesCursor(t *testing.T) {
	tests := []struct {
		name      string
		response  openAISweepResponse
		configure func(*peoplesweep.Config)
		class     peoplesweep.FailureClass
	}{
		{name: "invalid JSON", response: openAISweepResponse{Body: `{` + sweepProviderSecret}, class: peoplesweep.FailureInvalidOutput},
		{name: "schema mismatch", response: openAISweepResponse{Body: staticOpenAIEnvelope(t, sweepResponseModel, `{"claims":[{"unexpected":true}]}`, 19, 4)}, class: peoplesweep.FailureInvalidOutput},
		{name: "429", response: openAISweepResponse{Status: http.StatusTooManyRequests, Body: sweepProviderSecret}, class: peoplesweep.FailureRateLimited},
		{name: "500", response: openAISweepResponse{Status: http.StatusInternalServerError, Body: sweepProviderSecret}, class: peoplesweep.FailureProviderHTTP},
		{name: "timeout", response: openAISweepResponse{Wait: true}, configure: func(config *peoplesweep.Config) { config.Provider.RequestTimeout = 30 * time.Millisecond }, class: peoplesweep.FailureTimeout},
		{name: "redirect", response: openAISweepResponse{Status: http.StatusTemporaryRedirect, Headers: map[string]string{"Location": "/forbidden"}, Body: sweepProviderSecret}, class: peoplesweep.FailureProviderHTTP},
		{name: "oversized response", response: openAISweepResponse{Body: strings.Repeat(sweepProviderSecret, (1<<20)/len(sweepProviderSecret)+2)}, class: peoplesweep.FailureInvalidOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			var logs bytes.Buffer
			priorLogger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(priorLogger) })
			provider := &openAISweepServer{t: t, responses: []openAISweepResponse{test.response}}
			server := httptest.NewTLSServer(provider)
			defer server.Close()
			fixture := newOpenAISweepFixture(t, server, provider, 1, test.configure)

			_, err := runOpenAISweep(t, fixture)
			require.Error(err)
			attempt := openAISweepAttempt(t, fixture)
			assert.Equal(test.class, attempt.FailureClass)
			assertOpenAISweepCursorUnchanged(t, fixture)
			for _, unsafe := range []string{sweepSourceSecret, sweepProviderSecret} {
				assert.NotContains(err.Error(), unsafe)
				assert.NotContains(logs.String(), unsafe)
			}
			if test.name == "timeout" {
				assert.True(errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "deadline"))
			}
		})
	}
}
