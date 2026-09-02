package store_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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
	parityInputTokens  int64 = 50_000
	parityOutputTokens int64 = 8_000
	parityCostMicroUSD int64 = 148_000
)

type personSweepParityFixture struct {
	store          *store.Store
	server         *httptest.Server
	provider       *parityProvider
	worker         peoplesweep.Worker
	sourceRecorder *paritySourceRecorder
	sinkRecorder   *paritySinkRecorder
	config         peoplesweep.Config
	profile        peoplesweep.ProviderProfile
	catalog        personfacts.Catalog
	target         personfacts.TargetDescriptor
	personID       int64
	aliceID        int64
	aliasID        int64
	sourceID       int64
	conversationID int64
	messageID      int64
	key            peoplesweep.CursorKey
	initialItem    peoplesweep.EvidenceItem
	editedItem     peoplesweep.EvidenceItem
	now            time.Time
	nextID         int
}

type parityPacket struct {
	Catalog struct {
		Targets []struct {
			Key  string `json:"key"`
			Slug string `json:"slug"`
		} `json:"targets"`
	} `json:"catalog"`
	Seeds []struct {
		ID string `json:"id"`
	} `json:"seeds"`
}

type parityProvider struct {
	t       *testing.T
	mu      sync.Mutex
	calls   int
	revoke  func(int)
	packets []parityPacket
	wires   [][]byte
}

type parityRewriteTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (transport parityRewriteTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	cloned.URL.Scheme = transport.target.Scheme
	cloned.URL.Host = transport.target.Host
	return transport.base.RoundTrip(cloned)
}

func (p *parityProvider) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	raw, err := io.ReadAll(request.Body)
	if !assert.NoError(p.t, err) {
		http.Error(w, "invalid synthetic request", http.StatusBadRequest)
		return
	}
	assert.Equal(p.t, http.MethodPost, request.Method)
	assert.Equal(p.t, "/v1/chat/completions", request.URL.Path)
	assert.Equal(p.t, "Bearer synthetic-parity-key", request.Header.Get("Authorization"))
	var wire struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if !assert.NoError(p.t, json.Unmarshal(raw, &wire)) ||
		!assert.Len(p.t, wire.Messages, 2) {
		http.Error(w, "invalid synthetic wire", http.StatusBadRequest)
		return
	}
	const marker = "Evidence packet JSON:\n"
	markerAt := strings.Index(wire.Messages[1].Content, marker)
	if !assert.NotEqual(p.t, -1, markerAt) {
		http.Error(w, "missing synthetic packet", http.StatusBadRequest)
		return
	}
	var packet parityPacket
	if !assert.NoError(p.t, json.Unmarshal(
		[]byte(wire.Messages[1].Content[markerAt+len(marker):]), &packet)) {
		http.Error(w, "invalid synthetic packet", http.StatusBadRequest)
		return
	}
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.packets = append(p.packets, packet)
	p.wires = append(p.wires, append([]byte(nil), raw...))
	p.mu.Unlock()
	if p.revoke != nil {
		p.revoke(call)
	}
	claims := []any{}
	if call == 2 || call == 4 {
		if !assert.NotEmpty(p.t, packet.Seeds) {
			http.Error(w, "missing synthetic seed", http.StatusBadRequest)
			return
		}
		targetKey := ""
		for _, candidate := range packet.Catalog.Targets {
			if candidate.Slug == store.AttributeSlugPrimaryChannel {
				targetKey = candidate.Key
			}
		}
		if !assert.NotEmpty(p.t, targetKey) {
			http.Error(w, "missing synthetic target", http.StatusBadRequest)
			return
		}
		claims = append(claims, map[string]any{
			"target_key": targetKey, "relation": "support", "value": "chat",
			"evidence_ids": []string{packet.Seeds[0].ID}, "valid_from": nil,
			"valid_until": nil, "confidence_basis_points": 1000,
		})
	}
	content, err := json.Marshal(map[string]any{"claims": claims})
	if !assert.NoError(p.t, err) {
		http.Error(w, "invalid synthetic response", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(map[string]any{
		"model":   "parity-model-v1",
		"choices": []any{map[string]any{"message": map[string]any{"content": string(content)}}},
		"usage": map[string]any{"prompt_tokens": parityInputTokens,
			"completion_tokens": parityOutputTokens},
	})
	if !assert.NoError(p.t, err) {
		http.Error(w, "invalid synthetic envelope", http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Request-ID", fmt.Sprintf("parity-request-%d", call))
	w.Header().Set("Content-Type", "application/json")
	_, err = w.Write(body)
	assert.NoError(p.t, err)
}

type paritySourceRecorder struct {
	store    *store.Store
	mu       sync.Mutex
	requests []peoplesweep.WindowRequest
	windows  []peoplesweep.PersonWindow
}

func (r *paritySourceRecorder) LoadPersonSweepWindow(
	ctx context.Context, request peoplesweep.WindowRequest,
) (peoplesweep.PersonWindow, error) {
	window, err := r.store.LoadPersonSweepWindow(ctx, request)
	r.mu.Lock()
	r.requests = append(r.requests, request)
	r.windows = append(r.windows, window)
	r.mu.Unlock()
	return window, err
}

func (r *paritySourceRecorder) LoadPersonFactState(
	ctx context.Context, personID int64, catalog personfacts.Catalog,
) (peoplesweep.PersonFactState, error) {
	return r.store.LoadPersonFactState(ctx, personID, catalog)
}

func (r *paritySourceRecorder) BuildPersonSweepEvidenceStatusChanges(
	ctx context.Context, personID int64, changes []peoplesweep.ArchiveChange,
) ([]personfacts.EvidenceStatusChange, error) {
	return r.store.BuildPersonSweepEvidenceStatusChanges(ctx, personID, changes)
}

type paritySinkRecorder struct {
	store    *store.Store
	mu       sync.Mutex
	requests []peoplesweep.ApplyRequest
	results  []peoplesweep.ApplyResult
}

// parityRedactedRunner models a caller that has already redacted its synthetic
// archive excerpts. Sensitive-policy behavior is covered separately; this
// fixture keeps its long-lived state-machine fingerprints stable.
type parityRedactedRunner struct{ peoplesweep.StructuredRunner }

func (r parityRedactedRunner) PrepareStructured(
	ctx context.Context, request peoplesweep.StructuredRequest,
) (peoplesweep.PreparedStructuredRequest, error) {
	request.ContainsSensitive = false
	return r.StructuredRunner.PrepareStructured(ctx, request)
}

func (r *paritySinkRecorder) ApplyPersonSweep(
	ctx context.Context, request peoplesweep.ApplyRequest,
) (peoplesweep.ApplyResult, error) {
	result, err := r.store.ApplyPersonSweep(ctx, request)
	if err == nil {
		r.mu.Lock()
		r.requests = append(r.requests, request)
		r.results = append(r.results, result)
		r.mu.Unlock()
	}
	return result, err
}

func TestPersonSweepStateMachineParity(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	f := newProductionPersonSweepParityFixture(t)
	defer f.server.Close()
	ctx := t.Context()

	// A provider-completed stale attempt is conservatively charged by reclaim.
	// Its exact lease-lost replay is a no-op and cannot rewrite the new owner.
	oldLease := parityClaim(t, f.store, "parity-old-worker")
	oldAssembly := parityAssembly(t, f)
	oldRunID, oldAttemptID := "run-parity-old", "attempt-parity-old"
	parityStartRunAttempt(t, f, oldRunID, oldAttemptID, oldLease,
		oldAssembly.CursorEnvelope, peoplesweep.RunIncremental)
	require.Len(oldAssembly.Batches, 1)
	reservation, _ := parityRunChargedBatch(t, f, oldRunID, oldAttemptID, oldLease,
		oldAssembly.Batches[0])
	_, err := f.store.DB().ExecContext(ctx, f.store.Rebind(`
		UPDATE person_sweep_work SET lease_until = ? WHERE person_id = ?`),
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), f.personID)
	require.NoError(err)
	newLease := parityClaim(t, f.store, "parity-new-worker")
	require.NoError(f.store.FinalizePersonSweepFailure(ctx, peoplesweep.FailureFinalization{
		Lease: *oldLease, AttemptID: oldAttemptID, Class: peoplesweep.FailureLeaseLost,
		RetryAt:      f.now.Add(time.Hour),
		Reservations: []peoplesweep.BudgetReservation{reservation},
		FinalizedAt:  f.now,
	}))
	require.NoError(f.store.FinishPersonSweepRun(ctx, oldRunID,
		peoplesweep.RunFailed, f.now))
	work := parityWorkSnapshotNow(t, f.store, f.personID)
	assert.Equal("parity-new-worker", work.LeaseOwner)
	assert.Equal(newLease.Fence, work.Fence)

	parityRunPerson(t, f, "success", newLease, peoplesweep.RunIncremental, false)
	require.Len(f.sinkRecorder.requests, 1)
	replayed, err := f.store.ApplyPersonFactGenerationContext(ctx,
		f.sinkRecorder.requests[0].Generation, store.PersonSweepEvidenceAligner{Store: f.store})
	require.NoError(err)
	assert.Equal(f.sinkRecorder.results[0].Generation, *replayed)
	assert.Equal([]string{"chat"}, parityCurrentChannel(t, f))
	parityPinProjectionTimes(t, f)

	paritySyncMutation(t, f, func() {
		require.NoError(f.store.MarkMessageDeleted(f.sourceID, "parity-late-old"))
	})
	deleted := parityRunClaimedPerson(t, f, "deleted", peoplesweep.RunIncremental)
	assert.Equal(peoplesweep.StatusOnlyProvider, deleted.Generation.Provider)
	assert.Empty(deleted.Batches)
	assert.Empty(parityCurrentChannel(t, f))

	paritySyncMutation(t, f, func() {
		require.NoError(f.store.ClearMessageDeletedFromSource(
			f.sourceID, "parity-late-old"))
	})
	reimported := parityRunClaimedPerson(t, f, "reimported", peoplesweep.RunIncremental)
	assert.Equal(string(peoplesweep.ProtocolOpenAIChat), reimported.Generation.Provider)
	assert.Equal([]string{"chat"}, parityCurrentChannel(t, f))
	parityPinProjectionTimes(t, f)

	parityEditMessage(t, f, "Chat remains the preferred channel after correction.")
	edited, err := f.store.HydratePersonSweepMessages(ctx, f.personID, []int64{f.messageID})
	require.NoError(err)
	require.Len(edited, 1)
	f.editedItem = edited[0]
	parityRunClaimedPerson(t, f, "edit", peoplesweep.RunIncremental)
	assert.Equal([]string{"chat"}, parityCurrentChannel(t, f))
	parityPinProjectionTimes(t, f)

	parityEditMessage(t, f, "Chat is still the preferred channel after correction.")
	revokedLease := parityClaim(t, f.store, "parity-revoked-worker")
	revokedBefore := parityRevocationSnapshotNow(t, f)
	f.provider.revoke = func(call int) {
		if call != 5 {
			return
		}
		revoked, revokeErr := f.store.RevokePersonInferenceConsent(
			context.Background(), f.profile.Fingerprint, "parity-revocation")
		require.NoError(revokeErr)
		assert.True(revoked)
	}
	parityRunPerson(t, f, "revoked", revokedLease, peoplesweep.RunIncremental, true)
	revokedAfter := parityRevocationSnapshotNow(t, f)
	assert.Equal(revokedBefore.Facts, revokedAfter.Facts)
	assert.Equal(revokedBefore.Cursor, revokedAfter.Cursor)
	assert.Equal(revokedBefore.Work.DirtyThrough, revokedAfter.Work.DirtyThrough)
	assert.Equal(revokedBefore.Work.Fence, revokedAfter.Work.Fence)
	assert.Equal(int64(1), revokedAfter.Work.Fence)
	assert.Empty(revokedAfter.Work.LeaseOwner)
	assert.Equal(peoplesweep.FailurePolicy, revokedAfter.Work.Failure)
	assert.Equal(revokedBefore.Usage.Requests+1, revokedAfter.Usage.Requests)
	assert.Equal(revokedBefore.Usage.InputTokens+parityInputTokens, revokedAfter.Usage.InputTokens)
	assert.Equal(revokedBefore.Usage.OutputTokens+parityOutputTokens, revokedAfter.Usage.OutputTokens)
	assert.Equal(revokedBefore.Usage.EstimatedCostMicroUSD+parityCostMicroUSD,
		revokedAfter.Usage.EstimatedCostMicroUSD)

	_, _, err = f.store.GrantPersonInferenceConsent(ctx, f.profile.Fingerprint, "parity-regrant")
	require.NoError(err)
	definition, err := f.store.GetAttributeDefinitionBySlugContext(ctx,
		store.AttributeObjectPerson, store.AttributeSlugAskMeAbout)
	require.NoError(err)
	description := "Topics worth raising with this person after catalog revision"
	descriptionPointer := &description
	_, err = f.store.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision,
		store.AttributeDefinitionUpdate{Description: &descriptionPointer})
	require.NoError(err)
	newCatalog, err := f.store.BuildPersonFactCatalogContext(ctx, false)
	require.NoError(err)
	require.NotEqual(f.catalog.Fingerprint, newCatalog.Fingerprint)
	newKey := f.key
	newKey.CatalogFingerprint = newCatalog.Fingerprint
	f.provider.revoke = nil
	result := parityRunWholeWorker(t, f, peoplesweep.RunBackstop)
	assert.Equal(1, result.PeopleSucceeded)
	assertActualBackstopWindow(t, f)
	assertPersonSweepParitySnapshot(t, f, newKey)
}

func newProductionPersonSweepParityFixture(t *testing.T) *personSweepParityFixture {
	t.Helper()
	f := &personSweepParityFixture{store: testutil.NewTestStore(t),
		now: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)}
	f.provider = &parityProvider{t: t}
	f.server = httptest.NewServer(f.provider)
	source, err := f.store.GetOrCreateSource("slack", "person-sweep-production-parity")
	require.NoError(t, err)
	f.sourceID = source.ID
	f.conversationID, err = f.store.EnsureConversationWithType(source.ID,
		"person-sweep-production-parity", "direct_chat", "Synthetic parity")
	require.NoError(t, err)
	f.aliceID, err = f.store.EnsureParticipant("alice@example.test", "Alice", "example.test")
	require.NoError(t, err)
	f.aliasID, err = f.store.EnsureParticipant(
		"alice.alias@example.test", "Alice Alias", "example.test")
	require.NoError(t, err)
	person, _, err := f.store.CreatePersonFromParticipant(f.aliceID)
	require.NoError(t, err)
	f.personID = person.ID
	_, err = f.store.LinkParticipants(f.aliceID, f.aliasID)
	require.NoError(t, err)
	_, err = f.store.SetPersonTrackingContext(t.Context(), f.personID, true)
	require.NoError(t, err)
	f.config = peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"},
		Providers: map[string]peoplesweep.ProviderConfig{"default": {
			Protocol: peoplesweep.ProtocolOpenAIChat,
			Endpoint: "https://parity-provider.example.test/v1",
			Model:    "parity-request-model", Auth: peoplesweep.AuthBearer,
			Credential: peoplesweep.CredentialEnv, CredentialEnv: "PARITY_SYNTHETIC_KEY",
			OutputMode: peoplesweep.OutputModeNativeJSONSchema, TokenLimitParameter: "max_completion_tokens",
			RetentionPosture: "zero_retention", TrainingPosture: "no_training",
			AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText},
			SourceSince:    "2000-01-01", RequestTimeout: 2 * time.Second,
		}}}
	f.config.ApplyDefaults()
	f.config.RetryBase, f.config.RetryMax = time.Millisecond, time.Millisecond
	f.config.Budgets.InputCostMicroUSDPerMillionTokens = 2_000_000
	f.config.Budgets.OutputCostMicroUSDPerMillionTokens = 6_000_000
	f.config.Budgets.MaxEstimatedCostMicroUSDPerRun = 1_000_000_000
	f.config.Budgets.MaxEstimatedCostMicroUSDPerDay = 1_000_000_000
	f.profile, err = f.config.Profile()
	require.NoError(t, err)
	_, err = f.store.EnsurePersonInferenceProfile(t.Context(), f.profile)
	require.NoError(t, err)
	require.NoError(t, f.store.RecordPersonInferenceCheck(t.Context(), store.PersonInferenceCheck{
		ProfileFingerprint: f.profile.Fingerprint,
		CheckedAt:          f.now,
		DriverVersion:      f.profile.DriverVersion,
		OutputMode:         f.profile.OutputMode,
		ModelVersion:       "parity-reported-model",
	}))
	_, _, err = f.store.GrantPersonInferenceConsent(t.Context(), f.profile.Fingerprint, "parity")
	require.NoError(t, err)
	f.catalog, err = f.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	for _, candidate := range f.catalog.Targets {
		if candidate.Slug == store.AttributeSlugPrimaryChannel {
			f.target = candidate
		}
	}
	require.NotEmpty(t, f.target.Key)
	f.key = peoplesweep.CursorKey{PersonID: f.personID,
		SourceLane:         peoplesweep.SourceConversationText,
		ProgramFingerprint: peoplesweep.ProgramFingerprint(),
		CatalogFingerprint: f.catalog.Fingerprint}
	_, err = f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{f.key})
	require.NoError(t, err)
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	require.NoError(t, err)
	body := "Chat is my preferred channel."
	f.messageID, err = f.store.PersistMessage(&store.MessagePersistData{
		Message: &store.Message{SourceID: f.sourceID, SourceMessageID: "parity-late-old",
			ConversationID: f.conversationID, MessageType: "email",
			SenderID: sql.NullInt64{Int64: f.aliasID, Valid: true},
			SentAt:   sql.NullTime{Time: time.Date(2001, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true},
			Subject:  sql.NullString{String: "Synthetic parity", Valid: true}},
		BodyText: sql.NullString{String: body, Valid: true},
		FTS: &store.FTSDoc{Subject: "Synthetic parity", Body: body,
			FromAddr: "alice.alias@example.test"},
	})
	require.NoError(t, err)
	// The production persist path stamps archived_at with the database clock;
	// pin it so evidence RecordedTime — and every downstream content hash —
	// is reproducible for the frozen snapshot below.
	_, err = f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE messages SET archived_at = ? WHERE id = ?`),
		time.Date(2026, 8, 22, 11, 0, 0, 0, time.UTC), f.messageID)
	require.NoError(t, err)
	require.NoError(t, f.store.CompleteSync(syncID, "parity-imported"))
	items, err := f.store.HydratePersonSweepMessages(t.Context(), f.personID, []int64{f.messageID})
	require.NoError(t, err)
	require.Len(t, items, 1)
	f.initialItem = items[0]
	targetURL, err := url.Parse(f.server.URL)
	require.NoError(t, err)
	httpClient := f.server.Client()
	httpClient.Transport = parityRewriteTransport{base: httpClient.Transport, target: targetURL}
	registry, err := peoplesweep.NewDriverRegistry(httpClient, nil, nil)
	require.NoError(t, err)
	runner, err := peoplesweep.NewRunner(f.config, f.store,
		registry,
		peoplesweep.NewCredentialResolver(nil, func(name string) (string, bool) {
			assert.Equal(t, "PARITY_SYNTHETIC_KEY", name)
			return "synthetic-parity-key", true
		}))
	require.NoError(t, err)
	f.sourceRecorder = &paritySourceRecorder{store: f.store}
	f.sinkRecorder = &paritySinkRecorder{store: f.store}
	f.worker = peoplesweep.Worker{Config: f.config, Store: f.store,
		Source: f.sourceRecorder, Context: peoplesweep.NewContextRetriever(f.store),
		Sink: f.sinkRecorder, Runner: parityRedactedRunner{StructuredRunner: runner}, Catalog: f.store,
		Clock:    func() time.Time { return f.now },
		NewID:    func() string { f.nextID++; return fmt.Sprintf("attempt-parity-%02d", f.nextID) },
		WorkerID: "parity-production-worker"}
	return f
}

func parityAssembly(t *testing.T, f *personSweepParityFixture) peoplesweep.Assembly {
	t.Helper()
	cursors, err := f.store.EnsurePersonSweepCursors(t.Context(), []peoplesweep.CursorKey{f.key})
	require.NoError(t, err)
	assembly, err := (peoplesweep.Assembler{Source: f.sourceRecorder,
		Context:  peoplesweep.NewContextRetriever(f.store),
		MaxBytes: f.config.EvidenceMaxBytes, MaxItems: f.config.EvidenceMaxItems,
		ContextPerTarget: f.config.ContextPerTarget}).Build(t.Context(), peoplesweep.AssemblyRequest{
		PersonID: f.personID, Cursors: cursors, Catalog: f.catalog, Profile: f.profile,
		Now: f.now, BackstopInterval: f.config.BackstopInterval})
	require.NoError(t, err)
	return assembly
}

func parityStartRunAttempt(t *testing.T, f *personSweepParityFixture,
	runID, attemptID string, lease *peoplesweep.Lease,
	envelope []peoplesweep.GenerationCursor, mode peoplesweep.RunMode,
) {
	t.Helper()
	_, err := f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{ID: runID,
		Kind: peoplesweep.RunManual, Mode: mode,
		ProgramFingerprint:  peoplesweep.ProgramFingerprint(),
		CatalogFingerprint:  envelope[0].Key.CatalogFingerprint,
		ProviderFingerprint: f.profile.Fingerprint, StartedAt: f.now})
	require.NoError(t, err)
	encoded, err := json.Marshal(envelope)
	require.NoError(t, err)
	digest := sha256.Sum256(encoded)
	require.NoError(t, f.store.StartPersonSweepAttempt(t.Context(), peoplesweep.StartAttempt{
		ID: attemptID, RunID: runID, PersonID: f.personID, LeaseFence: lease.Fence,
		Mode: mode, CursorEnvelope: envelope, EnvelopeHash: hex.EncodeToString(digest[:]),
		StartedAt: f.now}))
}

func parityRunChargedBatch(t *testing.T, f *personSweepParityFixture,
	runID, attemptID string, lease *peoplesweep.Lease, batch peoplesweep.PacketBatch,
) (peoplesweep.BudgetReservation, peoplesweep.StructuredResponse) {
	t.Helper()
	prepared, err := f.worker.Runner.PrepareStructured(t.Context(), batch.Request)
	require.NoError(t, err)
	estimate, err := peoplesweep.EstimateWireTokenReservation(
		prepared.WireRequest(), batch.Request.MaxOutputTokens)
	require.NoError(t, err)
	cost, err := peoplesweep.EstimateCostMicroUSD(estimate, f.config.Budgets)
	require.NoError(t, err)
	reservation, err := f.store.ReservePersonSweepBudget(t.Context(),
		peoplesweep.BudgetReservationRequest{RunID: runID, AttemptID: attemptID,
			BatchOrdinal: batch.Ordinal, CallOrdinal: 0,
			Purpose: peoplesweep.ProviderCallPurposePrimary, PersonID: f.personID,
			ProviderFingerprint: f.profile.Fingerprint,
			UTCDate:             f.now.Format(time.DateOnly), InputHash: prepared.WireSHA256(),
			ItemCount: len(batch.Packet.Seeds) + len(batch.Packet.Context), EstimatedRequests: 1,
			EstimatedInputTokens: estimate.InputTokens, EstimatedOutputTokens: estimate.OutputTokens,
			EstimatedCostMicroUSD: cost, Budget: f.config.Budgets})
	require.NoError(t, err)
	require.NoError(t, f.store.MarkPersonSweepBudgetStarted(t.Context(), reservation, *lease))
	response, err := f.worker.Runner.RunPreparedStructured(t.Context(), prepared)
	require.NoError(t, err)
	assert.Equal(t, parityInputTokens, response.Usage.InputTokens)
	assert.Equal(t, parityOutputTokens, response.Usage.OutputTokens)
	return reservation, response
}

func parityClaim(t *testing.T, st *store.Store, worker string) *peoplesweep.Lease {
	t.Helper()
	lease, err := st.ClaimPersonSweep(t.Context(), peoplesweep.ClaimRequest{
		WorkerID: worker, LeaseDuration: time.Hour,
		AvailableAt: time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)})
	require.NoError(t, err)
	require.NotNil(t, lease)
	return lease
}

func parityRunPerson(t *testing.T, f *personSweepParityFixture, suffix string,
	lease *peoplesweep.Lease, mode peoplesweep.RunMode, wantFailure bool,
) {
	t.Helper()
	runID := "run-parity-" + suffix
	_, err := f.store.StartPersonSweepRun(t.Context(), peoplesweep.StartRun{ID: runID,
		Kind: peoplesweep.RunManual, Mode: mode,
		ProgramFingerprint:  peoplesweep.ProgramFingerprint(),
		CatalogFingerprint:  parityCatalogFingerprint(t, f),
		ProviderFingerprint: f.profile.Fingerprint, StartedAt: f.now})
	require.NoError(t, err)
	_, runErr := f.worker.RunPerson(t.Context(), runID, *lease, mode)
	if wantFailure {
		require.ErrorIs(t, runErr, peoplesweep.ErrPersonSweepConsentRevoked)
		require.NoError(t, f.store.FinishPersonSweepRun(t.Context(), runID,
			peoplesweep.RunFailed, f.now))
		return
	}
	require.NoError(t, runErr)
	require.NoError(t, f.store.FinishPersonSweepRun(t.Context(), runID,
		peoplesweep.RunSucceeded, f.now))
}

func parityCatalogFingerprint(t *testing.T, f *personSweepParityFixture) string {
	t.Helper()
	catalog, err := f.store.BuildPersonFactCatalogContext(t.Context(), false)
	require.NoError(t, err)
	return catalog.Fingerprint
}

func parityRunClaimedPerson(t *testing.T, f *personSweepParityFixture,
	suffix string, mode peoplesweep.RunMode,
) peoplesweep.ApplyRequest {
	t.Helper()
	before := len(f.sinkRecorder.requests)
	lease := parityClaim(t, f.store, "parity-"+suffix+"-worker")
	parityRunPerson(t, f, suffix, lease, mode, false)
	require.Len(t, f.sinkRecorder.requests, before+1)
	return f.sinkRecorder.requests[before]
}

func parityRunWholeWorker(t *testing.T, f *personSweepParityFixture,
	mode peoplesweep.RunMode,
) peoplesweep.RunResult {
	t.Helper()
	ids := 0
	f.worker.NewID = func() string {
		ids++
		if ids == 1 {
			return "run-parity-backstop"
		}
		return fmt.Sprintf("attempt-parity-backstop-%d", ids)
	}
	result, err := f.worker.Run(t.Context(), peoplesweep.RunRequest{Kind: peoplesweep.RunManual,
		Mode: mode, PersonID: f.personID, Limit: 1})
	require.NoError(t, err)
	return result
}

func paritySyncMutation(t *testing.T, f *personSweepParityFixture, mutate func()) {
	t.Helper()
	syncID, err := f.store.StartSync(f.sourceID, "incremental")
	require.NoError(t, err)
	mutate()
	require.NoError(t, f.store.CompleteSync(syncID, fmt.Sprintf("parity-sync-%d", syncID)))
}

func parityEditMessage(t *testing.T, f *personSweepParityFixture, body string) {
	t.Helper()
	paritySyncMutation(t, f, func() {
		err := f.store.UpdateMessageDerivedText(f.messageID,
			sql.NullString{String: body, Valid: true}, sql.NullString{}, sql.NullString{},
			store.FTSDoc{Subject: "Synthetic parity", Body: body,
				FromAddr: "alice.alias@example.test"})
		require.NoError(t, err)
	})
}

func parityCurrentChannel(t *testing.T, f *personSweepParityFixture) []string {
	t.Helper()
	values, err := f.store.ListPersonAttributeValuesContext(t.Context(), f.personID,
		store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugPrimaryChannel})
	require.NoError(t, err)
	result := make([]string, 0, len(values))
	for _, value := range values {
		require.NotNil(t, value.Value.Text)
		result = append(result, *value.Value.Text)
	}
	slices.Sort(result)
	return result
}

// parityPinProjectionTimes aligns projection transaction times with the fixed
// fixture clock. Production apply stamps person_attribute created_at with the
// database clock; later resolutions read it as TransactionTime, so unpinned
// rows would make the frozen input fingerprints unreproducible.
func parityPinProjectionTimes(t *testing.T, f *personSweepParityFixture) {
	t.Helper()
	_, err := f.store.DB().ExecContext(t.Context(), f.store.Rebind(
		`UPDATE person_attribute_values SET created_at = ? WHERE person_id = ?`),
		f.now, f.personID)
	require.NoError(t, err)
}

type parityWorkSnapshot struct {
	Exists       bool
	DirtyThrough int64
	Attempts     int
	Failure      peoplesweep.FailureClass
	LeaseOwner   string
	Fence        int64
}

func parityWorkSnapshotNow(t *testing.T, st *store.Store, personID int64) parityWorkSnapshot {
	t.Helper()
	var result parityWorkSnapshot
	err := st.DB().QueryRowContext(t.Context(), st.Rebind(`
		SELECT dirty_through_sequence, attempt_count, last_failure_class,
		       lease_owner, lease_fence
		FROM person_sweep_work WHERE person_id = ?`), personID).Scan(
		&result.DirtyThrough, &result.Attempts, &result.Failure,
		&result.LeaseOwner, &result.Fence)
	if errors.Is(err, sql.ErrNoRows) {
		return result
	}
	require.NoError(t, err)
	result.Exists = true
	return result
}

type parityRevocationSnapshot struct {
	Facts  string
	Cursor peoplesweep.Cursor
	Work   parityWorkSnapshot
	Usage  peoplesweep.Usage
}

type parityFactSnapshot struct {
	Evidence   []personfacts.Evidence `json:"evidence"`
	Claims     []personfacts.Claim    `json:"claims"`
	Decisions  []personfacts.Decision `json:"decisions"`
	Projection []string               `json:"projection"`
}

func parityRevocationSnapshotNow(t *testing.T, f *personSweepParityFixture) parityRevocationSnapshot {
	t.Helper()
	evidence, err := f.store.ListPersonFactEvidenceContext(t.Context(), f.personID,
		personfacts.EvidenceFilter{Limit: 100})
	require.NoError(t, err)
	claims, err := f.store.ListPersonFactClaimsContext(t.Context(), f.personID,
		personfacts.ClaimFilter{Limit: 100})
	require.NoError(t, err)
	decisions, err := f.store.ListPersonFactDecisionsContext(t.Context(), f.personID,
		personfacts.DecisionFilter{Limit: 100})
	require.NoError(t, err)
	//nolint:musttag // The snapshot deliberately uses the production ledger structs verbatim.
	facts, err := json.Marshal(parityFactSnapshot{Evidence: evidence, Claims: claims,
		Decisions: decisions, Projection: parityCurrentChannel(t, f)})
	require.NoError(t, err)
	runs, err := f.store.ListPersonSweepRuns(t.Context(), peoplesweep.RunFilter{
		PersonID: f.personID, Limit: 100})
	require.NoError(t, err)
	var usage peoplesweep.Usage
	for _, run := range runs {
		usage.Requests += run.Usage.Requests
		usage.InputTokens += run.Usage.InputTokens
		usage.OutputTokens += run.Usage.OutputTokens
		usage.EstimatedCostMicroUSD += run.Usage.EstimatedCostMicroUSD
	}
	return parityRevocationSnapshot{Facts: string(facts),
		Cursor: loadPersonSweepCursor(t, f.store, f.key),
		Work:   parityWorkSnapshotNow(t, f.store, f.personID), Usage: usage}
}

func assertActualBackstopWindow(t *testing.T, f *personSweepParityFixture) {
	t.Helper()
	f.sourceRecorder.mu.Lock()
	defer f.sourceRecorder.mu.Unlock()
	found := false
	for index, request := range f.sourceRecorder.requests {
		if request.Mode != peoplesweep.GenerationCursorBackstop {
			continue
		}
		found = true
		window := f.sourceRecorder.windows[index]
		assert.NotEmpty(t, window.CapturedUpperKey)
		assert.True(t, window.ReconciliationDone)
		assert.NotEmpty(t, window.Seeds)
		assert.Equal(t, f.messageID, window.Seeds[0].Ref.MessageID)
	}
	assert.True(t, found, "production assembler did not load a backstop window")
}

func parityAssertAttempt(t *testing.T, f *personSweepParityFixture,
	got peoplesweep.AttemptSummary, id string, status peoplesweep.AttemptStatus,
	failure peoplesweep.FailureClass, envelope []peoplesweep.GenerationCursor,
	claims, decisions, writes int, requestID string, usage peoplesweep.Usage, hasGeneration bool,
) {
	t.Helper()
	assert.Equal(t, id, got.ID)
	assert.Equal(t, status, got.Status)
	assert.Equal(t, failure, got.FailureClass)
	assert.Equal(t, envelope, got.CursorEnvelope)
	assert.Len(t, got.EnvelopeHash, 64)
	assert.Equal(t, peoplesweep.ProgramFingerprint(), got.ProgramFingerprint)
	assert.Equal(t, envelope[0].Key.CatalogFingerprint, got.CatalogFingerprint)
	assert.Equal(t, f.profile.Fingerprint, got.ProviderFingerprint)
	assert.Zero(t, got.SeedCount)
	assert.Zero(t, got.ContextCount)
	assert.Equal(t, claims, got.ClaimCount)
	assert.Equal(t, decisions, got.DecisionCount)
	assert.Equal(t, writes, got.ProjectedWrites)
	assert.Equal(t, requestID, got.ProviderRequestID)
	assert.Equal(t, usage, got.Usage)
	if hasGeneration {
		assert.NotNil(t, got.GenerationID)
		assert.True(t, strings.HasPrefix(got.GenerationKey, "sha256:"))
	} else {
		assert.Nil(t, got.GenerationID)
		assert.Empty(t, got.GenerationKey)
	}
}

type parityEvidenceSnapshot struct {
	Key                 string
	Input               personfacts.EvidenceInput
	Supported           bool
	LatestSourceVersion string
	LatestReason        personfacts.EvidenceStatusReason
}

type parityResolutionSnapshot struct {
	RunID                     string
	TargetKind                string
	TargetKey                 string
	TargetRevision            string
	ResolverVersion           string
	InputFingerprint          string
	ProviderPolicyFingerprint string
	ResolvedAt                string
	DecisionKey               string
	ClaimKey                  string
	Action                    string
	Reason                    string
	ScoreJSON                 string
	CompetingClaimKey         string
	ProjectionKind            string
	ProjectionRowID           int64
}

func parityResolutionSnapshots(t *testing.T, f *personSweepParityFixture) []parityResolutionSnapshot {
	t.Helper()
	rows, err := f.store.DB().QueryContext(t.Context(), f.store.Rebind(`
		SELECT a.run_id, r.target_kind, r.target_key, r.target_revision,
		       r.resolver_version, r.input_fingerprint, r.provider_policy_fingerprint,
		       r.resolved_at, d.decision_key, COALESCE(c.claim_key, ''),
		       d.action, d.reason, CAST(d.score_json AS TEXT),
		       COALESCE(competing.claim_key, ''), COALESCE(d.projection_kind, ''),
		       COALESCE(d.projection_row_id, 0)
		FROM person_fact_resolutions r
		JOIN person_sweep_attempts a ON a.generation_id = r.generation_id
		JOIN person_fact_decisions d ON d.resolution_id = r.id
		LEFT JOIN person_fact_claims c ON c.id = d.claim_id
		LEFT JOIN person_fact_claims competing ON competing.id = d.competing_claim_id
		WHERE r.person_id = ? AND r.target_key = ?
		ORDER BY a.run_id, d.decision_key`), f.personID, f.target.Key)
	require.NoError(t, err)
	defer func() { require.NoError(t, rows.Close()) }()
	result := make([]parityResolutionSnapshot, 0)
	for rows.Next() {
		var item parityResolutionSnapshot
		var resolvedAt time.Time
		require.NoError(t, rows.Scan(&item.RunID, &item.TargetKind, &item.TargetKey,
			&item.TargetRevision, &item.ResolverVersion, &item.InputFingerprint,
			&item.ProviderPolicyFingerprint, &resolvedAt, &item.DecisionKey, &item.ClaimKey,
			&item.Action, &item.Reason, &item.ScoreJSON, &item.CompetingClaimKey,
			&item.ProjectionKind, &item.ProjectionRowID))
		// PostgreSQL stores score_json as JSONB, which re-serializes key order
		// and spacing; canonicalize through Go so both backends compare the
		// exact same value-keyed JSON.
		var scoreValue map[string]any
		require.NoError(t, json.Unmarshal([]byte(item.ScoreJSON), &scoreValue))
		canonicalScore, err := json.Marshal(scoreValue)
		require.NoError(t, err)
		item.ScoreJSON = string(canonicalScore)
		item.ResolvedAt = resolvedAt.UTC().Format(time.RFC3339Nano)
		result = append(result, item)
	}
	require.NoError(t, rows.Err())
	return result
}

func assertPersonSweepParitySnapshot(t *testing.T, f *personSweepParityFixture,
	newKey peoplesweep.CursorKey,
) {
	t.Helper()
	runs, err := f.store.ListPersonSweepRuns(t.Context(), peoplesweep.RunFilter{
		PersonID: f.personID, Limit: 100})
	require.NoError(t, err)
	type runState struct {
		Status                                peoplesweep.RunStatus
		Attempts, Successes, Failures, Writes int
		Usage                                 peoplesweep.Usage
	}
	runSnapshot := make(map[string]runState, len(runs))
	for _, run := range runs {
		runSnapshot[run.ID] = runState{run.Status, run.Attempts, run.Successes,
			run.Failures, run.ProjectedWrites, run.Usage}
	}
	charged := peoplesweep.Usage{Requests: 1, InputTokens: parityInputTokens,
		OutputTokens: parityOutputTokens, EstimatedCostMicroUSD: parityCostMicroUSD}
	reclaimed := runSnapshot["run-parity-old"]
	assert.Equal(t, runState{peoplesweep.RunFailed, 1, 0, 1, 0, reclaimed.Usage}, reclaimed)
	assert.Equal(t, 1, reclaimed.Usage.Requests)
	assert.Positive(t, reclaimed.Usage.InputTokens)
	assert.Positive(t, reclaimed.Usage.OutputTokens)
	assert.LessOrEqual(t, reclaimed.Usage.EstimatedCostMicroUSD, charged.EstimatedCostMicroUSD)
	delete(runSnapshot, "run-parity-old")
	assert.Equal(t, map[string]runState{
		"run-parity-success":    {peoplesweep.RunSucceeded, 1, 1, 0, 1, charged},
		"run-parity-deleted":    {peoplesweep.RunSucceeded, 1, 1, 0, 1, peoplesweep.Usage{}},
		"run-parity-reimported": {peoplesweep.RunSucceeded, 1, 1, 0, 1, charged},
		"run-parity-edit":       {peoplesweep.RunSucceeded, 1, 1, 0, 0, charged},
		"run-parity-revoked":    {peoplesweep.RunFailed, 1, 0, 1, 0, charged},
		"run-parity-backstop":   {peoplesweep.RunSucceeded, 1, 1, 0, 0, charged},
	}, runSnapshot)

	attempts, err := f.store.ListPersonSweepAttempts(t.Context(), peoplesweep.AttemptFilter{
		PersonID: f.personID, Limit: 100})
	require.NoError(t, err)
	require.Len(t, attempts, 7)
	byRun := make(map[string]peoplesweep.AttemptSummary, len(attempts))
	for _, attempt := range attempts {
		byRun[attempt.RunID] = attempt
	}
	parityAssertAttempt(t, f, byRun["run-parity-old"], "attempt-parity-old",
		peoplesweep.AttemptFailed, peoplesweep.FailureLeaseLost,
		[]peoplesweep.GenerationCursor{
			{Key: f.key, Mode: peoplesweep.GenerationCursorOptimistic, CursorThrough: 2},
			{Key: f.key, Mode: peoplesweep.GenerationCursorBackstop,
				ReconcileToKey: "00000000000000000001", BackstopUpperKey: "00000000000000000001"}}, 0, 0, 0, "", reclaimed.Usage, false)
	parityAssertAttempt(t, f, byRun["run-parity-success"], "attempt-parity-01",
		peoplesweep.AttemptSucceeded, "", []peoplesweep.GenerationCursor{
			{Key: f.key, Mode: peoplesweep.GenerationCursorOptimistic, CursorThrough: 2},
			{Key: f.key, Mode: peoplesweep.GenerationCursorBackstop,
				ReconcileToKey: "00000000000000000001", BackstopUpperKey: "00000000000000000001"}}, 1, 1, 1, "parity-request-2", charged, true)
	parityAssertAttempt(t, f, byRun["run-parity-deleted"], "attempt-parity-02",
		peoplesweep.AttemptSucceeded, "", []peoplesweep.GenerationCursor{{Key: f.key,
			Mode: peoplesweep.GenerationCursorOptimistic, CursorFrom: 2, CursorThrough: 3}},
		0, 1, 1, "", peoplesweep.Usage{}, true)
	parityAssertAttempt(t, f, byRun["run-parity-reimported"], "attempt-parity-03",
		peoplesweep.AttemptSucceeded, "", []peoplesweep.GenerationCursor{{Key: f.key,
			Mode: peoplesweep.GenerationCursorOptimistic, CursorFrom: 3, CursorThrough: 4}},
		0, 1, 1, "parity-request-3", charged, true)
	parityAssertAttempt(t, f, byRun["run-parity-edit"], "attempt-parity-04",
		peoplesweep.AttemptSucceeded, "", []peoplesweep.GenerationCursor{{Key: f.key,
			Mode: peoplesweep.GenerationCursorOptimistic, CursorFrom: 4, CursorThrough: 5}},
		1, 2, 0, "parity-request-4", charged, true)
	parityAssertAttempt(t, f, byRun["run-parity-revoked"], "attempt-parity-05",
		peoplesweep.AttemptFailed, peoplesweep.FailurePolicy,
		[]peoplesweep.GenerationCursor{{Key: f.key, Mode: peoplesweep.GenerationCursorOptimistic,
			CursorFrom: 5, CursorThrough: 6}}, 0, 0, 0, "parity-request-5", charged, false)
	parityAssertAttempt(t, f, byRun["run-parity-backstop"], "attempt-parity-backstop-2",
		peoplesweep.AttemptSucceeded, "", []peoplesweep.GenerationCursor{
			{Key: newKey, Mode: peoplesweep.GenerationCursorReconciliation,
				ReconcileToKey: "00000000000000000001"},
			{Key: newKey, Mode: peoplesweep.GenerationCursorBackstop,
				ReconcileToKey: "00000000000000000001", BackstopUpperKey: "00000000000000000001"}}, 0, 0, 0,
		"parity-request-6", charged, true)

	evidence, err := f.store.ListPersonFactEvidenceContext(t.Context(), f.personID,
		personfacts.EvidenceFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, evidence, 2)
	byExcerpt := make(map[string]parityEvidenceSnapshot, len(evidence))
	for _, item := range evidence {
		reason := personfacts.EvidenceStatusReason("")
		latestVersion := ""
		if item.LatestStatus != nil {
			reason = item.LatestStatus.Reason
			latestVersion = item.LatestStatus.SourceVersion
		}
		byExcerpt[item.Input.Excerpt] = parityEvidenceSnapshot{
			Key: item.Key, Input: item.Input, Supported: item.Supported,
			LatestSourceVersion: latestVersion, LatestReason: reason}
	}
	initialInput, err := peoplesweep.PersonFactEvidenceInput(f.initialItem)
	require.NoError(t, err)
	editedInput, err := peoplesweep.PersonFactEvidenceInput(f.editedItem)
	require.NoError(t, err)
	assert.Equal(t, parityExpectedEvidence(t, initialInput, false,
		personfacts.EvidenceStatusSourceEdited), byExcerpt[initialInput.Excerpt])
	assert.Equal(t, parityExpectedEvidence(t, editedInput, true, ""), byExcerpt[editedInput.Excerpt])
	statusEvents, err := f.store.ListPersonFactEvidenceStatusEventsContext(t.Context(), f.personID,
		personfacts.EvidenceStatusFilter{Limit: 100})
	require.NoError(t, err)
	require.Len(t, statusEvents, 3)
	assert.Equal(t, []personfacts.EvidenceStatusReason{
		personfacts.EvidenceStatusSourceEdited, personfacts.EvidenceStatusSourceReimported,
		personfacts.EvidenceStatusSourceDeleted}, []personfacts.EvidenceStatusReason{
		statusEvents[0].Reason, statusEvents[1].Reason, statusEvents[2].Reason})
	for _, event := range statusEvents {
		assert.Equal(t, initialInput.SourceVersion, event.SourceVersion)
		assert.Equal(t, byExcerpt[initialInput.Excerpt].Key, event.EvidenceKey)
	}

	claims, err := f.store.ListPersonFactClaimsContext(t.Context(), f.personID,
		personfacts.ClaimFilter{Target: &personfacts.TargetRef{Kind: f.target.Kind,
			Key: f.target.Key, Revision: f.target.Revision}, Limit: 100})
	require.NoError(t, err)
	require.Len(t, claims, 2)
	for _, claim := range claims {
		assert.Equal(t, f.personID, claim.PersonID)
		assert.Equal(t, personfacts.TargetRef{Kind: f.target.Kind, Key: f.target.Key,
			Revision: f.target.Revision}, claim.Target)
		assert.Equal(t, personfacts.RelationSupport, claim.Relation)
		assert.JSONEq(t, `"chat"`, string(claim.SubmittedValue))
		assert.Nil(t, claim.ValidFrom)
		assert.Nil(t, claim.ValidUntil)
		assert.Equal(t, personfacts.OriginExtraction, claim.Origin)
		assert.Equal(t, 1000, claim.Confidence.ReportedScore)
		require.Len(t, claim.EvidenceIDs, 1)
		assert.Equal(t, string(peoplesweep.ProtocolOpenAIChat), claim.Generation.Provider)
		assert.Equal(t, "openai-chat-completions-json-schema-v1",
			claim.Generation.ProviderVersion)
		assert.Equal(t, "parity-model-v1", claim.Generation.ModelVersion)
	}
	assert.NotEqual(t, claims[0].EvidenceIDs[0], claims[1].EvidenceIDs[0])

	decisions, err := f.store.ListPersonFactDecisionsContext(t.Context(), f.personID,
		personfacts.DecisionFilter{Target: &personfacts.TargetRef{Kind: f.target.Kind,
			Key: f.target.Key, Revision: f.target.Revision}, Limit: 100})
	require.NoError(t, err)
	require.NotEmpty(t, decisions)
	require.Len(t, decisions, 5)
	actions := make([]string, 0, len(decisions))
	projected := 0
	for _, decision := range decisions {
		actions = append(actions, fmt.Sprintf("%s/%s/%d", decision.Action,
			decision.Reason, decision.Score.Total))
		assert.NotEmpty(t, decision.DecisionKey)
		assert.NotZero(t, decision.ResolutionID)
		if decision.Projection != nil {
			projected++
		}
	}
	slices.Sort(actions)
	assert.Equal(t, []string{"applied/applied-projection/900",
		"applied/applied-projection/900", "applied/applied-projection/900",
		"retained/evidence-unsupported/0", "retained/evidence-unsupported/0"}, actions)
	resolutionSnapshot := parityResolutionSnapshots(t, f)
	const (
		targetKey      = "59e9a7d3-4904-4d0e-97d1-d0680e1e9e55"
		targetRevision = "sha256:99794c3a2a04a5e39a49ee51f24d5a1790e3d787ff98d57f00ff1c6d80741787"
		resolver       = "person-fact-resolver-v1"
		providerPolicy = "b4cd8faea9a6903d08c6df206eb542032028c6c83f974b81eff3dd4128dfda7a"
		resolvedAt     = "2026-08-22T12:00:00Z"
		zeroScore      = `{"authority":0,"confidence":0,"corroboration":0,"directness":0,"freshness":0,"source_class":0,"total":0}`
		appliedScore   = `{"authority":100,"confidence":100,"corroboration":0,"directness":400,"freshness":0,"source_class":300,"total":900}`
		originalClaim  = "sha256:ff40882f9438a8662b1c8efba4563eaf85368b5bdff7ba10ef86688cfbaa0995"
		editedClaim    = "sha256:0ae7ed902d9b0d2c463d0b53e9d100dfa4bfd6a7632a1b0f64a5f9a4258abd39"
		editInput      = "sha256:8c0a16286aa0a4ef422f7fdf0b91afc1cd9cadde2c9d91f232931c80809e90c4"
	)
	wantResolutions := []parityResolutionSnapshot{
		{RunID: "run-parity-deleted", TargetKind: "attribute", TargetKey: targetKey,
			TargetRevision: targetRevision, ResolverVersion: resolver,
			InputFingerprint:          "sha256:7faedc14ae9a8dea36a13a7411d61d7828ae243e4b2e1924fcfd57f66349b4f4",
			ProviderPolicyFingerprint: providerPolicy, ResolvedAt: resolvedAt,
			DecisionKey: "sha256:b637f53636a20671ccf95ada04df23a8ac6e33c75ff5c5d57d2fb8bae9d7fd87",
			ClaimKey:    originalClaim, Action: "retained", Reason: "evidence-unsupported",
			ScoreJSON: zeroScore, ProjectionKind: "person_attribute", ProjectionRowID: 1},
		{RunID: "run-parity-edit", TargetKind: "attribute", TargetKey: targetKey,
			TargetRevision: targetRevision, ResolverVersion: resolver,
			InputFingerprint:          editInput,
			ProviderPolicyFingerprint: providerPolicy, ResolvedAt: resolvedAt,
			DecisionKey: "sha256:bc2d8472acf0448b2bf20d05e2b324a0f91ac1264346a45e19078a5ba0404a06",
			ClaimKey:    editedClaim, Action: "applied", Reason: "applied-projection",
			ScoreJSON: appliedScore},
		{RunID: "run-parity-edit", TargetKind: "attribute", TargetKey: targetKey,
			TargetRevision: targetRevision, ResolverVersion: resolver,
			InputFingerprint:          editInput,
			ProviderPolicyFingerprint: providerPolicy, ResolvedAt: resolvedAt,
			DecisionKey: "sha256:c73212d2b952bb0991bc021ad51c6570b5b0cd983740413405761c125366f7fd",
			ClaimKey:    originalClaim, Action: "retained", Reason: "evidence-unsupported",
			ScoreJSON: zeroScore},
		{RunID: "run-parity-reimported", TargetKind: "attribute", TargetKey: targetKey,
			TargetRevision: targetRevision, ResolverVersion: resolver,
			InputFingerprint:          "sha256:0b11f64aec91f7ecefad9e41ad0d921d57d970c598a6dadd6f6d73cc4141807a",
			ProviderPolicyFingerprint: providerPolicy, ResolvedAt: resolvedAt,
			DecisionKey: "sha256:a127d52d18c4275edb485d891c335f63bd472fcd3fca8b68f30ec8b54fcd06d8",
			ClaimKey:    originalClaim, Action: "applied", Reason: "applied-projection",
			ScoreJSON: appliedScore, ProjectionKind: "person_attribute", ProjectionRowID: 2},
		{RunID: "run-parity-success", TargetKind: "attribute", TargetKey: targetKey,
			TargetRevision: targetRevision, ResolverVersion: resolver,
			InputFingerprint:          "sha256:071b0d30b9043029eb179d36e62343965768b342c89683ccaeb98325e12d2d6f",
			ProviderPolicyFingerprint: providerPolicy, ResolvedAt: resolvedAt,
			DecisionKey: "sha256:1eb79ab4c0460e34049444735f12520fe72fd715ad397540318352fe13531271",
			ClaimKey:    originalClaim, Action: "applied", Reason: "applied-projection",
			ScoreJSON: appliedScore, ProjectionKind: "person_attribute", ProjectionRowID: 1},
	}
	assert.Equal(t, wantResolutions, resolutionSnapshot)
	mutatedResolutions := slices.Clone(wantResolutions)
	mutatedResolutions[0].InputFingerprint = strings.Repeat("0", 71)
	assert.NotEqual(t, wantResolutions, mutatedResolutions,
		"resolution content mutation must be visible to the exact snapshot")
	assert.Equal(t, []string{"chat"}, parityCurrentChannel(t, f))

	oldCursor := loadPersonSweepCursor(t, f.store, f.key)
	newCursor := loadPersonSweepCursor(t, f.store, newKey)
	backstopAt := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	wantOldCursor := peoplesweep.Cursor{Key: peoplesweep.CursorKey{PersonID: f.personID,
		SourceLane:         peoplesweep.SourceConversationText,
		ProgramFingerprint: "c9f200223106e7ae9ec07d3407fecf3fcd094cf183273c187a0e75b9aebecb43",
		CatalogFingerprint: "sha256:c732ff8419310766e10bf4a47eb9960161a2a2dc53bcf98b6514f22f52c59380"},
		OptimisticSequence: 5, ReconciliationComplete: true, LastBackstopAt: &backstopAt}
	wantNewCursor := peoplesweep.Cursor{Key: peoplesweep.CursorKey{PersonID: f.personID,
		SourceLane:         peoplesweep.SourceConversationText,
		ProgramFingerprint: "c9f200223106e7ae9ec07d3407fecf3fcd094cf183273c187a0e75b9aebecb43",
		CatalogFingerprint: "sha256:0b5c18796574920f080c71c27db3681a0aa3d219ce0fa4278015b3d93a51c9f0"},
		OptimisticSequence: 6, ReconcileUpperKey: "00000000000000000001",
		ReconcileAfterKey: "00000000000000000001", ReconciliationComplete: true,
		LastBackstopAt: &backstopAt}
	assert.Equal(t, wantOldCursor, oldCursor)
	assert.Equal(t, wantNewCursor, newCursor)
	mutatedCursor := wantNewCursor
	mutatedCursor.ReconcileAfterKey = "00000000000000000000"
	assert.NotEqual(t, wantNewCursor, mutatedCursor,
		"cursor coordinate mutation must be visible to the exact snapshot")
	work := parityWorkSnapshotNow(t, f.store, f.personID)
	assert.False(t, work.Exists, "clean production apply should remove durable work")
	f.provider.mu.Lock()
	assert.Equal(t, 6, f.provider.calls)
	assert.Len(t, f.provider.wires, 6)
	wires := append([][]byte(nil), f.provider.wires...)
	f.provider.mu.Unlock()
	for index, wire := range wires {
		var reservedHash string
		if index == 0 {
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT input_hash FROM person_sweep_batches
				WHERE attempt_id = 'attempt-parity-old' AND batch_ordinal = 0`)).Scan(&reservedHash))
		} else {
			require.NoError(t, f.store.DB().QueryRowContext(t.Context(), f.store.Rebind(`
				SELECT input_hash FROM person_sweep_batches WHERE provider_request_id = ?`),
				fmt.Sprintf("parity-request-%d", index+1)).Scan(&reservedHash))
		}
		digest := sha256.Sum256(wire)
		assert.Equal(t, hex.EncodeToString(digest[:]), reservedHash,
			"durable reservation must cover exact prepared wire %d", index+1)
	}
	require.NoError(t, f.store.DB().PingContext(t.Context()))
}

func parityExpectedEvidence(t *testing.T, input personfacts.EvidenceInput, supported bool,
	reason personfacts.EvidenceStatusReason,
) parityEvidenceSnapshot {
	t.Helper()
	key, err := personfacts.EvidenceKey(input)
	require.NoError(t, err)
	latestVersion := ""
	if reason != "" {
		latestVersion = input.SourceVersion
	}
	return parityEvidenceSnapshot{Key: key, Input: input, Supported: supported,
		LatestSourceVersion: latestVersion, LatestReason: reason}
}

var _ peoplesweep.AssemblySource = (*paritySourceRecorder)(nil)
var _ peoplesweep.ClaimSink = (*paritySinkRecorder)(nil)
