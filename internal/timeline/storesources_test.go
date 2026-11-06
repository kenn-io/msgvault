package timeline

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/activity"
	"go.kenn.io/msgvault/internal/config"
	"go.kenn.io/msgvault/internal/store"
	"go.kenn.io/msgvault/internal/testutil"
)

func TestStoreSourcesDefaultLoadedTimezoneHint(t *testing.T) {
	st := testutil.NewTestStore(t)
	for _, tc := range []struct {
		name, body, zone string
		hint             bool
	}{
		{"omitted", "", "UTC", true},
		{"explicit UTC", "[activity]\ntimezone = \"UTC\"\n", "UTC", true},
		{"configured IANA", "[activity]\ntimezone = \"America/Vancouver\"\n", "America/Vancouver", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			requirements.NoError(os.WriteFile(path, []byte(tc.body), 0o600))
			cfg, err := config.Load(path, "")
			requirements.NoError(err)
			src, err := NewStoreSources(st, cfg)
			requirements.NoError(err)
			assertions.Equal(tc.zone, src.Location.String())
			assertions.Equal(tc.hint, src.ZoneDefault)
		})
	}
	src, err := NewStoreSources(st, config.NewDefaultConfig())
	require.NoError(t, err)
	assert.True(t, src.ZoneDefault)
}

// Stored projection policy governs full-audience reads.
func TestStoreSourcesUsesStoredProjectionPolicy(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	src, err := st.GetOrCreateSource("gmail", "owner@example.com")
	requirements.NoError(err)
	now := time.Date(2027, 3, 1, 12, 0, 0, 0, time.UTC)
	sources, err := NewStoreSources(st, config.NewDefaultConfig())
	requirements.NoError(err)
	sources.Clock = func() time.Time { return now }
	owner, err := st.EnsureParticipant("owner@example.com", "Owner", "example.com")
	requirements.NoError(err)
	peer, err := st.EnsureParticipant("peer@example.com", "Peer", "example.com")
	requirements.NoError(err)
	other, err := st.EnsureParticipant("other@example.com", "Other", "example.com")
	requirements.NoError(err)
	requirements.NoError(st.AddAccountIdentity(src.ID, "owner@example.com", "manual"))
	conversation, err := st.EnsureConversation(src.ID, "policy-audience", "Policy audience")
	requirements.NoError(err)
	id, err := st.UpsertMessage(&store.Message{SourceID: src.ID, ConversationID: conversation, SourceMessageID: "policy-audience", MessageType: "email", SentAt: sql.NullTime{Time: now.Add(-time.Hour), Valid: true}, SenderID: sql.NullInt64{Int64: peer, Valid: true}})
	requirements.NoError(err)
	requirements.NoError(st.ReplaceMessageRecipients(id, "from", []int64{peer}, nil))
	requirements.NoError(st.ReplaceMessageRecipients(id, "to", []int64{owner, other}, nil))
	projector, err := activity.NewProjector(st, activity.Options{Timezone: "UTC", MaxDirectCounterparts: 1})
	requirements.NoError(err)
	_, err = projector.RunOnce(t.Context())
	requirements.NoError(err)
	for _, includeNoise := range []bool{false, true} {
		req := dayRequest()
		req.Date, req.IncludeNoise = "2027-03-01", includeNoise
		got, err := Build(t.Context(), sources, req)
		requirements.NoError(err)
		var bursts []Entry
		for _, day := range got.Days {
			bursts = append(bursts, day.Bursts...)
		}
		if includeNoise {
			requirements.Len(bursts, 1)
			requirements.NotNil(bursts[0].Burst)
			assertions.Zero(bursts[0].Burst.Inbound)
			assertions.Zero(bursts[0].Burst.Outbound)
		} else {
			assertions.Empty(bursts)
		}
	}
}
