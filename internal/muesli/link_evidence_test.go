package muesli

import (
	"encoding/json/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMuesliMetadataSeriesID(t *testing.T) {
	for _, seriesID := range []string{"source-local-series-1", ""} {
		t.Run(seriesID, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)
			meeting := sampleMeeting()
			meeting.CalendarSeriesID = seriesID
			snapshot, err := meeting.ArchiveSnapshot(1, "example", "recorder@example.com")
			require.NoError(err)
			var metadata map[string]any
			require.NoError(json.Unmarshal(snapshot.Metadata, &metadata))
			if seriesID != "" {
				assert.Equal(seriesID, metadata["calendar_series_id"])
			} else {
				assert.NotContains(metadata, "calendar_series_id")
			}
			assert.Equal("event-1", metadata["calendar_event_id"])
			assert.Equal("eventKit", metadata["calendar_source"])
			assert.NotContains(metadata, "calendar_ical_uid", "source-local IDs are not iCal UIDs")
		})
	}
}
