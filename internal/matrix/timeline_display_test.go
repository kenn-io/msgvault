package matrix

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/archivemarker"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestImportedEncryptedMarkerDrivesTimelineDisplayAndVolume(t *testing.T) {
	s := testutil.NewTestStore(t)
	src, err := s.GetOrCreateSource(SourceType, "@archive:example.org")
	require.NoError(t, err)
	conv, err := s.EnsureConversation(src.ID, "!room:example.org", "Example room")
	require.NoError(t, err)
	client, err := mautrix.NewClient("https://example.org", id.UserID("@archive:example.org"), "token")
	require.NoError(t, err)
	imp := NewImporter(s, &Runtime{Client: client})
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	require.NoError(t, imp.persistMessage(t.Context(), src.ID, conv, &event.Event{ID: "$encrypted", Sender: "@member:example.org", Timestamp: at.UnixMilli()}, nil, archivemarker.EncryptedMessagePlaceholder, nil, &ImportSummary{}))
	ids, err := s.MessageExistsBatch(src.ID, []string{"$encrypted"})
	require.NoError(t, err)
	messageID := ids["$encrypted"]
	var snippet string
	require.NoError(t, s.DB().QueryRowContext(t.Context(), s.Rebind(`SELECT snippet FROM messages WHERE id=?`), messageID).Scan(&snippet))
	assert.Equal(t, archivemarker.EncryptedMessagePlaceholder, snippet)
	// Hydration and volume use the stored snippet even when no body is present.
	_, err = s.DB().ExecContext(t.Context(), s.Rebind(`DELETE FROM message_bodies WHERE message_id=?`), messageID)
	require.NoError(t, err)
	display, err := s.MessageDisplayContext(t.Context(), []int64{messageID})
	require.NoError(t, err)
	assert.True(t, display[messageID].Encrypted)
	assert.Empty(t, display[messageID].Snippet)
	projector, err := activity.NewProjector(s, activity.Options{Timezone: "UTC"})
	require.NoError(t, err)
	_, err = projector.RunOnce(t.Context())
	require.NoError(t, err)
	bursts, err := s.ListActivityBurstsContext(t.Context(), store.BurstQuery{FromDate: "2026-10-22", ToDate: "2026-10-22", Limit: 10, Filters: store.ActivityFilters{IncludeNoise: true}})
	require.NoError(t, err)
	require.Len(t, bursts.Bursts, 1)
	assert.Zero(t, bursts.Bursts[0].Inbound)
	assert.Zero(t, bursts.Bursts[0].Outbound)
	for _, counterpart := range bursts.Bursts[0].Counterparts {
		assert.False(t, counterpart.Counted)
	}
}
