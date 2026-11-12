package store_test

import (
	"bytes"
	"database/sql"
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
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(true), 1)
	older := add("older-intro", at.Add(-24*time.Hour))
	project()
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	rows = values(true)
	require.Len(t, rows, 2)
	assert.Equal(t, fmt.Sprintf("first-appearance:email_intro:%d", older), *rows[0].SourceRef)
	assert.Equal(t, *rows[0].Value.Text, *rows[1].Value.Text)
	request.SourceSince = "2021-03-15"
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "the later allowed event must not replace the earliest one")
	require.Len(t, values(true), 2)
	request.SourceSince = "2021-03-14"
	request.SourceUntil = "2021-03-14"
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	current := values(false)[0]
	_, err = f.Store.SupersedePersonAttributeValueContext(ctx, store.PersonAttributeSupersedeInput{PersonID: person.ID, DefinitionSlug: store.AttributeSlugHowWeMet, ExpectedValueID: &current.ID})
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "a manual clear remains pinned")
	catalog, err := f.Store.BuildPersonFactCatalogContext(ctx, profile.AllowSensitive)
	require.NoError(t, err)
	var target personfacts.TargetRef
	for _, item := range catalog.Targets {
		if item.Key == store.AttributeUniversalIDHowWeMet {
			target = personfacts.TargetRef{Kind: item.Kind, Key: item.Key, Revision: item.Revision}
		}
	}
	require.NotEmpty(t, target.Key)
	_, err = f.Store.SetPersonFactPinContext(ctx, person.ID, target, false, "test")
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
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
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	assert.Equal(t, "You shared a meeting in March 2021.", *values(false)[0].Value.Text)
	request.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceMeetingText}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "calendar events use the conversation lane")
	_, err = f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE messages SET message_type = ? WHERE id = ?`), "meeting_transcript", older)
	require.NoError(t, err)
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	require.Len(t, values(false), 1)
	request.AllowedSources = []peoplesweep.SourceClass{peoplesweep.SourceConversationText}
	require.NoError(t, f.Store.FillPersonHowWeMetContext(ctx, request))
	assert.Empty(t, values(false), "meeting transcripts use the meeting lane")
}
