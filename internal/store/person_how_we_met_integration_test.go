package store_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/peoplesweep"
	"go.kenn.io/msgvault/internal/personfacts"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

func TestHowWeMetFirstAppearanceHistoryAndPolicy(t *testing.T) {
	f := storetest.New(t)
	ctx := t.Context()
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.test", "manual"))
	owner := f.EnsureParticipant("owner@example.test", "Owner", "example.test")
	sam := f.EnsureParticipant("sam@example.test", "Sam Example", "example.test")
	samPerson, _, err := f.Store.CreatePersonFromParticipant(sam)
	require.NoError(t, err)
	alex := f.EnsureParticipant("alex@example.test", "Alex Example", "example.test")
	person, _, err := f.Store.CreatePersonFromParticipant(alex)
	require.NoError(t, err)
	_, err = f.Store.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(t, err)
	config := peoplesweep.Config{Enabled: true, Provider: peoplesweep.ProviderSelection{Name: "default"}, Providers: map[string]peoplesweep.ProviderConfig{"default": {
		Protocol: peoplesweep.ProtocolOpenAIChat, Endpoint: "https://api.example.test/v1", Model: "test", Auth: peoplesweep.AuthBearer,
		Credential: peoplesweep.CredentialEnv, CredentialEnv: "TEST_KEY", OutputMode: peoplesweep.OutputModeNativeJSONSchema,
		TokenLimitParameter: "max_completion_tokens", RetentionPosture: "zero_retention", TrainingPosture: "no_training",
		AllowedSources: []peoplesweep.SourceClass{peoplesweep.SourceConversationText}, SourceSince: "2020-01-01", RequestTimeout: time.Second,
	}}}
	config.ApplyDefaults()
	profile, err := config.Profile()
	require.NoError(t, err)
	_, err = f.Store.EnsurePersonInferenceProfile(ctx, profile)
	require.NoError(t, err)
	catalogBefore, err := f.Store.BuildPersonFactCatalogContext(ctx, profile.AllowSensitive)
	require.NoError(t, err)
	definitionBefore, err := f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugHowWeMet)
	require.NoError(t, err)
	request := peoplesweep.HowWeMetRequest{PersonIDs: []int64{person.ID}, ProviderFingerprint: profile.Fingerprint, AllowedSources: profile.AllowedSources, SourceSince: profile.SourceSince}
	values := func(history bool) []store.PersonAttributeValue {
		t.Helper()
		rows, err := f.Store.ListPersonAttributeValuesContext(ctx, person.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugHowWeMet, IncludeHistory: history})
		require.NoError(t, err)
		return rows
	}
	add := func(id string, at time.Time) int64 {
		t.Helper()
		message := f.NewMessage().WithSourceMessageID(id).WithSentAt(at).Build()
		message.SenderID = sql.NullInt64{Int64: sam, Valid: true}
		messageID, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message})
		require.NoError(t, err)
		require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "from", []int64{sam}, []string{"Sam Example"}))
		require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "to", []int64{owner, alex}, []string{"Owner", "Alex Example"}))
		return messageID
	}
	project := func() {
		t.Helper()
		projector, err := activity.NewProjector(f.Store, activity.Options{Timezone: "UTC", BatchSize: 10, MaxDirectCounterparts: 25})
		require.NoError(t, err)
		_, err = projector.RunOnce(ctx)
		require.NoError(t, err)
	}
	at := time.Date(2021, 3, 15, 12, 0, 0, 0, time.UTC)
	add("first-intro", at)
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "consent is required")
	_, _, err = f.Store.GrantPersonInferenceConsent(ctx, profile.Fingerprint, "test")
	require.NoError(t, err)
	runner := &noProviderSweepRunner{}
	idNumber := 0
	source := cursorOnlyAssemblySource{Store: f.Store}
	worker := peoplesweep.Worker{Config: config, Store: f.Store, Source: source,
		Context: peoplesweep.NewContextRetriever(f.Store), Sink: f.Store, Runner: runner,
		Catalog: f.Store, HowWeMet: f.Store, Clock: time.Now,
		NewID: func() string { idNumber++; return fmt.Sprintf("how-we-met-%d", idNumber) }, WorkerID: "how-we-met-worker"}
	result, err := worker.Run(ctx, peoplesweep.RunRequest{Kind: peoplesweep.RunManual, Mode: peoplesweep.RunIncremental, PersonID: person.ID, Limit: 1, Brief: peoplesweep.BriefModeSkip})
	require.NoError(t, err)
	assert.Zero(t, runner.calls)
	assert.Zero(t, result.Usage.Requests)
	var reservations int
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_batches`).Scan(&reservations))
	assert.Zero(t, reservations)
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM person_sweep_daily_usage`).Scan(&reservations))
	assert.Zero(t, reservations)
	initial := values(false)
	require.Len(t, initial, 1)
	assert.Equal(t, "Sam Example introduced them by email in March 2021.", *initial[0].Value.Text)
	catalogAfter, err := f.Store.BuildPersonFactCatalogContext(ctx, profile.AllowSensitive)
	require.NoError(t, err)
	definitionAfter, err := f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugHowWeMet)
	require.NoError(t, err)
	assert.Equal(t, catalogBefore.Fingerprint, catalogAfter.Fingerprint)
	assert.Equal(t, definitionBefore.Revision, definitionAfter.Revision)
	// Trace a fresh reconciliation: the worker has already filled the value.
	var trace bytes.Buffer
	previousLogger := slog.Default()
	store.ConfigureSQLLogging(store.SQLLogOptions{FullTrace: true, MaxStmtChars: 10000})
	slog.SetDefault(slog.New(slog.NewJSONHandler(&trace, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { store.ConfigureSQLLogging(store.SQLLogOptions{}); slog.SetDefault(previousLogger) })
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Contains(t, trace.String(), `"msg":"sql"`)
	assert.NotContains(t, strings.ToLower(trace.String()), "message_bodies")
	store.ConfigureSQLLogging(store.SQLLogOptions{})
	slog.SetDefault(previousLogger)
	rows := values(false)
	require.Len(t, rows, 1)
	assert.Equal(t, "Sam Example introduced them by email in March 2021.", *rows[0].Value.Text)
	assert.Equal(t, store.ProvenanceSystem, rows[0].Source)
	var revisionBefore, revisionAfter int64
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT vcard_projection_revision FROM persons WHERE id = ?`), person.ID).Scan(&revisionBefore))
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.NoError(t, f.Store.DB().QueryRowContext(ctx, f.Store.Rebind(`SELECT vcard_projection_revision FROM persons WHERE id = ?`), person.ID).Scan(&revisionAfter))
	assert.Equal(t, revisionBefore, revisionAfter)
	require.Len(t, values(true), 1)
	var target personfacts.TargetRef
	var descriptor personfacts.TargetDescriptor
	for _, item := range catalogAfter.Targets {
		if item.Key == store.AttributeUniversalIDHowWeMet {
			target = personfacts.TargetRef{Kind: item.Kind, Key: item.Key, Revision: item.Revision}
			descriptor = item
		}
	}
	require.NotEmpty(t, target.Key)
	pinned, err := f.Store.SetPersonFactPinContext(ctx, person.ID, target, true, "test")
	require.NoError(t, err)
	require.NotNil(t, pinned.State.EventID)
	older := add("older-intro", at.Add(-24*time.Hour))
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(true), 1)
	require.Len(t, values(false), 1)
	assert.Equal(t, initial[0].ID, values(false)[0].ID, "changed evidence preserves the explicit pin")
	require.NoError(t, f.Store.MarkMessageDeletedBySourceMessageID(ctx, f.Source.ID, false, "older-intro"))
	require.NoError(t, f.Store.MarkMessageDeletedBySourceMessageID(ctx, f.Source.ID, false, "first-intro"))
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	assert.Equal(t, initial[0].ID, values(false)[0].ID, "missing evidence preserves the explicit pin")
	require.NoError(t, f.Store.ClearMessageDeletedFromSourceContext(ctx, f.Source.ID, "older-intro"))
	require.NoError(t, f.Store.ClearMessageDeletedFromSourceContext(ctx, f.Source.ID, "first-intro"))
	project()
	_, err = f.Store.SetPersonFactPinContext(ctx, person.ID, target, false, "test")
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	rows = values(true)
	require.Len(t, rows, 2)
	assert.Equal(t, fmt.Sprintf("first-appearance:email_intro:%d", older), *rows[0].SourceRef)
	assert.Equal(t, *rows[0].Value.Text, *rows[1].Value.Text)
	_, err = f.Store.UpdatePersonDisplayNameContext(ctx, samPerson.ID, samPerson.Revision, new("Riley Example"))
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	rows = values(true)
	require.Len(t, rows, 3)
	assert.Equal(t, "Riley Example introduced them by email in March 2021.", *rows[0].Value.Text)
	assert.Equal(t, *rows[1].SourceRef, *rows[0].SourceRef)
	request.SourceSince = "2021-03-15"
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "the later allowed event must not replace the earliest one")
	require.Len(t, values(true), 3)
	request.SourceSince = "2021-03-14"
	request.SourceUntil = "2021-03-14"
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	current := values(false)[0]
	_, err = f.Store.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugHowWeMet, ExpectedValueID: &current.ID})
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "a manual clear remains pinned")
	_, err = f.Store.SetPersonFactPinContext(ctx, person.ID, target, false, "test")
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	_, err = f.Store.SetPersonFactPinContext(ctx, person.ID, target, true, "test")
	require.NoError(t, err)
	request.SourceSince = "2022-01-01"
	request.SourceUntil = ""
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Len(t, values(false), 1, "an explicit pin freezes our value")
	_, err = f.Store.SetPersonFactPinContext(ctx, person.ID, target, false, "test")
	require.NoError(t, err)
	_, err = f.Store.RevokePersonInferenceConsent(ctx, profile.Fingerprint, "test")
	require.NoError(t, err)
	request.SourceSince, request.SourceUntil = "2022-01-01", ""
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Len(t, values(false), 1, "revocation prevents retirement")
	_, _, err = f.Store.GrantPersonInferenceConsent(ctx, profile.Fingerprint, "test")
	require.NoError(t, err)
	definition, err := f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugHowWeMet)
	require.NoError(t, err)
	_, err = f.Store.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, store.AttributeDefinitionUpdate{IsActive: new(false)})
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Len(t, values(false), 1, "inactive definition is skipped")
	definition, err = f.Store.GetAttributeDefinitionBySlugContext(ctx, store.AttributeObjectPerson, store.AttributeSlugHowWeMet)
	require.NoError(t, err)
	_, err = f.Store.UpdateAttributeDefinitionContext(ctx, definition.ID, definition.Revision, store.AttributeDefinitionUpdate{IsActive: new(true)})
	require.NoError(t, err)
	_, err = f.Store.SetPersonTrackingContext(ctx, person.ID, false)
	require.NoError(t, err)
	request.SourceSince = "2022-01-01"
	request.SourceUntil = ""
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Len(t, values(false), 1, "untracking mid-page skips the write")
	_, err = f.Store.SetPersonTrackingContext(ctx, person.ID, true)
	require.NoError(t, err)
	request.SourceSince = "2020-01-01"
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), "calendar_event", older)
	require.NoError(t, err)
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	assert.Equal(t, "In your archive, they first appeared in a meeting in March 2021.", *values(false)[0].Value.Text)
	request.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceMeetingText}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "calendar events use the conversation lane")
	request.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), "email", older)
	require.NoError(t, err)
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	require.NoError(t, f.Store.MarkMessageDeletedBySourceMessageID(ctx, f.Source.ID, false, "older-intro"))
	require.NoError(t, f.Store.MarkMessageDeletedBySourceMessageID(ctx, f.Source.ID, false, "first-intro"))
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "removing all evidence retires our value")
	require.NoError(t, f.Store.ClearMessageDeletedFromSourceContext(ctx, f.Source.ID, "older-intro"))
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	// An explicit unpin allows the real fact projector to replace B1's value.
	now := time.Now().UTC().Add(time.Minute)
	subject := person.ID
	claim := personfacts.ProposedClaim{Target: descriptor, Relation: personfacts.RelationSupport,
		SubmittedValue: json.RawMessage(`"Met at a conference"`),
		Evidence: []personfacts.EvidenceInput{{PersonID: person.ID, SourceClass: personfacts.EvidencePublic,
			Directness: personfacts.DirectSelf, Authority: personfacts.AuthorityAuthoritative,
			SourceURL: "https://example.test/claim", SubjectPersonID: &subject, SubjectRef: "synthetic-person",
			Excerpt: "Synthetic evidence", SourceVersion: "v1", EventTime: now.Add(-time.Hour), RecordedTime: now, IdentityScore: 990}},
		Origin: personfacts.OriginExtraction, Confidence: personfacts.ConfidenceInputs{ReportedScore: 900}}
	generation := personfacts.GenerationInput{PersonID: person.ID,
		SourceCursors: []personfacts.SourceCursor{{Lane: "fixture", Start: "model", End: "model-end"}},
		ProgramID:     "fact-projection-fixture", ProgramVersion: "v1", ProgramFingerprint: strings.Repeat("a", 64),
		CatalogFingerprint: "catalog-fixture", Provider: "fixture", ProviderVersion: "v1", Model: "fixture", ModelVersion: "v1",
		ResolvedAt: now, Policy: personfacts.PolicyContext{ProviderPolicyFingerprint: "policy-v1"}, Claims: []personfacts.ProposedClaim{claim}}
	applied, err := f.Store.ApplyPersonFactGenerationContext(ctx, generation, nil)
	require.NoError(t, err)
	require.NotEmpty(t, applied.Projections)
	modelValue := values(false)
	require.Len(t, modelValue, 1)
	assert.Equal(t, "Met at a conference", *modelValue[0].Value.Text)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Equal(t, modelValue[0].ID, values(false)[0].ID, "B1 preserves the model projection")

	for _, source := range []store.Provenance{store.ProvenanceUser, store.ProvenanceArchiveObservation, store.ProvenanceExtraction, store.ProvenanceEnrichment, store.ProvenanceSystem} {
		t.Run(string(source), func(t *testing.T) {
			ctx := t.Context()
			participant := f.EnsureParticipant(string(source)+"@example.test", "Example Person", "example.test")
			other, _, err := f.Store.CreatePersonFromParticipant(participant)
			require.NoError(t, err)
			_, err = f.Store.SetPersonTrackingContext(ctx, other.ID, true)
			require.NoError(t, err)
			message := f.NewMessage().WithSourceMessageID("provenance-" + string(source)).WithSentAt(at).Build()
			message.SenderID = sql.NullInt64{Int64: sam, Valid: true}
			messageID, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message})
			require.NoError(t, err)
			require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "from", []int64{sam}, []string{"Sam Example"}))
			require.NoError(t, f.Store.ReplaceMessageRecipients(messageID, "to", []int64{owner, participant}, []string{"Owner", "Example Person"}))
			project()
			ref := "other-system:fixture"
			write, err := f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: other.ID,
				DefinitionSlug: store.AttributeSlugHowWeMet, Source: source, SourceRef: &ref,
				Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Existing account")}})
			require.NoError(t, err)
			policy := request
			policy.PersonIDs = []int64{other.ID}
			for _, since := range []string{"2020-01-01", "2022-01-01"} {
				policy.SourceSince = since
				require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, policy))
				rows, err := f.Store.ListPersonAttributeValuesContext(ctx, other.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugHowWeMet, IncludeHistory: true})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				assert.Equal(t, write.Value.ID, rows[0].ID)
				assert.Equal(t, "Existing account", *rows[0].Value.Text)
			}
		})
	}
	clockID := f.EnsureParticipant("clock@example.test", "Clock Example", "example.test")
	clockPerson, _, err := f.Store.CreatePersonFromParticipant(clockID)
	require.NoError(t, err)
	_, err = f.Store.SetPersonTrackingContext(ctx, clockPerson.ID, true)
	require.NoError(t, err)
	clockMessage := f.NewMessage().WithSourceMessageID("clock-intro").WithSentAt(at).Build()
	clockMessage.SenderID = sql.NullInt64{Int64: sam, Valid: true}
	clockMessageID, err := f.Store.PersistMessage(&store.MessagePersistData{Message: clockMessage})
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(clockMessageID, "from", []int64{sam}, []string{"Sam Example"}))
	require.NoError(t, f.Store.ReplaceMessageRecipients(clockMessageID, "to", []int64{owner, clockID}, []string{"Owner", "Clock Example"}))
	project()
	future := time.Now().UTC().Add(time.Hour)
	priorRef := "first-appearance:email_intro:123"
	_, err = f.Store.SetPersonAttributeValueContext(ctx, store.PersonAttributeValueInput{PersonID: clockPerson.ID, DefinitionSlug: store.AttributeSlugHowWeMet,
		Source: store.ProvenanceSystem, SourceRef: &priorRef, ActiveFrom: &future,
		Value: store.AttributeValue{Type: store.AttributeValueText, Text: new("Earlier sentence")}})
	require.NoError(t, err)
	clockRequest := request
	clockRequest.PersonIDs = []int64{clockPerson.ID}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, clockRequest))
	clockRequest.SourceSince = "2022-01-01"
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, clockRequest))
	clockHistory, err := f.Store.ListPersonAttributeValuesContext(ctx, clockPerson.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugHowWeMet, IncludeHistory: true})
	require.NoError(t, err)
	require.Len(t, clockHistory, 2)
	for _, value := range clockHistory {
		require.NotNil(t, value.ActiveUntil)
		assert.False(t, value.ActiveUntil.Before(value.ActiveFrom), "supersede and retirement keep valid intervals")
	}
	missingID := f.EnsureParticipant("missing@example.test", "Missing Example", "example.test")
	missing, _, err := f.Store.CreatePersonFromParticipant(missingID)
	require.NoError(t, err)
	_, err = f.Store.SetPersonTrackingContext(ctx, missing.ID, true)
	require.NoError(t, err)
	missingRequest := request
	missingRequest.PersonIDs = []int64{missing.ID}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, missingRequest))
	missingValues := func() []store.PersonAttributeValue {
		t.Helper()
		result, err := f.Store.ListPersonAttributeValuesContext(ctx, missing.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugHowWeMet})
		require.NoError(t, err)
		return result
	}
	assert.Empty(t, missingValues())
	// A model claim still projects normally when B1 had no value to write.
	missingSubject := missing.ID
	missingClaim := claim
	missingClaim.Evidence = append([]personfacts.EvidenceInput(nil), claim.Evidence...)
	missingClaim.Evidence[0].PersonID = missing.ID
	missingClaim.Evidence[0].SubjectPersonID = &missingSubject
	missingClaim.Evidence[0].SourceURL = "https://example.test/missing-claim"
	missingGeneration := generation
	missingGeneration.PersonID = missing.ID
	missingGeneration.SourceCursors = []personfacts.SourceCursor{{Lane: "fixture", Start: "missing", End: "missing-end"}}
	missingGeneration.Claims = []personfacts.ProposedClaim{missingClaim}
	missingApplied, err := f.Store.ApplyPersonFactGenerationContext(ctx, missingGeneration, nil)
	require.NoError(t, err)
	require.NotEmpty(t, missingApplied.Projections)
	require.Len(t, missingValues(), 1)
	assert.Equal(t, "Met at a conference", *missingValues()[0].Value.Text)

	observedID := f.EnsureParticipant("observed@example.test", "Observed Example", "example.test")
	observed, _, err := f.Store.CreatePersonFromParticipant(observedID)
	require.NoError(t, err)
	_, err = f.Store.SetPersonTrackingContext(ctx, observed.ID, true)
	require.NoError(t, err)
	message := f.NewMessage().WithSourceMessageID("observed-first").WithSentAt(at).Build()
	message.SenderID = sql.NullInt64{Int64: observedID, Valid: true}
	observedMessage, err := f.Store.PersistMessage(&store.MessagePersistData{Message: message})
	require.NoError(t, err)
	require.NoError(t, f.Store.ReplaceMessageRecipients(observedMessage, "from", []int64{observedID}, []string{"Observed Example"}))
	project()
	state, err := f.Store.ContactStateContext(ctx, observed.ID, at)
	require.NoError(t, err)
	require.Equal(t, store.FirstContactObserved, state.FirstContactKind)
	observedRequest := request
	observedRequest.PersonIDs = []int64{observed.ID}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, observedRequest))
	observedValues, err := f.Store.ListPersonAttributeValuesContext(ctx, observed.ID, store.PersonAttributeQuery{DefinitionSlug: store.AttributeSlugHowWeMet})
	require.NoError(t, err)
	assert.Empty(t, observedValues)

}
