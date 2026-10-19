package client_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestMeetingRecordValidationAllowsPresentEmptyPacketMeetingTitle(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const payload = `{
		"action_items": [],
		"invited": [],
		"items": [{
			"basis": "",
			"content": {
				"archive_uid": "synthetic-archive",
				"meetings": [{
					"meeting": {
						"message_id": 12,
						"conversation_id": 8,
						"source_id": 2,
						"source_type": "meeting_import",
						"source_identifier": "example",
						"source_message_id": "meeting:weekly",
						"title": "",
						"occurred_at": null,
						"archive_path": "/api/v1/messages/12"
					},
					"participants": [],
					"content": {
						"summary": {"state": "available", "text": "Decision"},
						"notes": {"state": "unsupported"},
						"transcript": {"state": "omitted_by_request"},
						"actions": [],
						"action_coverage": "available",
						"duration_seconds": null
					}
				}],
				"omitted_message_ids": [],
				"requested_message_ids": [12],
				"schema_version": 1,
				"truncated": false,
				"truncations": []
			},
			"content_omitted": false,
			"deleted_from_source": false,
			"follow_up": "none",
			"message_id": 12,
			"origin": "",
			"recordings": [],
			"sent_at": "2026-10-18T00:00:00Z",
			"source_id": 2,
			"title": ""
		}],
		"owner_rsvp": "",
		"participants": [],
		"ref": "evt:12",
		"rooms": []
	}`

	var record generated.MeetingRecord
	requirements.NoError(json.Unmarshal([]byte(payload), &record))
	assertions.NoError(record.Validate())

	withoutMeetingTitle := strings.Replace(payload, `"title": "",`, "", 1)
	var missingTitle generated.MeetingRecord
	requirements.NoError(json.Unmarshal([]byte(withoutMeetingTitle), &missingTitle))
	assertions.ErrorContains(missingTitle.Validate(), "Title is required")
}

func TestMeetingRecordValidationAllowsPresentEmptyRecordingMIMEType(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	const payload = `{
		"action_items": [],
		"invited": [],
		"items": [{
			"basis": "",
			"content_omitted": false,
			"deleted_from_source": false,
			"follow_up": "none",
			"message_id": 12,
			"origin": "",
			"recordings": [{"attachment_id": 1, "mime_type": "", "size": 44}],
			"sent_at": "2026-10-18T00:00:00Z",
			"source_id": 2,
			"title": ""
		}],
		"owner_rsvp": "",
		"participants": [],
		"ref": "evt:12",
		"rooms": []
	}`

	var record generated.MeetingRecord
	requirements.NoError(json.Unmarshal([]byte(payload), &record))
	assertions.NoError(record.Validate())

	withoutMIMEType := strings.Replace(payload, `, "mime_type": "", "size": 44`, `, "size": 44`, 1)
	var missingMIMEType generated.MeetingRecord
	requirements.NoError(json.Unmarshal([]byte(withoutMIMEType), &missingMIMEType))
	assertions.ErrorContains(missingMIMEType.Validate(), "MimeType is required")
}
