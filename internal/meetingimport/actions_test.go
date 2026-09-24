package meetingimport

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/testutil"
)

func actionRequest(t *testing.T, actions string) Request {
	t.Helper()
	body := strings.Replace(validRequestJSON, `"external_id":`, `"action_items":`+actions+`, "external_id":`, 1)
	req, err := DecodeRequest(strings.NewReader(body), MaxRequestBytes)
	require.NoError(t, err)
	return req
}

func TestMeetingActionImportPresenceAndReplacement(t *testing.T) {
	assertions := assert.New(t)
	requirements := require.New(t)
	st := testutil.NewTestStore(t)
	importer := NewImporter(st, Hooks{})
	req := actionRequest(t, `[{"title":"  Send launch brief  ","description":"Review deliverables","assignee_email":" OWNER@example.com ","status":"Awaiting review","due_date":"next Friday"}]`)
	result, err := importer.Import(t.Context(), req)
	requirements.NoError(err)
	var title, email, status, sourceStatus, due string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT title, assignee_email, status, source_status, due_date FROM meeting_action_items WHERE message_id = ?`), result.MessageID).Scan(&title, &email, &status, &sourceStatus, &due))
	assertions.Equal("Send launch brief", title)
	assertions.Equal("owner@example.com", email)
	assertions.Equal("unknown", status)
	assertions.Equal("Awaiting review", sourceStatus)
	assertions.Equal("next Friday", due)
	var body string
	requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT body_text FROM message_bodies WHERE message_id = ?`), result.MessageID).Scan(&body))
	assertions.Contains(body, "Action items:")
	assertions.Contains(body, "Review deliverables")
	assertions.Less(strings.Index(body, "Action items:"), strings.Index(body, "Transcript:"))
	matches, total, err := st.SearchMessages("deliverables", 0, 10)
	requirements.NoError(err)
	assertions.Equal(int64(1), total)
	requirements.Len(matches, 1)
	assertions.Equal(result.MessageID, matches[0].ID)

	_, err = importer.Import(t.Context(), req)
	requirements.NoError(err)
	var count int
	requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM meeting_action_items`).Scan(&count))
	assertions.Equal(1, count)

	for _, tc := range []struct {
		name, coverage string
		req            Request
	}{
		{"explicit empty", "available", actionRequest(t, `[]`)},
		{"old omitted", "unsupported", validImportRequest(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertions := assert.New(t)
			requirements := require.New(t)
			_, err := importer.Import(t.Context(), tc.req)
			requirements.NoError(err)
			var coverage string
			requirements.NoError(st.DB().QueryRow(st.Rebind(`SELECT action_coverage FROM meeting_details WHERE message_id = ?`), result.MessageID).Scan(&coverage))
			assertions.Equal(tc.coverage, coverage)
			requirements.NoError(st.DB().QueryRow(`SELECT COUNT(*) FROM meeting_action_items`).Scan(&count))
			assertions.Zero(count)
			normalized, err := tc.req.Normalize()
			requirements.NoError(err)
			snapshot, err := BuildSnapshot(normalized)
			requirements.NoError(err)
			var raw map[string]json.RawMessage
			requirements.NoError(json.Unmarshal(snapshot.Raw, &raw))
			if tc.coverage == "available" {
				assertions.JSONEq(`[]`, string(raw["action_items"]))
			} else {
				assertions.NotContains(raw, "action_items")
			}
		})
	}
}

func TestMeetingActionValidation(t *testing.T) {
	for _, tc := range []struct{ name, items string }{
		{"blank title", `[{"title":"  "}]`},
		{"long unicode title", `[{"title":"` + strings.Repeat("界", 4097) + `"}]`},
		{"long description", `[{"title":"x","description":"` + strings.Repeat("x", 65537) + `"}]`},
		{"long source id", `[{"title":"x","source_id":"` + strings.Repeat("x", 257) + `"}]`},
		{"long name", `[{"title":"x","assignee_name":"` + strings.Repeat("x", 257) + `"}]`},
		{"long due text", `[{"title":"x","due_date":"` + strings.Repeat("x", 257) + `"}]`},
		{"long status", `[{"title":"x","status":"` + strings.Repeat("x", 129) + `"}]`},
		{"display email", `[{"title":"x","assignee_email":"Owner <owner@example.com>"}]`},
		{"too many", `[` + strings.Repeat(`{"title":"x"},`, 1000) + `{"title":"x"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := actionRequest(t, tc.items).Normalize()
			require.ErrorIs(t, err, ErrValidation)
		})
	}
	req := actionRequest(t, `[{"title":"`+strings.Repeat("界", 4096)+`","status":"custom"}]`)
	_, err := req.Normalize()
	require.NoError(t, err)
}

func TestMeetingActionNormalizationRejectsInvalidUTF8(t *testing.T) {
	for _, field := range []string{"title", "description", "source_id", "assignee_name", "assignee_email", "status", "due_date"} {
		t.Run(field, func(t *testing.T) {
			req := actionRequest(t, `[{"title":"Valid"}]`)
			action := &(*req.Meeting.ActionItems)[0]
			bad := string([]byte{0xff})
			switch field {
			case "title":
				action.Title = bad
			case "description":
				action.Description = bad
			case "source_id":
				action.SourceID = bad
			case "assignee_name":
				action.AssigneeName = bad
			case "assignee_email":
				action.AssigneeEmail = bad
			case "status":
				action.Status = bad
			case "due_date":
				action.DueDate = bad
			}
			_, err := req.Normalize()
			require.ErrorIs(t, err, ErrValidation)
		})
	}
}
