package meetingcontent

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

// decodeBland reads the versioned canonical projection beside preserved
// provider evidence. Telephone endpoints carry no ownership anchor.
func decodeBland(fields map[string]jsontext.Value) Content {
	var version int
	var content Content
	if json.Unmarshal(fields["version"], &version) != nil || version != 1 || json.Unmarshal(fields["content"], &content) != nil || content.Transcript.State == "" {
		return unavailableContent("invalid_raw")
	}
	return content
}
