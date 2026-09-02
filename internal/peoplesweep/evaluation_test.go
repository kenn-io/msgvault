package peoplesweep_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

var evaluationFixtureNames = []string{
	"explicit-facts.json",
	"subject-attribution.json",
	"temporal-changes.json",
	"contradictions.json",
	"prompt-injection.json",
	"sensitive-targets.json",
}

type evaluationFixture struct {
	Name                  string            `json:"name"`
	AllowSensitive        bool              `json:"allow_sensitive"`
	SensitiveTargetSlug   string            `json:"sensitive_target_slug"`
	SensitiveAllowedValue json.RawMessage   `json:"sensitive_allowed_value"`
	DescriptionOverrides  map[string]string `json:"description_overrides"`
	MaxRequests           int               `json:"max_requests"`
	Steps                 []evaluationStep  `json:"steps"`
}

type evaluationStep struct {
	Name                string                       `json:"name"`
	Excerpt             string                       `json:"excerpt"`
	Subject             string                       `json:"subject"`
	EventTime           string                       `json:"event_time"`
	ResolvedAt          string                       `json:"resolved_at"`
	ExpectedSpan        [2]int                       `json:"expected_span"`
	ExpectedPacketError string                       `json:"expected_packet_error"`
	ProviderClaims      []evaluationClaim            `json:"provider_claims"`
	ExpectedClaims      []evaluationClaim            `json:"expected_claims"`
	ExpectedDecisions   []evaluationDecision         `json:"expected_decisions"`
	ExpectedCurrent     map[string][]json.RawMessage `json:"expected_current"`
	RetainedValues      map[string][]json.RawMessage `json:"retained_values"`
	StaleResolution     bool                         `json:"stale_resolution"`
}

type evaluationDecision struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

type evaluationClaim struct {
	TargetSlug            string          `json:"target_slug"`
	Relation              string          `json:"relation"`
	Value                 json.RawMessage `json:"value"`
	EvidenceSteps         []int           `json:"evidence_steps"`
	ValidFrom             *string         `json:"valid_from"`
	ValidUntil            *string         `json:"valid_until"`
	ConfidenceBasisPoints int             `json:"confidence_basis_points"`
}

type evaluationMetrics struct {
	ExpectedClaims           int
	MatchedClaims            int
	ExtractedClaims          int
	EvidenceCitations        int
	AlignedCitations         int
	SubjectAttributionErrors int
	FalseDestructiveChanges  int
	StaleResolutionChecks    int
	StaleResolutionsCorrect  int
	Requests                 int
	PreparedWireRequests     int
	InputTokens              int64
	OutputTokens             int64
	EstimatedCostMicroUSD    int64
}

type evaluationStore struct {
	Store             *store.Store
	PersonID          int64
	ScopedParticipant int64
	OtherParticipant  int64
	SourceID          int64
	ConversationID    int64
}

func TestPersonSweepFrozenEvaluation(t *testing.T) {
	runPersonSweepFrozenEvaluation(t)
}

func runPersonSweepFrozenEvaluation(t *testing.T) {
	t.Helper()
	metrics := evaluationMetrics{}
	for _, name := range evaluationFixtureNames {
		fixture := loadEvaluationFixture(t, name)
		runEvaluationFixture(t, fixture, &metrics)
		if name == "sensitive-targets.json" {
			require.NotEmpty(t, fixture.SensitiveTargetSlug)
			require.NotEmpty(t, fixture.SensitiveAllowedValue)
			runEvaluationSensitivePolicy(t, fixture, &metrics)
		}
	}

	require.Positive(t, metrics.ExtractedClaims)
	assert.Equal(t, metrics.ExtractedClaims, metrics.MatchedClaims,
		"supported-fact precision must be 1.0")
	assert.Equal(t, metrics.ExpectedClaims, metrics.MatchedClaims,
		"every frozen expected claim must be extracted")
	require.Positive(t, metrics.EvidenceCitations)
	assert.Equal(t, metrics.EvidenceCitations, metrics.AlignedCitations,
		"evidence alignment must be 1.0")
	assert.Zero(t, metrics.SubjectAttributionErrors)
	assert.Zero(t, metrics.FalseDestructiveChanges)
	assert.Equal(t, metrics.StaleResolutionChecks, metrics.StaleResolutionsCorrect)
	assert.LessOrEqual(t, metrics.Requests, 24)
	assert.Equal(t, metrics.Requests, metrics.PreparedWireRequests,
		"every measured request must come from a production prepared wire")
	assert.Positive(t, metrics.InputTokens)
	assert.Positive(t, metrics.OutputTokens)
	assert.Positive(t, metrics.EstimatedCostMicroUSD)
}

func runEvaluationSensitivePolicy(
	t *testing.T, fixture evaluationFixture, metrics *evaluationMetrics,
) {
	t.Helper()
	f := newEvaluationStore(t)
	excludedCatalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	_, excluded := evaluationTargetsBySlug(excludedCatalog)[fixture.SensitiveTargetSlug]
	assert.False(t, excluded, "sensitive target must be absent from the disallowed catalog")

	allowedCatalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), true)
	require.NoError(t, err)
	allowedTargets := evaluationTargetsBySlug(allowedCatalog)
	target, ok := allowedTargets[fixture.SensitiveTargetSlug]
	require.True(t, ok)
	require.True(t, target.Sensitive)
	step := fixture.Steps[0]
	item := insertEvaluationEvidence(t, f, 100, step)
	packet := peoplesweep.EvidencePacket{PersonID: f.PersonID,
		ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion,
		Catalog: allowedCatalog, Seeds: []peoplesweep.EvidenceItem{item}}
	batches, err := peoplesweep.PartitionEvidencePacket(packet, 256*1024, 200)
	require.NoError(t, err)
	require.Len(t, batches, 1)
	providerClaims := []evaluationClaim{{TargetSlug: fixture.SensitiveTargetSlug,
		Relation: string(personfacts.RelationSupport), Value: fixture.SensitiveAllowedValue,
		EvidenceSteps: []int{0}, ConfidenceBasisPoints: 990}}
	providerJSON := evaluationProviderJSON(t, providerClaims, allowedTargets,
		evaluationEvidenceIDs(t, batches[0]), []peoplesweep.EvidenceItem{item})
	_, err = peoplesweep.ParseExtraction(providerJSON, batches[0],
		peoplesweep.ProviderProfile{AllowSensitive: false})
	require.ErrorContains(t, err, "policy-disabled sensitive target")

	var received = make(chan []byte, 1)
	var responseBody []byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, readErr := io.ReadAll(request.Body)
		if readErr != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		received <- body
		w.Header().Set("X-Request-ID", "frozen-sensitive-request")
		_, _ = w.Write(responseBody)
	}))
	defer server.Close()
	config := evaluationProviderConfig(true, server.URL+"/v1")
	profile, err := config.Profile()
	require.NoError(t, err)
	_, err = f.Store.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	require.NoError(t, f.Store.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: profile.Fingerprint,
		CheckedAt:          time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		DriverVersion:      profile.DriverVersion,
		OutputMode:         profile.OutputMode,
		ModelVersion:       "frozen-sensitive-model-v1",
	}))
	runner, err := peoplesweep.NewRunner(config, f.Store,
		peoplesweep.NewTestDriverRegistry(peoplesweep.ProtocolOpenAIChat,
			peoplesweep.NewOpenAIChatDriver(server.Client())),
		peoplesweep.NewCredentialResolver(nil, func(name string) (string, bool) {
			return "frozen-sensitive-key", name == "FROZEN_EVALUATION_API_KEY"
		}))
	require.NoError(t, err)
	prepared, err := runner.PrepareStructured(t.Context(), batches[0].Request)
	require.NoError(t, err)
	_, err = runner.RunPreparedStructured(t.Context(), prepared)
	require.ErrorContains(t, err, "active exact consent")
	require.ErrorIs(t, err, peoplesweep.ErrPersonSweepConsentRevoked)
	select {
	case <-received:
		require.FailNow(t, "provider called before exact consent")
	default:
	}
	_, _, err = f.Store.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "frozen-evaluation")
	require.NoError(t, err)
	estimate, err := peoplesweep.EstimateWireTokenReservation(
		prepared.WireRequest(), batches[0].Request.MaxOutputTokens)
	require.NoError(t, err)
	cost, err := peoplesweep.EstimateCostMicroUSD(estimate, config.Budgets)
	require.NoError(t, err)
	responseBody, err = json.Marshal(map[string]any{
		"model":   "frozen-sensitive-model-v1",
		"choices": []any{map[string]any{"message": map[string]any{"content": string(providerJSON)}}},
		"usage": map[string]any{"prompt_tokens": estimate.InputTokens,
			"completion_tokens": estimate.OutputTokens},
	})
	require.NoError(t, err)
	response, err := runner.RunPreparedStructured(t.Context(), prepared)
	require.NoError(t, err)
	assert.Equal(t, prepared.WireRequest(), <-received)
	claims, err := peoplesweep.ParseExtraction(response.Output, batches[0], profile)
	require.NoError(t, err)
	expected := evaluationClaim{TargetSlug: fixture.SensitiveTargetSlug,
		Relation: string(personfacts.RelationSupport), Value: fixture.SensitiveAllowedValue,
		EvidenceSteps: []int{0}, ConfidenceBasisPoints: 990}
	assertEvaluationClaims(t, []evaluationClaim{expected}, claims, allowedTargets,
		[]peoplesweep.EvidenceItem{item})
	generation := personfacts.GenerationInput{PersonID: f.PersonID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "sensitive-evaluation", Start: "0", End: "1"}},
		ProgramID:     peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion,
		ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: allowedCatalog.Fingerprint,
		Provider: string(profile.Protocol), ProviderVersion: response.ProviderVersion,
		Model: profile.Model, ModelVersion: response.ModelVersion,
		ResolvedAt: evaluationTime(t, step.ResolvedAt),
		Policy: personfacts.PolicyContext{AllowSensitive: true,
			ProviderPolicyFingerprint: profile.Fingerprint}, Claims: claims}
	result, err := f.Store.ApplyPersonFactGenerationContext(t.Context(), generation,
		store.PersonSweepEvidenceAligner{Store: f.Store})
	require.NoError(t, err)
	require.True(t, evaluationHasDecision(result.Decisions,
		personfacts.DecisionApplied, personfacts.ReasonAppliedProjection))
	current := evaluationCurrentValues(t, f.Store, f.PersonID,
		map[string]struct{}{fixture.SensitiveTargetSlug: {}})
	assertEvaluationCurrent(t,
		map[string][]json.RawMessage{fixture.SensitiveTargetSlug: {fixture.SensitiveAllowedValue}}, current)
	metrics.ExpectedClaims++
	metrics.ExtractedClaims++
	metrics.MatchedClaims++
	metrics.EvidenceCitations++
	metrics.AlignedCitations++
	metrics.Requests++
	metrics.PreparedWireRequests++
	metrics.InputTokens += estimate.InputTokens
	metrics.OutputTokens += estimate.OutputTokens
	metrics.EstimatedCostMicroUSD += cost
}

func loadEvaluationFixture(t *testing.T, name string) evaluationFixture {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "evaluation", name))
	require.NoError(t, err, name)
	var fixture evaluationFixture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	require.NoError(t, decoder.Decode(&fixture), name)
	require.NotEmpty(t, fixture.Name, name)
	require.NotEmpty(t, fixture.Steps, name)
	require.Positive(t, fixture.MaxRequests, name)
	return fixture
}

func runEvaluationFixture(t *testing.T, fixture evaluationFixture, metrics *evaluationMetrics) {
	t.Helper()
	t.Run(fixture.Name, func(t *testing.T) {
		runEvaluationFixtureCase(t, fixture, metrics)
	})
}

func runEvaluationFixtureCase(t *testing.T, fixture evaluationFixture, metrics *evaluationMetrics) {
	t.Helper()
	f := newEvaluationStore(t)
	applyEvaluationDescriptionOverrides(t, f.Store, fixture.DescriptionOverrides)
	catalog, err := f.Store.BuildPersonFactCatalogContext(t.Context(), fixture.AllowSensitive)
	require.NoError(t, err)
	targets := evaluationTargetsBySlug(catalog)
	aligner := store.PersonSweepEvidenceAligner{Store: f.Store}
	items := make([]peoplesweep.EvidenceItem, 0, len(fixture.Steps))
	fixtureRequests := 0

	for stepIndex, step := range fixture.Steps {
		item := insertEvaluationEvidence(t, f, stepIndex, step)
		items = append(items, item)
		assert.Equal(t, step.ExpectedSpan[0], item.Highlight.Start)
		assert.Equal(t, step.ExpectedSpan[1], item.Highlight.End)

		packet := peoplesweep.EvidencePacket{
			PersonID: f.PersonID, ProgramID: peoplesweep.ExtractionProgramID,
			ProgramVersion: peoplesweep.ExtractionProgramVersion, Catalog: catalog,
			Seeds: slices.Clone(items),
		}
		batches, err := peoplesweep.PartitionEvidencePacket(packet, 256*1024, 200)
		if step.ExpectedPacketError != "" {
			require.ErrorContains(t, err, step.ExpectedPacketError)
			assert.Empty(t, step.ExpectedClaims)
			assert.Empty(t, step.ProviderClaims)
			current := evaluationCurrentValues(t, f.Store, f.PersonID,
				map[string]struct{}{store.AttributeSlugLocation: {}})
			assertEvaluationCurrent(t, step.ExpectedCurrent, current)
			items = items[:len(items)-1]
			continue
		}
		require.NoError(t, err)
		require.Len(t, batches, 1)
		evidenceIDs := evaluationEvidenceIDs(t, batches[0])
		providerJSON := evaluationProviderJSON(t, step.ProviderClaims, targets, evidenceIDs, items)
		config := evaluationProviderConfig(fixture.AllowSensitive, "https://example.test/v1")
		profile, err := config.Profile()
		require.NoError(t, err)
		for _, batch := range batches {
			prepared, prepareErr := peoplesweep.NewOpenAIChatDriver(http.DefaultClient).
				Prepare(profile, batch.Request)
			require.NoError(t, prepareErr)
			estimate, estimateErr := peoplesweep.EstimateWireTokenReservation(
				prepared.WireRequest(), batch.Request.MaxOutputTokens)
			require.NoError(t, estimateErr)
			cost, costErr := peoplesweep.EstimateCostMicroUSD(estimate, config.Budgets)
			require.NoError(t, costErr)
			metrics.PreparedWireRequests++
			metrics.InputTokens += estimate.InputTokens
			metrics.OutputTokens += estimate.OutputTokens
			metrics.EstimatedCostMicroUSD += cost
		}
		claims, err := peoplesweep.ParseExtraction(providerJSON, batches[0],
			peoplesweep.ProviderProfile{AllowSensitive: fixture.AllowSensitive})
		require.NoError(t, err)
		assertEvaluationClaims(t, step.ExpectedClaims, claims, targets, items)

		metrics.ExpectedClaims += len(step.ExpectedClaims)
		metrics.ExtractedClaims += len(claims)
		metrics.MatchedClaims += len(step.ExpectedClaims)
		for _, claim := range claims {
			for _, evidence := range claim.Evidence {
				metrics.EvidenceCitations++
				alignment, alignErr := aligner.Align(t.Context(), evidence)
				require.NoError(t, alignErr)
				if alignment.Accepted {
					metrics.AlignedCitations++
				}
			}
		}

		generation := personfacts.GenerationInput{
			PersonID: f.PersonID,
			SourceCursors: []personfacts.SourceCursor{{Lane: string(peoplesweep.SourceConversationText),
				Start: fmt.Sprintf("%04d", stepIndex), End: fmt.Sprintf("%04d", stepIndex+1)}},
			ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion,
			ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint,
			Provider: "frozen-fixture", ProviderVersion: "v1", Model: "frozen-model", ModelVersion: "v1",
			ResolvedAt: evaluationTime(t, step.ResolvedAt),
			Policy: personfacts.PolicyContext{AllowSensitive: fixture.AllowSensitive,
				ProviderPolicyFingerprint: "frozen-evaluation-policy-v1"},
			Claims: claims,
		}
		first, err := f.Store.ApplyPersonFactGenerationContext(t.Context(), generation, aligner)
		require.NoError(t, err)
		assertEvaluationDecisions(t, step.ExpectedDecisions, first.Decisions)
		for _, claim := range claims {
			for _, evidence := range claim.Evidence {
				if evidence.SubjectPersonID == nil || *evidence.SubjectPersonID != f.PersonID {
					if !evaluationHasDecision(first.Decisions,
						personfacts.DecisionIdentityRejected, personfacts.ReasonIdentityMismatch) {
						metrics.SubjectAttributionErrors++
					}
				}
			}
		}
		replay, err := f.Store.ApplyPersonFactGenerationContext(t.Context(), generation, aligner)
		require.NoError(t, err)
		firstJSON, err := json.Marshal(first)
		require.NoError(t, err)
		replayJSON, err := json.Marshal(replay)
		require.NoError(t, err)
		assert.JSONEq(t, string(firstJSON), string(replayJSON), "generation replay changed the projection")

		observedSlugs := make(map[string]struct{}, len(step.ExpectedCurrent)+len(step.RetainedValues))
		for slug := range step.ExpectedCurrent {
			observedSlugs[slug] = struct{}{}
		}
		for slug := range step.RetainedValues {
			observedSlugs[slug] = struct{}{}
		}
		current := evaluationCurrentValues(t, f.Store, f.PersonID, observedSlugs)
		assertEvaluationCurrent(t, step.ExpectedCurrent, current)
		for slug, values := range step.RetainedValues {
			for _, value := range values {
				if !slices.Contains(current[slug], evaluationComparableJSON(t, slug, value)) {
					metrics.FalseDestructiveChanges++
				}
			}
		}
		if step.StaleResolution {
			metrics.StaleResolutionChecks++
			if evaluationCurrentEqual(t, step.ExpectedCurrent, current) {
				metrics.StaleResolutionsCorrect++
			}
		}

		fixtureRequests += len(batches)
		metrics.Requests += len(batches)
	}
	assert.LessOrEqual(t, fixtureRequests, fixture.MaxRequests)
}

func evaluationHasDecision(
	decisions []personfacts.Decision, action personfacts.DecisionAction, reason personfacts.DecisionReason,
) bool {
	for _, decision := range decisions {
		if decision.Action == action && decision.Reason == reason {
			return true
		}
	}
	return false
}

func evaluationProviderConfig(allowSensitive bool, endpoint string) peoplesweep.Config {
	config := configWithProvider(peoplesweep.ProviderConfig{
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: endpoint,
		Model: "frozen-model", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "FROZEN_EVALUATION_API_KEY",
		OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
		RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
		SourceSince:    "2000-01-01", AllowSensitive: allowSensitive,
	})
	config.Budgets.InputCostMicroUSDPerMillionTokens = 2_000_000
	config.Budgets.OutputCostMicroUSDPerMillionTokens = 6_000_000
	config.Budgets.MaxEstimatedCostMicroUSDPerRun = 1_000_000_000
	config.Budgets.MaxEstimatedCostMicroUSDPerDay = 1_000_000_000
	return config
}

func assertEvaluationDecisions(
	t *testing.T, want []evaluationDecision, got []personfacts.Decision,
) {
	t.Helper()
	for _, expected := range want {
		count := 0
		for _, decision := range got {
			if decision.Action == personfacts.DecisionAction(expected.Action) &&
				decision.Reason == personfacts.DecisionReason(expected.Reason) {
				count++
			}
		}
		assert.Equal(t, expected.Count, count, "%s/%s decision count in %#v",
			expected.Action, expected.Reason, got)
	}
}

func newEvaluationStore(t *testing.T) evaluationStore {
	t.Helper()
	st := testutil.NewTestStore(t)
	source, err := st.GetOrCreateSource("slack", "frozen-person-evaluation")
	require.NoError(t, err)
	conversationID, err := st.EnsureConversationWithType(
		source.ID, "frozen-person-evaluation", "direct_chat", "Synthetic evaluation")
	require.NoError(t, err)
	scoped, err := st.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	other, err := st.EnsureParticipant("bob@example.test", "Bob", "example.test")
	require.NoError(t, err)
	person, _, err := st.CreatePersonFromParticipant(scoped)
	require.NoError(t, err)
	_, _, err = st.CreatePersonFromParticipant(other)
	require.NoError(t, err)
	_, err = st.SetPersonTrackingContext(t.Context(), person.ID, true)
	require.NoError(t, err)
	return evaluationStore{Store: st, PersonID: person.ID, ScopedParticipant: scoped,
		OtherParticipant: other, SourceID: source.ID, ConversationID: conversationID}
}

func applyEvaluationDescriptionOverrides(t *testing.T, st *store.Store, overrides map[string]string) {
	t.Helper()
	for slug, description := range overrides {
		definition, err := st.GetAttributeDefinitionBySlugContext(
			t.Context(), store.AttributeObjectPerson, slug)
		require.NoError(t, err)
		descriptionPointer := &description
		_, err = st.UpdateAttributeDefinitionContext(t.Context(), definition.ID, definition.Revision,
			store.AttributeDefinitionUpdate{Description: &descriptionPointer})
		require.NoError(t, err)
	}
}

func insertEvaluationEvidence(
	t *testing.T, f evaluationStore, index int, step evaluationStep,
) peoplesweep.EvidenceItem {
	t.Helper()
	eventTime := evaluationTime(t, step.EventTime)
	senderID := f.ScopedParticipant
	if step.Subject == "other" {
		senderID = f.OtherParticipant
	} else {
		require.Equal(t, "scoped", step.Subject)
	}
	var messageID int64
	err := f.Store.DB().QueryRowContext(t.Context(), f.Store.Rebind(`
		INSERT INTO messages
			(source_id, source_message_id, conversation_id, message_type, sender_id, sent_at, subject)
		VALUES (?, ?, ?, 'chat', ?, ?, '')
		RETURNING id`), f.SourceID, fmt.Sprintf("evaluation-%02d", index),
		f.ConversationID, senderID, eventTime).Scan(&messageID)
	require.NoError(t, err)
	_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(
		`INSERT INTO message_bodies (message_id, body_text) VALUES (?, ?)`), messageID, step.Excerpt)
	require.NoError(t, err)
	if step.Subject == "other" {
		_, err = f.Store.DB().ExecContext(t.Context(), f.Store.Rebind(`
			INSERT INTO message_recipients (message_id, participant_id, recipient_type, email_address)
			VALUES (?, ?, 'to', 'alice@example.test')`), messageID, f.ScopedParticipant)
		require.NoError(t, err)
	}
	items, err := f.Store.HydratePersonSweepMessages(t.Context(), f.PersonID, []int64{messageID})
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, step.Excerpt, items[0].Excerpt)
	if step.Subject == "other" {
		assert.Nil(t, items[0].SubjectPersonID)
	}
	return items[0]
}

func evaluationTargetsBySlug(catalog personfacts.Catalog) map[string]personfacts.TargetDescriptor {
	targets := make(map[string]personfacts.TargetDescriptor, len(catalog.Targets))
	for _, target := range catalog.Targets {
		targets[target.Slug] = target
	}
	return targets
}

func evaluationEvidenceIDs(t *testing.T, batch peoplesweep.PacketBatch) map[int64]string {
	t.Helper()
	raw, err := peoplesweep.CanonicalPacketJSON(batch.Packet)
	require.NoError(t, err)
	var wire struct {
		Seeds []struct {
			ID        string `json:"id"`
			SourceRef string `json:"source_ref"`
		} `json:"seeds"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	ids := make(map[int64]string, len(wire.Seeds))
	for _, item := range wire.Seeds {
		ref, err := peoplesweep.DecodePersonSweepEvidenceRef(item.SourceRef)
		require.NoError(t, err)
		ids[ref.MessageID] = item.ID
	}
	return ids
}

func evaluationProviderJSON(
	t *testing.T,
	claims []evaluationClaim,
	targets map[string]personfacts.TargetDescriptor,
	evidenceIDs map[int64]string,
	items []peoplesweep.EvidenceItem,
) json.RawMessage {
	t.Helper()
	output := make([]map[string]any, 0, len(claims))
	for _, claim := range claims {
		target, ok := targets[claim.TargetSlug]
		require.True(t, ok, "provider fixture target %q is unavailable", claim.TargetSlug)
		ids := make([]string, 0, len(claim.EvidenceSteps))
		for _, evidenceStep := range claim.EvidenceSteps {
			require.Less(t, evidenceStep, len(items), "provider fixture evidence step %d is unavailable", evidenceStep)
			id, exists := evidenceIDs[items[evidenceStep].Ref.MessageID]
			require.True(t, exists, "provider fixture evidence step %d is unavailable", evidenceStep)
			ids = append(ids, id)
		}
		output = append(output, map[string]any{
			"target_key": target.Key, "relation": claim.Relation,
			"value": claim.Value, "evidence_ids": ids,
			"valid_from": claim.ValidFrom, "valid_until": claim.ValidUntil,
			"confidence_basis_points": claim.ConfidenceBasisPoints,
		})
	}
	raw, err := json.Marshal(map[string]any{"claims": output})
	require.NoError(t, err)
	return raw
}

func assertEvaluationClaims(
	t *testing.T,
	want []evaluationClaim,
	got []personfacts.ProposedClaim,
	targets map[string]personfacts.TargetDescriptor,
	items []peoplesweep.EvidenceItem,
) {
	t.Helper()
	require.Len(t, got, len(want))
	for index, expected := range want {
		target, ok := targets[expected.TargetSlug]
		require.True(t, ok)
		claim := got[index]
		assert.Equal(t, target, claim.Target)
		assert.Equal(t, personfacts.ClaimRelation(expected.Relation), claim.Relation)
		assert.JSONEq(t, string(expected.Value), string(claim.SubmittedValue))
		assert.Equal(t, evaluationOptionalTime(t, expected.ValidFrom), claim.ValidFrom)
		assert.Equal(t, evaluationOptionalTime(t, expected.ValidUntil), claim.ValidUntil)
		assert.Equal(t, personfacts.OriginExtraction, claim.Origin)
		assert.Equal(t, expected.ConfidenceBasisPoints, claim.Confidence.ReportedScore)
		require.Len(t, claim.Evidence, len(expected.EvidenceSteps))
		for evidenceIndex, stepIndex := range expected.EvidenceSteps {
			evidence := claim.Evidence[evidenceIndex]
			item := items[stepIndex]
			ref, err := peoplesweep.DecodePersonSweepEvidenceRef(evidence.SourceRef)
			require.NoError(t, err)
			assert.Equal(t, item.Ref, ref)
			assert.Equal(t, item.PersonID, evidence.PersonID)
			assert.Equal(t, item.SubjectPersonID, evidence.SubjectPersonID)
			assert.Equal(t, personfacts.EvidenceArchive, evidence.SourceClass)
			assert.Equal(t, item.Directness, evidence.Directness)
			assert.Equal(t, item.Authority, evidence.Authority)
			assert.Equal(t, item.Excerpt, evidence.Excerpt)
			assert.Equal(t, item.ContentSHA256, evidence.ContentSHA256)
			assert.Equal(t, item.SourceVersion, evidence.SourceVersion)
			assert.Equal(t, item.EventTime, evidence.EventTime)
			assert.Equal(t, item.RecordedTime, evidence.RecordedTime)
			assert.Equal(t, item.IdentityBasisPoints, evidence.IdentityScore)
			require.NotNil(t, evidence.SpanStart)
			require.NotNil(t, evidence.SpanEnd)
			assert.Equal(t, int64(item.Highlight.Start), *evidence.SpanStart)
			assert.Equal(t, int64(item.Highlight.End), *evidence.SpanEnd)
		}
	}
}

func evaluationOptionalTime(t *testing.T, value *string) *time.Time {
	t.Helper()
	if value == nil {
		return nil
	}
	parsed := evaluationTime(t, *value)
	return &parsed
}

func evaluationCurrentValues(
	t *testing.T, st *store.Store, personID int64, observedSlugs map[string]struct{},
) map[string][]string {
	t.Helper()
	values := make(map[string][]string)
	slugs := make([]string, 0, len(observedSlugs))
	for slug := range observedSlugs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		if slug == "employment" {
			employments, err := st.ListEmploymentsContext(t.Context(), store.EmploymentFilter{
				PersonID: personID, CurrentOnly: true, Limit: 100,
			})
			require.NoError(t, err)
			for _, employment := range employments {
				values[slug] = append(values[slug], evaluationEmploymentJSON(t, st, employment))
			}
			continue
		}
		attributeValues, err := st.ListPersonAttributeValuesContext(t.Context(), personID,
			store.PersonAttributeQuery{DefinitionSlug: slug})
		require.NoError(t, err)
		for _, value := range attributeValues {
			values[slug] = append(values[slug], evaluationAttributeJSON(t, value.Value))
		}
	}
	for slug := range values {
		sort.Strings(values[slug])
	}
	return values
}

func evaluationAttributeJSON(t *testing.T, value store.AttributeValue) string {
	t.Helper()
	var raw any
	switch value.Type {
	case store.AttributeValueText:
		raw = value.Text
	case store.AttributeValueInteger:
		raw = value.Integer
	case store.AttributeValueReal:
		raw = value.Real
	case store.AttributeValueBoolean:
		raw = value.Boolean
	case store.AttributeValueDate:
		raw = value.Date
	case store.AttributeValueTimestamp:
		require.NotNil(t, value.Timestamp)
		raw = value.Timestamp.UTC().Format(time.RFC3339Nano)
	default:
		require.FailNow(t, "unsupported evaluation attribute value", string(value.Type))
	}
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	return string(encoded)
}

func evaluationEmploymentJSON(t *testing.T, st *store.Store, employment store.Employment) string {
	t.Helper()
	organization, err := st.GetOrganizationContext(t.Context(), employment.OrganizationID)
	require.NoError(t, err)
	organizationValue := map[string]any{"name": organization.Name}
	if organization.PrimaryDomain != nil {
		organizationValue["domain"] = *organization.PrimaryDomain
	}
	value := map[string]any{"organization": organizationValue}
	for key, field := range map[string]*string{
		"title": employment.Title, "role": employment.Role, "department": employment.Department,
		"location": employment.Location,
	} {
		if field != nil {
			value[key] = *field
		}
	}
	if employment.StartDate != nil {
		value["start_date"] = employment.StartDate
	}
	if employment.EndDate != nil {
		value["end_date"] = employment.EndDate
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return evaluationComparableJSON(t, "employment", encoded)
}

func assertEvaluationCurrent(
	t *testing.T, want map[string][]json.RawMessage, got map[string][]string,
) {
	t.Helper()
	assert.True(t, evaluationCurrentEqual(t, want, got), "current projection mismatch: %#v", got)
}

func evaluationCurrentEqual(
	t *testing.T, want map[string][]json.RawMessage, got map[string][]string,
) bool {
	t.Helper()
	wantComparable := make(map[string][]string, len(want))
	for slug, values := range want {
		for _, value := range values {
			wantComparable[slug] = append(wantComparable[slug], evaluationComparableJSON(t, slug, value))
		}
		sort.Strings(wantComparable[slug])
	}
	cleanGot := make(map[string][]string, len(got))
	for slug, values := range got {
		if len(values) > 0 {
			cleanGot[slug] = slices.Clone(values)
		}
	}
	return assert.ObjectsAreEqual(wantComparable, cleanGot)
}

func evaluationComparableJSON(t *testing.T, slug string, raw json.RawMessage) string {
	t.Helper()
	var value any
	require.NoError(t, json.Unmarshal(raw, &value))
	if slug == "employment" {
		if object, ok := value.(map[string]any); ok {
			if organization, ok := object["organization"].(map[string]any); ok {
				delete(organization, "id")
			}
		}
	}
	canonical, err := json.Marshal(value)
	require.NoError(t, err)
	return string(canonical)
}

func evaluationTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	require.NoError(t, err)
	return parsed.UTC()
}
