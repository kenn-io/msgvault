package matrix

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/archivemarker"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

func TestImportedEncryptedMarkerDrivesTimelineDisplayAndVolume(t *testing.T) {
	requirements := require.New(t)
	assertions := assert.New(t)
	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource(SourceType, "@archive:example.org")
	requirements.NoError(err)
	requirements.NoError(s.AddAccountIdentity(src.ID, "@archive:example.org", "manual"))
	conv, err := s.EnsureConversation(src.ID, "!room:example.org", "Example room")
	requirements.NoError(err)
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`UPDATE conversations SET conversation_type='direct_chat' WHERE id=?`), conv)
	requirements.NoError(err)
	client, err := mautrix.NewClient("https://example.org", id.UserID("@archive:example.org"), "token")
	requirements.NoError(err)
	imp := NewImporter(s, &Runtime{Client: client})
	owner, err := imp.participant("@archive:example.org", "Owner")
	requirements.NoError(err)
	member, err := imp.participant("@member:example.org", "Member")
	requirements.NoError(err)
	requirements.NoError(s.EnsureConversationParticipant(conv, owner, "member"))
	requirements.NoError(s.EnsureConversationParticipant(conv, member, "member"))
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	clearAt := at.AddDate(0, 0, -1)
	clearEvent := matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.message","event_id":"$clear","sender":"@member:example.org","origin_server_ts":%d,"content":{"msgtype":"m.text","body":"An ordinary message"}}`, clearAt.UnixMilli()))
	encrypted := matrixTestEvent(t, fmt.Sprintf(`{"type":"m.room.encrypted","event_id":"$encrypted","sender":"@member:example.org","origin_server_ts":%d,"content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"synthetic-ciphertext","session_id":"synthetic-session"}}`, at.UnixMilli()))
	summary := &ImportSummary{}
	requirements.NoError(imp.persistEvent(t.Context(), src.ID, conv, clearEvent, summary))
	requirements.NoError(imp.persistEvent(t.Context(), src.ID, conv, encrypted, summary))
	ids, err := s.MessageExistsBatch(src.ID, []string{"$clear", "$encrypted"})
	requirements.NoError(err)
	messageID := ids["$encrypted"]
	requirements.NotZero(messageID)
	var snippet string
	requirements.NoError(s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT snippet FROM messages WHERE id=?`), messageID).Scan(&snippet))
	assertions.Equal(archivemarker.EncryptedMessagePlaceholder, snippet)
	// Hydration and volume use the stored marker even after the body is absent.
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`DELETE FROM message_bodies WHERE message_id=?`), messageID)
	requirements.NoError(err)
	display, err := s.MessageDisplayContext(t.Context(), []int64{messageID})
	requirements.NoError(err)
	assertions.True(display[messageID].Encrypted)
	assertions.Empty(display[messageID].Snippet)
	projector, err := activity.NewProjector(s, activity.Options{Timezone: "UTC"})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	bursts, err := s.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-21", ToDate: "2026-10-22", Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
	requirements.NoError(err)
	requirements.Len(bursts.Bursts, 2)
	assertions.Equal("2026-10-21", bursts.Bursts[0].LocalDate)
	assertions.Equal(1, bursts.Bursts[0].Inbound, "the owner-relative cleartext control must count")
	assertions.Equal("2026-10-22", bursts.Bursts[1].LocalDate)
	assertions.Zero(bursts.Bursts[1].Inbound)
	assertions.Zero(bursts.Bursts[1].Outbound)
	requirements.NotEmpty(bursts.Bursts[1].Counterparts)
	for _, counterpart := range bursts.Bursts[1].Counterparts {
		assertions.False(counterpart.Counted)
	}
	aggregate, err := s.MessageContactAggregateContext(t.Context(), store.ContactQuery{Subject: store.ActivitySubject{ParticipantIDs: []int64{member}}, Now: at, Location: time.UTC})
	requirements.NoError(err)
	assertions.Equal(1, aggregate.Count)
	requirements.NotNil(aggregate.LastAt)
	assertions.Equal(clearAt, *aggregate.LastAt, "the encrypted event must not advance last contact")
}
