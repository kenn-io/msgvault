package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
)

type capturingSweepRunner struct {
	requests []peoplesweep.StructuredRequest
	response peoplesweep.StructuredResponse
}

func (r *capturingSweepRunner) PrepareStructured(
	_ context.Context, request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	r.requests = append(r.requests, request)
	wire, err := json.Marshal(request)
	if err != nil {
		return peoplesweep.PreparedStructuredRequest{}, err
	}
	return peoplesweep.NewPreparedStructuredRequest(request, wire)
}

func (*capturingSweepRunner) PrepareRepair(
	peoplesweep.StructuredRequest, peoplesweep.ValidationFailure,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.PreparedStructuredRequest{}, errors.New("freshness sweep must not need repair")
}

func (r *capturingSweepRunner) BeginStructuredExecution(
	_ context.Context, _ peoplesweep.PreparedStructuredRequest,
) (peoplesweep.StructuredExecutionSession, error) {
	return capturingSweepSession{runner: r}, nil
}

func (r *capturingSweepRunner) RunPreparedStructured(
	context.Context, peoplesweep.PreparedStructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	return r.response, nil
}

func (*capturingSweepRunner) RunStructured(
	context.Context, peoplesweep.StructuredRequest,
) (peoplesweep.StructuredResponse, error) {
	return peoplesweep.StructuredResponse{}, errors.New("freshness sweep requires prepared execution")
}

type capturingSweepSession struct{ runner *capturingSweepRunner }

func (s capturingSweepSession) PrimaryCall(
	prepared peoplesweep.PreparedStructuredRequest,
) (peoplesweep.PreparedStructuredCall, error) {
	return capturingSweepCall{runner: s.runner, prepared: prepared}, nil
}

func (capturingSweepSession) SemanticValidationFailure(
	peoplesweep.StructuredResponse,
) (peoplesweep.ValidationFailure, error) {
	return peoplesweep.ValidationFailure{}, errors.New("freshness sweep must pass validation")
}

func (capturingSweepSession) PrepareRepair(
	peoplesweep.ValidationFailure,
) (peoplesweep.PreparedStructuredRequest, error) {
	return peoplesweep.PreparedStructuredRequest{}, errors.New("freshness sweep must not need repair")
}

func (capturingSweepSession) RepairCall(
	peoplesweep.PreparedStructuredRequest,
) (peoplesweep.PreparedStructuredCall, error) {
	return nil, errors.New("freshness sweep must not need repair")
}

type capturingSweepCall struct {
	runner   *capturingSweepRunner
	prepared peoplesweep.PreparedStructuredRequest
}

type emptyFreshnessContext struct{}

func (emptyFreshnessContext) RetrievePersonSweepContext(
	context.Context, peoplesweep.ContextRequest,
) ([]peoplesweep.EvidenceItem, error) {
	return nil, nil
}

type emptyProjectionSource struct{ peoplesweep.AssemblySource }

func (emptyProjectionSource) LoadPersonFactState(
	context.Context, int64, personfacts.Catalog,
) (peoplesweep.PersonFactState, error) {
	return peoplesweep.PersonFactState{}, nil
}

func (c capturingSweepCall) Execute(
	ctx context.Context, markStarted func(context.Context) error,
) (peoplesweep.StructuredResponse, error) {
	if err := markStarted(ctx); err != nil {
		return peoplesweep.StructuredResponse{}, err
	}
	return c.runner.RunPreparedStructured(ctx, c.prepared)
}

func packetFromRequest(t *testing.T, request peoplesweep.StructuredRequest) peoplesweep.EvidencePacket {
	t.Helper()
	_, packetJSON, found := strings.Cut(request.InputText, "\n\nEvidence packet JSON:\n")
	require.True(t, found, "extraction request must contain its packet")
	// EvidencePacket has no wire JSON tags. Decode the provider's field name
	// explicitly so a missing projection cannot silently satisfy the test.
	var wire struct {
		CurrentProjection []peoplesweep.ProjectedValue `json:"current_projection"`
	}
	require.NoError(t, json.Unmarshal([]byte(packetJSON), &wire))
	require.NotNil(t, wire.CurrentProjection, "wire packet must carry current_projection")
	return peoplesweep.EvidencePacket{CurrentProjection: wire.CurrentProjection}
}

func projectedValues(t *testing.T, projection []peoplesweep.ProjectedValue) []string {
	t.Helper()
	values := make([]string, 0, len(projection))
	for _, projected := range projection {
		var value string
		require.NoError(t, json.Unmarshal(projected.Value, &value))
		values = append(values, value)
	}
	return values
}

func newProjectionFreshnessSweep(t *testing.T) (personSweepJournalFixture, peoplesweep.Worker, personfacts.Catalog) {
	t.Helper()
	f := newPersonSweepJournalFixture(t, true, false)
	now := time.Now().UTC()
	messageID := f.insertMessage(t, "M1", "chat", f.aliceID, now.Add(-time.Hour))
	addSweepBody(t, f, messageID, "I live in Lisbon")
	config := peoplesweep.Config{Enabled: true,
		Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1",
			Model: "gpt-test", Auth: peoplesweep.AuthBearer,
			Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY",
			OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
			RetentionPosture: "zero_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			SourceSince:    "2025-01-01", AllowSensitive: true, RequestTimeout: time.Second,
		}}}
	config.ApplyDefaults()
	require.NoError(t, config.Validate())
	profile, err := config.Profile()
	require.NoError(t, err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(t, err)
	_, _, err = f.store.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test-owner")
	require.NoError(t, err)
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), profile.AllowSensitive)
	require.NoError(t, err)
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.alicePersonID, []int64{messageID}, 0)
	require.NoError(t, err)
	require.Len(t, items, 1)
	packetJSON, err := peoplesweep.CanonicalPacketJSON(peoplesweep.EvidencePacket{
		PersonID: f.alicePersonID, Catalog: catalog, Seeds: items,
		ProgramID: peoplesweep.ExtractionProgramID, ProgramVersion: peoplesweep.ExtractionProgramVersion,
	})
	require.NoError(t, err)
	var wire struct {
		Seeds []struct {
			ID string `json:"id"`
		} `json:"seeds"`
	}
	require.NoError(t, json.Unmarshal(packetJSON, &wire))
	require.Len(t, wire.Seeds, 1)
	var targetKey string
	for _, target := range catalog.Targets {
		if target.Slug == "location" {
			targetKey = target.Key
		}
	}
	require.NotEmpty(t, targetKey)
	runner := emptyFreshnessRunner()
	runner.response.Output = []byte(fmt.Sprintf(`{"claims":[{"target_key":%q,"relation":"support","value":"Lisbon","evidence_ids":[%q],"valid_from":null,"valid_until":null,"confidence_basis_points":1000}]}`, targetKey, wire.Seeds[0].ID))
	serial := 0
	worker := peoplesweep.Worker{Config: config, Store: f.store, Source: f.store,
		Context: peoplesweep.NewContextRetriever(f.store), Sink: f.store, Runner: runner,
		Catalog: f.store, Clock: func() time.Time { return now },
		NewID:    func() string { serial++; return fmt.Sprintf("freshness-%d", serial) },
		WorkerID: "freshness-worker"}
	runFreshnessSweep(t, worker, f.alicePersonID)
	require.Len(t, runner.requests, 1)
	state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
	require.NoError(t, err)
	require.Contains(t, projectedValues(t, state.Current), "Lisbon", "first real sweep must project the seeded claim")
	return f, worker, catalog
}

func emptyFreshnessRunner() *capturingSweepRunner {
	return &capturingSweepRunner{response: peoplesweep.StructuredResponse{
		Output: []byte(`{"claims":[]}`), ProviderVersion: "provider-v1", ModelVersion: "model-v1",
		Usage: peoplesweep.TokenUsage{InputTokens: 11, OutputTokens: 4}, UsageKnown: true,
	}}
}

func runFreshnessSweep(t *testing.T, worker peoplesweep.Worker, personID int64) peoplesweep.RunResult {
	t.Helper()
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: personID, Limit: 1,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.PeopleSucceeded, "%+v", result)
	return result
}

func TestPersonSweepEvidenceAlignmentAcceptsPortableTimestampPrecision(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newPersonSweepJournalFixture(t, true, false)
	messageID := f.insertMessage(t, "M1", "chat", f.aliceID,
		time.Date(2026, time.October, 24, 13, 24, 46, 736881547, time.UTC))
	addSweepBody(t, f, messageID, "I live in Lisbon")
	window := loadSweepWindow(t, f, peoplesweep.SourceConversationText, 0)
	require.Len(window.Seeds, 1)
	item := window.Seeds[0]
	sourceRef, err := peoplesweep.EncodePersonSweepEvidenceRef(item.Ref)
	require.NoError(err)
	spanStart, spanEnd := int64(item.Ref.SpanStart), int64(item.Ref.SpanEnd)
	input := personfacts.EvidenceInput{
		PersonID: f.alicePersonID, SourceClass: personfacts.EvidenceArchive,
		Directness: item.Directness, Authority: item.Authority,
		SourceRef: sourceRef, SubjectPersonID: item.SubjectPersonID,
		SpanStart: &spanStart, SpanEnd: &spanEnd, Excerpt: item.Excerpt,
		ContentSHA256: item.ContentSHA256, SourceVersion: item.SourceVersion,
		EventTime:     item.EventTime.Truncate(time.Microsecond),
		RecordedTime:  item.RecordedTime.Truncate(time.Microsecond),
		IdentityScore: item.IdentityBasisPoints,
	}
	alignment, err := (store.PersonSweepEvidenceAligner{Store: f.store}).Align(t.Context(), input)
	require.NoError(err)
	assert.True(alignment.Accepted, "%+v", alignment)
}

func TestPersonSweepProviderPacketExcludesValueRetractedThisRun(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	messageID := f.insertMessage(t, "M2", "chat", f.aliceID, worker.Clock().Add(-time.Minute))
	addSweepBody(t, f, messageID, "I started a new project")
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	result := runFreshnessSweep(t, worker, f.alicePersonID)
	assert.Equal(1, result.ProjectedWrites, "include the status-phase retirement")
	assertFreshnessAttemptCounts(t, f, result.People[0].AttemptID, 1, 1)
	require.Len(runner.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
	state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
	require.NoError(err)
	assert.NotContains(projectedValues(t, state.Current), "Lisbon")
}

func TestPersonSweepRetractionRunsBeforeEvidencePacketSizeValidation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	messageID := f.insertMessage(t, "M2", "chat", f.aliceID, worker.Clock().Add(-time.Minute))
	addSweepBody(t, f, messageID, "I started a new project")
	profile, err := worker.Config.Profile()
	require.NoError(err)
	keys := make([]peoplesweep.CursorKey, 0, len(profile.AllowedSources))
	for _, lane := range profile.AllowedSources {
		keys = append(keys, peoplesweep.CursorKey{PersonID: f.alicePersonID, SourceLane: lane,
			ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint})
	}
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), keys)
	require.NoError(err)
	request := peoplesweep.AssemblyRequest{PersonID: f.alicePersonID, Cursors: cursors, Catalog: catalog,
		Profile: profile, Now: worker.Clock(), BackstopInterval: worker.Config.BackstopInterval}
	assembler := peoplesweep.Assembler{Source: f.store, MaxBytes: 1 << 20,
		MaxItems: worker.Config.EvidenceMaxItems, WindowLimit: worker.Config.ChangeBatchSize}
	progress, err := assembler.Discover(t.Context(), request)
	require.NoError(err)
	require.Len(progress.Seeds, 1)
	fresh, err := (peoplesweep.Assembler{Source: emptyProjectionSource{AssemblySource: f.store},
		MaxBytes: 1 << 20, MaxItems: worker.Config.EvidenceMaxItems,
		WindowLimit: worker.Config.ChangeBatchSize}).BuildPacket(t.Context(), request, progress)
	require.NoError(err)
	require.Len(fresh.Batches, 1)
	freshInputBytes := len(fresh.Batches[0].Request.InputText)
	stale, err := assembler.BuildPacket(t.Context(), request, progress)
	require.NoError(err)
	require.Len(stale.Batches, 1)
	require.Greater(len(stale.Batches[0].Request.InputText), freshInputBytes)

	// The new packet fits. The stale projection makes this seed too large until
	// the worker commits the source retraction and reloads state.
	worker.Config.EvidenceMaxBytes = freshInputBytes
	worker.Context = emptyFreshnessContext{}
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.NoError(err)
	assert.Equal(1, result.PeopleSucceeded)
	require.Len(runner.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
}

func TestPersonSweepRetractionStillSelectsABudgetedSourceLane(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	providerConfig := worker.Config.Providers["default"]
	providerConfig.AllowedSources = []peoplesweep.SourceClass{
		peoplesweep.SourceConversationText, peoplesweep.SourceMeetingText,
	}
	worker.Config.Providers["default"] = providerConfig
	worker.Config.EvidenceMaxItems = 1
	worker.Config.Budgets.MaxRequestsPerPerson = 1
	profile, err := worker.Config.Profile()
	require.NoError(err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), profile)
	require.NoError(err)
	_, _, err = f.store.GrantPersonInferenceConsent(t.Context(), profile.Fingerprint, "test-owner")
	require.NoError(err)

	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	messageID := f.insertMessage(t, "M2", "chat", f.aliceID, worker.Clock().Add(-time.Minute))
	addSweepBody(t, f, messageID, "I started a new project")
	meetingID := f.insertMessage(t, "M3", "meeting_transcript", f.aliceID, worker.Clock().Add(-30*time.Second))
	addSweepBody(t, f, meetingID, "I will review the new proposal")
	worker.Context = emptyFreshnessContext{}

	keys := make([]peoplesweep.CursorKey, 0, len(profile.AllowedSources))
	for _, lane := range profile.AllowedSources {
		keys = append(keys, peoplesweep.CursorKey{PersonID: f.alicePersonID, SourceLane: lane,
			ProgramFingerprint: peoplesweep.ProgramFingerprint(), CatalogFingerprint: catalog.Fingerprint})
	}
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), keys)
	require.NoError(err)
	request := peoplesweep.AssemblyRequest{PersonID: f.alicePersonID, Cursors: cursors, Catalog: catalog,
		Profile: profile, Now: worker.Clock(), BackstopInterval: worker.Config.BackstopInterval}
	assembler := peoplesweep.Assembler{Source: f.store, Context: worker.Context,
		MaxBytes: worker.Config.EvidenceMaxBytes, MaxItems: worker.Config.EvidenceMaxItems,
		WindowLimit: worker.Config.ChangeBatchSize}
	progress, err := assembler.Discover(t.Context(), request)
	require.NoError(err)
	require.Len(progress.Seeds, 2)
	combined, err := assembler.BuildPacket(t.Context(), request, progress)
	require.NoError(err)
	require.Len(combined.Batches, 2)
	for _, cursor := range cursors {
		laneRequest := request
		laneRequest.Cursors = []peoplesweep.Cursor{cursor}
		laneProgress, discoverErr := assembler.Discover(t.Context(), laneRequest)
		require.NoError(discoverErr)
		laneAssembly, buildErr := assembler.BuildPacket(t.Context(), laneRequest, laneProgress)
		require.NoError(buildErr)
		require.Len(laneAssembly.Batches, 1)
	}

	runner := emptyFreshnessRunner()
	worker.Runner = runner
	result := runFreshnessSweep(t, worker, f.alicePersonID)
	require.Len(runner.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
	var envelopeJSON []byte
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT cursor_envelope_json FROM person_sweep_attempts WHERE id = ?`),
		result.People[0].AttemptID).Scan(&envelopeJSON))
	var envelope []peoplesweep.GenerationCursor
	require.NoError(json.Unmarshal(envelopeJSON, &envelope))
	assert.Len(envelope, 1, "the extraction attempt must bind only the selected source lane")
}

func TestPersonSweepBatchNarrowingFailureAfterStatusCommitFinalizesAttempt(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, _ := newProjectionFreshnessSweep(t)
	worker.Config.EvidenceMaxItems = 1
	worker.Config.Budgets.MaxRequestsPerPerson = 1
	worker.Context = emptyFreshnessContext{}
	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	for _, message := range []struct {
		id   string
		body string
		at   time.Duration
	}{
		{id: "M2", body: "I started a new project", at: -time.Minute},
		{id: "M3", body: "I changed teams", at: -30 * time.Second},
	} {
		messageID := f.insertMessage(t, message.id, "chat", f.aliceID,
			worker.Clock().Add(message.at))
		addSweepBody(t, f, messageID, message.body)
	}
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.ErrorContains(err, "person sweep assembly exceeded its request batch limit")
	assert.Empty(runner.requests)
	total, statuses, events := freshnessGenerationCounts(t, f)
	assert.Equal(2, total, "the status-only retraction commits before bounded packet selection")
	assert.Equal(1, statuses)
	assert.Equal(1, events)
	var runStatus, attemptStatus string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT r.status, a.status FROM person_sweep_runs r
		JOIN person_sweep_attempts a ON a.run_id = r.id WHERE r.id = ?`),
		result.RunID).Scan(&runStatus, &attemptStatus))
	assert.Equal(string(peoplesweep.RunFailed), runStatus)
	assert.Equal(string(peoplesweep.AttemptFailed), attemptStatus)
}

func TestPersonSweepDeletionOnlyRunMakesNoProviderCall(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	before := latestPersonSweepSequence(t, f.store)
	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	through := latestPersonSweepSequence(t, f.store)
	require.Greater(through, before)
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	runFreshnessSweep(t, worker, f.alicePersonID)
	assert.Empty(runner.requests)
	var generations, claims, events int
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&generations))
	assert.Equal(1, generations)
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_claims c WHERE c.person_id = ? AND EXISTS (
			SELECT 1 FROM person_fact_generations g WHERE g.id = c.generation_id AND g.provider = ?)`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&claims))
	assert.Zero(claims)
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_evidence_status_events e WHERE EXISTS (
			SELECT 1 FROM person_fact_generations g WHERE g.id = e.generation_id AND g.person_id = ? AND g.provider = ?)`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&events))
	assert.Equal(1, events)
	var sequence int64
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors WHERE person_id = ? AND source_lane = ?
		AND program_fingerprint = ? AND catalog_fingerprint = ?`), f.alicePersonID,
		peoplesweep.SourceConversationText, peoplesweep.ProgramFingerprint(), catalog.Fingerprint).Scan(&sequence))
	assert.Equal(through, sequence)
	state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
	require.NoError(err)
	assert.NotContains(projectedValues(t, state.Current), "Lisbon")
}

func TestPersonSweepDeletionAndBriefWithoutExtractionCommitsOneGeneration(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, _ := newProjectionFreshnessSweep(t)
	messageID := f.insertMessage(t, "M2", "chat", f.aliceID, worker.Clock().Add(-time.Minute))
	addSweepBody(t, f, messageID, "I started a new project")
	worker.Runner = emptyFreshnessRunner()
	runFreshnessSweep(t, worker, f.alicePersonID)
	before, _, _ := freshnessGenerationCounts(t, f)
	_, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.alicePersonID, true, "test-owner", false)
	require.NoError(err)
	require.NoError(f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	runner := &scriptedBriefRunner{t: t}
	worker.Runner, worker.Brief, worker.Archive = runner, f.store, f.store
	result := runFreshnessSweep(t, worker, f.alicePersonID)
	assert.Equal(1, runner.briefRequests)
	assert.Zero(runner.extractionRequests)
	require.Len(result.People, 1)
	assert.Positive(result.People[0].BriefVersion)
	after, _, _ := freshnessGenerationCounts(t, f)
	assert.Equal(before+1, after, "brief-only work needs no preliminary status generation")
}

// The source still reads the real archive. It transfers the lease only after
// a reload can observe the committed status generation, at the phase boundary.
type leaseLosingFreshnessSource struct{ *store.Store }

func (s leaseLosingFreshnessSource) LoadPersonFactState(
	ctx context.Context, personID int64, catalog personfacts.Catalog,
) (peoplesweep.PersonFactState, error) {
	state, err := s.Store.LoadPersonFactState(ctx, personID, catalog)
	if err != nil {
		return state, err
	}
	var statuses int
	if err := s.DB().QueryRowContext(ctx, s.Rebind(`
		SELECT COUNT(*) FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		personID, peoplesweep.StatusOnlyProvider).Scan(&statuses); err != nil {
		return state, err
	}
	if statuses > 0 {
		_, err = s.DB().ExecContext(ctx, s.Rebind(`
			UPDATE person_sweep_work SET lease_fence = lease_fence + 1,
			lease_owner = 'successor-worker' WHERE person_id = ?`), personID)
	}
	return state, err
}

type failingFreshnessRunner struct{ *capturingSweepRunner }

func (r failingFreshnessRunner) BeginStructuredExecution(
	context.Context, peoplesweep.PreparedStructuredRequest,
) (peoplesweep.StructuredExecutionSession, error) {
	return failingFreshnessSession{}, nil
}

type failingFreshnessSession struct{ capturingSweepSession }

func (failingFreshnessSession) PrimaryCall(
	peoplesweep.PreparedStructuredRequest,
) (peoplesweep.PreparedStructuredCall, error) {
	return failingFreshnessCall{}, nil
}

type failingFreshnessCall struct{}

var errFreshnessProvider = errors.New("synthetic provider failure after status commit")

func (failingFreshnessCall) Execute(
	ctx context.Context, markStarted func(context.Context) error,
) (peoplesweep.StructuredResponse, error) {
	if err := markStarted(ctx); err != nil {
		return peoplesweep.StructuredResponse{}, err
	}
	return peoplesweep.StructuredResponse{}, errFreshnessProvider
}

func addFreshnessRetractionAndMessage(t *testing.T, f personSweepJournalFixture, now time.Time) int64 {
	t.Helper()
	require.NoError(t, f.store.MarkMessageDeletedBySourceMessageID(f.sourceID, false, "M1"))
	messageID := f.insertMessage(t, "M2", "chat", f.aliceID, now.Add(-time.Minute))
	addSweepBody(t, f, messageID, "I started a new project")
	return latestPersonSweepSequence(t, f.store)
}

func freshnessGenerationCounts(t *testing.T, f personSweepJournalFixture) (int, int, int) {
	t.Helper()
	var total, statuses, events int
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN provider = ? THEN 1 ELSE 0 END), 0)
		FROM person_fact_generations WHERE person_id = ?`),
		peoplesweep.StatusOnlyProvider, f.alicePersonID).Scan(&total, &statuses))
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT COUNT(*) FROM person_fact_evidence_status_events e WHERE EXISTS (
			SELECT 1 FROM person_fact_generations g WHERE g.id = e.generation_id
			AND g.person_id = ? AND g.provider = ?)`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&events))
	return total, statuses, events
}

func freshnessCursorSequence(t *testing.T, f personSweepJournalFixture, catalog personfacts.Catalog) int64 {
	t.Helper()
	var sequence int64
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT optimistic_sequence FROM person_sweep_cursors WHERE person_id = ? AND source_lane = ?
		AND program_fingerprint = ? AND catalog_fingerprint = ?`), f.alicePersonID,
		peoplesweep.SourceConversationText, peoplesweep.ProgramFingerprint(), catalog.Fingerprint).Scan(&sequence))
	return sequence
}

func TestPersonSweepTwoPhaseApplyLeaseLossAbortsBeforeProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	before := freshnessCursorSequence(t, f, catalog)
	addFreshnessRetractionAndMessage(t, f, worker.Clock())
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	worker.Source = leaseLosingFreshnessSource{Store: f.store}
	_, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.ErrorIs(err, peoplesweep.ErrLeaseLost)
	assert.Empty(runner.requests)
	total, statuses, events := freshnessGenerationCounts(t, f)
	assert.Equal(2, total)
	assert.Equal(1, statuses)
	assert.Equal(1, events)
	assert.Equal(before, freshnessCursorSequence(t, f, catalog), "status phase must not advance cursors")
	state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
	require.NoError(err)
	assert.NotContains(projectedValues(t, state.Current), "Lisbon")
	var owner string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT lease_owner FROM person_sweep_work WHERE person_id = ?`), f.alicePersonID).Scan(&owner))
	assert.Equal("successor-worker", owner, "stale cleanup must leave successor work intact")
}

func TestPersonSweepTwoPhaseApplyFailureAfterStatusCommitRetries(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	before := freshnessCursorSequence(t, f, catalog)
	through := addFreshnessRetractionAndMessage(t, f, worker.Clock())
	failed := emptyFreshnessRunner()
	worker.Runner = failingFreshnessRunner{capturingSweepRunner: failed}
	_, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.ErrorIs(err, errFreshnessProvider)
	require.Len(failed.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, failed.requests[0]).CurrentProjection), "Lisbon")
	total, statuses, events := freshnessGenerationCounts(t, f)
	assert.Equal(2, total)
	assert.Equal(1, statuses)
	assert.Equal(1, events)
	assert.Equal(before, freshnessCursorSequence(t, f, catalog))
	var statusKey string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT generation_key FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&statusKey))
	var failedAttemptID string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT id FROM person_sweep_attempts WHERE person_id = ? AND status = 'failed'`),
		f.alicePersonID).Scan(&failedAttemptID))
	assertFreshnessAttemptCounts(t, f, failedAttemptID, 1, 1)
	// Make the persisted retry due without waiting for a real database clock.
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
		UPDATE person_sweep_work SET available_at = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), f.alicePersonID)
	require.NoError(err)
	previousClock := worker.Clock()
	worker.Clock = func() time.Time { return previousClock.Add(time.Minute) }
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	result := runFreshnessSweep(t, worker, f.alicePersonID)
	assert.Zero(result.ProjectedWrites, "replay must not count the retirement twice")
	assertFreshnessAttemptCounts(t, f, result.People[0].AttemptID, 0, 0)
	require.Len(runner.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
	total, statuses, events = freshnessGenerationCounts(t, f)
	assert.Equal(3, total, "retry must reuse the status generation")
	assert.Equal(1, statuses)
	assert.Equal(1, events, "retry must not duplicate status events")
	var replayKey string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT generation_key FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&replayKey))
	assert.Equal(statusKey, replayKey)
	assert.Equal(through, freshnessCursorSequence(t, f, catalog))
}

func TestPersonSweepRetractionWithProviderCallCommitsTwoGenerations(t *testing.T) {
	for _, hasCurrent := range []bool{true, false} {
		t.Run(fmt.Sprintf("current_projection_%t", hasCurrent), func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			f, worker, catalog := newProjectionFreshnessSweep(t)
			if !hasCurrent {
				_, err := f.store.SupersedePersonAttributeValueContext(t.Context(), store.PersonAttributeSupersedeInput{
					PersonID: f.alicePersonID, DefinitionSlug: "location",
				})
				require.NoError(err)
				state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
				require.NoError(err)
				require.NotContains(projectedValues(t, state.Current), "Lisbon")
			}
			through := addFreshnessRetractionAndMessage(t, f, worker.Clock())
			runner := emptyFreshnessRunner()
			worker.Runner = runner
			runFreshnessSweep(t, worker, f.alicePersonID)
			require.Len(runner.requests, 1)
			assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
			total, statuses, events := freshnessGenerationCounts(t, f)
			assert.Equal(3, total, "one seed generation, then status and extraction generations")
			assert.Equal(1, statuses)
			assert.Equal(1, events)
			assert.Equal(through, freshnessCursorSequence(t, f, catalog))
			var extractionStatuses int
			require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT COUNT(*) FROM person_fact_evidence_status_events e WHERE EXISTS (
					SELECT 1 FROM person_fact_generations g WHERE g.id = e.generation_id
					AND g.person_id = ? AND g.provider <> ?)`),
				f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&extractionStatuses))
			assert.Zero(extractionStatuses, "final extraction must exclude already applied status changes")
		})
	}
}

type hookedFreshnessSink struct {
	*store.Store

	before func()
	after  func()
}

func (s hookedFreshnessSink) ApplyPersonSweepStatusOnly(
	ctx context.Context, request peoplesweep.StatusOnlyApplyRequest,
) (peoplesweep.Lease, error) {
	if s.before != nil {
		s.before()
	}
	lease, err := s.Store.ApplyPersonSweepStatusOnly(ctx, request)
	if err == nil && s.after != nil {
		s.after()
	}
	return lease, err
}

func TestPersonSweepTwoPhaseApplyStaleFenceCommitsNothing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	before := freshnessCursorSequence(t, f, catalog)
	addFreshnessRetractionAndMessage(t, f, worker.Clock())
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	worker.Sink = hookedFreshnessSink{Store: f.store, before: func() {
		_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(`
			UPDATE person_sweep_work SET lease_fence = lease_fence + 1,
			lease_owner = 'successor-worker' WHERE person_id = ?`), f.alicePersonID)
		require.NoError(err)
	}}
	_, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.ErrorIs(err, peoplesweep.ErrLeaseLost)
	assert.Empty(runner.requests)
	total, statuses, events := freshnessGenerationCounts(t, f)
	assert.Equal(1, total)
	assert.Zero(statuses)
	assert.Zero(events)
	assert.Equal(before, freshnessCursorSequence(t, f, catalog))
	state, err := f.store.LoadPersonFactState(t.Context(), f.alicePersonID, catalog)
	require.NoError(err)
	assert.Contains(projectedValues(t, state.Current), "Lisbon")
}

func TestPersonSweepTwoPhaseApplyChangedRangeDefersProvider(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, catalog := newProjectionFreshnessSweep(t)
	before := freshnessCursorSequence(t, f, catalog)
	addFreshnessRetractionAndMessage(t, f, worker.Clock())
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	worker.Sink = hookedFreshnessSink{Store: f.store, after: func() {
		messageID := f.insertMessage(t, "M3", "chat", f.aliceID, worker.Clock())
		addSweepBody(t, f, messageID, "I have another update")
	}}
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID, Limit: 1,
	})
	require.NoError(err, "a new journal range is deferred without failing a manual sweep")
	assert.Empty(runner.requests)
	total, statuses, events := freshnessGenerationCounts(t, f)
	assert.Equal(2, total)
	assert.Equal(1, statuses)
	assert.Equal(1, events)
	assert.Equal(before, freshnessCursorSequence(t, f, catalog))
	var queued bool
	var attempts, failures int
	var failureClass, runStatus, attemptStatus string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT lease_owner = '' AND lease_until IS NULL AND available_at <= CURRENT_TIMESTAMP,
		attempt_count, last_failure_class FROM person_sweep_work WHERE person_id = ?`),
		f.alicePersonID).Scan(&queued, &attempts, &failureClass))
	assert.True(queued, "new journal work remains immediately claimable")
	assert.Zero(attempts, "a benign race must not increase the retry counter")
	assert.Empty(failureClass)
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT r.status, r.failure_count, a.status FROM person_sweep_runs r
		JOIN person_sweep_attempts a ON a.run_id = r.id WHERE r.id = ?`),
		result.RunID).Scan(&runStatus, &failures, &attemptStatus))
	assert.Equal(string(peoplesweep.RunSucceeded), runStatus)
	assert.Zero(failures)
	assert.Equal(string(peoplesweep.AttemptCancelled), attemptStatus)
	var statusKey string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT generation_key FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&statusKey))
	worker.Sink = f.store
	retry := runFreshnessSweep(t, worker, f.alicePersonID)
	assertFreshnessAttemptCounts(t, f, retry.People[0].AttemptID, 0, 0)
	require.Len(runner.requests, 1)
	assert.NotContains(projectedValues(t, packetFromRequest(t, runner.requests[0]).CurrentProjection), "Lisbon")
	total, statuses, events = freshnessGenerationCounts(t, f)
	assert.Equal(3, total, "the changed-range retry reuses the committed status generation")
	assert.Equal(1, statuses)
	assert.Equal(1, events)
	var retryKey string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT generation_key FROM person_fact_generations WHERE person_id = ? AND provider = ?`),
		f.alicePersonID, peoplesweep.StatusOnlyProvider).Scan(&retryKey))
	assert.Equal(statusKey, retryKey)
	assert.Equal(latestPersonSweepSequence(t, f.store), freshnessCursorSequence(t, f, catalog))
}

func TestPersonSweepForcedBriefChangedRangeReturnsDeferredError(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, _ := newProjectionFreshnessSweep(t)
	worker.Config.Brief.Enabled = new(true)
	_, err := f.store.SetPersonBriefEnrollmentContext(t.Context(), f.alicePersonID, true, "test-owner", false)
	require.NoError(err)
	addFreshnessRetractionAndMessage(t, f, worker.Clock())
	runner := &scriptedBriefRunner{t: t}
	worker.Runner, worker.Brief, worker.Archive = runner, f.store, f.store
	worker.Sink = hookedFreshnessSink{Store: f.store, after: func() {
		messageID := f.insertMessage(t, "M3", "chat", f.aliceID, worker.Clock())
		addSweepBody(t, f, messageID, "I have another update")
	}}
	_, err = worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: f.alicePersonID,
		Limit: 1, Brief: peoplesweep.BriefModeForce,
	})
	require.ErrorIs(err, peoplesweep.ErrPersonSweepDeferred)
	assert.Empty(runner.briefRequests, "a deferred forced brief must not report success without generating a version")
	assert.Zero(runner.extractionRequests, "the changed range must not reach the provider")
}

func TestPersonSweepDeferralContinuesWithOtherPeople(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f, worker, _ := newProjectionFreshnessSweep(t)
	_, err := f.store.SetPersonTrackingContext(t.Context(), f.bobPersonID, true)
	require.NoError(err)
	bobMessageID := f.insertMessage(t, "B1", "chat", f.bobID, worker.Clock().Add(-time.Minute))
	addSweepBody(t, f, bobMessageID, "I will review the deployment plan")
	addFreshnessRetractionAndMessage(t, f, worker.Clock())
	runner := emptyFreshnessRunner()
	worker.Runner = runner
	worker.Sink = hookedFreshnessSink{Store: f.store, after: func() {
		messageID := f.insertMessage(t, "M3", "chat", f.aliceID, worker.Clock())
		addSweepBody(t, f, messageID, "The archive changed during the sweep")
	}}
	result, err := worker.Run(t.Context(), peoplesweep.RunRequest{
		Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, Limit: 2,
	})
	require.NoError(err)
	assert.Equal(2, result.PeopleAttempted)
	assert.Equal(1, result.PeopleSucceeded)
	require.Len(result.People, 1)
	assert.Equal(f.bobPersonID, result.People[0].PersonID)
	assert.Len(runner.requests, 1, "only the unaffected person's packet should reach the provider")
	var status string
	require.NoError(f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(
		`SELECT status FROM person_sweep_runs WHERE id = ?`), result.RunID).Scan(&status))
	assert.Equal(string(peoplesweep.RunPartial), status)
}

func assertFreshnessAttemptCounts(t *testing.T, f personSweepJournalFixture, attemptID string, decisions, writes int) {
	t.Helper()
	var gotDecisions, gotWrites, runWrites int
	require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
		SELECT a.decision_count, a.projected_write_count, r.projected_write_count
		FROM person_sweep_attempts a JOIN person_sweep_runs r ON r.id = a.run_id
		WHERE a.id = ?`), attemptID).Scan(&gotDecisions, &gotWrites, &runWrites))
	assert.Equal(t, decisions, gotDecisions)
	assert.Equal(t, writes, gotWrites)
	assert.Equal(t, writes, runWrites)
}
