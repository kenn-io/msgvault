package timeline

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/callhistory"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/conversationpolicy"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil/storetest"
)

type timelineFixture struct {
	*storetest.Fixture

	src          Sources
	now          time.Time
	owner, peer  int64
	serial       int
	tierEvidence map[int64][]*store.Message
}

func newTimelineFixture(t *testing.T) *timelineFixture {
	t.Helper()
	f := storetest.New(t)
	src, err := NewStoreSources(f.Store, config.NewDefaultConfig())
	require.NoError(t, err)
	now := time.Date(2026, 10, 22, 12, 0, 0, 123456789, time.UTC)
	src.Clock = func() time.Time { return now }
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	require.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
	return &timelineFixture{Fixture: f, src: src, now: now, owner: owner, peer: peer}
}
func (f *timelineFixture) add(at time.Time, out bool, change ...func(*store.Message)) int64 {
	f.T.Helper()
	m := f.NewMessage().WithSentAt(at).WithIsFromMe(out).WithSnippet("Synthetic message").Build()
	sender, recipient := f.peer, f.owner
	if out {
		sender, recipient = f.owner, f.peer
	}
	m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
	for _, fn := range change {
		fn(m)
	}
	id, err := f.Store.UpsertMessage(m)
	require.NoError(f.T, err)
	require.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	require.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, nil))
	return id
}
func (f *timelineFixture) project() {
	f.T.Helper()
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: f.src.Location.String(), BatchSize: 10000})
	require.NoError(f.T, err)
	_, err = p.RunOnce(f.T.Context())
	require.NoError(f.T, err)
	require.NoError(f.T, f.retractColdActivity(f.T.Context()))
}
func (f *timelineFixture) calendar(key, raw string) int64 {
	f.T.Helper()
	src, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	require.NoError(f.T, err)
	require.NoError(f.T, f.Store.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	var metadata struct {
		UID     string `json:"iCalUID"`
		Summary string `json:"summary"`
	}
	require.NoError(f.T, json.Unmarshal([]byte(raw), &metadata))
	title := metadata.Summary
	if title == "" {
		title = "Synthetic event"
	}
	meta, err := json.Marshal(map[string]string{"ical_uid": metadata.UID, "calendar_id": "calendar@example.com"})
	require.NoError(f.T, err)
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Metadata:     &sql.NullString{String: string(meta), Valid: true},
		Message:      &store.Message{SourceID: src.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: title, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: "Synthetic event"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json",
	})
	require.NoError(f.T, err)
	return id
}
func (f *timelineFixture) transcript(key, uid string, at time.Time) int64 {
	f.T.Helper()
	src, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	require.NoError(f.T, err)
	raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":%q},"participants":[{"displayName":"Alice","handle":"alice@example.com"}]}`, uid, at.Format(time.RFC3339))
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: src.ID, SourceMessageID: key, MessageType: "meeting_transcript", IsFromMe: true, Subject: sql.NullString{String: "Synthetic meeting", Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting", Title: "Synthetic meeting"},
		BodyText:     sql.NullString{String: "Synthetic notes", Valid: true}, RawMIME: []byte(raw), RawFormat: "twenty_json",
	})
	require.NoError(f.T, err)
	require.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
	return id
}
func (f *timelineFixture) call(at time.Time) int64 {
	f.T.Helper()
	f.serial++
	o := callhistory.Observation{RecordID: strconv.Itoa(f.serial), Version: 1, Provider: "signal", Account: "example", NativeCallID: strconv.Itoa(f.serial), Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined", OccurredAt: at, Participants: []callhistory.Participant{{ID: f.peer, Participation: "joined"}}, Raw: []byte(`{}`)}
	src, err := f.Store.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	require.NoError(f.T, err)
	id, err := f.Store.PutCallObservationContext(f.T.Context(), src.ID, o)
	require.NoError(f.T, err)
	return id
}
func (f *timelineFixture) savePerson() int64 {
	f.T.Helper()
	p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	require.NoError(f.T, err)
	return p.ID
}
func (f *timelineFixture) tier(person int64, tier int) {
	f.T.Helper()
	if f.tierEvidence == nil {
		f.tierEvidence = map[int64][]*store.Message{}
	}
	for _, message := range f.tierEvidence[person] {
		message.IsSent = sql.NullBool{Bool: false, Valid: true}
		_, err := f.Store.UpsertMessageContext(f.T.Context(), message)
		require.NoError(f.T, err)
	}
	f.tierEvidence[person] = nil
	var participant int64
	require.NoError(f.T, f.Store.DB().QueryRowContext(f.T.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=? ORDER BY participant_id LIMIT 1`), person).Scan(&participant))
	conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("tier-evidence-%d", person), "Tier evidence")
	require.NoError(f.T, err)
	days := 6
	if tier == 2 {
		days = 3
	}
	for i := range days {
		out := i%2 == 0
		sender, recipient := participant, f.owner
		if out {
			sender, recipient = f.owner, participant
		}
		var evidence *store.Message
		id := f.add(f.now.AddDate(0, 0, -i-1), out, func(m *store.Message) {
			evidence = m
			m.ConversationID = conv
			m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		})
		require.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
		require.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, nil))
		f.tierEvidence[person] = append(f.tierEvidence[person], evidence)
	}
}

func dayRequest() Request {
	return Request{Scope: Scope{Owner: true}, Date: "2026-10-22", Format: "json", Budget: 65536}
}
func allDayEntries(t *Timeline) []Entry {
	var entries []Entry
	for _, d := range t.Days {
		entries = append(entries, d.Meetings...)
		entries = append(entries, d.Bursts...)
		entries = append(entries, d.Calls...)
	}
	return entries
}

// Seed a chat conversation and its effective cold policy.
func (f *timelineFixture) seedConversationPolicy(ctx context.Context, sourceID int64, key string) error {
	if _, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE source_id=? AND source_conversation_id=?`), sourceID, key); err != nil {
		return err
	}
	if err := f.Store.SetConversationPolicyOverride(ctx, sourceID, key, conversationpolicy.Cold); err != nil {
		return err
	}
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE conversation_policy SET effective_tier=? WHERE conversation_id IN (SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?)`), conversationpolicy.Cold, sourceID, key)
	if err != nil {
		return err
	}
	return f.retractColdActivity(ctx)
}

// Emulate the cold eligibility producer, including retraction after projection.
func (f *timelineFixture) retractColdActivity(ctx context.Context) error {
	for _, table := range []string{"activity_event_persons", "activity_events"} {
		_, err := f.Store.DB().ExecContext(ctx, `DELETE FROM `+table+` WHERE message_id IN (SELECT m.id FROM messages m JOIN conversation_policy p ON p.conversation_id=m.conversation_id WHERE p.effective_tier='cold')`)
		if err != nil {
			return err
		}
	}
	return nil
}
