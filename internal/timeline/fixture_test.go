package timeline

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"testing"
	"time"

	requirepkg "github.com/stretchr/testify/require"
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
	tierEvidence map[int64][]int64
}

func newTimelineFixture(t *testing.T) *timelineFixture {
	t.Helper()
	f := storetest.New(t)
	src, err := NewStoreSources(f.Store, config.NewDefaultConfig())
	requirepkg.NoError(t, err)
	now := time.Date(2026, 10, 22, 12, 0, 0, 123456789, time.UTC)
	src.Clock = func() time.Time { return now }
	owner := f.EnsureParticipant("owner@example.com", "Owner", "example.com")
	peer := f.EnsureParticipant("alice@example.com", "Alice", "example.com")
	requirepkg.NoError(t, f.Store.AddAccountIdentity(f.Source.ID, "owner@example.com", "manual"))
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
	requirepkg.NoError(f.T, err)
	requirepkg.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
	requirepkg.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, nil))
	return id
}
func (f *timelineFixture) project() {
	f.T.Helper()
	p, err := activity.NewProjector(f.Store, activity.Options{Timezone: f.src.Location.String(), BatchSize: 10000})
	requirepkg.NoError(f.T, err)
	_, err = p.RunBackstop(f.T.Context())
	requirepkg.NoError(f.T, err)
}
func (f *timelineFixture) calendar(key, raw string) int64 {
	f.T.Helper()
	src, err := f.Store.GetOrCreateSource("gcal", "calendar@example.com")
	requirepkg.NoError(f.T, err)
	requirepkg.NoError(f.T, f.Store.UpdateSourceSyncConfig(src.ID, `{"calendar_id":"calendar@example.com","time_zone":"UTC"}`))
	var metadata struct {
		UID     string `json:"iCalUID"`
		Summary string `json:"summary"`
	}
	requirepkg.NoError(f.T, json.Unmarshal([]byte(raw), &metadata))
	title := metadata.Summary
	if title == "" {
		title = "Synthetic event"
	}
	meta, err := json.Marshal(map[string]string{"ical_uid": metadata.UID, "calendar_id": "calendar@example.com"})
	requirepkg.NoError(f.T, err)
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Metadata:     &sql.NullString{String: string(meta), Valid: true},
		Message:      &store.Message{SourceID: src.ID, SourceMessageID: key, MessageType: "calendar_event", Subject: sql.NullString{String: title, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "calendar", Title: "Synthetic event"},
		RawMIME:      []byte(raw), RawFormat: "gcal_json",
	})
	requirepkg.NoError(f.T, err)
	return id
}
func (f *timelineFixture) transcript(key, uid string, at time.Time) int64 {
	f.T.Helper()
	src, err := f.Store.GetOrCreateSource("meeting_import", "notes@example.com")
	requirepkg.NoError(f.T, err)
	raw := fmt.Sprintf(`{"schema_version":1,"recording":{},"calendar_event":{"iCalUid":%q,"startsAt":%q},"participants":[{"displayName":"Alice","handle":"alice@example.com"}]}`, uid, at.Format(time.RFC3339))
	id, err := f.Store.PersistMessage(&store.MessagePersistData{
		Message:      &store.Message{SourceID: src.ID, SourceMessageID: key, MessageType: "meeting_transcript", IsFromMe: true, Subject: sql.NullString{String: "Synthetic meeting", Valid: true}, SentAt: sql.NullTime{Time: at, Valid: true}},
		Conversation: &store.ConversationPersistData{SourceConversationID: key, ConversationType: "meeting", Title: "Synthetic meeting"},
		BodyText:     sql.NullString{String: "Synthetic notes", Valid: true}, RawMIME: []byte(raw), RawFormat: "twenty_json",
	})
	requirepkg.NoError(f.T, err)
	requirepkg.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{f.peer}, nil))
	return id
}
func (f *timelineFixture) call(at time.Time) int64 {
	f.T.Helper()
	f.serial++
	o := callhistory.Observation{RecordID: strconv.Itoa(f.serial), Version: 1, Provider: "signal", Account: "example", NativeCallID: strconv.Itoa(f.serial), Kind: "call", Direction: "incoming", Outcome: "answered", OwnerParticipation: "joined", OccurredAt: at, Precision: "instant", Participants: []callhistory.Participant{{ID: f.peer, Participation: "joined"}}, Raw: []byte(`{}`)}
	src, err := f.Store.GetOrCreateSource(callhistory.SourceType, o.SourceIdentifier())
	requirepkg.NoError(f.T, err)
	id, err := f.Store.PutCallObservationContext(f.T.Context(), src.ID, o)
	requirepkg.NoError(f.T, err)
	return id
}
func (f *timelineFixture) savePerson() int64 {
	f.T.Helper()
	p, _, err := f.Store.CreatePersonFromParticipant(f.peer)
	requirepkg.NoError(f.T, err)
	return p.ID
}
func (f *timelineFixture) tier(person int64, tier int) {
	f.T.Helper()
	if f.tierEvidence == nil {
		f.tierEvidence = map[int64][]int64{}
	}
	for _, id := range f.tierEvidence[person] {
		requirepkg.NoError(f.T, f.Store.SetMessageSentState(id, false))
	}
	f.tierEvidence[person] = nil
	var participant int64
	requirepkg.NoError(f.T, f.Store.DB().QueryRowContext(f.T.Context(), f.Store.Rebind(`SELECT participant_id FROM person_participants WHERE person_id=? ORDER BY participant_id LIMIT 1`), person).Scan(&participant))
	conv, err := f.Store.EnsureConversation(f.Source.ID, fmt.Sprintf("tier-evidence-%d", person), "Tier evidence")
	requirepkg.NoError(f.T, err)
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
		id := f.add(f.now.AddDate(0, 0, -i-1), out, func(m *store.Message) {
			m.ConversationID = conv
			m.SenderID = sql.NullInt64{Int64: sender, Valid: true}
		})
		requirepkg.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "from", []int64{sender}, nil))
		requirepkg.NoError(f.T, f.Store.ReplaceMessageRecipients(id, "to", []int64{recipient}, nil))
		f.tierEvidence[person] = append(f.tierEvidence[person], id)
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
		entries = append(entries, d.Notes...)
	}
	return entries
}

// The policy producer is staged upstream; seed its effective result explicitly.
func (f *timelineFixture) seedConversationPolicy(ctx context.Context, sourceID int64, key string, tier conversationpolicy.Tier) error {
	if err := f.Store.SetConversationPolicyOverride(ctx, sourceID, key, tier); err != nil {
		return err
	}
	_, err := f.Store.DB().ExecContext(ctx, f.Store.Rebind(`UPDATE conversation_policy SET effective_tier=? WHERE conversation_id IN (SELECT id FROM conversations WHERE source_id=? AND source_conversation_id=?)`), tier, sourceID, key)
	return err
}
