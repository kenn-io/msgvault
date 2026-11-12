package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/store"
)

// scriptedBriefRunner answers the extraction call with an empty claim set and
// the brief call with a brief built from the packet the worker actually sent,
// exactly as a provider would: it reads the seed IDs and the catalog target out
// of the request rather than being handed them by the test. Everything else on
// the path — window, packet, parser, renderer, apply — is production code.
type scriptedBriefRunner struct {
	t             *testing.T
	briefRequests int
	briefPackets  []map[string]any

	extractionRequests int
	extractionInputs   []string
	candidate          func(map[string]any) json.RawMessage
	afterBrief         func()
}

func (r *scriptedBriefRunner) PrepareStructured(
	_ context.Context, request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	wire, err := json.Marshal(request)
	if err != nil {
		return peoplesweep.PreparedStructuredRequest{}, err
	}
	return peoplesweep.NewPreparedStructuredRequest(request, wire)
}

func (r *scriptedBriefRunner) PrepareRepair(
	peoplesweep.StructuredRequest, peoplesweep.ValidationFailure,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.PreparedStructuredRequest{},
		errors.New("scripted brief runner must not need a repair")
}

func (r *scriptedBriefRunner) BeginStructuredExecution(
	_ context.Context, primary peoplesweep.PreparedStructuredRequest,
) (peoplesweep.StructuredExecutionSession, error) {
	return scriptedBriefSession{runner: r}, nil
}

func (r *scriptedBriefRunner) RunPreparedStructured(
	_ context.Context, prepared peoplesweep.PreparedStructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	request := prepared.Request()
	response := peoplesweep.StructuredResponse{
		ProviderRequestID: "request-" + request.ProgramID,
		ProviderVersion:   "provider-v1", ModelVersion: "model-v1",
		Usage:      peoplesweep.TokenUsage{InputTokens: 11, OutputTokens: 4},
		UsageKnown: true,
	}
	if request.ProgramID != peoplesweep.BriefProgramID {
		r.extractionRequests++
		r.extractionInputs = append(r.extractionInputs, request.InputText)
		response.Output = json.RawMessage(`{"claims":[]}`)
		return response, nil
	}
	r.briefRequests++
	packet := decodeBriefPacket(r.t, request.InputText)
	r.briefPackets = append(r.briefPackets, packet)
	response.Output = briefCandidateForPacket(r.t, packet)
	if r.candidate != nil {
		response.Output = r.candidate(packet)
	}
	if r.afterBrief != nil {
		r.afterBrief()
	}
	return response, nil
}

func (r *scriptedBriefRunner) RunStructured(
	context.Context, peoplesweep.StructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	return peoplesweep.StructuredResponse{},
		errors.New("scripted brief runner requires the prepared path")
}

type scriptedBriefSession struct{ runner *scriptedBriefRunner }

func (s scriptedBriefSession) PrimaryCall(
	prepared peoplesweep.PreparedStructuredRequest,
) (peoplesweep.PreparedStructuredCall, error) {
	return scriptedBriefCall{runner: s.runner, prepared: prepared}, nil
}

func (scriptedBriefSession) SemanticValidationFailure(
	peoplesweep.StructuredResponse,
) (peoplesweep.ValidationFailure, error) {
	return peoplesweep.ValidationFailure{},
		errors.New("scripted brief runner must not fail semantic validation")
}

func (scriptedBriefSession) PrepareRepair(
	peoplesweep.ValidationFailure,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.PreparedStructuredRequest{},
		errors.New("scripted brief runner must not need a repair")
}

func (scriptedBriefSession) RepairCall(
	peoplesweep.PreparedStructuredRequest,
) (peoplesweep.PreparedStructuredCall, error) {
	return nil, errors.New("scripted brief runner must not need a repair call")
}

type scriptedBriefCall struct {
	runner   *scriptedBriefRunner
	prepared peoplesweep.PreparedStructuredRequest
}

func (c scriptedBriefCall) Execute(
	ctx context.Context, markStarted func(context.Context) error,
) (peoplesweep.StructuredResponse, error) {
	if err := markStarted(ctx); err != nil {
		return peoplesweep.StructuredResponse{}, err
	}
	return c.runner.RunPreparedStructured(ctx, c.prepared)
}

// decodeBriefPacket pulls the evidence packet back out of the request text the
// worker sent, which is the only place a provider learns the evidence IDs.
func decodeBriefPacket(t *testing.T, inputText string) map[string]any {
	t.Helper()
	const marker = "Evidence packet JSON:\n"
	index := len(inputText) - 1
	for ; index >= 0; index-- {
		if len(inputText[index:]) >= len(marker) && inputText[index:index+len(marker)] == marker {
			break
		}
	}
	require.GreaterOrEqual(t, index, 0, "brief request must carry its evidence packet")
	var packet map[string]any
	require.NoError(t, json.Unmarshal([]byte(inputText[index+len(marker):]), &packet))
	return packet
}

func briefPacketSeedIDs(t *testing.T, packet map[string]any) []string {
	t.Helper()
	seeds, ok := packet["seeds"].([]any)
	require.True(t, ok, "brief packet must carry seed evidence")
	ids := make([]string, 0, len(seeds))
	for _, seed := range seeds {
		entry, entryOK := seed.(map[string]any)
		require.True(t, entryOK)
		id, idOK := entry["id"].(string)
		require.True(t, idOK)
		ids = append(ids, id)
	}
	return ids
}

func briefPacketTargetKey(t *testing.T, packet map[string]any) string {
	t.Helper()
	catalog, ok := packet["catalog"].(map[string]any)
	require.True(t, ok)
	targets, ok := catalog["targets"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, targets, "brief packet must carry a catalog to propose against")
	first, ok := targets[0].(map[string]any)
	require.True(t, ok)
	key, ok := first["key"].(string)
	require.True(t, ok)
	return key
}

// briefCandidateForPacket is a well-evidenced brief over the packet's own
// seeds, including one proposed attribute so the brief-origin claim path is
// exercised end to end.
func briefCandidateForPacket(t *testing.T, packet map[string]any) json.RawMessage {
	t.Helper()
	ids := briefPacketSeedIDs(t, packet)
	require.NotEmpty(t, ids)
	newest := ids[0]
	target := briefPacketTargetKey(t, packet)
	return json.RawMessage(fmt.Sprintf(`{
		"last_meaningful_interaction":{"evidence_id":%q,"summary":"they start a new role in September"},
		"highlights":[{"text":"they start a new role in September","speaker":"person",
			"evidence_ids":[%q],"observed_at":null,"confidence_basis_points":800,"life_event":null}],
		"follow_ups":[{"question":"how the first week went","why":"they were about to start",
			"highlight_index":0,"evidence_ids":[%q]}],
		"appreciations":[],"thanks_suggestions":[],
		"uncertainties":[{"text":"the new role may already have started","kind":"stale",
			"evidence_ids":[%q]}],
		"possible_attributes":[{"target_key":%q,"relation":"support","value":"Riverton",
			"evidence_ids":[%q],"valid_from":null,"valid_until":null,
			"confidence_basis_points":900}]}`,
		newest, newest, newest, newest, target, newest))
}

func briefPacketEventContextEvents(t *testing.T, packet map[string]any) []any {
	t.Helper()
	context, ok := packet["brief_context"].(map[string]any)
	require.True(t, ok, "brief packet must carry event context")
	events, exists := context["events"]
	require.True(t, exists)
	if events == nil {
		return nil
	}
	list, ok := events.([]any)
	require.True(t, ok)
	return list
}

type briefWorkerEndToEndFixture struct {
	journal personSweepJournalFixture
	config  peoplesweep.Config
	runner  *scriptedBriefRunner
	worker  peoplesweep.Worker
	now     time.Time
}

func newBriefWorkerEndToEndFixture(
	t *testing.T, suffix string,
) briefWorkerEndToEndFixture {
	t.Helper()
	requirements := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	first := f.insertMessage(t, "brief-e2e-1-"+suffix, "chat", f.aliceID,
		time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f, first, "I finally finished the move last weekend")
	second := f.insertMessage(t, "brief-e2e-2-"+suffix, "chat", f.aliceID,
		time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f, second, "I start the new role in September")
	_, err := f.store.SetPersonBriefEnrollmentContext(
		t.Context(), f.alicePersonID, true, "test-owner", false)
	requirements.NoError(err)

	config := peoplesweep.Config{Enabled: true,
		Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
			Model: "gpt-test", Auth: peoplesweep.AuthBearer,
			Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode:          peoplesweep.OutputModeNativeJSONSchema,
			TokenLimitParameter: "max_completion_tokens",
			RetentionPosture:    "zero_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			SourceSince:    "2025-01-01", AllowSensitive: true, RequestTimeout: time.Second,
		}}}
	config.ApplyDefaults()
	requirements.NoError(config.Validate())
	profile, err := config.Profile()
	requirements.NoError(err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	requirements.NoError(err)
	_, _, err = f.store.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test-owner")
	requirements.NoError(err)

	runner := &scriptedBriefRunner{t: t}
	ids := []string{"run-" + suffix, "attempt-" + suffix}
	worker := peoplesweep.Worker{Config: config, Store: f.store, Source: f.store,
		Context: peoplesweep.NewContextRetriever(f.store), Sink: f.store, Runner: runner,
		Catalog: f.store, Brief: f.store, Archive: f.store,
		Clock: func() time.Time { return now },
		NewID: func() string {
			id := ids[0]
			if len(ids) > 1 {
				ids = ids[1:]
			}
			return id
		}, WorkerID: "worker-" + suffix}
	return briefWorkerEndToEndFixture{journal: f, config: config, runner: runner,
		worker: worker, now: now}
}

// TestPersonSweepWorkerStoresARealBriefThroughApplyPersonSweep is the
// worker-to-store seam: a brief the real ParseBrief and RenderBrief produced,
// over a real archive window, committed by the real ApplyPersonSweep.
func TestPersonSweepWorkerStoresARealBriefThroughApplyPersonSweep(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "brief-e2e")
	st := f.journal.store
	personID := f.journal.alicePersonID

	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	requirements.NoError(err)
	requirements.Equal(1, result.PeopleSucceeded)
	requirements.Equal(1, f.runner.briefRequests, "exactly one brief call, no repair")
	requirements.Len(result.People, 1)
	checks.Equal("attempt-brief-e2e", result.People[0].AttemptID)
	checks.Equal(1, result.People[0].BriefVersion)
	checks.Empty(result.People[0].BriefFailureClass)

	stored, err := st.GetPersonBriefContext(t.Context(), personID, 0)
	requirements.NoError(err)
	checks.Equal(1, stored.Version)
	checks.Equal(store.PersonBriefStatusCurrent, stored.Status)
	checks.Equal(peoplesweep.BriefProgramID, stored.ProgramID)
	checks.Equal(peoplesweep.BriefProgramFingerprint(), stored.ProgramFingerprint)
	checks.Equal(peoplesweep.BriefRendererPolicyV3, stored.RendererPolicy)
	checks.Contains(stored.RenderedText, "Last time you talked")
	checks.Equal(string(f.config.Providers["default"].Protocol), stored.Provider)
	checks.Equal("provider-v1", stored.ProviderVersion)
	checks.Equal("gpt-test", stored.Model)
	checks.Equal("model-v1", stored.ModelVersion)
	checks.Zero(stored.DroppedItemCount, "a well-evidenced brief drops nothing")

	// The structure re-validates against the frozen schema it was stored under.
	var structured map[string]any
	requirements.NoError(json.Unmarshal(stored.Structured, &structured))
	checks.Len(structured["highlights"], 1)
	checks.Len(structured["follow_ups"], 1)
	checks.Empty(structured["appreciations"])

	var boundary peoplesweep.BriefBoundary
	requirements.NoError(json.Unmarshal(stored.Boundary, &boundary))
	highWater := latestPersonSweepSequence(t, st)
	checks.Equal(highWater, boundary.ThroughSequence,
		"the boundary records the archive high water the plan captured")
	checks.Equal(2, boundary.ItemCount)
	checks.Equal([]string{"conversation_text"}, boundary.Lanes)

	pointers, err := st.ListPersonBriefEvidenceContext(t.Context(), stored.ID)
	requirements.NoError(err)
	requirements.Len(pointers, 1, "the brief cited one item, so it has one pointer")
	checks.Zero(pointers[0].Ordinal)
	checks.True(pointers[0].Supported)
	checks.NotEmpty(pointers[0].SourceRef)
	var evidencePerson int64
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT person_id FROM person_fact_evidence WHERE id = ?`),
		pointers[0].EvidenceID).Scan(&evidencePerson))
	checks.Equal(personID, evidencePerson,
		"the pointer references a real person_fact_evidence row for this person")

	var briefClaims int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM person_fact_claims
		WHERE person_id = ? AND generation_id = ? AND origin = 'brief'`),
		personID, stored.GenerationID).Scan(&briefClaims))
	checks.Zero(briefClaims, "brief suggestions must not enter the profile fact ledger")
	checks.Zero(result.ProjectedWrites, "a brief suggestion must not change the curated profile")
	checks.Len(structured["possible_attributes"], 1, "the saved brief retains the suggestion")

	var attemptStatus, briefFailureClass string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT status, brief_failure_class FROM person_sweep_attempts WHERE id = ?`),
		"attempt-brief-e2e").Scan(&attemptStatus, &briefFailureClass))
	checks.Equal("succeeded", attemptStatus)
	checks.Empty(briefFailureClass)

	var briefBatches int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT COUNT(*) FROM person_sweep_batches
		WHERE attempt_id = ? AND purpose = 'brief' AND status = 'succeeded'`),
		"attempt-brief-e2e").Scan(&briefBatches))
	checks.Equal(1, briefBatches, "the brief call is reconciled like any other")
}

func TestPersonAppreciationsRefuseUnknownBriefVersions(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "brief-version")
	st, personID := f.journal.store, f.journal.alicePersonID
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.PeopleSucceeded)
	profile, err := f.config.Profile()
	require.NoError(t, err)
	for _, version := range []string{"v1", "v2", "v4"} {
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET program_version = ? WHERE person_id = ? AND status = 'current'`), version, personID)
		require.NoError(t, err)
		projection, err := st.ProjectPersonAppreciationsContext(t.Context(), personID, profile)
		if version == "v4" {
			assert.ErrorIs(t, err, store.ErrPersonAppreciationVersionUnsupported)
		} else {
			require.NoError(t, err)
			assert.Empty(t, projection.Appreciations)
		}
	}
}

func TestPersonSweepAppreciationProofProjectionAndDismissal(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "appreciation-proof")
	st, journal := f.journal.store, f.journal
	require.NoError(t, st.EnsureConversationParticipantContext(t.Context(), journal.conversationID, journal.aliceID, "member"))
	require.NoError(t, st.EnsureConversationParticipantContext(t.Context(), journal.conversationID, journal.bobID, "member"))
	require.NoError(t, st.SetConversationMemberCountContext(t.Context(), journal.conversationID, 2))
	peerID := journal.insertMessage(t, "helped", "chat", journal.aliceID, f.now.Add(-2*time.Hour))
	addSweepBody(t, journal, peerID, "I carried your boxes upstairs.")
	ownerID := journal.insertMessage(t, "owner-thanks", "chat", journal.bobID, f.now.Add(-time.Hour))
	addSweepBody(t, journal, ownerID, "Thanks for carrying my boxes upstairs!")
	_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET source_is_from_me = TRUE, is_sent = TRUE WHERE id = ?`), ownerID)
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		peers, ok := packet["appreciation_peers"].([]any)
		require.True(t, ok)
		owners, ok := packet["appreciation_owners"].([]any)
		require.True(t, ok)
		require.NotEmpty(t, owners)
		assert.Equal(t, "Thanks for carrying my boxes upstairs!", owners[0].(map[string]any)["exact_excerpt"])
		var anchor string
		for _, value := range peers {
			peer := value.(map[string]any)
			if peer["exact_excerpt"] == "I carried your boxes upstairs." {
				anchor = peer["anchor_id"].(string)
			}
		}
		require.NotEmpty(t, anchor)
		owner := owners[0].(map[string]any)
		return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they helped with the move"},
 "highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"carrying the boxes","anchor_id":%q,
 "quote":"carried your boxes","thanked":{"evidence_id":%q,"quote":"Thanks for carrying my boxes upstairs!"}}],
 "uncertainties":[],"possible_attributes":[]}`, anchor, anchor, owner["evidence_id"].(string)))
	}
	run, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	require.NotEmpty(t, f.runner.extractionInputs)
	assert.Contains(t, f.runner.extractionInputs[0], "I carried your boxes upstairs.")
	for _, input := range f.runner.extractionInputs {
		assert.NotContains(t, input, "Thanks for carrying my boxes upstairs!")
	}
	profile, err := f.config.Profile()
	require.NoError(t, err)
	projection, err := st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	require.NoError(t, err)
	require.Len(t, projection.Appreciations, 1)
	key := projection.Appreciations[0].Suggestion.Key
	assert.Equal(t, "already_thanked", projection.Appreciations[0].Suggestion.State)
	states, err := st.GetPersonAppreciationStatesContext(t.Context(), []string{key})
	require.NoError(t, err)
	require.NotNil(t, states[key].ThanksBriefID)
	assert.Equal(t, projection.Brief.ID, *states[key].ThanksBriefID)
	proofID := projection.Brief.ID
	proofBoundary := append([]byte(nil), projection.Brief.Boundary...)
	var laterBoundary peoplesweep.BriefBoundary
	require.NoError(t, json.Unmarshal(projection.Brief.Boundary, &laterBoundary))
	require.Len(t, laterBoundary.AppreciationMemory, 1)
	later := &laterBoundary.AppreciationMemory[0]
	later.Suggestion.State, later.Suggestion.PriorThanks = "suggested", "unknown"
	later.Suggestion.Thanked, later.OwnerRef, later.OwnerRawSHA256 = nil, nil, ""
	laterJSON, err := json.Marshal(laterBoundary)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET status = 'superseded' WHERE id = ?`), proofID)
	require.NoError(t, err)
	for version := 2; version <= 202; version++ {
		status := "superseded"
		if version == 202 {
			status = "current"
		}
		_, err = st.DB().ExecContext(t.Context(), st.Rebind(`INSERT INTO person_briefs
 (person_id, version, generation_id, status, program_id, program_version, program_fingerprint,
 provider, provider_version, model, model_version, provider_policy_fingerprint, boundary_json,
 structured_json, rendered_text, renderer_policy, dropped_item_count, generated_at)
 SELECT person_id, ?, generation_id, ?, program_id, program_version, program_fingerprint,
 provider, provider_version, model, model_version, provider_policy_fingerprint, ?,
 structured_json, rendered_text, renderer_policy, dropped_item_count, generated_at FROM person_briefs WHERE id = ?`), version, status, string(laterJSON), proofID)
		require.NoError(t, err)
	}
	projection, err = st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	require.NoError(t, err)
	require.Len(t, projection.Appreciations, 1)
	assert.Equal(t, 202, projection.Brief.Version)
	assert.Equal(t, "already_thanked", projection.Appreciations[0].Suggestion.State, "the exact proof is older than the recent-version window")
	retired, err := st.PersonSweepAppreciationRetired(t.Context(), journal.alicePersonID, []string{key}, profile)
	require.NoError(t, err)
	assert.True(t, retired[key], "a valid old proof frees the active slot before provider input")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET boundary_json = ? WHERE id = ?`), `{"appreciation_memory":[{}, {}, {}, {}]}`, proofID)
	require.NoError(t, err)
	_, err = st.PersonSweepAppreciationRetired(t.Context(), journal.alicePersonID, []string{key}, profile)
	assert.ErrorContains(t, err, "invalid appreciation proof boundary")
	_, err = st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	assert.ErrorContains(t, err, "invalid appreciation proof boundary")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET boundary_json = ? WHERE id = ?`), string(proofBoundary), proofID)
	require.NoError(t, err)
	// Simulate another apply publishing the proof while this provider call is
	// in flight. The apply fence must suppress the re-admitted contribution.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_appreciation_state SET thanks_brief_id = NULL WHERE contribution_key = ?`), key)
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		for _, value := range packet["appreciation_peers"].([]any) {
			peer := value.(map[string]any)
			if peer["contribution_key"] == key {
				anchor := peer["anchor_id"].(string)
				return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they helped"},"highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"carrying boxes","anchor_id":%q,"quote":"carried your boxes","thanked":null}],"uncertainties":[],"possible_attributes":[]}`, anchor, anchor))
			}
		}
		require.FailNow(t, "expected contribution before proof is restored")
		return nil
	}
	f.runner.afterBrief = func() {
		_, err := st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_appreciation_state SET thanks_brief_id = ? WHERE contribution_key = ?`), proofID, key)
		require.NoError(t, err)
	}
	var raceID int
	f.worker.NewID = func() string { raceID++; return fmt.Sprintf("proof-race-%d", raceID) }
	run, err = f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	brief, err := st.GetPersonBriefContext(t.Context(), journal.alicePersonID, 0)
	require.NoError(t, err)
	var raceBoundary peoplesweep.BriefBoundary
	require.NoError(t, json.Unmarshal(brief.Boundary, &raceBoundary))
	require.Len(t, raceBoundary.AppreciationMemory, 1)
	assert.Equal(t, "already_thanked", raceBoundary.AppreciationMemory[0].Suggestion.State, "proof published during inference retires the in-flight item")
	assert.Contains(t, brief.RenderedText, "You already thanked them")
	f.runner.afterBrief = nil
	// Restore the independently seeded version with the contribution so the
	// following read exercises stale proof against an otherwise live snapshot.
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET status = 'superseded' WHERE id = ?`), brief.ID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET status = 'current' WHERE person_id = ? AND version = 202`), journal.alicePersonID)
	require.NoError(t, err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE message_bodies SET body_text = ? WHERE message_id = ?`), "Edited thanks", ownerID)
	require.NoError(t, err)
	projection, err = st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	require.NoError(t, err)
	require.Len(t, projection.Appreciations, 1)
	assert.Equal(t, "unknown", projection.Appreciations[0].Suggestion.PriorThanks)
	retired, err = st.PersonSweepAppreciationRetired(t.Context(), journal.alicePersonID, []string{key}, profile)
	require.NoError(t, err)
	assert.False(t, retired[key], "stale proof does not prevent a fresh suggestion")
	newOwnerID := journal.insertMessage(t, "new-owner-thanks", "chat", journal.bobID, f.now.Add(-30*time.Minute))
	addSweepBody(t, journal, newOwnerID, "Thank you for carrying the boxes!")
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE messages SET source_is_from_me = TRUE, is_sent = TRUE WHERE id = ?`), newOwnerID)
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		var anchor, owner string
		for _, value := range packet["appreciation_peers"].([]any) {
			peer := value.(map[string]any)
			if peer["contribution_key"] == key {
				anchor = peer["anchor_id"].(string)
			}
		}
		for _, value := range packet["appreciation_owners"].([]any) {
			entry := value.(map[string]any)
			if entry["exact_excerpt"] == "Thank you for carrying the boxes!" {
				owner = entry["evidence_id"].(string)
			}
		}
		require.NotEmpty(t, anchor)
		require.NotEmpty(t, owner)
		return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they helped"},"highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"carrying boxes","anchor_id":%q,"quote":"carried your boxes","thanked":{"evidence_id":%q,"quote":"Thank you for carrying the boxes!"}}],"uncertainties":[],"possible_attributes":[]}`, anchor, anchor, owner))
	}
	var nextID int
	f.worker.NewID = func() string { nextID++; return fmt.Sprintf("new-proof-%d", nextID) }
	run, err = f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	states, err = st.GetPersonAppreciationStatesContext(t.Context(), []string{key})
	require.NoError(t, err)
	require.NotNil(t, states[key].ThanksBriefID)
	assert.NotEqual(t, proofID, *states[key].ThanksBriefID)
	projection, err = st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	require.NoError(t, err)
	require.Len(t, projection.Appreciations, 1)
	assert.Equal(t, "evidenced", projection.Appreciations[0].Suggestion.PriorThanks)
	first, err := st.DismissPersonAppreciationContext(t.Context(), journal.alicePersonID, key, "owner", f.now, profile)
	require.NoError(t, err)
	retry, err := st.DismissPersonAppreciationContext(t.Context(), journal.alicePersonID, key, "other", f.now.Add(time.Hour), profile)
	require.NoError(t, err)
	assert.Equal(t, first.DismissedAt, retry.DismissedAt)
	assert.Equal(t, first.DismissedBy, retry.DismissedBy)
	projection, err = st.ProjectPersonAppreciationsContext(t.Context(), journal.alicePersonID, profile)
	require.NoError(t, err)
	assert.Empty(t, projection.Appreciations)
}

func TestPersonSweepApplyDoesNotReviveDismissalDuringGeneration(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "appreciation-race")
	key, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", "brief-e2e-1-appreciation-race")
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		peers, ok := packet["appreciation_peers"].([]any)
		require.True(t, ok)
		var anchor string
		for _, value := range peers {
			peer := value.(map[string]any)
			if peer["contribution_key"] == key {
				anchor = peer["anchor_id"].(string)
			}
		}
		require.NotEmpty(t, anchor)
		return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they finished the move"},
 "highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"helping with the move",
 "anchor_id":%q,"quote":"finished the move","thanked":null}],"uncertainties":[],"possible_attributes":[]}`, anchor, anchor))
	}
	f.runner.afterBrief = func() {
		_, err := f.journal.store.RecordPersonAppreciationDismissalContext(t.Context(), key, "person-a", "owner", f.now)
		require.NoError(t, err)
	}
	run, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	brief, err := f.journal.store.GetPersonBriefContext(t.Context(), f.journal.alicePersonID, 0)
	require.NoError(t, err)
	assert.NotContains(t, string(brief.Structured), key)
	assert.NotContains(t, brief.RenderedText, "helping with the move")
	var boundary peoplesweep.BriefBoundary
	require.NoError(t, json.Unmarshal(brief.Boundary, &boundary))
	assert.Empty(t, boundary.AppreciationMemory)
}

// The status-only case: no changed seed, so extraction runs no batch, and the
// brief call is the only provider work in the attempt. The generation must
// still carry the provider identity rather than the host-only one.
func TestPersonSweepWorkerStoresABriefOnAStatusOnlyAttemptThroughApplyPersonSweep(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "brief-e2e-status")
	st := f.journal.store
	personID := f.journal.alicePersonID

	// Sweep the two in-range messages, then journal one more change the profile
	// cannot use: a message dated before source_since. The optimistic cursor
	// still advances over it, so the attempt has real cursor progress and no
	// changed seed — a genuine status-only extraction. The brief window works
	// from event time rather than the cursor, so it still sees the two
	// in-range messages.
	profile, err := f.config.Profile()
	requirements.NoError(err)
	sweepCursorsToHighWater(t, st, personID, profile, f.now)
	f.journal.insertMessage(t, "brief-e2e-status-out-of-range", "chat", f.journal.aliceID,
		time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC))

	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	requirements.NoError(err)
	requirements.Equal(1, result.PeopleSucceeded)
	requirements.Len(result.People, 1)
	checks.Equal(1, result.People[0].BriefVersion)

	stored, err := st.GetPersonBriefContext(t.Context(), personID, 0)
	requirements.NoError(err)
	var provider, model string
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT provider, model FROM person_fact_generations WHERE id = ?`),
		stored.GenerationID).Scan(&provider, &model))
	checks.NotEqual(peoplesweep.StatusOnlyProvider, provider,
		"a completed brief call makes the generation a provider generation")
	checks.Equal("gpt-test", model)

	// The extraction half really did run no batch: the brief is the only
	// provider call in the attempt, and it took ordinal zero.
	rows, err := st.DB().QueryContext(t.Context(), st.Rebind(`
		SELECT batch_ordinal, call_ordinal, purpose FROM person_sweep_batches
		WHERE attempt_id = ? ORDER BY batch_ordinal, call_ordinal`),
		"attempt-brief-e2e-status")
	requirements.NoError(err)
	defer func() { requirements.NoError(rows.Close()) }()
	coordinates := make([]peoplesweep.ProviderCallCoordinate, 0, 1)
	for rows.Next() {
		var coordinate peoplesweep.ProviderCallCoordinate
		requirements.NoError(rows.Scan(&coordinate.BatchOrdinal,
			&coordinate.CallOrdinal, &coordinate.Purpose))
		coordinates = append(coordinates, coordinate)
	}
	requirements.NoError(rows.Err())
	checks.Equal([]peoplesweep.ProviderCallCoordinate{{BatchOrdinal: 0, CallOrdinal: 0,
		Purpose: peoplesweep.ProviderCallPurposeBrief}}, coordinates)
}

// A manual force for a person who is not enrolled is refused before any work is
// published and before any provider call is paid for.
func TestPersonSweepWorkerRefusesAForcedBriefForAnUnenrolledPersonEndToEnd(t *testing.T) {
	checks := assert.New(t)
	requirements := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "brief-e2e-unenrolled")
	st := f.journal.store
	personID := f.journal.alicePersonID
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), personID, false, "test-owner", false)
	requirements.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(
		`DELETE FROM person_sweep_work WHERE person_id = ?`), personID)
	requirements.NoError(err)

	_, err = f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	requirements.ErrorIs(err, peoplesweep.ErrPersonBriefNotEnrolled)
	checks.Zero(f.runner.briefRequests, "and pays for no provider call")

	var workRows, runRows int
	requirements.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
		`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&workRows))
	checks.Zero(workRows, "an unenrolled person must not have work published")
	requirements.NoError(st.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM person_sweep_runs`).Scan(&runRows))
	checks.Zero(runRows, "a request that cannot produce a brief starts no run")
}

// sweepCursorsToHighWater marks one person's sweep as fully caught up: the
// optimistic cursor at the journal high water, reconciliation complete, and a
// recent backstop. It is the durable state a finished scheduled sweep leaves.
func sweepCursorsToHighWater(
	t *testing.T, st *store.Store, personID int64, profile peoplesweep.ProviderProfile, now time.Time,
) {
	t.Helper()
	catalog, err := st.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
	require.NoError(t, err)
	keys := make([]peoplesweep.CursorKey, 0, len(profile.AllowedSources))
	for _, lane := range profile.AllowedSources {
		keys = append(keys, peoplesweep.CursorKey{PersonID: personID, SourceLane: lane,
			ProgramFingerprint: peoplesweep.ProgramFingerprint(),
			CatalogFingerprint: catalog.Fingerprint})
	}
	_, err = st.EnsurePersonSweepCursors(t.Context(), keys)
	require.NoError(t, err)
	result, err := st.DB().ExecContext(t.Context(), st.Rebind(`
		UPDATE person_sweep_cursors
		SET optimistic_sequence = ?, reconciliation_complete = TRUE,
		    reconcile_after_key = reconcile_upper_key, last_backstop_at = ?
		WHERE person_id = ?`), latestPersonSweepSequence(t, st), now, personID)
	require.NoError(t, err)
	changed, err := result.RowsAffected()
	require.NoError(t, err)
	require.Positive(t, changed, "the person must already have sweep cursors")
}

func TestPersonSweepWorkerSchedulesBriefForCaughtUpPerson(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-caught-up")
	profile, err := f.config.Profile()
	require.NoError(err)
	sweepCursorsToHighWater(t, f.journal.store, f.journal.alicePersonID, profile, f.now)
	_, err = f.journal.store.DB().ExecContext(t.Context(), `DELETE FROM person_sweep_work`)
	require.NoError(err)
	ids := 0
	f.worker.NewID = func() string {
		ids++
		return fmt.Sprintf("scheduled-caught-up-%d", ids)
	}
	request := peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 1,
		Brief: peoplesweep.BriefModeSkip,
	}
	skipped, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	assert.Zero(skipped.PeopleAttempted)
	request.Brief = peoplesweep.BriefModeAuto
	f.worker.Config.Brief.Enabled = new(false)
	disabled, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	assert.Zero(disabled.PeopleAttempted)
	f.worker.Config.Brief.Enabled = new(true)
	result, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(result.People, 1)
	assert.Equal(1, result.People[0].BriefVersion)
}

func TestPersonSweepWorkerSchedulesBriefWhenIntervalElapsesAfterExtraction(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-interval")
	now := f.now
	f.worker.Clock = func() time.Time { return now }
	f.worker.Config.Brief.MinInterval = 2 * time.Hour
	ids := 0
	f.worker.NewID = func() string {
		ids++
		return fmt.Sprintf("scheduled-interval-%d", ids)
	}
	request := peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 1,
	}
	first, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(first.People, 1)
	require.Equal(1, first.People[0].BriefVersion)
	profile, err := f.config.Profile()
	require.NoError(err)
	sweepCursorsToHighWater(t, f.journal.store, f.journal.alicePersonID, profile, now)
	now = now.Add(time.Hour)
	message := f.journal.insertMessage(t, "scheduled-interval-new", "chat", f.journal.aliceID, now)
	addSweepBody(t, f.journal, message, "The first week went well")
	extraction, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(extraction.People, 1)
	assert.Zero(extraction.People[0].BriefVersion, "new activity is extracted before the brief interval")
	now = now.Add(time.Hour)
	due, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(due.People, 1)
	assert.Equal(2, due.People[0].BriefVersion, "elapsed interval schedules the already extracted activity")
}

func TestPersonSweepWorkerSchedulesRejectedBriefWithoutNewActivity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-rejected")
	ids := 0
	f.worker.NewID = func() string {
		ids++
		return fmt.Sprintf("scheduled-rejected-%d", ids)
	}
	request := peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 1,
	}
	first, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(first.People, 1)
	require.Equal(1, first.People[0].BriefVersion)
	profile, err := f.config.Profile()
	require.NoError(err)
	sweepCursorsToHighWater(t, f.journal.store, f.journal.alicePersonID, profile, f.now)
	_, err = f.journal.store.RejectPersonBriefContext(t.Context(), f.journal.alicePersonID,
		"The summary missed the point", f.now)
	require.NoError(err)
	regenerated, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(regenerated.People, 1)
	assert.Equal(2, regenerated.People[0].BriefVersion)
}

func TestPersonSweepWorkerDoesNotRestoreRejectedEventMemoryFromAnInFlightWindow(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "rejected-event-memory-race")
	st, personID := f.journal.store, f.journal.alicePersonID
	quote := "I am moving to Riverton on September 10."
	message := f.journal.insertMessage(t, "rejected-event-memory-race", "chat",
		f.journal.aliceID, time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC))
	addSweepBody(t, f.journal, message, quote)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		return storeEventCandidate(t, packet, quote, "2026-09-10", "their move", "How did the move go?")
	}
	f.worker.Clock = func() time.Time { return f.now }
	id := 0
	f.worker.NewID = func() string {
		id++
		return fmt.Sprintf("rejected-event-memory-race-%d", id)
	}
	request := peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: personID, Limit: 1, Brief: peoplesweep.BriefModeForce}
	first, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(first.People, 1)
	assert.Equal(1, first.People[0].BriefVersion)
	prior, err := st.GetPersonBriefContext(t.Context(), personID, 1)
	require.NoError(err)
	var priorBoundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(prior.Boundary, &priorBoundary))
	require.Len(priorBoundary.EventMemory, 1)

	f.runner.candidate = nil
	newMessage := f.journal.insertMessage(t, "rejected-event-memory-new-activity", "chat",
		f.journal.aliceID, f.now.Add(-time.Hour))
	addSweepBody(t, f.journal, newMessage, "I started a new role yesterday.")
	f.runner.afterBrief = func() {
		rejected, rejectErr := st.RejectPersonBriefContext(t.Context(), personID,
			"The event question was not wanted", f.now)
		require.NoError(rejectErr)
		assert.Equal(prior.Version, rejected.Version)
	}
	second, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(second.People, 1)
	assert.Zero(second.People[0].BriefVersion,
		"a result based on rejected event memory is not stored")
	assert.Equal(peoplesweep.FailureInvalidOutput, second.People[0].BriefFailureClass)
	require.Len(f.runner.briefPackets, 2)
	assert.Len(briefPacketEventContextEvents(t, f.runner.briefPackets[1]), 1,
		"the rejected event was present in the in-flight packet")
	rejected, err := st.GetPersonBriefContext(t.Context(), personID, 1)
	require.NoError(err)
	assert.Equal(store.PersonBriefStatusRejected, rejected.Status)
	versions, err := st.ListPersonBriefVersionsContext(t.Context(), personID, 10)
	require.NoError(err)
	require.Len(versions, 1, "the rejected version remains the only stored version")

	f.runner.afterBrief = nil
	f.now = f.now.Add(time.Hour)
	third, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(third.People, 1)
	assert.Equal(2, third.People[0].BriefVersion)
	require.Len(f.runner.briefPackets, 3)
	assert.Empty(briefPacketEventContextEvents(t, f.runner.briefPackets[2]),
		"a later retry starts without the rejected event memory")
	current, err := st.GetPersonBriefContext(t.Context(), personID, 0)
	require.NoError(err)
	var currentBoundary peoplesweep.BriefBoundary
	require.NoError(json.Unmarshal(current.Boundary, &currentBoundary))
	assert.Empty(currentBoundary.EventMemory)
}

func TestPersonSweepWorkerReconsidersBriefDeferredByBudget(t *testing.T) {
	for _, retry := range []string{"due", "forced"} {
		t.Run(retry, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newBriefWorkerEndToEndFixture(t, "scheduled-budget-"+retry)
			st, personID := f.journal.store, f.journal.alicePersonID
			f.worker.Clock = func() time.Time { return time.Now().UTC() }
			f.worker.Config.RetryBase, f.worker.Config.RetryMax = time.Hour, 4*time.Hour
			ids := 0
			f.worker.NewID = func() string {
				ids++
				return fmt.Sprintf("scheduled-budget-%s-%d", retry, ids)
			}
			f.worker.Config.Budgets.MaxRequestsPerPerson = 1
			request := peoplesweep.RunRequest{
				Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 1,
			}
			deferred, err := f.worker.Run(t.Context(), request)
			require.NoError(err)
			require.Len(deferred.People, 1)
			require.Equal(peoplesweep.FailureBudget, deferred.People[0].BriefFailureClass)
			assert.Equal(1, deferred.PeopleSucceeded, "extraction still succeeds")
			assert.Zero(deferred.People[0].BriefVersion)
			var count int
			var failure string
			var backedOff bool
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(`
				SELECT attempt_count, last_failure_class, available_at > CURRENT_TIMESTAMP
				FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&count, &failure, &backedOff))
			assert.Equal(1, count)
			assert.Equal(string(peoplesweep.FailureBudget), failure)
			assert.True(backedOff)
			profile, err := f.config.Profile()
			require.NoError(err)
			sweepCursorsToHighWater(t, st, personID, profile, time.Now().UTC())
			f.worker.Config.Budgets.MaxRequestsPerPerson = 2
			waiting, err := f.worker.Run(t.Context(), request)
			require.NoError(err)
			assert.Zero(waiting.PeopleAttempted, "reconciliation must preserve brief retry backoff")
			message := f.journal.insertMessage(t, "during-brief-backoff-"+retry, "chat", f.journal.aliceID, time.Now().UTC())
			addSweepBody(t, f.journal, message, "The new role is going well")
			waiting, err = f.worker.Run(t.Context(), request)
			require.NoError(err)
			require.Zero(waiting.PeopleAttempted, "new journal activity must not bypass brief retry backoff")
			var dirtyThrough int64
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT dirty_through_sequence FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&dirtyThrough))
			assert.Equal(latestPersonSweepSequence(t, st), dirtyThrough, "new activity remains queued")
			if retry == "forced" {
				request.Kind, request.PersonID, request.Brief = peoplesweep.RunManual, personID, peoplesweep.BriefModeForce
			} else {
				_, err := st.DB().ExecContext(t.Context(), st.Rebind(
					`UPDATE person_sweep_work SET available_at = ? WHERE person_id = ?`), time.Now().UTC().Add(-time.Hour), personID)
				require.NoError(err)
			}
			retried, err := f.worker.Run(t.Context(), request)
			require.NoError(err)
			require.Len(retried.People, 1)
			assert.Equal(1, retried.People[0].BriefVersion)
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ? AND last_failure_class <> ''`), personID).Scan(&count))
			assert.Zero(count, "successful generation clears retry failure state")
		})
	}
}

func TestPersonSweepWorkerDoesNotScheduleCaughtUpBriefWithoutEvidence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-no-evidence")
	_, err := f.journal.store.DB().ExecContext(t.Context(), f.journal.store.Rebind(
		`UPDATE sources SET source_type = 'gmail' WHERE id = ?`), f.journal.sourceID)
	require.NoError(err)
	profile, err := f.config.Profile()
	require.NoError(err)
	sweepCursorsToHighWater(t, f.journal.store, f.journal.alicePersonID, profile, f.now)
	_, err = f.journal.store.DB().ExecContext(t.Context(), `DELETE FROM person_sweep_work`)
	require.NoError(err)
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 1,
	})
	require.NoError(err)
	assert.Zero(result.PeopleAttempted, "an email-only archive cannot produce a brief")
}

func TestPersonSweepWorkerSchedulesBriefBeyondFirstTrackedPage(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-page")
	st := f.journal.store
	_, err := st.SetPersonBriefEnrollmentContext(t.Context(), f.journal.alicePersonID, false, "test-owner", false)
	require.NoError(err)
	_, err = st.SetPersonBriefEnrollmentContext(t.Context(), f.journal.bobPersonID, true, "test-owner", true)
	require.NoError(err)
	message := f.journal.insertMessage(t, "scheduled-page-bob", "chat", f.journal.bobID, f.now)
	addSweepBody(t, f.journal, message, "I finished the move")
	profile, err := f.config.Profile()
	require.NoError(err)
	for _, personID := range []int64{f.journal.alicePersonID, f.journal.bobPersonID} {
		sweepCursorsToHighWater(t, st, personID, profile, f.now)
	}
	_, err = st.DB().ExecContext(t.Context(), `DELETE FROM person_sweep_work`)
	require.NoError(err)
	f.worker.Config.WorkBatchSize = 1
	ids := 0
	f.worker.NewID = func() string {
		ids++
		return fmt.Sprintf("scheduled-page-%d", ids)
	}
	request := peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, Limit: 1,
		PersonID: f.journal.alicePersonID,
	}
	scoped, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	assert.Zero(scoped.PeopleAttempted)
	var queued int
	require.NoError(st.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM person_sweep_work`).Scan(&queued))
	assert.Zero(queued, "a person-scoped run does not publish another person's brief work")
	request.Kind, request.PersonID = peoplesweep.RunScheduled, 0
	result, err := f.worker.Run(t.Context(), request)
	require.NoError(err)
	require.Len(result.People, 1)
	assert.Equal(f.journal.bobPersonID, result.People[0].PersonID)
	assert.Equal(1, result.People[0].BriefVersion)
}

func TestPersonSweepWorkerBriefAdmissionFailureKeepsOtherExtractionWork(t *testing.T) {
	t.Run("unsupported stored memory", testPersonSweepWorkerUnsupportedMemoryKeepsOtherExtractionWork)
	assert := assert.New(t)
	require := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "scheduled-admission")
	_, err := f.journal.store.SetPersonTrackingContext(t.Context(), f.journal.bobPersonID, true)
	require.NoError(err)
	message := f.journal.insertMessage(t, "scheduled-admission-bob", "chat", f.journal.bobID, f.now)
	addSweepBody(t, f.journal, message, "I finished the move")
	f.worker.Config.Brief.MaxBytes = 1
	ids := 0
	f.worker.NewID = func() string {
		ids++
		return fmt.Sprintf("scheduled-admission-%d", ids)
	}
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunScheduled, Mode: peoplesweep.RunIncremental, Limit: 2,
	})
	require.NoError(err)
	require.Len(result.People, 2)
	for _, person := range result.People {
		assert.NotEmpty(person.CursorAdvances, "both people's extraction progress commits")
		if person.PersonID == f.journal.alicePersonID {
			assert.NotEmpty(person.BriefFailureClass, "the oversized brief is a per-person outcome")
			assert.Zero(person.BriefVersion)
		} else {
			assert.Equal(f.journal.bobPersonID, person.PersonID)
			assert.Empty(person.BriefFailureClass)
		}
	}
}

func testPersonSweepWorkerUnsupportedMemoryKeepsOtherExtractionWork(t *testing.T) {
	t.Helper()
	checks := assert.New(t)
	required := require.New(t)
	f := newBriefWorkerEndToEndFixture(t, "unsupported-memory")
	st := f.journal.store
	ids := 0
	f.worker.NewID = func() string { ids++; return fmt.Sprintf("unsupported-memory-%d", ids) }
	_, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual,
		Mode: peoplesweep.RunIncremental, PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	required.NoError(err)
	prior, err := st.GetPersonBriefContext(t.Context(), f.journal.alicePersonID, 0)
	required.NoError(err)
	var boundary map[string]any
	required.NoError(json.Unmarshal(prior.Boundary, &boundary))
	boundary["event_memory_version"] = 2
	encoded, err := json.Marshal(boundary)
	required.NoError(err)
	_, err = st.DB().ExecContext(t.Context(), st.Rebind(`UPDATE person_briefs SET boundary_json = ? WHERE id = ?`), string(encoded), prior.ID)
	required.NoError(err)
	prior, err = st.GetPersonBriefContext(t.Context(), f.journal.alicePersonID, 0)
	required.NoError(err)
	_, err = st.SetPersonTrackingContext(t.Context(), f.journal.bobPersonID, true)
	required.NoError(err)
	for _, item := range []struct {
		id          string
		participant int64
	}{
		{"unsupported-memory-alice", f.journal.aliceID}, {"unsupported-memory-bob", f.journal.bobID},
	} {
		message := f.journal.insertMessage(t, item.id, "chat", item.participant, f.now.Add(time.Hour))
		addSweepBody(t, f.journal, message, "A new update.")
	}
	beforeBrief := f.runner.briefRequests
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunScheduled,
		Mode: peoplesweep.RunIncremental, Limit: 2})
	required.NoError(err)
	required.Len(result.People, 2)
	for _, person := range result.People {
		checks.NotEmpty(person.CursorAdvances)
		if person.PersonID == f.journal.alicePersonID {
			checks.Equal(peoplesweep.FailureInternal, person.BriefFailureClass)
			checks.Zero(person.BriefVersion)
		} else {
			checks.Equal(f.journal.bobPersonID, person.PersonID)
			checks.Empty(person.BriefFailureClass)
		}
	}
	checks.Equal(beforeBrief, f.runner.briefRequests)
	current, err := st.GetPersonBriefContext(t.Context(), f.journal.alicePersonID, 0)
	required.NoError(err)
	checks.Equal(prior.Version, current.Version)
	checks.Equal(prior.Boundary, current.Boundary)
	history, err := st.ListPersonBriefVersionsContext(t.Context(), f.journal.alicePersonID, 10)
	required.NoError(err)
	required.Len(history, 1)
	checks.Equal(store.PersonBriefStatusCurrent, history[0].Status)
	checks.Equal(prior.Boundary, history[0].Boundary)
}

func TestPersonSweepWorkerDiscardsObsoleteBriefRetry(t *testing.T) {
	for _, mode := range []string{"unenrolled", "disabled"} {
		for _, newActivity := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/new_activity=%t", mode, newActivity), func(t *testing.T) {
				assert := assert.New(t)
				require := require.New(t)
				f := newBriefWorkerEndToEndFixture(t, "obsolete-brief")
				st, personID := f.journal.store, f.journal.alicePersonID
				f.worker.Clock = func() time.Time { return time.Now().UTC() }
				ids := 0
				f.worker.NewID = func() string {
					ids++
					return fmt.Sprintf("obsolete-brief-%d", ids)
				}
				f.worker.Config.Budgets.MaxRequestsPerPerson = 1
				request := peoplesweep.RunRequest{Kind: peoplesweep.RunScheduled,
					Mode: peoplesweep.RunIncremental, Limit: 1}
				first, err := f.worker.Run(t.Context(), request)
				require.NoError(err)
				require.Len(first.People, 1)
				require.Equal(peoplesweep.FailureBudget, first.People[0].BriefFailureClass)
				profile, err := f.config.Profile()
				require.NoError(err)
				sweepCursorsToHighWater(t, st, personID, profile, time.Now().UTC())
				if mode == "unenrolled" {
					_, err = st.SetPersonBriefEnrollmentContext(t.Context(), personID, false, "", false)
					require.NoError(err)
				} else {
					f.worker.Config.Brief.Enabled = new(false)
				}
				if newActivity {
					message := f.journal.insertMessage(t, "after-brief-disabled", "chat", f.journal.aliceID, time.Now().UTC())
					addSweepBody(t, f.journal, message, "The new role is going well")
				}
				_, err = st.DB().ExecContext(t.Context(), st.Rebind(
					`UPDATE person_sweep_work SET available_at = ? WHERE person_id = ?`), time.Now().UTC().Add(-time.Hour), personID)
				require.NoError(err)
				callsBefore := f.runner.extractionRequests
				result, err := f.worker.Run(t.Context(), request)
				require.NoError(err, "obsolete brief work must finish without another retry")
				assert.Equal(1, result.PeopleSucceeded)
				assert.Zero(f.runner.briefRequests)
				if newActivity {
					assert.Greater(f.runner.extractionRequests, callsBefore, "real extraction work must still run")
				} else {
					assert.Equal(callsBefore, f.runner.extractionRequests, "idle work must not call the provider")
				}
				var remaining int
				require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
					`SELECT COUNT(*) FROM person_sweep_work WHERE person_id = ?`), personID).Scan(&remaining))
				assert.Zero(remaining)
				next, err := f.worker.Run(t.Context(), request)
				require.NoError(err)
				assert.Zero(next.PeopleAttempted, "obsolete work must not recur on the next tick")
			})
		}
	}
}

func TestCompleteIdlePersonSweepKeepsNewWorkAndRequiresCurrentLease(t *testing.T) {
	for _, name := range []string{"idle", "new activity", "stale fence"} {
		t.Run(name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f := newBriefWorkerEndToEndFixture(t, "idle-completion")
			st, personID := f.journal.store, f.journal.alicePersonID
			profile, err := f.config.Profile()
			require.NoError(err)
			sweepCursorsToHighWater(t, st, personID, profile, time.Now().UTC())
			catalog, err := st.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
			require.NoError(err)
			_, err = st.EnsurePersonSweepWork(t.Context(), personID, true)
			require.NoError(err)
			lease := claimPersonSweepFixture(t, st, "idle-completion")
			switch name {
			case "new activity":
				message := f.journal.insertMessage(t, "after-idle-assembly", "chat", f.journal.aliceID, time.Now().UTC())
				addSweepBody(t, f.journal, message, "The new role is going well")
			case "stale fence":
				lease.Fence--
			}
			err = st.CompleteIdlePersonSweep(t.Context(), *lease, peoplesweep.ProgramFingerprint(), catalog.Fingerprint)
			if name == "stale fence" {
				require.ErrorIs(err, peoplesweep.ErrLeaseLost)
			} else {
				require.NoError(err)
			}
			var count int
			var owner string
			require.NoError(st.DB().QueryRowContext(t.Context(), st.Rebind(
				`SELECT COUNT(*), COALESCE(MAX(lease_owner), '') FROM person_sweep_work WHERE person_id = ?`),
				personID).Scan(&count, &owner))
			switch name {
			case "idle":
				assert.Zero(count)
			case "new activity":
				assert.Equal(1, count, "activity arriving after assembly remains queued")
				assert.Empty(owner, "new work can be claimed by the next run")
			case "stale fence":
				assert.Equal(1, count)
				assert.Equal("idle-completion", owner, "a stale worker cannot release the current claim")
			}
		})
	}
}

func TestPersonAppreciationDismissalSerializesWithSourceIDMigration(t *testing.T) {
	for _, dismissalFirst := range []bool{true, false} {
		name := "migration-first"
		if dismissalFirst {
			name = "dismissal-first"
		}
		t.Run(name, func(t *testing.T) {
			f := newBriefWorkerEndToEndFixture(t, "identity-race-"+name)
			oldID := "brief-e2e-1-identity-race-" + name
			newID := "scoped-" + oldID
			oldKey, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", oldID)
			require.NoError(t, err)
			newKey, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", newID)
			require.NoError(t, err)
			f.runner.candidate = func(packet map[string]any) json.RawMessage {
				peers := packet["appreciation_peers"].([]any)
				var anchor string
				for _, value := range peers {
					peer := value.(map[string]any)
					if peer["contribution_key"] == oldKey {
						anchor = peer["anchor_id"].(string)
					}
				}
				require.NotEmpty(t, anchor)
				return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they finished the move"},"highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"helping with the move","anchor_id":%q,"quote":"finished the move","thanked":null}],"uncertainties":[],"possible_attributes":[]}`, anchor, anchor))
			}
			_, err = f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
			require.NoError(t, err)
			profile, err := f.config.Profile()
			require.NoError(t, err)
			locked, release := make(chan struct{}), make(chan struct{})
			restore := f.journal.store.SetAttributionAfterLockHookForTest(func(sourceIDs []int64) {
				if (len(sourceIDs) == 0) == dismissalFirst {
					close(locked)
					<-release
				}
			})
			defer restore()
			dismissDone, migrateDone := make(chan error, 1), make(chan error, 1)
			dismiss := func() {
				_, err := f.journal.store.DismissPersonAppreciationContext(t.Context(), f.journal.alicePersonID, oldKey, "owner", f.now, profile)
				dismissDone <- err
			}
			migrate := func() {
				migrateDone <- f.journal.store.MigrateSourceMessageID(t.Context(), f.journal.sourceID, f.journal.conversationID, oldID, newID)
			}
			if dismissalFirst {
				go dismiss()
			} else {
				go migrate()
			}
			<-locked
			if dismissalFirst {
				go migrate()
			} else {
				go dismiss()
			}
			close(release)
			dismissErr, migrateErr := <-dismissDone, <-migrateDone
			require.NoError(t, migrateErr)
			if dismissalFirst {
				require.NoError(t, dismissErr)
			} else {
				require.ErrorIs(t, dismissErr, store.ErrPersonAppreciationNotFound)
			}
			states, err := f.journal.store.GetPersonAppreciationStatesContext(t.Context(), []string{oldKey, newKey})
			require.NoError(t, err)
			if dismissalFirst {
				require.NotNil(t, states[oldKey].DismissedAt)
				require.NotNil(t, states[newKey].DismissedAt)
				assert.Equal(t, states[oldKey].DismissedAt, states[newKey].DismissedAt)
			} else {
				assert.Empty(t, states)
			}
		})
	}
}

func TestAppreciationDismissalDoesNotAuthorizeRetryAfterPersonSplit(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "appreciation-merge-split")
	st, journal := f.journal.store, f.journal
	alice, err := st.GetPersonContext(t.Context(), journal.alicePersonID)
	require.NoError(t, err)
	bob, err := st.GetPersonContext(t.Context(), journal.bobPersonID)
	require.NoError(t, err)
	merged, err := st.MergePersonsContext(t.Context(), store.PersonMergeRequest{SurvivorID: alice.ID, AbsorbedID: bob.ID,
		ExpectedSurvivorRevision: alice.Revision, ExpectedAbsorbedRevision: bob.Revision, IdempotencyKey: "appreciation-scope-merge", Actor: "owner"})
	require.NoError(t, err)
	peerID := journal.insertMessage(t, "split-peer-help", "chat", journal.bobID, f.now.Add(-time.Hour))
	addSweepBody(t, journal, peerID, "I carried your boxes upstairs.")
	key, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", "split-peer-help")
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		var anchor string
		for _, value := range packet["appreciation_peers"].([]any) {
			peer := value.(map[string]any)
			if peer["contribution_key"] == key {
				anchor = peer["anchor_id"].(string)
			}
		}
		require.NotEmpty(t, anchor)
		return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":{"evidence_id":%q,"summary":"they helped with the move"},"highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"carrying boxes","anchor_id":%q,"quote":"carried your boxes","thanked":null}],"uncertainties":[],"possible_attributes":[]}`, anchor, anchor))
	}
	run, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: merged.Person.ID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	profile, err := f.config.Profile()
	require.NoError(t, err)
	first, err := st.DismissPersonAppreciationContext(t.Context(), merged.Person.ID, key, "owner", f.now, profile)
	require.NoError(t, err)
	current, err := st.GetPersonContext(t.Context(), merged.Person.ID)
	require.NoError(t, err)
	_, err = st.SplitPersonMergeContext(t.Context(), store.PersonSplitRequest{SourcePersonID: merged.Person.ID, MergeID: merged.Merge.ID,
		ParticipantIDs: []int64{journal.bobID}, ExpectedSourceRevision: current.Revision, IdempotencyKey: "appreciation-scope-split", Actor: "owner"})
	require.NoError(t, err)
	_, err = st.DismissPersonAppreciationContext(t.Context(), merged.Person.ID, key, "owner", f.now.Add(time.Hour), profile)
	assert.ErrorIs(t, err, store.ErrPersonAppreciationNotFound)
	states, err := st.GetPersonAppreciationStatesContext(t.Context(), []string{key})
	require.NoError(t, err)
	assert.Equal(t, first, states[key])
}

func TestPersonSweepEmptyAfterConcurrentDismissalPreservesExtraction(t *testing.T) {
	f := newBriefWorkerEndToEndFixture(t, "appreciation-empty-race")
	key, err := peoplesweep.AppreciationContributionKey("slack", "person-sweep-journal", "brief-e2e-1-appreciation-empty-race")
	require.NoError(t, err)
	f.runner.candidate = func(packet map[string]any) json.RawMessage {
		for _, value := range packet["appreciation_peers"].([]any) {
			peer := value.(map[string]any)
			if peer["contribution_key"] == key {
				return json.RawMessage(fmt.Sprintf(`{"last_meaningful_interaction":null,"highlights":[],"follow_ups":[],"appreciations":[],"thanks_suggestions":[{"text":"helping with the move","anchor_id":%q,"quote":"finished the move","thanked":null}],"uncertainties":[],"possible_attributes":[]}`, peer["anchor_id"].(string)))
			}
		}
		require.FailNow(t, "expected contribution in brief input")
		return nil
	}
	f.runner.afterBrief = func() {
		_, err := f.journal.store.RecordPersonAppreciationDismissalContext(t.Context(), key, "person-a", "owner", f.now)
		require.NoError(t, err)
	}
	run, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental,
		PersonID: f.journal.alicePersonID, Limit: 1, Brief: peoplesweep.BriefModeForce})
	require.NoError(t, err)
	require.Equal(t, 1, run.PeopleSucceeded)
	require.Len(t, run.People, 1)
	assert.Equal(t, peoplesweep.FailureInvalidOutput, run.People[0].BriefFailureClass)
	_, err = f.journal.store.GetPersonBriefContext(t.Context(), f.journal.alicePersonID, 0)
	assert.ErrorIs(t, err, store.ErrPersonBriefNotFound)
	var successfulBatches int
	require.NoError(t, f.journal.store.DB().QueryRowContext(t.Context(), f.journal.store.Rebind(`SELECT COUNT(*) FROM person_sweep_batches WHERE attempt_id = ? AND status = 'succeeded'`), run.People[0].AttemptID).Scan(&successfulBatches))
	assert.GreaterOrEqual(t, successfulBatches, 2, "paid extraction and brief calls are recorded")
	var generationID sql.NullInt64
	var inputTokens, outputTokens int
	require.NoError(t, f.journal.store.DB().QueryRowContext(t.Context(), f.journal.store.Rebind(`SELECT generation_id, input_tokens, output_tokens FROM person_sweep_attempts WHERE id = ?`), run.People[0].AttemptID).Scan(&generationID, &inputTokens, &outputTokens))
	assert.True(t, generationID.Valid, "extraction generation is committed")
	assert.GreaterOrEqual(t, inputTokens, 22)
	assert.GreaterOrEqual(t, outputTokens, 8)
	var cursor int64
	require.NoError(t, f.journal.store.DB().QueryRowContext(t.Context(), f.journal.store.Rebind(`SELECT MAX(optimistic_sequence) FROM person_sweep_cursors WHERE person_id = ?`), f.journal.alicePersonID).Scan(&cursor))
	assert.Positive(t, cursor, "extraction cursor advances")
	var retryClass string
	require.NoError(t, f.journal.store.DB().QueryRowContext(t.Context(), f.journal.store.Rebind(`SELECT last_failure_class FROM person_sweep_work WHERE person_id = ?`), f.journal.alicePersonID).Scan(&retryClass))
	assert.Equal(t, string(peoplesweep.FailureInvalidOutput), retryClass)
}
