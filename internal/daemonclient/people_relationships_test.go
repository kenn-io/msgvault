package daemonclient

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/peoplebrowser"
)

func TestPeopleBrowserRelationshipCadenceRoundTrip(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writePeopleBrowserJSON(t, w, http.StatusOK, fmt.Sprintf(`{"status":"ok","api_schema_version":%q}`, RelationshipCadenceMinAPISchemaVersion))
			return
		}
		assert.Equal("/api/v1/relationships", r.URL.Path)
		body := decodePeopleBrowserBody(t, r)
		assert.Equal("due", body["cadence"])
		assert.InDelta(float64(7), body["due_within_days"], 0)
		writePeopleBrowserJSON(t, w, http.StatusOK, `{"rows":[{"canonical_id":11,"display_label":"Alex Example","member_ids":[11,12],"score":1.5,"signals":{"sent_to_them":3,"received_from_them":2,"meetings_together":0,"sent_count":3,"meeting_count":0,"modalities":1,"last_interaction_at":"2026-10-01T12:00:00Z"},"last_at":"2026-10-01T12:00:00Z","person_id":7,"cadence":{"frequency_days":21,"list":"A","due_at":"2026-10-22T12:00:00Z","status":"overdue","days_overdue":3,"stale":false},"last_contact_channel":"email"}],"total_count":1,"cache_revision":"rev-1","identity_revision":4}`)
	}))
	days := 7
	page, err := engine.ListRelationships(t.Context(), peoplebrowser.RelationshipListRequest{Cadence: "due", DueWithinDays: &days})
	require.NoError(err)
	require.Len(page.Rows, 1)
	row := page.Rows[0]
	require.NotNil(row.PersonID)
	assert.Equal(int64(7), *row.PersonID)
	assert.Equal([]int64{11, 12}, row.MemberIDs)
	assert.Equal("email", row.LastContactChannel)
	require.NotNil(row.Cadence)
	assert.Equal("A", row.Cadence.List)
	assert.Equal("overdue", row.Cadence.Status)
	require.NotNil(row.Cadence.DaysOverdue)
	assert.Equal(3, *row.Cadence.DaysOverdue)
	assert.Equal(int64(1), page.TotalCount)
	assert.Equal("rev-1", page.CacheRevision)
}

func TestPeopleBrowserRelationshipCadenceRequiresCurrentSchema(t *testing.T) {
	requests := 0
	engine := newPeopleBrowserTestEngine(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health" {
			writePeopleBrowserJSON(t, w, http.StatusOK, `{"status":"ok","api_schema_version":"3.43.0"}`)
			return
		}
		requests++
		writePeopleBrowserJSON(t, w, http.StatusOK, `{"rows":[],"total_count":0,"cache_revision":"rev-1","identity_revision":1}`)
	}))
	_, err := engine.ListRelationships(t.Context(), peoplebrowser.RelationshipListRequest{Cadence: "overdue"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upgrade the daemon")
	assert.Zero(t, requests, "an old daemon must not silently ignore cadence filtering")
}
