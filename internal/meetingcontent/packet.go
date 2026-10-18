package meetingcontent

import (
	"encoding/json/v2"
	"errors"
	"fmt"
)

// PacketFromResult retains the bounded JSON envelope produced by Render.
func PacketFromResult(result *PacketResult) (*Packet, error) {
	if result == nil || result.Format != FormatJSON {
		return nil, errors.New("meeting packet requires a JSON result")
	}
	var packet Packet
	if err := json.Unmarshal([]byte(result.Content), &packet); err != nil {
		return nil, fmt.Errorf("decode meeting packet: %w", err)
	}
	return &packet, nil
}
