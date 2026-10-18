package meetingcontent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPacketFromResultRejectsNonJSON(t *testing.T) {
	assertions := assert.New(t)
	for _, result := range []*PacketResult{nil, {Format: FormatMarkdown, Content: "# Meeting"}, {Format: FormatJSON, Content: "invalid"}} {
		_, err := PacketFromResult(result)
		assertions.Error(err)
	}
}
