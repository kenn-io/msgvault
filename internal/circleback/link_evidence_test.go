package circleback

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/meetingidentity"
)

func TestCirclebackJoinURLKnownHostsOnly(t *testing.T) {
	const meet = "https://meet.google.com/abc-defg-hij"
	for _, tt := range []struct {
		name, url string
		known     bool
	}{
		{"meet", meet, true},
		{"zoom", "https://zoom.us/j/123456789?pwd=example", true},
		{"zoom subdomain", "https://us02web.zoom.us/j/123456789?pwd=example", true},
		{"zoom government", "https://zoomgov.com/j/123456789?pwd=example", true},
		{"zoom government subdomain", "https://example.zoomgov.com/j/123456789", true},
		{"teams", "https://teams.microsoft.com/l/meetup-join/19%3Aexample%40thread.v2/0?context=example", true},
		{"teams consumer", "https://teams.live.com/meet/123456789?p=example", true},
		{"teams government", "https://gov.teams.microsoft.us/l/meetup-join/example", true},
		{"unknown", "https://meet.example.com/planning", false},
		{"deceptive zoom", "https://evilzoom.us/j/123456789", false},
		{"deceptive zoom government", "https://evilzoomgov.com/j/123456789", false},
		{"deceptive meet", "https://meet.google.com.example.org/abc-defg-hij", false},
		{"deceptive teams", "https://teams.microsoft.com.example.org/l/meetup-join/example", false},
		{"deceptive teams government", "https://teams.microsoft.us.example.org/l/meetup-join/example", false},
		{"invalid", "://meet.google.com", false},
		{"non web scheme", "ftp://zoom.us/j/123456789", false},
		{"padded", " " + meet + "\n", true},
		{"empty", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			imp := &Importer{}
			meta := imp.buildMetadata(&Meeting{ID: "42", MeetingURL: tt.url}, nil, "recorder@example.com", "", transcriptStateUnavailable, "")
			encoded, err := json.Marshal(meta)
			require.NoError(err)
			var metadata map[string]any
			require.NoError(json.Unmarshal(encoded, &metadata))
			if tt.url != "" {
				assert.Equal(tt.url, metadata["meeting_url"])
			}
			if tt.known {
				assert.Equal(strings.TrimSpace(tt.url), metadata["join_url"])
			} else {
				assert.NotContains(metadata, "join_url")
			}
		})
	}
}

func TestCirclebackJoinURLKeepsLegacyURLPrecedence(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	imp := &Importer{}
	meta := imp.buildMetadata(&Meeting{ID: "42", URL: "https://circleback.example.org/meeting/42", MeetingURL: "https://meet.google.com/abc-defg-hij"}, nil, "recorder@example.com", "", transcriptStateUnavailable, "")
	encoded, err := json.Marshal(meta)
	require.NoError(err)
	var metadata map[string]any
	require.NoError(json.Unmarshal(encoded, &metadata))
	assert.Equal("https://circleback.example.org/meeting/42", metadata["meeting_url"])
	assert.Equal("https://meet.google.com/abc-defg-hij", metadata["join_url"])
}

func TestCirclebackUnchangedSnapshotRegeneratesMetadataOnce(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	imp, st := newTestImporter(t, &fakeSource{})
	source, err := st.GetOrCreateSource(SourceType, "recorder@example.com")
	require.NoError(err)
	const providerJSON = `{"id":42,"name":"Planning","startTime":"2026-10-13T10:00:00Z","createdAt":"2026-10-13T10:00:00Z","meetingUrl":"https://meet.google.com/abc-defg-hij"}`
	var meeting Meeting
	require.NoError(json.Unmarshal([]byte(providerJSON), &meeting))
	meeting.Raw = []byte(providerJSON)
	opts := ImportOptions{Identifier: "recorder@example.com"}
	ingest := func() (bool, int64) {
		result, err := imp.ingestMeeting(t.Context(), source.ID, opts, meetingidentity.Set{}, &meeting, nil, transcriptStateUnavailable)
		require.NoError(err)
		return result.Changed, result.MessageID
	}
	_, msgID := ingest()
	metadata := circlebackMetadataMap(t, st, msgID)
	// This hash is the version-1 snapshot of the literal provider JSON above.
	metadata["snapshot_hash"] = "64270723226208a7ce6100cea0628f1e13cfae8755372685fc2cc8763d3feff4"
	delete(metadata, "join_url")
	encoded, err := json.Marshal(metadata)
	require.NoError(err)
	require.NoError(st.SetMessageMetadata(msgID, sql.NullString{String: string(encoded), Valid: true}))
	updated, refreshedID := ingest()
	assert.True(updated, "unchanged version-1 evidence must regenerate metadata")
	assert.Equal(msgID, refreshedID)
	assert.Equal("https://meet.google.com/abc-defg-hij", circlebackMetadataMap(t, st, msgID)["join_url"])
	afterRefresh, err := st.GetMessageMetadata(msgID)
	require.NoError(err)
	updated, _ = ingest()
	assert.False(updated, "same snapshot must become a no-op after one repair")
	afterNoop, err := st.GetMessageMetadata(msgID)
	require.NoError(err)
	assert.Equal(afterRefresh, afterNoop)
}
