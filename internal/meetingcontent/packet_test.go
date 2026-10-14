package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPacketFromResultPreservesBoundedEvidence(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	entry := Entry{Meeting: MeetingRef{MessageID: 9, Title: "Planning"}, Content: Content{Summary: Section{State: StateAvailable, Text: "Decisions"}, Transcript: Transcript{State: StateAvailable, Text: "Transcript evidence"}, ActionCoverage: CoverageUnsupported}}
	result, err := Render("synthetic-archive", []Entry{entry}, PacketOptions{Format: FormatJSON, IncludeTranscript: false, MaxBytes: 4096})
	requirements.NoError(err)
	packet, err := PacketFromResult(result)
	requirements.NoError(err)
	requirements.Len(packet.Meetings, 1)
	assertions.Equal([]int64{9}, packet.RequestedMessageIDs)
	assertions.Equal(StateOmittedByRequest, packet.Meetings[0].Content.Transcript.State)
	assertions.Equal(CoverageUnsupported, packet.Meetings[0].Content.ActionCoverage)
	assertions.Equal(result.Truncated, packet.Truncated)
}

func TestPacketFromResultRejectsNonJSON(t *testing.T) {
	assertions := assert.New(t)
	for _, result := range []*PacketResult{nil, {Format: FormatMarkdown, Content: "# Meeting"}, {Format: FormatJSON, Content: "invalid"}} {
		_, err := PacketFromResult(result)
		assertions.Error(err)
	}
}
